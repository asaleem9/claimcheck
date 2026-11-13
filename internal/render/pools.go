package render

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/asaleem9/claimcheck/internal/diagnose"
)

// PoolsReport is the input for the pools view.
type PoolsReport struct {
	Context string              `json:"context,omitempty"`
	Nodes   int                 `json:"nodes"`
	Pools   []diagnose.PoolView `json:"pools"`
}

// PoolsJSON writes the report as indented JSON.
func PoolsJSON(w io.Writer, r PoolsReport) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

// stateOrder is the display order of states, worst first.
var stateOrder = []diagnose.State{
	diagnose.InUseState, diagnose.SharedState, diagnose.TaintedState,
	diagnose.BlockedState, diagnose.UnavailableState, diagnose.Free,
}

var stateLabel = map[diagnose.State]string{
	diagnose.InUseState:       "in use",
	diagnose.SharedState:      "shared",
	diagnose.TaintedState:     "tainted",
	diagnose.BlockedState:     "blocked",
	diagnose.UnavailableState: "unavailable",
	diagnose.Free:             "free",
}

func (s Style) glyph(st diagnose.State) string {
	switch st {
	case diagnose.InUseState:
		return s.Red("█")
	case diagnose.SharedState:
		return s.Cyan("▓")
	case diagnose.TaintedState:
		return s.Yellow("▚")
	case diagnose.BlockedState:
		return s.Magenta("▒")
	case diagnose.UnavailableState:
		return s.Dim("×")
	}
	return s.Green("░")
}

const maxStrip = 48

// Pools writes the capacity view for a terminal.
func Pools(w io.Writer, r PoolsReport, o TextOptions) error {
	s := o.Style
	var b strings.Builder
	fmt.Fprintf(&b, "\n%s", s.Bold("DRA capacity"))
	ctx := fmt.Sprintf("  %s · %s", plural(r.Nodes, "node"), plural(len(r.Pools), "pool"))
	if r.Context != "" {
		ctx = "  " + r.Context + " ·" + ctx[1:]
	}
	fmt.Fprintf(&b, "%s\n", s.Dim(ctx))

	b.WriteString(classTable(s, r.Pools))

	for _, p := range r.Pools {
		node := p.Node
		if node == "" {
			node = "(multi-node)"
		}
		fmt.Fprintf(&b, "\n%s %s %s\n", s.Bold(node), s.Dim("· pool"), p.Pool+s.Dim("  "+p.Driver))
		if p.Incomplete != "" {
			fmt.Fprintf(&b, "  %s\n", s.Yellow(p.Incomplete))
		}
		if len(p.Devices) > 0 {
			fmt.Fprintf(&b, "  %s  %s\n", strip(s, p.Devices), stateSummary(s, p.Count()))
		}
		var rows [][]cell
		// Counter sets with identical usage (common when every GPU is
		// partitioned the same way) collapse into one row.
		type counterRow struct {
			sets      []string
			name      string
			used, all resource.Quantity
			info      string
		}
		var crows []*counterRow
		byKey := map[string]*counterRow{}
		for _, ctr := range p.Counters {
			used := ctr.Total.DeepCopy()
			used.Sub(ctr.Left)
			info := fmt.Sprintf("%s of %s used", used.String(), ctr.Total.String())
			switch {
			case ctr.LargestFree != nil:
				info += " · largest free partition " + ctr.LargestFree.String()
			case ctr.Left.Sign() > 0:
				info += " · no free partition fits"
			}
			k := ctr.Name + "|" + info
			if r := byKey[k]; r != nil {
				r.sets = append(r.sets, ctr.Set)
				continue
			}
			r := &counterRow{sets: []string{ctr.Set}, name: ctr.Name, used: used, all: ctr.Total, info: info}
			byKey[k] = r
			crows = append(crows, r)
		}
		for _, r := range crows {
			label := r.sets[0]
			if n := len(r.sets); n > 1 {
				label = fmt.Sprintf("%s … %s (%d)", r.sets[0], r.sets[n-1], n)
			}
			rows = append(rows, []cell{c(label+" "+r.name, nil), c(bar(s, r.used, r.all), nil), c(r.info, s.Dim)})
		}
		for _, cp := range p.Capacities {
			info := fmt.Sprintf("%s of %s used", cp.Used.String(), cp.Total.String())
			if cp.Claims > 0 {
				info += fmt.Sprintf(" by %s", plural(cp.Claims, "claim"))
			}
			rows = append(rows, []cell{c(cp.Device+" "+cp.Name, nil), c(bar(s, cp.Used, cp.Total), nil), c(info, s.Dim)})
		}
		b.WriteString(table("  ", rows))

		if o.Devices {
			var drows [][]cell
			for _, d := range p.Devices {
				detail := d.Detail
				if len(d.Owners) > 0 {
					detail = strings.TrimSpace(diagnose.Short(d.Owners) + "  " + detail)
				}
				drows = append(drows, []cell{c(d.Name, nil), c(s.glyph(d.State)+" "+stateLabel[d.State], nil), c(detail, s.Dim)})
			}
			b.WriteString(table("    ", drows))
		}
	}

	fmt.Fprintf(&b, "\n%s", s.Dim("legend "))
	for _, st := range stateOrder {
		fmt.Fprintf(&b, " %s %s", s.glyph(st), s.Dim(stateLabel[st]))
	}
	b.WriteString("\n")
	if !o.Devices {
		fmt.Fprintf(&b, "%s\n", s.Dim("Run with --devices to list every device with its holder or reason."))
	}
	b.WriteString("\n")
	_, err := io.WriteString(w, b.String())
	return err
}

// classTable summarizes devices per DeviceClass across all pools.
func classTable(s Style, pools []diagnose.PoolView) string {
	counts := map[string]map[diagnose.State]int{}
	for _, p := range pools {
		for _, d := range p.Devices {
			for _, cl := range d.Classes {
				if counts[cl] == nil {
					counts[cl] = map[diagnose.State]int{}
				}
				counts[cl][d.State]++
			}
		}
	}
	if len(counts) == 0 {
		return ""
	}
	var names []string
	for n := range counts {
		names = append(names, n)
	}
	sort.Strings(names)
	header := []cell{c("DEVICECLASS", s.Dim), c("TOTAL", s.Dim)}
	for _, st := range stateOrder {
		header = append(header, c(strings.ToUpper(stateLabel[st]), s.Dim))
	}
	rows := [][]cell{header}
	for _, n := range names {
		total := 0
		for _, v := range counts[n] {
			total += v
		}
		row := []cell{c(n, s.Bold), c(fmt.Sprint(total), nil)}
		for _, st := range stateOrder {
			v := counts[n][st]
			style := s.Dim
			if v > 0 {
				style = map[diagnose.State]func(string) string{
					diagnose.Free: s.Green, diagnose.InUseState: s.Red, diagnose.SharedState: s.Cyan,
					diagnose.TaintedState: s.Yellow, diagnose.BlockedState: s.Magenta, diagnose.UnavailableState: s.Dim,
				}[st]
			}
			row = append(row, c(fmt.Sprint(v), style))
		}
		rows = append(rows, row)
	}
	return "\n" + table("  ", rows)
}

// strip draws one glyph per device, grouped by state, scaled down for
// large pools.
func strip(s Style, devs []diagnose.DeviceView) string {
	counts := map[diagnose.State]int{}
	for _, d := range devs {
		counts[d.State]++
	}
	n := len(devs)
	width := min(n, maxStrip)
	var b strings.Builder
	drawn := 0
	for i, st := range stateOrder {
		k := counts[st] * width / n
		if counts[st] > 0 && k == 0 {
			k = 1
		}
		if i == len(stateOrder)-1 {
			k = max(width-drawn, 0)
		}
		k = min(k, width-drawn)
		b.WriteString(strings.Repeat(s.glyph(st), k))
		drawn += k
	}
	return b.String()
}

func stateSummary(s Style, counts map[diagnose.State]int) string {
	var parts []string
	for _, st := range stateOrder {
		if counts[st] > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", counts[st], stateLabel[st]))
		}
	}
	return s.Dim(strings.Join(parts, " · "))
}

// bar renders used/total as a 20-cell meter, colored by how full it is.
func bar(s Style, used, total resource.Quantity) string {
	const width = 20
	frac := 0.0
	if total.Sign() > 0 {
		frac = used.AsApproximateFloat64() / total.AsApproximateFloat64()
	}
	frac = min(max(frac, 0), 1)
	filled := int(frac*width + 0.5)
	color := s.Green
	switch {
	case frac >= 0.9:
		color = s.Red
	case frac >= 0.6:
		color = s.Yellow
	}
	return color(strings.Repeat("█", filled)) + s.Dim(strings.Repeat("░", width-filled))
}

func plural(n int, word string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, word)
	}
	return fmt.Sprintf("%d %ss", n, word)
}
