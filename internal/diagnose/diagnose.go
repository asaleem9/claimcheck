// Package diagnose explains, device by device, why a set of ResourceClaims
// can or cannot be allocated on a node.
//
// The checks run in the same order as the scheduler's allocator
// (k8s.io/dynamic-resource-allocation/structured), and reuse its building
// blocks (CEL compiler, node matching, toleration matching), so each device
// is attributed to the first check that would reject it. Whether the claims
// fit at all is decided by package verdict; callers compare both answers.
package diagnose

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/dynamic-resource-allocation/cel"
	"k8s.io/dynamic-resource-allocation/resourceclaim"
	"k8s.io/dynamic-resource-allocation/structured"
	"k8s.io/utils/ptr"

	"github.com/asaleem9/claimcheck/internal/snapshot"
)

// Reason is the first check a device failed for a request.
type Reason string

const (
	Available       Reason = "Available"
	ClassSelector   Reason = "ClassSelector"
	RequestSelector Reason = "RequestSelector"
	SelectorError   Reason = "SelectorError"
	DeviceNode      Reason = "DeviceNodeMismatch"
	InUse           Reason = "InUse"
	Tainted         Reason = "Tainted"
	FeatureDisabled Reason = "FeatureDisabled"
)

// Device is one device's outcome for one request.
type Device struct {
	Driver string `json:"driver"`
	Pool   string `json:"pool"`
	Name   string `json:"name"`
	Reason Reason `json:"reason"`
	// Selector is the index of the failing selector, for selector reasons.
	Selector   *int   `json:"selector,omitempty"`
	Expression string `json:"expression,omitempty"`
	// Detail is human-readable evidence: the attribute values the failing
	// selector looked at, the claim holding the device, or the taint.
	Detail string `json:"detail,omitempty"`
}

// ID returns the device's driver/pool/name triple.
func (d Device) ID() structured.DeviceID { return structured.MakeDeviceID(d.Driver, d.Pool, d.Name) }

// Request is the outcome for one request (or one firstAvailable alternative).
type Request struct {
	Claim          string                           `json:"claim"`
	Name           string                           `json:"name"`
	Class          string                           `json:"class"`
	Mode           resourceapi.DeviceAllocationMode `json:"mode"`
	Count          int64                            `json:"count,omitempty"`
	AdminAccess    bool                             `json:"adminAccess,omitempty"`
	ClassSelectors []string                         `json:"classSelectors,omitempty"`
	Selectors      []string                         `json:"selectors,omitempty"`

	// Needed is how many devices the request needs on this node, Available
	// how many passed every check.
	Needed    int  `json:"needed"`
	Available int  `json:"available"`
	Satisfied bool `json:"satisfied"`
	// Problem is a request-level reason, e.g. a missing DeviceClass.
	Problem string `json:"problem,omitempty"`
	// Fatal means the allocator fails with an error rather than "no fit",
	// e.g. for a missing DeviceClass.
	Fatal   bool     `json:"fatal,omitempty"`
	Devices []Device `json:"devices,omitempty"`

	// Alternatives holds firstAvailable subrequests in priority order;
	// Chosen is the first satisfiable one, or -1.
	Alternatives []Request `json:"alternatives,omitempty"`
	Chosen       int       `json:"chosen,omitempty"`
}

// Node is the diagnosis for one node.
type Node struct {
	Node     string    `json:"node"`
	Requests []Request `json:"requests"`
	// Fits is the diagnosis' own conclusion; compare with the verdict.
	Fits bool `json:"fits"`
	// Contention is set when each request could be met on its own but not
	// all of them at once, because they compete for the same devices.
	Contention string `json:"contention,omitempty"`
	// Unanalyzed lists claim features the diagnosis doesn't model yet, which
	// is the likely cause if it disagrees with the verdict.
	Unanalyzed []string `json:"unanalyzed,omitempty"`
}

// Diagnoser holds the cluster state shared by all nodes.
type Diagnoser struct {
	Features  structured.Features
	Classes   structured.DeviceClassLister
	Slices    []*resourceapi.ResourceSlice
	Allocated structured.AllocatedState
	Owners    map[structured.DeviceID][]snapshot.Owner
	Taints    map[structured.DeviceID][]snapshot.TaintSource
	CEL       *cel.Cache

	latestGen map[string]int64
}

type candidate struct {
	slice *resourceapi.ResourceSlice
	index int
	// nodeOK is false for per-device node selection that excludes this node.
	nodeOK bool
}

// Diagnose explains the given claims on one node. All claims are considered
// together because a pod's claims must be allocated together.
func (d *Diagnoser) Diagnose(ctx context.Context, node *corev1.Node, claims []*resourceapi.ResourceClaim) (Node, error) {
	out := Node{Node: node.Name, Fits: true}
	devices, err := d.devicesFor(node)
	if err != nil {
		return out, err
	}
	for _, claim := range claims {
		name := claim.Namespace + "/" + claim.Name
		for _, req := range claim.Spec.Devices.Requests {
			var r Request
			switch {
			case req.Exactly != nil:
				r = d.request(ctx, name, req.Name, accessorFromExact(req.Exactly), devices)
			case len(req.FirstAvailable) > 0:
				r = Request{Claim: name, Name: req.Name, Chosen: -1}
				for _, sub := range req.FirstAvailable {
					alt := d.request(ctx, name, req.Name+"/"+sub.Name, accessorFromSub(&sub), devices)
					r.Alternatives = append(r.Alternatives, alt)
					if alt.Satisfied && r.Chosen < 0 {
						r.Chosen = len(r.Alternatives) - 1
					}
				}
				r.Satisfied = r.Chosen >= 0
				if !r.Satisfied {
					r.Problem = fmt.Sprintf("none of the %d alternatives can be satisfied", len(r.Alternatives))
				}
			default:
				r = Request{Claim: name, Name: req.Name, Problem: "request has neither exactly nor firstAvailable"}
			}
			if !r.Satisfied {
				out.Fits = false
			}
			out.Requests = append(out.Requests, r)
		}
		for _, c := range claim.Spec.Devices.Constraints {
			switch {
			case c.MatchAttribute != nil:
				out.Unanalyzed = append(out.Unanalyzed, fmt.Sprintf("%s: matchAttribute %s", name, *c.MatchAttribute))
			case c.DistinctAttribute != nil:
				out.Unanalyzed = append(out.Unanalyzed, fmt.Sprintf("%s: distinctAttribute %s", name, *c.DistinctAttribute))
			}
		}
	}
	if out.Fits {
		out.Contention = resolve(out.Requests)
		out.Fits = out.Contention == ""
	}
	return out, nil
}

// maxCombinations bounds the search over firstAvailable alternatives.
const maxCombinations = 4096

// resolve picks one alternative per firstAvailable request such that all
// requests can be served with distinct devices. Like the allocator, it
// prefers earlier alternatives and backtracks to later ones when devices
// collide. It sets Chosen on each request and returns "" on success, or a
// description of the contention for the preferred combination.
func resolve(reqs []Request) string {
	var choices [][]int // per request: satisfiable alternative indices
	var multi []int     // indices of requests with alternatives
	combos := 1
	for i, r := range reqs {
		if len(r.Alternatives) == 0 {
			continue
		}
		var sat []int
		for a, alt := range r.Alternatives {
			if alt.Satisfied {
				sat = append(sat, a)
			}
		}
		choices = append(choices, sat)
		multi = append(multi, i)
		combos *= len(sat)
	}
	preferred := contention(reqs)
	if preferred == "" || len(multi) == 0 || combos > maxCombinations {
		return preferred
	}
	pos := make([]int, len(multi))
	for {
		for k, i := range multi {
			reqs[i].Chosen = choices[k][pos[k]]
		}
		if contention(reqs) == "" {
			return ""
		}
		// Advance in lexicographic order: last request varies fastest, so
		// earlier requests keep their preferred alternative the longest.
		k := len(pos) - 1
		for k >= 0 {
			pos[k]++
			if pos[k] < len(choices[k]) {
				break
			}
			pos[k] = 0
			k--
		}
		if k < 0 {
			break
		}
	}
	for k, i := range multi {
		reqs[i].Chosen = choices[k][0]
	}
	return preferred
}

// devicesFor returns the devices the allocator would consider for node, the
// same way GatherPools selects slices: current pool generation only,
// node-local or shared slices that match the node.
func (d *Diagnoser) devicesFor(node *corev1.Node) ([]candidate, error) {
	if d.latestGen == nil {
		d.latestGen = map[string]int64{}
		for _, s := range d.Slices {
			k := s.Spec.Driver + "/" + s.Spec.Pool.Name
			d.latestGen[k] = max(d.latestGen[k], s.Spec.Pool.Generation)
		}
	}
	var out []candidate
	for _, s := range d.Slices {
		if s.Spec.Pool.Generation < d.latestGen[s.Spec.Driver+"/"+s.Spec.Pool.Name] {
			continue
		}
		perDevice := ptr.Deref(s.Spec.PerDeviceNodeSelection, false)
		if !d.Features.PartitionableDevices && (s.Spec.PerDeviceNodeSelection != nil || len(s.Spec.SharedCounters) > 0) {
			continue
		}
		nodeName, allNodes := ptr.Deref(s.Spec.NodeName, ""), ptr.Deref(s.Spec.AllNodes, false)
		switch {
		case nodeName != "" || allNodes || s.Spec.NodeSelector != nil:
			ok, err := structured.NodeMatches(d.Features, node, nodeName, allNodes, s.Spec.NodeSelector)
			if err != nil {
				return nil, fmt.Errorf("slice %s: %w", s.Name, err)
			}
			if !ok {
				continue
			}
			for i := range s.Spec.Devices {
				out = append(out, candidate{slice: s, index: i, nodeOK: true})
			}
		case perDevice:
			for i, dev := range s.Spec.Devices {
				ok, err := structured.NodeMatches(d.Features, node, ptr.Deref(dev.NodeName, ""), ptr.Deref(dev.AllNodes, false), dev.NodeSelector)
				if err != nil {
					return nil, fmt.Errorf("slice %s device %s: %w", s.Name, dev.Name, err)
				}
				out = append(out, candidate{slice: s, index: i, nodeOK: ok})
			}
		}
	}
	return out, nil
}

// accessor abstracts over ExactDeviceRequest and DeviceSubRequest.
type accessor struct {
	class       string
	selectors   []resourceapi.DeviceSelector
	mode        resourceapi.DeviceAllocationMode
	count       int64
	adminAccess bool
	tolerations []resourceapi.DeviceToleration
}

func accessorFromExact(r *resourceapi.ExactDeviceRequest) accessor {
	return accessor{r.DeviceClassName, r.Selectors, r.AllocationMode, r.Count, ptr.Deref(r.AdminAccess, false), r.Tolerations}
}

func accessorFromSub(r *resourceapi.DeviceSubRequest) accessor {
	return accessor{r.DeviceClassName, r.Selectors, r.AllocationMode, r.Count, false, r.Tolerations}
}

func (d *Diagnoser) request(ctx context.Context, claim, name string, a accessor, devices []candidate) Request {
	r := Request{Claim: claim, Name: name, Class: a.class, Mode: a.mode, Count: a.count, AdminAccess: a.adminAccess}
	if r.Mode == "" {
		r.Mode = resourceapi.DeviceAllocationModeExactCount
	}
	if r.Mode == resourceapi.DeviceAllocationModeExactCount && r.Count == 0 {
		r.Count = 1
	}
	for _, s := range a.selectors {
		r.Selectors = append(r.Selectors, s.CEL.Expression)
	}
	class, err := d.Classes.Get(a.class)
	if err != nil {
		r.Problem = fmt.Sprintf("DeviceClass %q not found", a.class)
		r.Fatal = true
		return r
	}
	for _, s := range class.Spec.Selectors {
		r.ClassSelectors = append(r.ClassSelectors, s.CEL.Expression)
	}

	selectable := 0
	for _, c := range devices {
		dev := d.device(ctx, c, class, a)
		if dev.Reason == Available || dev.Reason == InUse || dev.Reason == Tainted || dev.Reason == FeatureDisabled {
			selectable++
		}
		if dev.Reason == Available {
			r.Available++
		}
		r.Devices = append(r.Devices, dev)
	}

	switch r.Mode {
	case resourceapi.DeviceAllocationModeAll:
		r.Needed = selectable
		r.Satisfied = selectable > 0 && r.Available == selectable
		switch {
		case selectable == 0:
			r.Problem = "allocationMode All needs at least one matching device"
		case !r.Satisfied:
			r.Problem = fmt.Sprintf("allocationMode All needs all %d matching devices, %d are usable", selectable, r.Available)
		}
	default:
		r.Needed = int(r.Count)
		r.Satisfied = r.Available >= r.Needed
		if !r.Satisfied {
			r.Problem = fmt.Sprintf("needs %d, %d usable", r.Needed, r.Available)
		}
	}
	return r
}

func (d *Diagnoser) device(ctx context.Context, c candidate, class *resourceapi.DeviceClass, a accessor) Device {
	dev := &c.slice.Spec.Devices[c.index]
	out := Device{Driver: c.slice.Spec.Driver, Pool: c.slice.Spec.Pool.Name, Name: dev.Name}
	id := out.ID()

	if !d.Features.DeviceBindingAndStatus && len(dev.BindingConditions) > 0 {
		out.Reason, out.Detail = FeatureDisabled, "device has binding conditions but DRADeviceBindingConditions is disabled"
		return out
	}
	if r, ok := d.selectors(ctx, dev, out.Driver, class.Spec.Selectors, ClassSelector); ok {
		r.Driver, r.Pool, r.Name = out.Driver, out.Pool, out.Name
		return r
	}
	if r, ok := d.selectors(ctx, dev, out.Driver, a.selectors, RequestSelector); ok {
		r.Driver, r.Pool, r.Name = out.Driver, out.Pool, out.Name
		return r
	}
	if !c.nodeOK {
		out.Reason, out.Detail = DeviceNode, "device's own node selection excludes this node"
		return out
	}

	shareable := d.Features.ConsumableCapacity && ptr.Deref(dev.AllowMultipleAllocations, false)
	if !a.adminAccess && !shareable && structured.IsDeviceAllocated(id, &d.Allocated) {
		out.Reason = InUse
		var owners []string
		for _, o := range d.Owners[id] {
			owners = append(owners, o.String())
		}
		out.Detail = "allocated to " + strings.Join(owners, ", ")
		return out
	}
	if len(c.slice.Spec.SkipNodeOperations) > 0 && !d.Features.OptionalNodeOperations {
		out.Reason, out.Detail = FeatureDisabled, "slice skips node operations but DRAOptionalNodeOperations is disabled"
		return out
	}
	if len(dev.ConsumesCounters) > 0 && !d.Features.PartitionableDevices {
		out.Reason, out.Detail = FeatureDisabled, "device consumes counters but DRAPartitionableDevices is disabled"
		return out
	}
	if d.Features.DeviceTaints {
		for _, ts := range d.Taints[id] {
			t := ts.Taint
			if t.Effect != resourceapi.DeviceTaintEffectNoSchedule && t.Effect != resourceapi.DeviceTaintEffectNoExecute {
				continue
			}
			if tolerated(t, a.tolerations) {
				continue
			}
			out.Reason = Tainted
			out.Detail = formatTaint(t)
			if ts.Rule != "" {
				out.Detail += " (DeviceTaintRule " + ts.Rule + ")"
			}
			return out
		}
	}
	out.Reason = Available
	return out
}

// selectors evaluates CEL selectors in order and reports the first that
// rejects the device. ok is false when all selectors match.
func (d *Diagnoser) selectors(ctx context.Context, dev *resourceapi.Device, driver string, sels []resourceapi.DeviceSelector, reason Reason) (Device, bool) {
	for i, s := range sels {
		if s.CEL == nil {
			continue
		}
		expr := d.CEL.GetOrCompile(s.CEL.Expression)
		out := Device{Selector: ptr.To(i), Expression: s.CEL.Expression}
		if expr.Error != nil {
			out.Reason, out.Detail = SelectorError, "CEL compile error: "+expr.Error.Error()
			return out, true
		}
		match, _, err := expr.DeviceMatches(ctx, cel.Device{
			Driver:                   driver,
			AllowMultipleAllocations: dev.AllowMultipleAllocations,
			Attributes:               dev.Attributes,
			Capacity:                 dev.Capacity,
		})
		if err != nil {
			out.Reason, out.Detail = SelectorError, ShortCELError(cel.EnhanceRuntimeError(err).Error())
			return out, true
		}
		if !match {
			out.Reason, out.Detail = reason, Referenced(s.CEL.Expression, dev, driver)
			return out, true
		}
	}
	return Device{}, false
}

func tolerated(t resourceapi.DeviceTaint, tols []resourceapi.DeviceToleration) bool {
	for _, tol := range tols {
		if resourceclaim.ToleratesTaint(tol, t) {
			return true
		}
	}
	return false
}

func formatTaint(t resourceapi.DeviceTaint) string {
	s := t.Key
	if t.Value != "" {
		s += "=" + t.Value
	}
	return "taint " + s + ":" + string(t.Effect)
}

// contention checks whether all satisfied requests can be met at the same
// time with distinct devices, using bipartite matching between the devices
// each request needs and the devices usable for it.
func contention(reqs []Request) string {
	type slot struct {
		request string
		options []structured.DeviceID
	}
	var slots []slot
	for _, r := range reqs {
		if len(r.Alternatives) > 0 && r.Chosen >= 0 {
			r = r.Alternatives[r.Chosen]
		}
		if r.AdminAccess {
			continue
		}
		var usable []structured.DeviceID
		for _, dv := range r.Devices {
			if dv.Reason == Available {
				usable = append(usable, dv.ID())
			}
		}
		if r.Mode == resourceapi.DeviceAllocationModeAll {
			for _, id := range usable {
				slots = append(slots, slot{r.Name, []structured.DeviceID{id}})
			}
			continue
		}
		for range r.Needed {
			slots = append(slots, slot{r.Name, usable})
		}
	}
	if len(slots) < 2 {
		return ""
	}
	owner := map[structured.DeviceID]int{}
	var try func(i int, seen map[structured.DeviceID]bool) bool
	try = func(i int, seen map[structured.DeviceID]bool) bool {
		for _, id := range slots[i].options {
			if seen[id] {
				continue
			}
			seen[id] = true
			if j, taken := owner[id]; !taken || try(j, seen) {
				owner[id] = i
				return true
			}
		}
		return false
	}
	matched := 0
	for i := range slots {
		if try(i, map[structured.DeviceID]bool{}) {
			matched++
		}
	}
	if matched == len(slots) {
		return ""
	}
	// Name the requests that couldn't all be served and the devices they share.
	var names, devices []string
	seenReq, seenDev := map[string]bool{}, map[structured.DeviceID]bool{}
	needed := 0
	for _, s := range slots {
		if !seenReq[s.request] {
			seenReq[s.request] = true
			names = append(names, strconv.Quote(s.request))
		}
		needed++
		for _, id := range s.options {
			if !seenDev[id] {
				seenDev[id] = true
				devices = append(devices, id.Device.String())
			}
		}
	}
	sort.Slice(devices, func(i, j int) bool { return naturalLess(devices[i], devices[j]) })
	return fmt.Sprintf("%s need %d devices between them but can only use %d: %s",
		strings.Join(names, " and "), needed, len(devices), strings.Join(devices, ", "))
}

// ShortCELError drops the advice the CEL library appends to runtime errors
// ("consider using CEL optional chaining..."), which claimcheck prints once
// as a hint instead of on every device.
func ShortCELError(msg string) string {
	if i := strings.Index(msg, ". consider "); i >= 0 {
		return msg[:i]
	}
	return msg
}

// SortDevices orders devices by pool then name, for stable output.
func SortDevices(ds []Device) {
	sort.Slice(ds, func(i, j int) bool {
		if ds[i].Pool != ds[j].Pool {
			return ds[i].Pool < ds[j].Pool
		}
		return naturalLess(ds[i].Name, ds[j].Name)
	})
}

// naturalLess sorts "gpu-2" before "gpu-10".
func naturalLess(a, b string) bool {
	for a != "" && b != "" {
		ai, bi := digitPrefix(a), digitPrefix(b)
		if ai > 0 && bi > 0 {
			na, nb := strings.TrimLeft(a[:ai], "0"), strings.TrimLeft(b[:bi], "0")
			if len(na) != len(nb) {
				return len(na) < len(nb)
			}
			if na != nb {
				return na < nb
			}
			a, b = a[ai:], b[bi:]
			continue
		}
		if a[0] != b[0] {
			return a[0] < b[0]
		}
		a, b = a[1:], b[1:]
	}
	return len(a) < len(b)
}

func digitPrefix(s string) int {
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	return i
}
