// Package render prints explain reports for humans (text) and machines (JSON).
package render

import (
	"os"
	"strings"
	"unicode/utf8"

	"golang.org/x/term"
)

// Style applies ANSI styling when enabled.
type Style struct{ on bool }

// AutoStyle enables color for terminals unless NO_COLOR is set.
func AutoStyle(f *os.File) Style {
	if os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb" {
		return Style{}
	}
	return Style{on: term.IsTerminal(int(f.Fd()))}
}

// Plain returns a style that never emits escape codes.
func Plain() Style { return Style{} }

// Color forces color on, for tests and --color=always.
func Color() Style { return Style{on: true} }

func (s Style) wrap(code, text string) string {
	if !s.on || text == "" {
		return text
	}
	return "\x1b[" + code + "m" + text + "\x1b[0m"
}

func (s Style) Bold(t string) string   { return s.wrap("1", t) }
func (s Style) Dim(t string) string    { return s.wrap("2", t) }
func (s Style) Red(t string) string    { return s.wrap("31", t) }
func (s Style) Green(t string) string  { return s.wrap("32", t) }
func (s Style) Yellow(t string) string { return s.wrap("33", t) }
func (s Style) Cyan(t string) string   { return s.wrap("36", t) }
func (s Style) Badge(code, t string) string {
	return s.wrap(code, " "+t+" ")
}

// cell is a table cell: plain text for measuring, plus a styler.
type cell struct {
	text  string
	style func(string) string
}

func c(text string, style func(string) string) cell { return cell{text, style} }

// table renders rows with columns padded to their widest plain-text cell.
func table(indent string, rows [][]cell) string {
	var widths []int
	for _, r := range rows {
		for i, x := range r {
			if i >= len(widths) {
				widths = append(widths, 0)
			}
			widths[i] = max(widths[i], utf8.RuneCountInString(x.text))
		}
	}
	var b strings.Builder
	for _, r := range rows {
		b.WriteString(indent)
		for i, x := range r {
			txt := x.text
			if x.style != nil {
				txt = x.style(txt)
			}
			b.WriteString(txt)
			if i < len(r)-1 {
				b.WriteString(strings.Repeat(" ", widths[i]-utf8.RuneCountInString(x.text)+3))
			}
		}
		b.WriteString("\n")
	}
	return b.String()
}
