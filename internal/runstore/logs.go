package runstore

import (
	"bufio"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Stream names.
const (
	Stdout = "stdout"
	Stderr = "stderr"
)

// Line is one timestamped line of output.
type Line struct {
	TS     string `json:"ts"`
	Stream string `json:"stream"`
	Text   string `json:"line"`
}

// FormatLogLine renders a line as stored on disk: "<ts> <text>\n".
func FormatLogLine(t time.Time, text string) string {
	return t.UTC().Format(TimeFormat) + " " + text + "\n"
}

// ReadLogs reads the requested streams and merges them by timestamp (stable,
// so lines from one stream keep their order).
func (s *Store) ReadLogs(id string, streams ...string) ([]Line, error) {
	var all []Line
	for _, st := range streams {
		lines, err := readLog(filepath.Join(s.Dir(id), st+".log"), st)
		if err != nil {
			return nil, err
		}
		all = append(all, lines...)
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].TS < all[j].TS })
	return all, nil
}

func readLog(path, stream string) ([]Line, error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []Line
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for sc.Scan() {
		ts, text, ok := strings.Cut(sc.Text(), " ")
		if !ok || len(ts) != len(TimeFormat) {
			ts, text = "", sc.Text()
		}
		out = append(out, Line{TS: ts, Stream: stream, Text: text})
	}
	return out, sc.Err()
}
