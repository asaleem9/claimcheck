package diagnose

import (
	"fmt"
	"sort"
	"strings"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/dynamic-resource-allocation/structured"
)

// groupState mirrors the allocator's compatibilityGroupIntersection: the
// compatibility groups shared by every partition in use on one counter set
// (one physical device).
type groupState struct {
	hasMembers bool
	allEmpty   bool // members without groups only admit others without groups
	groups     sets.Set[string]
	users      []string
}

func (g groupState) admits(groups sets.Set[string]) bool {
	if !g.hasMembers {
		return true
	}
	if groups.Len() == 0 {
		return g.allEmpty
	}
	if g.allEmpty {
		return false
	}
	return g.groups.Intersection(groups).Len() > 0
}

func (g groupState) add(groups sets.Set[string], user string) groupState {
	g.users = append(append([]string(nil), g.users...), user)
	if !g.hasMembers {
		g.hasMembers = true
		if groups.Len() == 0 {
			g.allEmpty, g.groups = true, nil
		} else {
			g.groups = groups.Clone()
		}
		return g
	}
	if g.allEmpty || groups.Len() == 0 {
		return g
	}
	g.groups = g.groups.Intersection(groups)
	return g
}

// compatUse is one counter set a partition draws on, with its groups.
type compatUse struct {
	key    string // pool|counter set
	groups sets.Set[string]
}

func compatUses(driver, pool string, dev *resourceapi.Device) []compatUse {
	pk := poolKey(driver, pool)
	var out []compatUse
	for _, cc := range dev.ConsumesCounters {
		var g sets.Set[string]
		if len(cc.CompatibilityGroups) > 0 {
			g = sets.New(cc.CompatibilityGroups...)
		}
		out = append(out, compatUse{key: pk + "|" + cc.CounterSet, groups: g})
	}
	return out
}

// compatBaseline returns the group state of every counter set from the
// partitions already allocated, like compatibilityGroupsBaselineForPool.
func (d *Diagnoser) compatBaseline() map[string]groupState {
	if d.compatCache != nil {
		return d.compatCache
	}
	d.compatCache = map[string]groupState{}
	for _, s := range d.current() {
		for i := range s.Spec.Devices {
			dev := &s.Spec.Devices[i]
			id := structured.MakeDeviceID(s.Spec.Driver, s.Spec.Pool.Name, dev.Name)
			if !structured.IsDeviceAllocated(id, &d.Allocated) {
				continue
			}
			for _, u := range compatUses(s.Spec.Driver, s.Spec.Pool.Name, dev) {
				d.compatCache[u.key] = d.compatCache[u.key].add(u.groups, dev.Name)
			}
		}
	}
	return d.compatCache
}

// incompatible explains why a partition's compatibility groups don't fit
// with the partitions in use on its physical device, or returns "".
func (d *Diagnoser) incompatible(driver, pool string, dev *resourceapi.Device) string {
	base := d.compatBaseline()
	for _, u := range compatUses(driver, pool, dev) {
		st := base[u.key]
		if st.admits(u.groups) {
			continue
		}
		_, set, _ := strings.Cut(u.key, "|")
		users := append([]string(nil), st.users...)
		sort.Slice(users, func(i, j int) bool { return naturalLess(users[i], users[j]) })
		return fmt.Sprintf("compatibility groups %s don't overlap %s, required by %s on counter set %s",
			formatGroupSet(u.groups), formatGroupState(st), strings.Join(users, ", "), set)
	}
	return ""
}

func formatGroupSet(g sets.Set[string]) string {
	if g.Len() == 0 {
		return "(none)"
	}
	return "[" + strings.Join(sets.List(g), ",") + "]"
}

func formatGroupState(st groupState) string {
	if st.allEmpty {
		return "(none)"
	}
	return formatGroupSet(st.groups)
}
