package logfile

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRotate(t *testing.T) {
	p := filepath.Join(t.TempDir(), "daemon.log")
	w, err := Open(p, 10, 2)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"aaaaaaaa\n", "bbbbbbbb\n", "cccccccc\n", "dddddddd\n"} {
		w.Write([]byte(s))
	}
	w.Close()
	cur, _ := os.ReadFile(p)
	one, _ := os.ReadFile(p + ".1")
	two, _ := os.ReadFile(p + ".2")
	if string(cur) != "dddddddd\n" || string(one) != "cccccccc\n" || string(two) != "bbbbbbbb\n" {
		t.Fatalf("%q %q %q", cur, one, two)
	}
	if _, err := os.Stat(p + ".3"); err == nil {
		t.Fatal("kept too many")
	}
}
