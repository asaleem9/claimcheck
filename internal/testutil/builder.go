// Package testutil builds small, readable DRA cluster snapshots for tests.
package testutil

import (
	"fmt"
	"sort"

	corev1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	"github.com/asaleem9/claimcheck/internal/snapshot"
)

const Driver = "gpu.example.com"

// GPU describes one fake device.
type GPU struct {
	Name   string
	Model  string
	Memory string
	Extra  map[string]string // extra string attributes
	Taints []resourceapi.DeviceTaint
}

// B builds a snapshot fluently.
type B struct{ S *snapshot.Snapshot }

func New() *B {
	return &B{S: &snapshot.Snapshot{APIVersion: snapshot.APIVersion, Kind: snapshot.Kind}}
}

// Node adds a node with labels.
func (b *B) Node(name string, labels map[string]string) *B {
	if labels == nil {
		labels = map[string]string{}
	}
	labels["kubernetes.io/hostname"] = name
	b.S.Nodes = append(b.S.Nodes, corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}})
	return b
}

// GPUClass adds the DeviceClass every test uses.
func (b *B) GPUClass() *B {
	return b.Class("gpu.example.com", fmt.Sprintf("device.driver == %q", Driver))
}

func (b *B) Class(name string, selectors ...string) *B {
	c := resourceapi.DeviceClass{ObjectMeta: metav1.ObjectMeta{Name: name}}
	for _, s := range selectors {
		c.Spec.Selectors = append(c.Spec.Selectors, resourceapi.DeviceSelector{CEL: &resourceapi.CELDeviceSelector{Expression: s}})
	}
	b.S.DeviceClasses = append(b.S.DeviceClasses, c)
	return b
}

// Slice publishes GPUs on a node, one pool per node like most drivers do.
func (b *B) Slice(node string, gpus ...GPU) *B {
	sl := resourceapi.ResourceSlice{
		ObjectMeta: metav1.ObjectMeta{Name: node + "-" + Driver},
		Spec: resourceapi.ResourceSliceSpec{
			Driver:   Driver,
			NodeName: ptr.To(node),
			Pool:     resourceapi.ResourcePool{Name: node, ResourceSliceCount: 1},
		},
	}
	for _, g := range gpus {
		d := resourceapi.Device{
			Name: g.Name,
			Attributes: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
				"model": {StringValue: ptr.To(g.Model)},
			},
			Capacity: map[resourceapi.QualifiedName]resourceapi.DeviceCapacity{
				"memory": {Value: resource.MustParse(g.Memory)},
			},
			Taints: g.Taints,
		}
		for k, v := range g.Extra {
			d.Attributes[resourceapi.QualifiedName(k)] = resourceapi.DeviceAttribute{StringValue: ptr.To(v)}
		}
		sl.Spec.Devices = append(sl.Spec.Devices, d)
	}
	b.S.ResourceSlices = append(b.S.ResourceSlices, sl)
	return b
}

// Req is a compact request description.
type Req struct {
	Name        string
	Class       string
	Selectors   []string
	Count       int64
	All         bool
	Tolerations []resourceapi.DeviceToleration
	// FirstAvailable, when set, turns the request into a prioritized list.
	FirstAvailable []Req
	// Derived maps derived attribute names to CEL expressions.
	Derived map[string]string
}

// Claim adds an unallocated claim.
func (b *B) Claim(ns, name string, reqs ...Req) *B {
	c := resourceapi.ResourceClaim{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, UID: types.UID("uid-" + ns + "-" + name)}}
	for _, r := range reqs {
		c.Spec.Devices.Requests = append(c.Spec.Devices.Requests, request(r))
	}
	b.S.ResourceClaims = append(b.S.ResourceClaims, c)
	return b
}

// DistinctAttribute adds a distinctAttribute constraint to the last claim.
func (b *B) DistinctAttribute(attr string, requests ...string) *B {
	c := &b.S.ResourceClaims[len(b.S.ResourceClaims)-1]
	c.Spec.Devices.Constraints = append(c.Spec.Devices.Constraints, resourceapi.DeviceConstraint{
		Requests:          requests,
		DistinctAttribute: ptr.To(resourceapi.FullyQualifiedName(attr)),
	})
	return b
}

// MatchAttribute adds a matchAttribute constraint to the last claim.
func (b *B) MatchAttribute(attr string, requests ...string) *B {
	c := &b.S.ResourceClaims[len(b.S.ResourceClaims)-1]
	c.Spec.Devices.Constraints = append(c.Spec.Devices.Constraints, resourceapi.DeviceConstraint{
		Requests:       requests,
		MatchAttribute: ptr.To(resourceapi.FullyQualifiedName(attr)),
	})
	return b
}

func request(r Req) resourceapi.DeviceRequest {
	if r.Class == "" {
		r.Class = "gpu.example.com"
	}
	if len(r.FirstAvailable) > 0 {
		dr := resourceapi.DeviceRequest{Name: r.Name}
		for _, s := range r.FirstAvailable {
			ex := exact(s)
			dr.FirstAvailable = append(dr.FirstAvailable, resourceapi.DeviceSubRequest{
				Name: s.Name, DeviceClassName: ex.DeviceClassName, Selectors: ex.Selectors,
				AllocationMode: ex.AllocationMode, Count: ex.Count, Tolerations: ex.Tolerations,
			})
		}
		return dr
	}
	return resourceapi.DeviceRequest{Name: r.Name, Exactly: exact(r)}
}

func exact(r Req) *resourceapi.ExactDeviceRequest {
	if r.Class == "" {
		r.Class = "gpu.example.com"
	}
	ex := &resourceapi.ExactDeviceRequest{DeviceClassName: r.Class, Tolerations: r.Tolerations}
	if r.All {
		ex.AllocationMode = resourceapi.DeviceAllocationModeAll
	} else {
		ex.AllocationMode = resourceapi.DeviceAllocationModeExactCount
		ex.Count = max(r.Count, 1)
	}
	for _, s := range r.Selectors {
		ex.Selectors = append(ex.Selectors, resourceapi.DeviceSelector{CEL: &resourceapi.CELDeviceSelector{Expression: s}})
	}
	for name, expr := range r.Derived {
		ex.DerivedAttributes = append(ex.DerivedAttributes, resourceapi.DeviceDerivedAttribute{
			Name: resourceapi.FullyQualifiedName(name), Expression: expr,
		})
	}
	return ex
}

// Allocated adds a claim that already holds the given devices on node.
func (b *B) Allocated(ns, name, node string, devices ...string) *B {
	c := resourceapi.ResourceClaim{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, UID: types.UID("uid-" + ns + "-" + name)}}
	c.Spec.Devices.Requests = []resourceapi.DeviceRequest{{Name: "gpu", Exactly: &resourceapi.ExactDeviceRequest{
		DeviceClassName: "gpu.example.com", AllocationMode: resourceapi.DeviceAllocationModeExactCount, Count: int64(len(devices)),
	}}}
	alloc := &resourceapi.AllocationResult{NodeSelector: &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{
		MatchFields: []corev1.NodeSelectorRequirement{{Key: "metadata.name", Operator: corev1.NodeSelectorOpIn, Values: []string{node}}},
	}}}}
	for _, d := range devices {
		alloc.Devices.Results = append(alloc.Devices.Results, resourceapi.DeviceRequestAllocationResult{
			Request: "gpu", Driver: Driver, Pool: node, Device: d,
		})
	}
	c.Status.Allocation = alloc
	b.S.ResourceClaims = append(b.S.ResourceClaims, c)
	return b
}

// Pod adds a pod that references existing claims by name.
func (b *B) Pod(ns, name string, nodeSelector map[string]string, claims ...string) *B {
	p := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, UID: types.UID("uid-pod-" + name)},
		Spec:       corev1.PodSpec{NodeSelector: nodeSelector},
		Status:     corev1.PodStatus{Phase: corev1.PodPending},
	}
	for _, c := range claims {
		p.Spec.ResourceClaims = append(p.Spec.ResourceClaims, corev1.PodResourceClaim{Name: c, ResourceClaimName: ptr.To(c)})
	}
	b.S.Pods = append(b.S.Pods, p)
	return b
}

// TaintRule adds a DeviceTaintRule for all devices of the test driver on a pool.
func (b *B) TaintRule(name, pool string, t resourceapi.DeviceTaint) *B {
	b.S.DeviceTaintRules = append(b.S.DeviceTaintRules, resourceapi.DeviceTaintRule{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: resourceapi.DeviceTaintRuleSpec{
			DeviceSelector: &resourceapi.DeviceTaintSelector{Driver: ptr.To(Driver), Pool: ptr.To(pool)},
			Taint:          t,
		},
	})
	return b
}

// Standard returns a two-worker cluster: worker-a has 4×40Gi GPUs, worker-b
// has 2×80Gi and 2×40Gi. The control-plane node has no devices.
func Standard() *B {
	return New().
		Node("control-plane", map[string]string{"node-role.kubernetes.io/control-plane": ""}).
		Node("worker-a", map[string]string{"pool": "general"}).
		Node("worker-b", map[string]string{"pool": "training"}).
		GPUClass().
		Slice("worker-a",
			GPU{Name: "gpu-0", Model: "L40S", Memory: "40Gi"},
			GPU{Name: "gpu-1", Model: "L40S", Memory: "40Gi"},
			GPU{Name: "gpu-2", Model: "L40S", Memory: "40Gi"},
			GPU{Name: "gpu-3", Model: "L40S", Memory: "40Gi"},
		).
		Slice("worker-b",
			GPU{Name: "gpu-0", Model: "H100", Memory: "80Gi"},
			GPU{Name: "gpu-1", Model: "H100", Memory: "80Gi"},
			GPU{Name: "gpu-2", Model: "L40S", Memory: "40Gi"},
			GPU{Name: "gpu-3", Model: "L40S", Memory: "40Gi"},
		)
}

// Partition is one slice of a physical GPU that consumes its memory counter.
type Partition struct {
	Name    string
	Profile string
	Memory  string
}

// PartitionedSlice publishes physical GPUs as partitions that share a memory
// counter per GPU, the way MIG-style drivers do. gpus maps a physical GPU
// name to its total memory and partitions.
func (b *B) PartitionedSlice(node string, gpus map[string]struct {
	Memory     string
	Partitions []Partition
}) *B {
	// Since v1.36, counter sets live in their own slice, separate from the
	// devices that consume them.
	pool := resourceapi.ResourcePool{Name: node, ResourceSliceCount: 2}
	counters := resourceapi.ResourceSlice{
		ObjectMeta: metav1.ObjectMeta{Name: node + "-counters"},
		Spec:       resourceapi.ResourceSliceSpec{Driver: Driver, NodeName: ptr.To(node), Pool: pool},
	}
	sl := resourceapi.ResourceSlice{
		ObjectMeta: metav1.ObjectMeta{Name: node + "-partitions"},
		Spec:       resourceapi.ResourceSliceSpec{Driver: Driver, NodeName: ptr.To(node), Pool: pool},
	}
	names := make([]string, 0, len(gpus))
	for n := range gpus {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, phys := range names {
		g := gpus[phys]
		counters.Spec.SharedCounters = append(counters.Spec.SharedCounters, resourceapi.CounterSet{
			Name:     phys,
			Counters: map[string]resourceapi.Counter{"memory": {Value: resource.MustParse(g.Memory)}},
		})
		for _, p := range g.Partitions {
			sl.Spec.Devices = append(sl.Spec.Devices, resourceapi.Device{
				Name: p.Name,
				Attributes: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
					"model":   {StringValue: ptr.To("H100")},
					"profile": {StringValue: ptr.To(p.Profile)},
					"parent":  {StringValue: ptr.To(phys)},
				},
				Capacity: map[resourceapi.QualifiedName]resourceapi.DeviceCapacity{
					"memory": {Value: resource.MustParse(p.Memory)},
				},
				ConsumesCounters: []resourceapi.DeviceCounterConsumption{{
					CounterSet: phys,
					Counters:   map[string]resourceapi.Counter{"memory": {Value: resource.MustParse(p.Memory)}},
				}},
			})
		}
	}
	b.S.ResourceSlices = append(b.S.ResourceSlices, counters, sl)
	return b
}
