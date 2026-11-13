package render

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/asaleem9/claimcheck/internal/explain"
	"github.com/asaleem9/claimcheck/internal/whatif"
)

// WhatIfReport compares a target before and after hypothetical changes.
type WhatIfReport struct {
	Changes []whatif.Change `json:"changes"`
	Before  *explain.Report `json:"before"`
	After   *explain.Report `json:"after"`
}

// SuggestReport lists single changes that would make the target fit.
type SuggestReport struct {
	Target      explain.Target      `json:"target"`
	Status      explain.Status      `json:"status"`
	Tried       int                 `json:"tried"`
	Suggestions []whatif.Suggestion `json:"suggestions"`
}

// JSONAny writes any report as indented JSON.
func JSONAny(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// WhatIf writes the before/after comparison, then the full "after" report.
func WhatIf(w io.Writer, r WhatIfReport, o TextOptions) error {
	s := o.Style
	var names []string
	for _, c := range r.Changes {
		names = append(names, c.String())
	}
	var b strings.Builder
	fmt.Fprintf(&b, "\n%s %s\n\n", s.Bold("What if you"), s.Bold(strings.Join(names, " and ")+"?"))
	// The badge cell measures its plain text and renders the colored badge.
	badgeCell := func(st explain.Status) cell {
		return c(badge(Plain(), st), func(string) string { return badge(s, st) })
	}
	rows := [][]cell{
		{c("before", s.Dim), badgeCell(r.Before.Status), c(r.Before.Summary, nil)},
		{c("after", s.Dim), badgeCell(r.After.Status), c(r.After.Summary, nil)},
	}
	b.WriteString(table("  ", rows))
	if _, err := io.WriteString(w, b.String()); err != nil {
		return err
	}
	return Text(w, r.After, o)
}

// Suggest writes the changes that would make the target fit.
func Suggest(w io.Writer, r SuggestReport, o TextOptions) error {
	s := o.Style
	var b strings.Builder
	fmt.Fprintf(&b, "\n%s %s  %s\n\n", s.Bold(r.Target.String()), badge(s, r.Status), s.Dim(fmt.Sprintf("tried %s", plural(r.Tried, "single change"))))
	switch {
	case r.Status == explain.Fits || r.Status == explain.Allocated:
		fmt.Fprintf(&b, "  Nothing to change: it already fits or is allocated.\n")
	case len(r.Suggestions) == 0:
		fmt.Fprintf(&b, "  No single change makes it fit. Run %s to see everything that's in the way.\n", s.Cyan("kubectl claimcheck explain "+r.Target.Kind+"/"+r.Target.Name+" -n "+r.Target.Namespace))
	default:
		fmt.Fprintf(&b, "  %s\n", "Any one of these would let it fit:")
		var rows [][]cell
		for _, sg := range r.Suggestions {
			holds := ""
			if sg.Holds != "" {
				holds = "holds " + sg.Holds
			}
			rows = append(rows, []cell{c(sg.Change.String(), nil), c(holds, s.Dim), c("→ fits on "+strings.Join(sg.Nodes, ", "), s.Green)})
		}
		b.WriteString(table("    ", rows))
		first := r.Suggestions[0].Change
		flag := map[whatif.Kind]string{
			whatif.FreeClaim:       "--free resourceclaim/" + first.Name,
			whatif.RemoveTaintRule: "--remove-taint-rule " + first.Name,
		}[first.Kind]
		fmt.Fprintf(&b, "\n  %s %s\n", s.Dim("Details:"), s.Cyan("kubectl claimcheck whatif "+r.Target.Kind+"/"+r.Target.Name+" -n "+r.Target.Namespace+" "+flag))
	}
	b.WriteString("\n")
	_, err := io.WriteString(w, b.String())
	return err
}
