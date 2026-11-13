package explain

import (
	"fmt"
	"sort"
	"strings"

	"k8s.io/dynamic-resource-allocation/structured"

	"github.com/asaleem9/claimcheck/internal/diagnose"
)

// requestView is one request (or alternative) seen across all eligible
// nodes, with each device counted once.
type requestView struct {
	key     string
	class   string
	problem string
	// best is the eligible node with the most usable devices; its problem
	// is the one reported, since that node came closest.
	best      string
	bestAvail int
	needed    int
	satisfied bool
	devices   map[structured.DeviceID]diagnose.Device
	order     []structured.DeviceID
	// satisfiedOnExcluded lists nodes outside the pod's reach that would work.
	satisfiedOnExcluded []string
}

func hints(rep *Report) []string {
	if rep.Status == Fits || rep.Status == Allocated {
		return nil
	}
	views := map[string]*requestView{}
	var keys []string
	add := func(n NodeReport, r diagnose.Request) {
		k := r.Claim + " " + r.Name
		v := views[k]
		if v == nil {
			v = &requestView{key: k, class: r.Class, devices: map[structured.DeviceID]diagnose.Device{}}
			views[k] = v
			keys = append(keys, k)
		}
		if n.Excluded != "" {
			if r.Satisfied {
				v.satisfiedOnExcluded = append(v.satisfiedOnExcluded, n.Node+" ("+n.Excluded+")")
			}
			return
		}
		if r.Problem != "" && (v.best == "" || r.Available > v.bestAvail) {
			v.problem, v.best, v.bestAvail, v.needed = r.Problem, n.Node, r.Available, r.Needed
		}
		v.satisfied = v.satisfied || r.Satisfied
		for _, d := range r.Devices {
			id := d.ID()
			if prev, ok := v.devices[id]; ok && prev.Reason != diagnose.DeviceNode {
				continue
			}
			if _, ok := v.devices[id]; !ok {
				v.order = append(v.order, id)
			}
			v.devices[id] = d
		}
	}
	var contention []string
	constraintErr := ""
	var unanalyzed []string
	for _, n := range rep.Nodes {
		for _, r := range n.Diagnosis.Requests {
			if len(r.Alternatives) == 0 {
				add(n, r)
				continue
			}
			for _, alt := range r.Alternatives {
				add(n, alt)
			}
		}
		if n.Excluded == "" && IsDerivedError(n.Verdict.Err) && constraintErr == "" {
			constraintErr = fmt.Sprintf("A CEL-derived attribute fails to evaluate on %s, and the allocator stops with an error:\n    %s\n    guard optional attributes in the derived expression with has(...) or .?attr.orValue(...)", n.Node, diagnose.ShortCELError(n.Verdict.Err))
		}
		if n.Excluded == "" && IsConstraintError(n.Verdict.Err) && constraintErr == "" {
			constraintErr = fmt.Sprintf("The allocator stops with an error on %s instead of trying other devices: a device for an allocationMode: All request breaks one of the claim's constraints.\n    All-mode requests take every matching device, so each of them must satisfy the claim's matchAttribute/distinctAttribute constraints.", n.Node)
		}
		if n.Excluded == "" && n.Diagnosis.CounterConflict != "" {
			contention = append(contention, fmt.Sprintf("On %s each partition fits on its own, but not together:\n    %s", n.Node, n.Diagnosis.CounterConflict))
		}
		if n.Excluded == "" && n.Diagnosis.ConstraintFailure != "" {
			contention = append(contention, fmt.Sprintf("On %s the requests can get enough devices, but not ones that satisfy the claim's constraints:\n    %s", n.Node, n.Diagnosis.ConstraintFailure))
		}
		if n.Excluded == "" && n.Diagnosis.Contention != "" {
			contention = append(contention, fmt.Sprintf("On %s every request can be met on its own, but not all at once:\n    %s", n.Node, n.Diagnosis.Contention))
		}
		if n.Excluded == "" && !n.Agree {
			unanalyzed = append(unanalyzed, n.Diagnosis.Unanalyzed...)
		}
	}

	var out []string
	for _, k := range keys {
		v := views[k]
		if v.satisfied {
			continue
		}
		out = append(out, requestHints(v)...)
	}
	out = append(out, contention...)
	if constraintErr != "" {
		out = append(out, constraintErr)
	}
	if rep.Status == Unexplained {
		msg := "claimcheck could not explain the allocator's decision on every node; trust the verdict column, not the reasons."
		if len(unanalyzed) > 0 {
			msg += " The claims use features claimcheck doesn't model yet: " + strings.Join(dedupe(unanalyzed), "; ") + "."
		}
		out = append(out, msg)
	}
	return out
}

func requestHints(v *requestView) []string {
	name := "request " + quote(v.key)
	var out []string
	if len(v.devices) == 0 {
		if strings.Contains(v.problem, "not found") {
			return []string{fmt.Sprintf("%s: %s.", name, v.problem)}
		}
		msg := fmt.Sprintf("%s: no devices are visible on any eligible node. Is the DRA driver for DeviceClass %q running and publishing ResourceSlices?", name, v.class)
		return append(out, msg)
	}

	type bucket struct {
		reason   diagnose.Reason
		selector int
		expr     string
		details  map[string]int
		n        int
	}
	buckets := map[string]*bucket{}
	var bkeys []string
	usable := 0
	for _, id := range v.order {
		d := v.devices[id]
		if d.Reason == diagnose.Available {
			usable++
			continue
		}
		sel := -1
		if d.Selector != nil {
			sel = *d.Selector
		}
		k := fmt.Sprintf("%s#%d", d.Reason, sel)
		b := buckets[k]
		if b == nil {
			b = &bucket{reason: d.Reason, selector: sel, expr: d.Expression, details: map[string]int{}}
			buckets[k] = b
			bkeys = append(bkeys, k)
		}
		b.n++
		b.details[d.Detail]++
	}
	sort.SliceStable(bkeys, func(i, j int) bool { return buckets[bkeys[i]].n > buckets[bkeys[j]].n })

	total := len(v.devices)
	if v.problem != "" {
		where := ""
		if v.best != "" && v.bestAvail > 0 {
			where = " on " + v.best + ", the closest node"
		}
		out = append(out, fmt.Sprintf("%s: %s%s (%s considered).", name, v.problem, where, plural(total, "device")))
	}
	for _, k := range bkeys {
		b := buckets[k]
		of := fmt.Sprintf("%d of %d devices", b.n, total)
		if total == 1 {
			of = "the only device"
		}
		// vb picks the verb form for the number of devices in the bucket.
		vb := func(one, many string) string {
			if b.n == 1 {
				return one
			}
			return many
		}
		switch b.reason {
		case diagnose.RequestSelector:
			out = append(out, fmt.Sprintf("  selector #%d rejects %s: %s\n      values seen: %s", b.selector, of, b.expr, histogram(b.details)))
		case diagnose.ClassSelector:
			out = append(out, fmt.Sprintf("  DeviceClass %s selector #%d rejects %s: %s\n      values seen: %s", v.class, b.selector, of, b.expr, histogram(b.details)))
		case diagnose.SelectorError:
			msg := fmt.Sprintf("  selector #%d fails at runtime on %s: %s\n      the scheduler treats this as an error for the whole pod", b.selector, of, firstKey(b.details))
			if strings.Contains(firstKey(b.details), "no such key") {
				msg += "\n      guard optional attributes with has(...) or optional chaining (.?...orValue())"
			}
			out = append(out, msg)
		case diagnose.InUse:
			out = append(out, fmt.Sprintf("  %s %s already allocated: %s", of, vb("matches but is", "match but are"), histogram(trimPrefix(b.details, "allocated to "))))
		case diagnose.Tainted:
			out = append(out, fmt.Sprintf("  %s %s tainted: %s\n      add a matching toleration to the request, or remove the taint", of, vb("matches but is", "match but are"), histogram(b.details)))
		case diagnose.CapacityUsedUp:
			out = append(out, fmt.Sprintf("  %s %s, but other claims have used up %s shared capacity: %s", of, vb("matches", "match"), vb("its", "their"), histogram(b.details)))
		case diagnose.CapacityNeverFits:
			out = append(out, fmt.Sprintf("  %s can never give this request what it asks for: %s", of, histogram(b.details)))
		case diagnose.CapacityError:
			out = append(out, fmt.Sprintf("  computing capacity fails on %s: %s\n      the scheduler treats this as an error for the whole pod", of, firstKey(b.details)))
		case diagnose.Incompatible:
			out = append(out, fmt.Sprintf("  %s %s, but can't share a physical device with the partitions in use: %s", of, vb("matches", "match"), histogramN(b.details, 2)))
		case diagnose.CountersExhausted:
			out = append(out, fmt.Sprintf("  %s %s, but %s physical device has no counters left: %s", of, vb("matches", "match"), vb("its", "their"), histogramN(b.details, 2)))
		case diagnose.PoolIncomplete:
			out = append(out, fmt.Sprintf("  %s %s skipped because %s pool is incomplete: %s", of, vb("is", "are"), vb("its", "their"), firstKey(b.details)))
		case diagnose.DeviceNode:
			out = append(out, fmt.Sprintf("  %s %s bound to other nodes by per-device node selection", of, vb("is", "are")))
		case diagnose.FeatureDisabled:
			out = append(out, fmt.Sprintf("  %s %s a feature gate that is off: %s (override with --features)", of, vb("needs", "need"), firstKey(b.details)))
		}
	}
	if v.problem != "" && v.needed > 0 && usable >= v.needed && v.bestAvail < v.needed {
		out = append(out, fmt.Sprintf("  %d usable devices exist across nodes, but a claim is allocated on a single node; the most on one node is %d.", usable, v.bestAvail))
	}
	if len(v.satisfiedOnExcluded) > 0 {
		out = append(out, fmt.Sprintf("  it would fit on %s, but the pod can't run there.", strings.Join(v.satisfiedOnExcluded, ", ")))
	}
	return out
}

func histogram(m map[string]int) string { return histogramN(m, 4) }

func histogramN(m map[string]int, limit int) string {
	type kv struct {
		k string
		n int
	}
	var kvs []kv
	for k, n := range m {
		if k == "" {
			k = "(no attributes referenced)"
		}
		kvs = append(kvs, kv{k, n})
	}
	sort.Slice(kvs, func(i, j int) bool {
		if kvs[i].n != kvs[j].n {
			return kvs[i].n > kvs[j].n
		}
		return kvs[i].k < kvs[j].k
	})
	var parts []string
	for i, e := range kvs {
		if i == limit {
			parts = append(parts, fmt.Sprintf("… %d more", len(kvs)-limit))
			break
		}
		parts = append(parts, fmt.Sprintf("%s ×%d", e.k, e.n))
	}
	return strings.Join(parts, " · ")
}

func trimPrefix(m map[string]int, p string) map[string]int {
	out := map[string]int{}
	for k, n := range m {
		out[strings.TrimPrefix(k, p)] += n
	}
	return out
}

func firstKey(m map[string]int) string {
	var keys []string
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if len(keys) == 0 {
		return ""
	}
	return keys[0]
}

func quote(key string) string {
	claim, req, _ := strings.Cut(key, " ")
	return fmt.Sprintf("%q in %s", req, claim)
}

func dedupe(s []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, x := range s {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}
