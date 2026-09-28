// Package diag holds validation diagnostics that point at a file, line and key.
package diag

import (
	"fmt"
	"strings"
)

type Severity string

const (
	SevError   Severity = "error"
	SevWarning Severity = "warning"
)

type Diagnostic struct {
	File     string   `json:"file"`
	Line     int      `json:"line,omitempty"`
	Key      string   `json:"key,omitempty"`
	Severity Severity `json:"severity"`
	Message  string   `json:"message"`
}

func (d Diagnostic) String() string {
	var b strings.Builder
	b.WriteString(d.File)
	if d.Line > 0 {
		fmt.Fprintf(&b, ":%d", d.Line)
	}
	b.WriteString(": ")
	if d.Severity == SevWarning {
		b.WriteString("warning: ")
	}
	if d.Key != "" {
		fmt.Fprintf(&b, "%s: ", d.Key)
	}
	b.WriteString(d.Message)
	return b.String()
}

type List []Diagnostic

func (l List) HasErrors() bool {
	for _, d := range l {
		if d.Severity == SevError {
			return true
		}
	}
	return false
}

func (l List) Errors() List {
	var out List
	for _, d := range l {
		if d.Severity == SevError {
			out = append(out, d)
		}
	}
	return out
}

func (l List) Error() string {
	parts := make([]string, len(l))
	for i, d := range l {
		parts[i] = d.String()
	}
	return strings.Join(parts, "\n")
}
