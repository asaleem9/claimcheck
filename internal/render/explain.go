package render

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/asaleem9/claimcheck/internal/diagnose"
	"github.com/asaleem9/claimcheck/internal/explain"
)

// TextOptions controls the human-readable report.
type TextOptions struct {
	Style Style
	// Devices prints the per-device table for every eligible node.
	Devices bool
}

// JSON writes the report as indented JSON.
func JSON(w io.Writer, rep *explain.Report) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(rep)
}

// Text writes the report for a terminal.
func Text(w io.Writer, rep *explain.Report, o TextOptions) error {
	s := o.Style
	var b strings.Builder

	title := s.Bold(rep.Target.Kind + " " + rep.Target.Namespace + "/" + rep.Target.Name)
	if rep.Phase != "" {
		title += s.Dim("  (" + rep.Phase + ")")
	}
	fmt.Fprintf(&b, "\n%s\n\n", title)
	fmt.Fprintf(&b, "%s  %s\n", badge(s, rep.Status), rep.Summary)

	if len(rep.Claims) > 0 {
		fmt.Fprintf(&b, "\n%s\n", s.Bold("Claims"))
		var rows [][]cell
		for _, cl := range rep.Claims {
			state := c("pending", s.Yellow)
			if cl.Allocated {
				state = c("allocated", s.Green)
			}
			pc := ""
			if cl.PodClaim != "" && cl.PodClaim != cl.Name {
				pc = "pod claim " + cl.PodClaim
			}
			rows = append(rows, []cell{c(cl.Name, nil), state, c(pc, s.Dim)})
		}
		b.WriteString(table("  ", rows))
	}
	if rep.Extended != "" {
		if len(rep.Claims) == 0 {
			fmt.Fprintf(&b, "\n%s\n", s.Bold("Claims"))
		}
		fmt.Fprintf(&b, "  %s   %s   %s\n", "(extended resources)", s.Yellow("pending"), s.Dim(rep.Extended+"; the scheduler creates this claim when it binds the pod"))
	}

	if len(rep.Nodes) > 0 {
		fmt.Fprintf(&b, "\n%s %s\n", s.Bold("Nodes"), s.Dim("(allocator verdict · why)"))
		var rows [][]cell
		for _, n := range rep.Nodes {
			rows = append(rows, nodeRow(s, n))
		}
		b.WriteString(table("  ", rows))
	}

	if len(rep.Hints) > 0 {
		fmt.Fprintf(&b, "\n%s\n", s.Bold("Why"))
		for _, h := range rep.Hints {
			lines := strings.Split(h, "\n")
			for i, l := range lines {
				switch {
				case i == 0 && !strings.HasPrefix(l, "  "):
					fmt.Fprintf(&b, "  %s\n", l)
				case isAdvice(l):
					fmt.Fprintf(&b, "  %s\n", s.Dim(l))
				default:
					fmt.Fprintf(&b, "  %s\n", highlightExpr(s, l))
				}
			}
		}
	}

	if len(rep.Notes) > 0 {
		fmt.Fprintf(&b, "\n%s\n", s.Bold("Notes"))
		for _, n := range rep.Notes {
			fmt.Fprintf(&b, "  %s %s\n", s.Dim("•"), n)
		}
	}

	if o.Devices {
		for _, n := range rep.Nodes {
			if n.Excluded != "" || !hasDevices(n) {
				continue
			}
			b.WriteString(deviceTable(s, n))
		}
	} else if rep.Status != explain.Allocated && len(rep.Nodes) > 0 {
		fmt.Fprintf(&b, "\n%s\n", s.Dim("Run with --devices for the per-device table, or -o json for everything."))
	}
	b.WriteString("\n")
	_, err := io.WriteString(w, b.String())
	return err
}

func isAdvice(l string) bool {
	t := strings.TrimSpace(l)
	for _, p := range []string{"values seen:", "add a ", "the scheduler", "guard optional"} {
		if strings.HasPrefix(t, p) {
			return true
		}
	}
	return false
}

func badge(s Style, st explain.Status) string {
	switch st {
	case explain.Fits:
		return s.Badge("1;30;42", "FITS")
	case explain.Allocated:
		return s.Badge("1;30;42", "ALLOCATED")
	case explain.Blocked:
		return s.Badge("1;30;41", "BLOCKED")
	case explain.AllocatorError:
		return s.Badge("1;30;41", "ERROR")
	default:
		return s.Badge("1;30;43", "UNEXPLAINED")
	}
}

func nodeRow(s Style, n explain.NodeReport) []cell {
	name := c(n.Node, nil)
	if n.Excluded != "" {
		return []cell{c(n.Node, s.Dim), c("–", s.Dim), c("excluded: "+n.Excluded, s.Dim)}
	}
	var mark cell
	switch {
	case n.Verdict.Err != "":
		mark = c("error", s.Red)
	case n.Verdict.Fits:
		mark = c("✓ fits", s.Green)
	default:
		mark = c("✗", s.Red)
	}
	why := nodeWhy(n)
	if n.Note != "" {
		why = strings.TrimPrefix(why+" · "+n.Note, " · ")
	}
	style := (func(string) string)(nil)
	if !n.Agree {
		why = "⚠ unexplained: allocator says " + map[bool]string{true: "fits", false: "no"}[n.Verdict.Fits] +
			", claimcheck says " + map[bool]string{true: "fits", false: "no"}[n.Diagnosis.Fits]
		style = s.Yellow
	}
	return []cell{name, mark, c(why, style)}
}

// nodeWhy summarizes one node in a single line.
func nodeWhy(n explain.NodeReport) string {
	switch {
	case n.Verdict.Err != "":
		return diagnose.ShortCELError(n.Verdict.Err)
	case n.Verdict.NodeReason != "":
		return n.Verdict.NodeReason
	case n.Verdict.Fits:
		var devs []string
		for _, a := range n.Verdict.Allocation {
			for _, r := range a.Devices.Results {
				devs = append(devs, r.Request+"→"+r.Device)
			}
		}
		return strings.Join(devs, ", ")
	}
	var parts []string
	for _, r := range n.Diagnosis.Requests {
		if r.Satisfied {
			continue
		}
		if len(r.Alternatives) > 0 {
			var alts []string
			for _, a := range r.Alternatives {
				alts = append(alts, requestWhy(a))
			}
			parts = append(parts, strings.Join(alts, " | "))
			continue
		}
		parts = append(parts, requestWhy(r))
	}
	if n.Diagnosis.CounterConflict != "" {
		parts = append(parts, "partitions exceed their shared counters together")
	}
	if c := n.Diagnosis.ConstraintFailure; c != "" {
		if f := strings.Fields(c); len(f) >= 2 && strings.HasSuffix(f[0], "Attribute") {
			parts = append(parts, f[0]+" "+f[1]+" can't be satisfied")
		} else {
			parts = append(parts, "constraints conflict")
		}
	}
	if n.Diagnosis.Contention != "" {
		parts = append(parts, "requests compete for the same devices")
	}
	return strings.Join(parts, "; ")
}

func requestWhy(r diagnose.Request) string {
	short := r.Name
	if r.Problem != "" && len(r.Devices) == 0 {
		if strings.Contains(r.Problem, "not found") {
			return short + ": " + r.Problem
		}
		return short + ": no devices"
	}
	counts := map[string]int{}
	var order []string
	for _, d := range r.Devices {
		if d.Reason == diagnose.Available {
			continue
		}
		label := reasonLabel(d)
		if counts[label] == 0 {
			order = append(order, label)
		}
		counts[label]++
	}
	sort.SliceStable(order, func(i, j int) bool { return counts[order[i]] > counts[order[j]] })
	var bits []string
	for _, l := range order {
		bits = append(bits, fmt.Sprintf("%d %s", counts[l], l))
	}
	need := fmt.Sprintf("%d/%d usable", r.Available, r.Needed)
	if len(bits) == 0 {
		return short + " " + need
	}
	return short + " " + need + " · " + strings.Join(bits, ", ")
}

func reasonLabel(d diagnose.Device) string {
	switch d.Reason {
	case diagnose.RequestSelector:
		return fmt.Sprintf("fail selector #%d", *d.Selector)
	case diagnose.ClassSelector:
		return fmt.Sprintf("fail class selector #%d", *d.Selector)
	case diagnose.SelectorError:
		return "selector error"
	case diagnose.InUse:
		return "in use"
	case diagnose.Tainted:
		return "tainted"
	case diagnose.DeviceNode:
		return "on other nodes"
	case diagnose.FeatureDisabled:
		return "need disabled feature"
	case diagnose.CountersExhausted:
		return "counters used up"
	case diagnose.Incompatible:
		return "incompatible group"
	case diagnose.PoolIncomplete:
		return "pool incomplete"
	case diagnose.CapacityUsedUp:
		return "capacity used up"
	case diagnose.CapacityNeverFits:
		return "capacity never fits"
	case diagnose.CapacityError:
		return "capacity error"
	}
	return string(d.Reason)
}

func deviceTable(s Style, n explain.NodeReport) string {
	var b strings.Builder
	fmt.Fprintf(&b, "\n%s %s\n", s.Bold("Devices on"), s.Bold(n.Node))
	rows := [][]cell{{c("REQUEST", s.Dim), c("DEVICE", s.Dim), c("RESULT", s.Dim), c("DETAIL", s.Dim)}}
	for _, r := range n.Diagnosis.Requests {
		reqs := []diagnose.Request{r}
		if len(r.Alternatives) > 0 {
			reqs = r.Alternatives
		}
		for _, rr := range reqs {
			devs := append([]diagnose.Device(nil), rr.Devices...)
			diagnose.SortDevices(devs)
			for _, d := range devs {
				res := c("✓ usable", s.Green)
				if d.Reason != diagnose.Available {
					res = c("✗ "+reasonLabel(d), s.Red)
				}
				rows = append(rows, []cell{c(rr.Name, nil), c(d.Pool+"/"+d.Name, nil), res, c(d.Detail, s.Dim)})
			}
			if len(devs) == 0 {
				rows = append(rows, []cell{c(rr.Name, nil), c("(none)", s.Dim), c("", nil), c("", nil)})
			}
		}
	}
	b.WriteString(table("  ", rows))
	return b.String()
}

func hasDevices(n explain.NodeReport) bool {
	for _, r := range n.Diagnosis.Requests {
		if len(r.Devices) > 0 {
			return true
		}
		for _, a := range r.Alternatives {
			if len(a.Devices) > 0 {
				return true
			}
		}
	}
	return false
}

// highlightExpr colors the CEL expression at the end of a selector line.
func highlightExpr(s Style, l string) string {
	if i := strings.Index(l, ": device."); i >= 0 {
		return l[:i+2] + s.Cyan(l[i+2:])
	}
	return l
}
