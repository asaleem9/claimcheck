package render

import (
	"fmt"
	"io"
	"strings"

	"github.com/asaleem9/claimcheck/internal/explain"
)

// Gang writes the PodGroup placement simulation for a terminal.
func Gang(w io.Writer, r *explain.GangReport, o TextOptions) error {
	s := o.Style
	var b strings.Builder
	policy := "basic scheduling"
	if r.Policy == "gang" {
		policy = fmt.Sprintf("gang, minCount %d", r.MinCount)
	}
	fmt.Fprintf(&b, "\n%s%s\n\n", s.Bold(r.Target.Kind+" "+r.Target.Namespace+"/"+r.Target.Name),
		s.Dim(fmt.Sprintf("  (%s, %s)", policy, plural(len(r.Members), "member"))))
	fmt.Fprintf(&b, "%s  %s\n", badge(s, r.Status), r.Summary)

	fmt.Fprintf(&b, "\n%s %s\n", s.Bold("Members"), s.Dim("(placed in name order; each sees the devices taken before it)"))
	var rows [][]cell
	var failed []explain.Member
	for _, m := range r.Members {
		switch {
		case m.Bound:
			rows = append(rows, []cell{c(m.Pod, nil), c("✓ "+m.Node, s.Green), c("already bound", s.Dim)})
		case m.Placed && m.Node == "":
			rows = append(rows, []cell{c(m.Pod, nil), c("✓", s.Green), c("claims already allocated", s.Dim)})
		case m.Placed:
			rows = append(rows, []cell{c(m.Pod, nil), c("✓ "+m.Node, s.Green), c(strings.Join(m.Devices, ", "), s.Dim)})
		default:
			why := "no node fits"
			if m.Report != nil {
				why = strings.ToLower(m.Report.Summary[:1]) + m.Report.Summary[1:]
			}
			rows = append(rows, []cell{c(m.Pod, nil), c("✗", s.Red), c(why, nil)})
			failed = append(failed, m)
		}
	}
	b.WriteString(table("  ", rows))

	if len(failed) > 0 && failed[0].Report != nil {
		f := failed[0]
		fmt.Fprintf(&b, "\n%s\n", s.Bold("Why "+f.Pod+" can't be placed"))
		var nrows [][]cell
		for _, n := range f.Report.Nodes {
			nrows = append(nrows, nodeRow(s, n))
		}
		b.WriteString(table("  ", nrows))
		for _, h := range f.Report.Hints {
			for i, l := range strings.Split(h, "\n") {
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

	if len(r.Notes) > 0 {
		fmt.Fprintf(&b, "\n%s\n", s.Bold("Notes"))
		for _, n := range r.Notes {
			fmt.Fprintf(&b, "  %s %s\n", s.Dim("•"), n)
		}
	}
	b.WriteString("\n")
	_, err := io.WriteString(w, b.String())
	return err
}
