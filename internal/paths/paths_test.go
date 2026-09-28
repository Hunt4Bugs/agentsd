package paths

import (
	"os"
	"path/filepath"
	"testing"
)

func TestExpand(t *testing.T) {
	cases := map[string]string{"~": "/h", "~/src": "/h/src", "/abs/../x": "/x", "~/a/../b": "/h/b"}
	for in, want := range cases {
		got, err := Expand(in, "/h")
		if err != nil || got != want {
			t.Errorf("Expand(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"rel", "~bob/x", ""} {
		if _, err := Expand(bad, "/h"); err == nil {
			t.Errorf("Expand(%q) should fail", bad)
		}
	}
}

func TestWithin(t *testing.T) {
	if !Within("/h/src/x", "/h/src") || !Within("/h/src", "/h/src") {
		t.Fatal("expected within")
	}
	if Within("/h/srcx", "/h/src") || Within("/h", "/h/src") {
		t.Fatal("expected not within")
	}
}

func TestWithinAnySymlinkEscape(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "src")
	outside := filepath.Join(base, "etc")
	must(t, os.MkdirAll(root, 0o755))
	must(t, os.MkdirAll(outside, 0o755))
	must(t, os.Symlink(outside, filepath.Join(root, "link")))
	if WithinAny(filepath.Join(root, "link", "passwd"), []string{root}) {
		t.Fatal("symlink escape was allowed")
	}
	if !WithinAny(filepath.Join(root, "new", "dir"), []string{root}) {
		t.Fatal("non-existent child should be within")
	}
}

func TestContract(t *testing.T) {
	if Contract("/h/src", "/h") != "~/src" || Contract("/x", "/h") != "/x" {
		t.Fatal("contract")
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
