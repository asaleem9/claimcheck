package diagnose

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/dynamic-resource-allocation/structured"
)

// slot is one device a request needs. A request for N devices has N slots
// with the same options; allocationMode All has one slot per device.
type slot struct {
	claim, request string
	options        []structured.DeviceID
	// interchangeable slots belong to the same ExactCount request and have
	// identical options, so their order doesn't matter.
	interchangeable bool
}

// constraintSpec is a claim's matchAttribute or distinctAttribute constraint.
type constraintSpec struct {
	claim    string
	requests sets.Set[string] // empty: all requests of the claim
	attr     resourceapi.FullyQualifiedName
	distinct bool
}

func (c constraintSpec) kind() string {
	if c.distinct {
		return "distinctAttribute"
	}
	return "matchAttribute"
}

// applies mirrors the allocator: a constraint names requests or
// "request/subrequest"; naming the parent covers all its alternatives.
func (c constraintSpec) applies(s slot) bool {
	if s.claim != c.claim {
		return false
	}
	if c.requests.Len() == 0 {
		return true
	}
	parent, _, _ := strings.Cut(s.request, "/")
	return c.requests.Has(s.request) || c.requests.Has(parent)
}

type valueState int

const (
	valueSet valueState = iota
	valueUnset
	valueUnsupported // list-typed attributes, which the solver doesn't model
)

// valueFunc looks up a device attribute the way the allocator does for
// constraints and returns a comparable key and a display string.
type valueFunc func(id structured.DeviceID, attr resourceapi.FullyQualifiedName) (key, display string, state valueState)

func lookupValue(devs map[structured.DeviceID]*resourceapi.Device) valueFunc {
	return func(id structured.DeviceID, attr resourceapi.FullyQualifiedName) (string, string, valueState) {
		dev := devs[id]
		if dev == nil {
			return "", "", valueUnset
		}
		a, ok := dev.Attributes[resourceapi.QualifiedName(attr)]
		if !ok {
			domain, name, found := strings.Cut(string(attr), "/")
			if !found || domain != id.Driver.String() {
				return "", "", valueUnset
			}
			if a, ok = dev.Attributes[resourceapi.QualifiedName(name)]; !ok {
				return "", "", valueUnset
			}
		}
		switch {
		case a.StringValue != nil:
			return "s:" + *a.StringValue, *a.StringValue, valueSet
		case a.IntValue != nil:
			v := strconv.FormatInt(*a.IntValue, 10)
			return "i:" + v, v, valueSet
		case a.BoolValue != nil:
			v := strconv.FormatBool(*a.BoolValue)
			return "b:" + v, v, valueSet
		case a.VersionValue != nil:
			return "v:" + *a.VersionValue, *a.VersionValue, valueSet
		}
		return "", "", valueUnsupported
	}
}

// solver decides whether slots can get distinct devices that satisfy all
// constraints. It is exact for matchAttribute (by trying each shared value
// and running bipartite matching) and uses bounded backtracking for
// distinctAttribute; when the bound is hit it reports "unknown" rather
// than guess.
type solver struct {
	slots  []slot
	cons   []constraintSpec
	value  valueFunc
	budget int
	// uses and left model shared counters of partitionable devices.
	uses func(structured.DeviceID) []counterUse
	left map[counterKey]resource.Quantity
}

// countersMatter reports whether any option consumes shared counters, in
// which case devices can't be picked independently of each other.
func (s *solver) countersMatter(slots []slot) bool {
	if s.uses == nil {
		return false
	}
	for _, sl := range slots {
		for _, id := range sl.options {
			if len(s.uses(id)) > 0 {
				return true
			}
		}
	}
	return false
}

const (
	maxValueCombinations = 4096
	backtrackBudget      = 200_000
)

// solve reports whether cons (a subset of s.cons) can all be satisfied.
func (s *solver) solve(cons []constraintSpec) (ok, unknown bool) {
	var match, distinct []constraintSpec
	for _, c := range cons {
		if !s.anyApplies(c) {
			continue
		}
		if c.distinct {
			distinct = append(distinct, c)
		} else {
			match = append(match, c)
		}
	}

	// Candidate shared values for each matchAttribute constraint.
	values := make([][]string, len(match))
	combos := 1
	for i, m := range match {
		seen := sets.New[string]()
		for _, sl := range s.slots {
			if !m.applies(sl) {
				continue
			}
			for _, id := range sl.options {
				key, _, st := s.value(id, m.attr)
				switch st {
				case valueUnsupported:
					return false, true
				case valueSet:
					seen.Insert(key)
				}
			}
		}
		if seen.Len() == 0 {
			return false, false
		}
		values[i] = sets.List(seen)
		combos *= len(values[i])
		if combos > maxValueCombinations {
			return false, true
		}
	}

	pos := make([]int, len(match))
	for {
		restricted := s.restrict(match, values, pos, distinct)
		if restricted == nil {
			return false, true
		}
		if len(distinct) == 0 && !s.countersMatter(restricted) {
			if matchAll(restricted) {
				return true, false
			}
		} else if matchAll(restricted) {
			found, exhausted := s.backtrack(restricted, distinct)
			if found {
				return true, false
			}
			unknown = unknown || exhausted
		}
		k := len(pos) - 1
		for k >= 0 {
			pos[k]++
			if pos[k] < len(values[k]) {
				break
			}
			pos[k] = 0
			k--
		}
		if k < 0 {
			return false, unknown
		}
	}
}

func (s *solver) anyApplies(c constraintSpec) bool {
	for _, sl := range s.slots {
		if c.applies(sl) {
			return true
		}
	}
	return false
}

// restrict narrows each slot's options to devices that carry the chosen
// matchAttribute values and have a value for every distinctAttribute.
// It returns nil when a list-typed attribute makes the problem unmodeled.
func (s *solver) restrict(match []constraintSpec, values [][]string, pos []int, distinct []constraintSpec) []slot {
	out := make([]slot, len(s.slots))
	for i, sl := range s.slots {
		out[i] = slot{claim: sl.claim, request: sl.request, interchangeable: sl.interchangeable}
	options:
		for _, id := range sl.options {
			for j, m := range match {
				if !m.applies(sl) {
					continue
				}
				key, _, st := s.value(id, m.attr)
				if st == valueUnsupported {
					return nil
				}
				if st != valueSet || key != values[j][pos[j]] {
					continue options
				}
			}
			for _, d := range distinct {
				if !d.applies(sl) {
					continue
				}
				_, _, st := s.value(id, d.attr)
				if st == valueUnsupported {
					return nil
				}
				if st != valueSet {
					continue options
				}
			}
			out[i].options = append(out[i].options, id)
		}
	}
	return out
}

// backtrack assigns devices slot by slot, most constrained slot first,
// keeping distinctAttribute values unique per constraint and shared
// counters non-negative. Slots of the same request are interchangeable, so
// they take options in increasing order to avoid re-trying permutations.
func (s *solver) backtrack(slots []slot, distinct []constraintSpec) (found, exhausted bool) {
	order := make([]int, len(slots))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool { return len(slots[order[a]].options) < len(slots[order[b]].options) })
	used := sets.New[structured.DeviceID]()
	taken := make([]sets.Set[string], len(distinct))
	for i := range taken {
		taken[i] = sets.New[string]()
	}
	left := make(map[counterKey]resource.Quantity, len(s.left))
	for k, v := range s.left {
		left[k] = v.DeepCopy()
	}
	chosen := make([]int, len(order))
	steps := 0
	var rec func(n int) bool
	rec = func(n int) bool {
		if n == len(order) {
			return true
		}
		sl := slots[order[n]]
		start := 0
		if n > 0 {
			prev := slots[order[n-1]]
			if sl.interchangeable && prev.interchangeable && prev.claim == sl.claim && prev.request == sl.request {
				start = chosen[n-1] + 1
			}
		}
	options:
		for oi := start; oi < len(sl.options); oi++ {
			id := sl.options[oi]
			steps++
			if steps > s.budget {
				exhausted = true
				return false
			}
			if used.Has(id) {
				continue
			}
			var keys []string
			for j, d := range distinct {
				key := ""
				if d.applies(sl) {
					key, _, _ = s.value(id, d.attr)
					if taken[j].Has(key) {
						continue options
					}
				}
				keys = append(keys, key)
			}
			var us []counterUse
			if s.uses != nil {
				us = s.uses(id)
			}
			for _, u := range us {
				if l, ok := left[u.key]; ok && l.Cmp(u.qty) < 0 {
					continue options
				}
			}
			used.Insert(id)
			for j, key := range keys {
				if key != "" {
					taken[j].Insert(key)
				}
			}
			for _, u := range us {
				if l, ok := left[u.key]; ok {
					l.Sub(u.qty)
					left[u.key] = l
				}
			}
			chosen[n] = oi
			if rec(n + 1) {
				return true
			}
			used.Delete(id)
			for j, key := range keys {
				if key != "" {
					taken[j].Delete(key)
				}
			}
			for _, u := range us {
				if l, ok := left[u.key]; ok {
					l.Add(u.qty)
					left[u.key] = l
				}
			}
			if exhausted {
				return false
			}
		}
		return false
	}
	return rec(0), exhausted
}

// matchAll reports whether every slot can get its own device (bipartite
// matching with augmenting paths).
func matchAll(slots []slot) bool {
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
	for i := range slots {
		if !try(i, map[structured.DeviceID]bool{}) {
			return false
		}
	}
	return true
}

// explain describes why the slots can't be filled: plain contention for
// devices, a specific constraint, or constraints that conflict together.
func (s *solver) explain() (contention, counters, constraint string) {
	if !matchAll(s.slots) {
		return contentionMessage(s.slots), "", ""
	}
	if s.countersMatter(s.slots) {
		if ok, unknown := s.solve(nil); !ok && !unknown {
			return "", s.counterMessage(), ""
		}
	}
	for _, c := range s.cons {
		if !s.anyApplies(c) {
			continue
		}
		if ok, unknown := s.solve([]constraintSpec{c}); !ok && !unknown {
			return "", "", s.describe(c)
		}
	}
	var kinds []string
	for _, c := range s.cons {
		kinds = append(kinds, c.kind()+" "+string(c.attr))
	}
	return "", "", "each constraint can be met on its own, but not all together: " + strings.Join(kinds, ", ")
}

// describe explains one failing constraint with the usable devices grouped
// by the attribute's value.
func (s *solver) describe(c constraintSpec) string {
	var reqs []string
	seenReq := sets.New[string]()
	need := 0
	opts := sets.New[structured.DeviceID]()
	for _, sl := range s.slots {
		if !c.applies(sl) {
			continue
		}
		need++
		if !seenReq.Has(sl.request) {
			seenReq.Insert(sl.request)
			reqs = append(reqs, strconv.Quote(sl.request))
		}
		opts.Insert(sl.options...)
	}
	groups := map[string][]string{}
	var unset []string
	for id := range opts {
		_, display, st := s.value(id, c.attr)
		if st != valueSet {
			unset = append(unset, id.Device.String())
			continue
		}
		groups[display] = append(groups[display], id.Device.String())
	}
	var parts []string
	for v, ds := range groups {
		sort.Slice(ds, func(i, j int) bool { return naturalLess(ds[i], ds[j]) })
		parts = append(parts, fmt.Sprintf("%s=%s: %s", attrName(c.attr), v, strings.Join(ds, ", ")))
	}
	sort.Strings(parts)
	if len(unset) > 0 {
		sort.Slice(unset, func(i, j int) bool { return naturalLess(unset[i], unset[j]) })
		parts = append(parts, "not set: "+strings.Join(unset, ", "))
	}
	want := "the same value"
	if c.distinct {
		want = "different values"
	}
	return fmt.Sprintf("%s %s on %s needs %d devices with %s; usable devices: %s",
		c.kind(), c.attr, strings.Join(reqs, " and "), need, want, strings.Join(parts, " · "))
}

// counterMessage explains that the candidate partitions can't all be
// allocated together because they draw on the same shared counters.
func (s *solver) counterMessage() string {
	var reqs []string
	seen := sets.New[string]()
	need := map[counterKey][2]resource.Quantity{} // min, max per device
	for _, sl := range s.slots {
		if !seen.Has(sl.request) {
			seen.Insert(sl.request)
			reqs = append(reqs, strconv.Quote(sl.request))
		}
		for _, id := range sl.options {
			for _, u := range s.uses(id) {
				mm, ok := need[u.key]
				if !ok {
					need[u.key] = [2]resource.Quantity{u.qty, u.qty}
					continue
				}
				if u.qty.Cmp(mm[0]) < 0 {
					mm[0] = u.qty
				}
				if u.qty.Cmp(mm[1]) > 0 {
					mm[1] = u.qty
				}
				need[u.key] = mm
			}
		}
	}
	var parts []string
	for k, mm := range need {
		per := mm[0].String()
		if mm[0].Cmp(mm[1]) != 0 {
			per = mm[0].String() + "–" + mm[1].String()
		}
		l := s.left[k]
		parts = append(parts, fmt.Sprintf("%s: %s left, %s per device", k, l.String(), per))
	}
	sort.Strings(parts)
	return fmt.Sprintf("%s need %d partitions, but together they use more of their shared counters than is left: %s",
		strings.Join(reqs, " and "), len(s.slots), strings.Join(parts, " · "))
}

func attrName(a resourceapi.FullyQualifiedName) string {
	if _, name, ok := strings.Cut(string(a), "/"); ok {
		return name
	}
	return string(a)
}

func contentionMessage(slots []slot) string {
	var names, devices []string
	seenReq, seenDev := map[string]bool{}, map[structured.DeviceID]bool{}
	for _, s := range slots {
		if !seenReq[s.request] {
			seenReq[s.request] = true
			names = append(names, strconv.Quote(s.request))
		}
		for _, id := range s.options {
			if !seenDev[id] {
				seenDev[id] = true
				devices = append(devices, id.Device.String())
			}
		}
	}
	sort.Slice(devices, func(i, j int) bool { return naturalLess(devices[i], devices[j]) })
	return fmt.Sprintf("%s need %d devices between them but can only use %d: %s",
		strings.Join(names, " and "), len(slots), len(devices), strings.Join(devices, ", "))
}

// buildSlots turns the chosen alternative of each request into slots.
// Admin-access requests don't take devices away from others and get no
// slots; constrained admin requests are reported as unmodeled by the caller.
func buildSlots(reqs []Request) []slot {
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
				slots = append(slots, slot{claim: r.Claim, request: r.Name, options: []structured.DeviceID{id}})
			}
			continue
		}
		for range r.Needed {
			slots = append(slots, slot{claim: r.Claim, request: r.Name, options: usable, interchangeable: true})
		}
	}
	return slots
}
