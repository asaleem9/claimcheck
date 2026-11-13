package diagnose

import (
	"fmt"
	"sort"
	"strings"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/dynamic-resource-allocation/structured"
)

// counterKey identifies one counter: pool, counter set and counter name.
type counterKey struct {
	pool, set, name string
}

func (k counterKey) String() string { return k.set + "/" + k.name }

// counterUse is how much of one counter a device consumes.
type counterUse struct {
	key counterKey
	qty resource.Quantity
}

// counterState holds what's left of every shared counter after the devices
// that are already allocated, and which devices hold each counter set.
type counterState struct {
	left  map[counterKey]resource.Quantity
	users map[string][]string // pool+set -> allocated devices consuming it
}

func poolKey(driver, pool string) string { return driver + "/" + pool }

// counters computes the counter state the way the allocator does: the
// initial values from the pool's SharedCounters, minus what every allocated
// device in the pool consumes.
func (d *Diagnoser) counters() *counterState {
	if d.counterCache != nil {
		return d.counterCache
	}
	st := &counterState{left: map[counterKey]resource.Quantity{}, users: map[string][]string{}}
	for _, s := range d.current() {
		pk := poolKey(s.Spec.Driver, s.Spec.Pool.Name)
		for _, cs := range s.Spec.SharedCounters {
			for name, c := range cs.Counters {
				st.left[counterKey{pk, cs.Name, name}] = c.Value.DeepCopy()
			}
		}
	}
	for _, s := range d.current() {
		pk := poolKey(s.Spec.Driver, s.Spec.Pool.Name)
		for _, dev := range s.Spec.Devices {
			id := structured.MakeDeviceID(s.Spec.Driver, s.Spec.Pool.Name, dev.Name)
			if !structured.IsDeviceAllocated(id, &d.Allocated) {
				continue
			}
			for _, cc := range dev.ConsumesCounters {
				st.users[pk+"|"+cc.CounterSet] = append(st.users[pk+"|"+cc.CounterSet], dev.Name)
				for name, c := range cc.Counters {
					k := counterKey{pk, cc.CounterSet, name}
					v, ok := st.left[k]
					if !ok {
						continue
					}
					v.Sub(c.Value)
					st.left[k] = v
				}
			}
		}
	}
	d.counterCache = st
	return st
}

// current returns the slices of each pool's newest generation.
func (d *Diagnoser) current() []*resourceapi.ResourceSlice {
	d.generations()
	var out []*resourceapi.ResourceSlice
	for _, s := range d.Slices {
		if s.Spec.Pool.Generation >= d.latestGen[s.Spec.Driver+"/"+s.Spec.Pool.Name] {
			out = append(out, s)
		}
	}
	return out
}

// uses lists a device's counter consumption.
func uses(driver, pool string, dev *resourceapi.Device) []counterUse {
	pk := poolKey(driver, pool)
	var out []counterUse
	for _, cc := range dev.ConsumesCounters {
		for name, c := range cc.Counters {
			out = append(out, counterUse{counterKey{pk, cc.CounterSet, name}, c.Value})
		}
	}
	return out
}

// counterShortfall explains why a device's counters don't fit, or returns
// "". Like the allocator, every counter of the pool must stay non-negative
// after adding this device, so an over-committed pool blocks all devices.
func (d *Diagnoser) counterShortfall(driver, pool string, dev *resourceapi.Device) string {
	st := d.counters()
	need := map[counterKey]resource.Quantity{}
	for _, u := range uses(driver, pool, dev) {
		q := need[u.key]
		q.Add(u.qty)
		need[u.key] = q
	}
	pk := poolKey(driver, pool)
	var keys []counterKey
	for k := range st.left {
		if k.pool == pk {
			keys = append(keys, k)
		}
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].String() < keys[j].String() })
	for _, k := range keys {
		left := st.left[k]
		q := need[k]
		if left.Cmp(q) >= 0 {
			continue
		}
		msg := fmt.Sprintf("needs %s=%s of counter set %s, %s left", k.name, q.String(), k.set, left.String())
		if q.IsZero() {
			msg = fmt.Sprintf("counter %s is already over-committed (%s)", k, left.String())
		}
		if u := st.users[pk+"|"+k.set]; len(u) > 0 {
			sort.Slice(u, func(i, j int) bool { return naturalLess(u[i], u[j]) })
			msg += "; used by " + strings.Join(u, ", ")
		}
		return msg
	}
	return ""
}
