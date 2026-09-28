// Package paths expands ~ and checks containment, resolving symlinks so that
// a path cannot escape a root through a link.
package paths

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Expand replaces a leading ~ with home and cleans the result. The result
// must be absolute.
func Expand(p, home string) (string, error) {
	switch {
	case p == "~":
		p = home
	case strings.HasPrefix(p, "~/"):
		p = filepath.Join(home, p[2:])
	case strings.HasPrefix(p, "~"):
		return "", fmt.Errorf("%q: ~user paths are not supported", p)
	}
	if !filepath.IsAbs(p) {
		return "", fmt.Errorf("%q: path must be absolute or start with ~/", p)
	}
	return filepath.Clean(p), nil
}

// Contract rewrites a path under home back to ~/... for display.
func Contract(p, home string) string {
	if p == home {
		return "~"
	}
	if rel, ok := strings.CutPrefix(p, home+string(filepath.Separator)); ok {
		return "~/" + rel
	}
	return p
}

// Real resolves symlinks in the longest existing prefix of p and appends the
// remaining, not-yet-existing components.
func Real(p string) string {
	p = filepath.Clean(p)
	var rest []string
	cur := p
	for {
		if r, err := filepath.EvalSymlinks(cur); err == nil {
			parts := append([]string{r}, rest...)
			return filepath.Join(parts...)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return p
		}
		rest = append([]string{filepath.Base(cur)}, rest...)
		cur = parent
	}
}

// Within reports whether p equals root or lies beneath it, lexically.
func Within(p, root string) bool {
	p, root = filepath.Clean(p), filepath.Clean(root)
	if p == root || root == "/" {
		return true
	}
	return strings.HasPrefix(p, root+string(filepath.Separator))
}

// WithinAny reports whether p (after symlink resolution) lies under any root
// (also resolved).
func WithinAny(p string, roots []string) bool {
	rp := Real(p)
	for _, r := range roots {
		if Within(rp, Real(r)) {
			return true
		}
	}
	return false
}

// Exists reports whether p exists (without following a final symlink).
func Exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}
