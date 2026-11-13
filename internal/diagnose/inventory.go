package diagnose

import (
	"context"
	"sort"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/dynamic-resource-allocation/structured"
	"k8s.io/utils/ptr"
)

// State is a device's availability for new claims.
type State string

const (
	Free State = "Free"
	// InUseState: allocated exclusively to a claim.
	InUseState State = "InUse"
	// SharedState: one or more claims hold a share of its capacity.
	SharedState  State = "Shared"
	TaintedState State = "Tainted"
	// BlockedState: free, but its shared counters are used up by sibling
	// partitions, so it can't be allocated (fragmentation).
	BlockedState State = "Blocked"
	// UnavailableState: its pool is incomplete, so the allocator skips it.
	UnavailableState State = "Unavailable"
)

type DeviceView struct {
	Name   string   `json:"name"`
	State  State    `json:"state"`
	Owners []string `json:"owners,omitempty"`
	Detail string   `json:"detail,omitempty"`
	// Classes lists the DeviceClasses whose selectors match the device.
	Classes []string `json:"classes,omitempty"`
}

type CounterView struct {
	Set   string            `json:"set"`
	Name  string            `json:"name"`
	Total resource.Quantity `json:"total"`
	Left  resource.Quantity `json:"left"`
	// LargestFree is the biggest amount of this counter a single free
	// device could still take; it shows fragmentation.
	LargestFree *resource.Quantity `json:"largestFree,omitempty"`
}

type CapacityView struct {
	Device string            `json:"device"`
	Name   string            `json:"name"`
	Total  resource.Quantity `json:"total"`
	Used   resource.Quantity `json:"used"`
	Claims int               `json:"claims"`
}

type PoolView struct {
	Driver string `json:"driver"`
	Pool   string `json:"pool"`
	// Node is the node the pool's slices are published for, or "" for
	// pools that serve several nodes.
	Node       string         `json:"node,omitempty"`
	Incomplete string         `json:"incomplete,omitempty"`
	Devices    []DeviceView   `json:"devices"`
	Counters   []CounterView  `json:"counters,omitempty"`
	Capacities []CapacityView `json:"capacities,omitempty"`
}

// Count returns how many devices of the pool are in each state.
func (p PoolView) Count() map[State]int {
	out := map[State]int{}
	for _, d := range p.Devices {
		out[d.State]++
	}
	return out
}

// Inventory describes every pool's devices as the allocator would see them
// for a new claim.
func (d *Diagnoser) Inventory(ctx context.Context) []PoolView {
	classes, _ := d.Classes.List()
	byPool := map[string]*PoolView{}
	var order []string
	for _, s := range d.current() {
		if !d.Features.PartitionableDevices && (s.Spec.PerDeviceNodeSelection != nil || len(s.Spec.SharedCounters) > 0) {
			continue
		}
		pk := poolKey(s.Spec.Driver, s.Spec.Pool.Name)
		pv := byPool[pk]
		if pv == nil {
			pv = &PoolView{Driver: s.Spec.Driver, Pool: s.Spec.Pool.Name, Node: ptr.Deref(s.Spec.NodeName, ""), Incomplete: d.poolIncomplete()[pk]}
			byPool[pk] = pv
			order = append(order, pk)
		}
		if ptr.Deref(s.Spec.NodeName, "") != pv.Node {
			pv.Node = ""
		}
		if d.Features.PartitionableDevices && len(s.Spec.SharedCounters) > 0 {
			continue
		}
		for i := range s.Spec.Devices {
			dev := &s.Spec.Devices[i]
			pv.Devices = append(pv.Devices, d.deviceView(ctx, s, dev, classes, pv.Incomplete))
			if d.Features.ConsumableCapacity && ptr.Deref(dev.AllowMultipleAllocations, false) {
				pv.Capacities = append(pv.Capacities, d.capacityViews(s, dev)...)
			}
		}
	}

	st := d.counters()
	for k, left := range st.left {
		pv := byPool[k.pool]
		if pv == nil {
			continue
		}
		cv := CounterView{Set: k.set, Name: k.name, Left: left}
		for _, s := range d.current() {
			if poolKey(s.Spec.Driver, s.Spec.Pool.Name) != k.pool {
				continue
			}
			for _, cs := range s.Spec.SharedCounters {
				if c, ok := cs.Counters[k.name]; ok && cs.Name == k.set {
					cv.Total = c.Value
				}
			}
			// Largest amount a free, allocatable partition would take.
			for i := range s.Spec.Devices {
				dev := &s.Spec.Devices[i]
				if v := findView(pv.Devices, dev.Name); v == nil || v.State != Free {
					continue
				}
				for _, u := range uses(s.Spec.Driver, s.Spec.Pool.Name, dev) {
					if u.key == k && (cv.LargestFree == nil || u.qty.Cmp(*cv.LargestFree) > 0) {
						cv.LargestFree = ptr.To(u.qty.DeepCopy())
					}
				}
			}
		}
		pv.Counters = append(pv.Counters, cv)
	}

	out := make([]PoolView, 0, len(order))
	for _, k := range order {
		pv := byPool[k]
		SortViews(pv.Devices)
		sort.Slice(pv.Counters, func(i, j int) bool {
			if pv.Counters[i].Set != pv.Counters[j].Set {
				return naturalLess(pv.Counters[i].Set, pv.Counters[j].Set)
			}
			return pv.Counters[i].Name < pv.Counters[j].Name
		})
		out = append(out, *pv)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Node != out[j].Node {
			return out[i].Node < out[j].Node
		}
		return out[i].Pool < out[j].Pool
	})
	return out
}

func findView(vs []DeviceView, name string) *DeviceView {
	for i := range vs {
		if vs[i].Name == name {
			return &vs[i]
		}
	}
	return nil
}

// SortViews orders devices naturally by name.
func SortViews(vs []DeviceView) {
	sort.Slice(vs, func(i, j int) bool { return naturalLess(vs[i].Name, vs[j].Name) })
}

func (d *Diagnoser) deviceView(ctx context.Context, s *resourceapi.ResourceSlice, dev *resourceapi.Device, classes []*resourceapi.DeviceClass, incomplete string) DeviceView {
	id := structured.MakeDeviceID(s.Spec.Driver, s.Spec.Pool.Name, dev.Name)
	v := DeviceView{Name: dev.Name, State: Free}
	for _, c := range classes {
		if _, rejected := d.selectors(ctx, dev, s.Spec.Driver, c.Spec.Selectors, ClassSelector); !rejected {
			v.Classes = append(v.Classes, c.Name)
		}
	}
	for _, o := range d.Owners[id] {
		v.Owners = append(v.Owners, o.String())
	}
	shareable := d.Features.ConsumableCapacity && ptr.Deref(dev.AllowMultipleAllocations, false)
	switch {
	case incomplete != "":
		v.State, v.Detail = UnavailableState, incomplete
	case d.Allocated.AllocatedDevices.Has(id):
		v.State = InUseState
	case shareable && d.capacityInUse(id):
		v.State = SharedState
	}
	if v.State == Free || v.State == SharedState {
		for _, ts := range d.Taints[id] {
			if ts.Taint.Effect == resourceapi.DeviceTaintEffectNoSchedule || ts.Taint.Effect == resourceapi.DeviceTaintEffectNoExecute {
				v.State, v.Detail = TaintedState, formatTaint(ts.Taint)
				if ts.Rule != "" {
					v.Detail += " (DeviceTaintRule " + ts.Rule + ")"
				}
				break
			}
		}
	}
	if v.State == Free && len(dev.ConsumesCounters) > 0 && d.Features.CompatibilityGroups {
		if why := d.incompatible(s.Spec.Driver, s.Spec.Pool.Name, dev); why != "" {
			v.State, v.Detail = BlockedState, why
		}
	}
	if v.State == Free && len(dev.ConsumesCounters) > 0 && d.Features.PartitionableDevices {
		if short := d.counterShortfall(s.Spec.Driver, s.Spec.Pool.Name, dev); short != "" {
			v.State, v.Detail = BlockedState, short
		}
	}
	return v
}

func (d *Diagnoser) capacityViews(s *resourceapi.ResourceSlice, dev *resourceapi.Device) []CapacityView {
	id := structured.MakeDeviceID(s.Spec.Driver, s.Spec.Pool.Name, dev.Name)
	used := d.consumedSoFar(id)
	claims := 0
	for _, o := range d.Owners[id] {
		if o.Shared {
			claims++
		}
	}
	var out []CapacityView
	for name, c := range dev.Capacity {
		out = append(out, CapacityView{Device: dev.Name, Name: string(name), Total: c.Value, Used: used[name], Claims: claims})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// NodesOf returns the node names a pool serves, for pools without a single
// node, by matching its slices against the given nodes.
func (d *Diagnoser) NodesOf(p PoolView, nodes []*corev1.Node) []string {
	var out []string
	for _, n := range nodes {
		for _, s := range d.current() {
			if s.Spec.Driver != p.Driver || s.Spec.Pool.Name != p.Pool {
				continue
			}
			ok, err := structured.NodeMatches(d.Features, n, ptr.Deref(s.Spec.NodeName, ""), ptr.Deref(s.Spec.AllNodes, false), s.Spec.NodeSelector)
			if err == nil && ok {
				out = append(out, n.Name)
				break
			}
		}
	}
	return out
}

// Short returns a compact owner list like "ml/a, ml/b +3".
func Short(owners []string) string {
	if len(owners) <= 2 {
		return strings.Join(owners, ", ")
	}
	return strings.Join(owners[:2], ", ") + " +" + strconv.Itoa(len(owners)-2)
}
