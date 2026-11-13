package diagnose

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/dynamic-resource-allocation/cel"
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
	// share holds, for shared (allowMultipleAllocations) devices, the
	// capacity this request would take from them.
	share map[structured.DeviceID]map[resourceapi.QualifiedName]resource.Quantity
	// derived maps derived attribute names of the slot's request to CEL.
	derived map[resourceapi.FullyQualifiedName]string
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
	// valueError: evaluating a derived attribute failed; the allocator
	// stops with an error when it reaches such a device.
	valueError
)

// value is an attribute's value as a set of typed keys ("s:a", "i:3"):
// one element for scalars, several for list-typed attributes. Keys of
// different types never compare equal, like in the allocator.
type value struct {
	keys    []string
	display []string
	state   valueState
	err     string
}

// valueFunc looks up an attribute for a device in the context of one slot,
// whose request may define it as a CEL-derived attribute.
type valueFunc func(sl slot, id structured.DeviceID, attr resourceapi.FullyQualifiedName) value

func lookupValue(ctx context.Context, devs map[structured.DeviceID]*resourceapi.Device, cache *cel.Cache, listTypes bool) valueFunc {
	derived := map[string]value{}
	return func(sl slot, id structured.DeviceID, attr resourceapi.FullyQualifiedName) value {
		dev := devs[id]
		if dev == nil {
			return value{state: valueUnset}
		}
		if expr, ok := sl.derived[attr]; ok {
			k := id.String() + "\x00" + expr
			if v, ok := derived[k]; ok {
				return v
			}
			v := evalDerived(ctx, cache, id, dev, expr, listTypes)
			derived[k] = v
			return v
		}
		a, ok := dev.Attributes[resourceapi.QualifiedName(attr)]
		if !ok {
			domain, name, found := strings.Cut(string(attr), "/")
			if !found || domain != id.Driver.String() {
				return value{state: valueUnset}
			}
			if a, ok = dev.Attributes[resourceapi.QualifiedName(name)]; !ok {
				return value{state: valueUnset}
			}
		}
		return typedValue(a, listTypes)
	}
}

func evalDerived(ctx context.Context, cache *cel.Cache, id structured.DeviceID, dev *resourceapi.Device, expr string, listTypes bool) value {
	c := cache.GetOrCompileDerivedAttribute(expr)
	if c.Error != nil {
		return value{state: valueError, err: "compile derived attribute: " + c.Error.Error()}
	}
	attr, _, err := c.EvaluateDerivedAttribute(ctx, cel.Device{
		Driver: id.Driver.String(), AllowMultipleAllocations: dev.AllowMultipleAllocations,
		Attributes: dev.Attributes, Capacity: dev.Capacity,
	})
	if err != nil {
		return value{state: valueError, err: ShortCELError(cel.EnhanceRuntimeError(err).Error())}
	}
	if attr == nil {
		return value{state: valueUnset}
	}
	return typedValue(*attr, listTypes)
}

// typedValue converts an attribute to a typed set. Without the
// list-type feature, list values can't satisfy a constraint, like in the
// allocator, so they count as unset.
func typedValue(a resourceapi.DeviceAttribute, listTypes bool) value {
	var v value
	add := func(prefix, s string) {
		v.keys = append(v.keys, prefix+s)
		v.display = append(v.display, s)
	}
	switch {
	case listTypes && len(a.IntValues) > 0:
		for _, x := range a.IntValues {
			add("i:", strconv.FormatInt(x, 10))
		}
	case listTypes && len(a.BoolValues) > 0:
		for _, x := range a.BoolValues {
			add("b:", strconv.FormatBool(x))
		}
	case listTypes && len(a.StringValues) > 0:
		for _, x := range a.StringValues {
			add("s:", x)
		}
	case listTypes && len(a.VersionValues) > 0:
		for _, x := range a.VersionValues {
			add("v:", x)
		}
	case a.StringValue != nil:
		add("s:", *a.StringValue)
	case a.IntValue != nil:
		add("i:", strconv.FormatInt(*a.IntValue, 10))
	case a.BoolValue != nil:
		add("b:", strconv.FormatBool(*a.BoolValue))
	case a.VersionValue != nil:
		add("v:", *a.VersionValue)
	default:
		return value{state: valueUnset}
	}
	return v
}

func (v value) has(key string) bool {
	for _, k := range v.keys {
		if k == key {
			return true
		}
	}
	return false
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
	// capLeft and capInUse model shared devices with consumable capacity.
	capLeft  func(structured.DeviceID) map[resourceapi.QualifiedName]resource.Quantity
	capInUse func(structured.DeviceID) bool
}

// sharingMatters reports whether any option is a shared device, which one
// claim can use for several requests while capacity lasts.
func sharingMatters(slots []slot) bool {
	for _, sl := range slots {
		if len(sl.share) > 0 {
			return true
		}
	}
	return false
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
				if v := s.value(sl, id, m.attr); v.state == valueSet {
					seen.Insert(v.keys...)
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
		sharing := sharingMatters(restricted)
		if len(distinct) == 0 && !s.countersMatter(restricted) && !sharing {
			if matchAll(restricted) {
				return true, false
			}
		} else if sharing || matchAll(restricted) {
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
		out[i] = slot{claim: sl.claim, request: sl.request, interchangeable: sl.interchangeable, share: sl.share, derived: sl.derived}
	options:
		for _, id := range sl.options {
			for j, m := range match {
				if !m.applies(sl) {
					continue
				}
				if v := s.value(sl, id, m.attr); v.state != valueSet || !v.has(values[j][pos[j]]) {
					continue options
				}
			}
			for _, d := range distinct {
				if !d.applies(sl) {
					continue
				}
				if s.value(sl, id, d.attr).state != valueSet {
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
	capLeft := map[structured.DeviceID]map[resourceapi.QualifiedName]resource.Quantity{}
	shares := map[structured.DeviceID]int{}                // uses of each shared device in this search
	sharedBy := map[structured.DeviceID]sets.Set[string]{} // requests already using it
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
			share, shared := sl.share[id]
			reqKey := sl.claim + " " + sl.request
			if shared {
				if sharedBy[id].Has(reqKey) {
					continue // a request takes a device at most once
				}
				if capLeft[id] == nil {
					capLeft[id] = s.capLeft(id)
				}
				for name, q := range share {
					if l := capLeft[id][name]; l.Cmp(q) < 0 {
						continue options
					}
				}
			} else if used.Has(id) {
				continue
			}
			// distinctAttribute: the device's values must be disjoint from
			// those of the devices already chosen under the constraint.
			keys := make([][]string, len(distinct))
			for j, d := range distinct {
				if d.applies(sl) {
					keys[j] = s.value(sl, id, d.attr).keys
					if taken[j].HasAny(keys[j]...) {
						continue options
					}
				}
			}
			var us []counterUse
			countersCharged := shared && (shares[id] > 0 || s.capInUse(id)) // charged once per shared device
			if s.uses != nil && !countersCharged {
				us = s.uses(id)
			}
			for _, u := range us {
				if l, ok := left[u.key]; ok && l.Cmp(u.qty) < 0 {
					continue options
				}
			}
			if shared {
				shares[id]++
				if sharedBy[id] == nil {
					sharedBy[id] = sets.New[string]()
				}
				sharedBy[id].Insert(reqKey)
				for name, q := range share {
					l := capLeft[id][name]
					l.Sub(q)
					capLeft[id][name] = l
				}
			} else {
				used.Insert(id)
			}
			for j := range keys {
				taken[j].Insert(keys[j]...)
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
			if shared {
				shares[id]--
				sharedBy[id].Delete(reqKey)
				for name, q := range share {
					l := capLeft[id][name]
					l.Add(q)
					capLeft[id][name] = l
				}
			} else {
				used.Delete(id)
			}
			for j := range keys {
				taken[j].Delete(keys[j]...)
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
	if sharingMatters(s.slots) {
		if ok, unknown := s.solve(nil); !ok && !unknown {
			return contentionMessage(s.slots) + " (shared devices are limited by their remaining capacity)", "", ""
		}
	} else if !matchAll(s.slots) {
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
	derived := false
	for _, sl := range s.slots {
		if !c.applies(sl) {
			continue
		}
		need++
		if _, ok := sl.derived[c.attr]; ok {
			derived = true
		}
		if !seenReq.Has(sl.request) {
			seenReq.Insert(sl.request)
			reqs = append(reqs, strconv.Quote(sl.request))
		}
	}
	verb := "that share a value"
	if c.distinct {
		verb = "with no value in common"
	}
	head := fmt.Sprintf("%s %s on %s needs %d devices %s", c.kind(), c.attr, strings.Join(reqs, " and "), need, verb)

	if derived {
		// A derived attribute can give the same device a different value
		// for each request, so show each request's view.
		var lines []string
		for _, r := range reqs {
			name, _ := strconv.Unquote(r)
			var sl slot
			for _, x := range s.slots {
				if c.applies(x) && x.request == name {
					sl = x
					break
				}
			}
			gs, unset, errs := s.groups(c, []slot{sl})
			lines = append(lines, fmt.Sprintf("%s computes %s", r, formatGroups(c, gs, unset, errs)))
		}
		return fmt.Sprintf("%s; %s is derived per request\n    %s", head, attrName(c.attr), strings.Join(lines, "\n    "))
	}

	var applicable []slot
	for _, sl := range s.slots {
		if c.applies(sl) {
			applicable = append(applicable, sl)
		}
	}
	gs, unset, errs := s.groups(c, applicable)
	most := 0
	if len(gs) > 0 {
		most = len(gs[0].devs)
	}
	summary := fmt.Sprintf("the most any value has is %d", most)
	if c.distinct {
		summary = fmt.Sprintf("usable devices have only %d distinct values", len(gs))
	}
	return fmt.Sprintf("%s; %s\n    usable: %s", head, summary, formatGroups(c, gs, unset, errs))
}

type valueGroup struct {
	value string
	devs  []string
}

// groups collects the usable devices of slots by attribute value (a device
// with a list value appears under each element), largest group first.
func (s *solver) groups(c constraintSpec, slots []slot) (gs []valueGroup, unset, errs []string) {
	byValue := map[string][]string{}
	seen := sets.New[structured.DeviceID]()
	for _, sl := range slots {
		for _, id := range sl.options {
			if seen.Has(id) {
				continue
			}
			seen.Insert(id)
			v := s.value(sl, id, c.attr)
			switch v.state {
			case valueError:
				errs = append(errs, id.Device.String()+" ("+v.err+")")
				continue
			case valueUnset:
				unset = append(unset, id.Device.String())
				continue
			}
			for _, d := range v.display {
				byValue[d] = append(byValue[d], id.Device.String())
			}
		}
	}
	for v, ds := range byValue {
		sort.Slice(ds, func(i, j int) bool { return naturalLess(ds[i], ds[j]) })
		gs = append(gs, valueGroup{v, ds})
	}
	sort.Slice(gs, func(i, j int) bool {
		if len(gs[i].devs) != len(gs[j].devs) {
			return len(gs[i].devs) > len(gs[j].devs)
		}
		return naturalLess(gs[i].value, gs[j].value)
	})
	sort.Slice(unset, func(i, j int) bool { return naturalLess(unset[i], unset[j]) })
	sort.Strings(errs)
	return gs, unset, errs
}

func formatGroups(c constraintSpec, gs []valueGroup, unset, errs []string) string {
	const shown = 3
	var parts []string
	for i, g := range gs {
		if i == shown {
			more := len(gs) - shown
			parts = append(parts, fmt.Sprintf("%d more value%s", more, map[bool]string{true: "", false: "s"}[more == 1]))
			break
		}
		parts = append(parts, fmt.Sprintf("%s=%s: %s", attrName(c.attr), g.value, strings.Join(g.devs, ", ")))
	}
	if len(unset) > 0 {
		parts = append(parts, "not set: "+strings.Join(unset, ", "))
	}
	if len(errs) > 0 {
		parts = append(parts, "error: "+strings.Join(errs, ", "))
	}
	return strings.Join(parts, " · ")
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
		var share map[structured.DeviceID]map[resourceapi.QualifiedName]resource.Quantity
		for _, dv := range r.Devices {
			if dv.Reason == Available {
				usable = append(usable, dv.ID())
				if dv.share != nil {
					if share == nil {
						share = map[structured.DeviceID]map[resourceapi.QualifiedName]resource.Quantity{}
					}
					share[dv.ID()] = dv.share
				}
			}
		}
		if r.Mode == resourceapi.DeviceAllocationModeAll {
			for _, id := range usable {
				slots = append(slots, slot{claim: r.Claim, request: r.Name, options: []structured.DeviceID{id}, share: share, derived: r.derived})
			}
			continue
		}
		for range r.Needed {
			slots = append(slots, slot{claim: r.Claim, request: r.Name, options: usable, interchangeable: true, share: share, derived: r.derived})
		}
	}
	return slots
}
