package tomlx

import "testing"

type sample struct {
	Name string `toml:"name"`
	Env  struct {
		Set map[string]string `toml:"set"`
	} `toml:"env"`
	Limits struct {
		Timeout string `toml:"timeout"`
	} `toml:"limits"`
}

func TestDecodeUnknownKeyHasLine(t *testing.T) {
	src := "name = \"a\"\n\n[limits]\ntimeout = \"1m\"\ntimout = \"2m\"\n"
	var s sample
	_, diags := Decode("a.toml", []byte(src), &s)
	if len(diags) != 1 {
		t.Fatalf("want 1 diag, got %v", diags)
	}
	if diags[0].Line != 5 || diags[0].Key != "limits.timout" {
		t.Fatalf("bad diag %+v", diags[0])
	}
}

func TestIndexLinesAndStrings(t *testing.T) {
	src := "name = \"a\"\n[env]\nset = { A = \"op://vault/x\", B = \"b\" }\n[limits]\ntimeout = \"1m\"\n"
	var s sample
	doc, diags := Decode("a.toml", []byte(src), &s)
	if len(diags) != 0 {
		t.Fatal(diags)
	}
	if got := doc.Line("limits.timeout"); got != 5 {
		t.Fatalf("limits.timeout line = %d", got)
	}
	if got := doc.Line("env.set.A"); got != 3 {
		t.Fatalf("env.set.A line = %d", got)
	}
	if got := doc.Line("limits.nope"); got != 4 {
		t.Fatalf("parent fallback line = %d", got)
	}
	r := ReservedSecretRefs(doc)
	if len(r) != 1 || r[0].Key != "env.set.A" || r[0].Line != 3 {
		t.Fatalf("reserved refs = %+v", r)
	}
}

func TestSyntaxError(t *testing.T) {
	var s sample
	_, diags := Decode("a.toml", []byte("name = \"a\"\nruntime = \"x\"\n[runtime]\n"), &s)
	if !diags.HasErrors() {
		t.Fatal("expected error")
	}
}

func TestTypeError(t *testing.T) {
	var s sample
	_, diags := Decode("a.toml", []byte("name = 3\n"), &s)
	if !diags.HasErrors() || diags[0].Line != 1 {
		t.Fatalf("diags = %+v", diags)
	}
}
