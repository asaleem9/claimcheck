package snapshot

import (
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/dynamic-resource-allocation/structured"
)

// Owner records which claim holds a device, so explanations can say
// "allocated to default/train-1" instead of just "in use".
type Owner struct {
	Namespace, Name string
	Shared          bool
}

func (o Owner) String() string { return o.Namespace + "/" + o.Name }

// AllocatedState builds the scheduler's view of devices already in use from
// every allocated claim except the ones in skip, mirroring how kube-scheduler
// feeds the allocator: admin-access results don't block a device, results
// with a share ID consume capacity instead of the whole device.
func (s *Snapshot) AllocatedState(skip sets.Set[types.UID]) (structured.AllocatedState, map[structured.DeviceID][]Owner) {
	state := structured.AllocatedState{
		AllocatedDevices:         sets.New[structured.DeviceID](),
		AllocatedSharedDeviceIDs: sets.New[structured.SharedDeviceID](),
		AggregatedCapacity:       structured.NewConsumedCapacityCollection(),
	}
	owners := map[structured.DeviceID][]Owner{}
	for i := range s.ResourceClaims {
		c := &s.ResourceClaims[i]
		if c.Status.Allocation == nil || skip.Has(c.UID) {
			continue
		}
		for _, r := range c.Status.Allocation.Devices.Results {
			if ptr.Deref(r.AdminAccess, false) {
				continue
			}
			id := structured.MakeDeviceID(r.Driver, r.Pool, r.Device)
			owner := Owner{Namespace: c.Namespace, Name: c.Name}
			if r.ShareID != nil {
				owner.Shared = true
				state.AllocatedSharedDeviceIDs.Insert(structured.MakeSharedDeviceID(id, r.ShareID))
				if r.ConsumedCapacity != nil {
					state.AggregatedCapacity.Insert(structured.NewDeviceConsumedCapacity(id, r.ConsumedCapacity))
				}
			} else {
				state.AllocatedDevices.Insert(id)
			}
			owners[id] = append(owners[id], owner)
		}
	}
	return state, owners
}

// TaintSource says where a device taint came from.
type TaintSource struct {
	Taint resourceapi.DeviceTaint
	Rule  string // empty when the taint is set in the ResourceSlice itself
}

// EffectiveSlices returns deep copies of all slices with DeviceTaintRules
// applied to matching devices, which is what the scheduler allocates from.
// The second result maps each device to its taints and their origin.
func (s *Snapshot) EffectiveSlices() ([]*resourceapi.ResourceSlice, map[structured.DeviceID][]TaintSource) {
	sources := map[structured.DeviceID][]TaintSource{}
	out := make([]*resourceapi.ResourceSlice, 0, len(s.ResourceSlices))
	for i := range s.ResourceSlices {
		sl := s.ResourceSlices[i].DeepCopy()
		for d := range sl.Spec.Devices {
			dev := &sl.Spec.Devices[d]
			id := structured.MakeDeviceID(sl.Spec.Driver, sl.Spec.Pool.Name, dev.Name)
			for _, t := range dev.Taints {
				sources[id] = append(sources[id], TaintSource{Taint: t})
			}
			for _, rule := range s.DeviceTaintRules {
				if ruleSelects(rule.Spec.DeviceSelector, sl, dev.Name) {
					dev.Taints = append(dev.Taints, rule.Spec.Taint)
					sources[id] = append(sources[id], TaintSource{Taint: rule.Spec.Taint, Rule: rule.Name})
				}
			}
		}
		out = append(out, sl)
	}
	return out, sources
}

func ruleSelects(sel *resourceapi.DeviceTaintSelector, sl *resourceapi.ResourceSlice, device string) bool {
	if sel == nil {
		return true
	}
	if sel.Driver != nil && *sel.Driver != sl.Spec.Driver {
		return false
	}
	if sel.Pool != nil && *sel.Pool != sl.Spec.Pool.Name {
		return false
	}
	if sel.Device != nil && *sel.Device != device {
		return false
	}
	return true
}
