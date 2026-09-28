// Package version holds build metadata, injected with -ldflags at release time.
package version

import "runtime/debug"

var (
	Version = "dev"
	Commit  = ""
	Date    = ""
)

func init() {
	if Commit != "" {
		return
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		if Version == "dev" && info.Main.Version != "" && info.Main.Version != "(devel)" {
			Version = info.Main.Version
		}
		for _, s := range info.Settings {
			switch s.Key {
			case "vcs.revision":
				Commit = s.Value
			case "vcs.time":
				Date = s.Value
			}
		}
	}
}
