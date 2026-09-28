// Package tomlx wraps go-toml with strict decoding and a key→line index so that
// semantic validation errors can point at the offending line.
package tomlx

import (
	"errors"
	"fmt"
	"strings"

	"github.com/pelletier/go-toml/v2"
	"github.com/pelletier/go-toml/v2/unstable"

	"github.com/Hunt4Bugs/agentsd/internal/diag"
)

// StringValue is a string literal found in the document.
type StringValue struct {
	Key   string
	Line  int
	Value string
}

// Doc is the positional index of a parsed TOML document.
type Doc struct {
	File    string
	lines   map[string]int
	Strings []StringValue
}

// Line returns the line of key, or of its nearest defined parent, or 0.
func (d *Doc) Line(key string) int {
	for key != "" {
		if l, ok := d.lines[key]; ok {
			return l
		}
		i := strings.LastIndexByte(key, '.')
		if i < 0 {
			break
		}
		key = key[:i]
	}
	return 0
}

// Has reports whether key was set explicitly in the document.
func (d *Doc) Has(key string) bool {
	_, ok := d.lines[key]
	return ok
}

// Err returns an error diagnostic located at key.
func (d *Doc) Err(key, format string, args ...any) diag.Diagnostic {
	return d.diag(diag.SevError, key, format, args...)
}

// Warn returns a warning diagnostic located at key.
func (d *Doc) Warn(key, format string, args ...any) diag.Diagnostic {
	return d.diag(diag.SevWarning, key, format, args...)
}

func (d *Doc) diag(sev diag.Severity, key, format string, args ...any) diag.Diagnostic {
	msg := format
	if len(args) > 0 {
		msg = fmt.Sprintf(format, args...)
	}
	return diag.Diagnostic{File: d.File, Line: d.Line(key), Key: key, Severity: sev, Message: msg}
}

func index(file string, data []byte) *Doc {
	d := &Doc{File: file, lines: map[string]int{}}
	var p unstable.Parser
	p.Reset(data)
	var prefix []string
	for p.NextExpression() {
		e := p.Expression()
		switch e.Kind {
		case unstable.Table, unstable.ArrayTable:
			prefix, _ = keyParts(&p, e.Key())
			d.set(&p, prefix, e.Key())
		case unstable.KeyValue:
			d.keyValue(&p, prefix, e)
		}
	}
	return d
}

func (d *Doc) keyValue(p *unstable.Parser, prefix []string, e *unstable.Node) {
	parts, _ := keyParts(p, e.Key())
	full := append(append([]string{}, prefix...), parts...)
	line := d.set(p, full, e.Key())
	d.value(p, full, line, e.Value())
}

func (d *Doc) value(p *unstable.Parser, key []string, line int, v *unstable.Node) {
	switch v.Kind {
	case unstable.String:
		d.Strings = append(d.Strings, StringValue{Key: strings.Join(key, "."), Line: line, Value: string(v.Data)})
	case unstable.Array:
		it := v.Children()
		for it.Next() {
			d.value(p, key, line, it.Node())
		}
	case unstable.InlineTable:
		it := v.Children()
		for it.Next() {
			if it.Node().Kind == unstable.KeyValue {
				d.keyValue(p, key, it.Node())
			}
		}
	}
}

func (d *Doc) set(p *unstable.Parser, key []string, it unstable.Iterator) int {
	line := 0
	if it.Next() {
		line = p.Shape(it.Node().Raw).Start.Line
	}
	d.lines[strings.Join(key, ".")] = line
	return line
}

func keyParts(p *unstable.Parser, it unstable.Iterator) ([]string, int) {
	var parts []string
	for it.Next() {
		parts = append(parts, string(it.Node().Data))
	}
	return parts, len(parts)
}

// Decode strictly decodes data into v. Unknown keys, syntax errors and type
// mismatches are returned as error diagnostics with line numbers.
func Decode(file string, data []byte, v any) (*Doc, diag.List) {
	var p unstable.Parser
	p.Reset(data)
	for p.NextExpression() {
	}
	if err := p.Error(); err != nil {
		var pe *unstable.ParserError
		line := 0
		if errors.As(err, &pe) && pe.Highlight != nil {
			line = p.Shape(p.Range(pe.Highlight)).Start.Line
		}
		return &Doc{File: file, lines: map[string]int{}}, diag.List{{File: file, Line: line, Severity: diag.SevError, Message: "syntax error: " + err.Error()}}
	}
	doc := index(file, data)
	dec := toml.NewDecoder(strings.NewReader(string(data))).DisallowUnknownFields()
	err := dec.Decode(v)
	if err == nil {
		return doc, nil
	}
	var sme *toml.StrictMissingError
	if errors.As(err, &sme) {
		var out diag.List
		for i := range sme.Errors {
			e := &sme.Errors[i]
			row, _ := e.Position()
			out = append(out, diag.Diagnostic{File: file, Line: row, Key: strings.Join(e.Key(), "."), Severity: diag.SevError, Message: "unknown key"})
		}
		return doc, out
	}
	var de *toml.DecodeError
	if errors.As(err, &de) {
		row, _ := de.Position()
		msg := strings.TrimPrefix(de.Error(), "toml: ")
		return doc, diag.List{{File: file, Line: row, Key: strings.Join(de.Key(), "."), Severity: diag.SevError, Message: msg}}
	}
	return doc, diag.List{{File: file, Severity: diag.SevError, Message: strings.TrimPrefix(err.Error(), "toml: ")}}
}

// ReservedSecretRefs flags keychain: and op:// references, which are reserved
// syntax in v0.1 (spec §9).
func ReservedSecretRefs(doc *Doc) diag.List {
	var out diag.List
	for _, s := range doc.Strings {
		if strings.HasPrefix(s.Value, "keychain:") || strings.HasPrefix(s.Value, "op://") {
			out = append(out, diag.Diagnostic{File: doc.File, Line: s.Line, Key: s.Key, Severity: diag.SevError,
				Message: "secret references (keychain:, op://) are reserved for a later version; pass credentials with env.pass"})
		}
	}
	return out
}
