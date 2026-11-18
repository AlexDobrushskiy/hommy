// Package version is the build version, set with
// -ldflags "-X github.com/AlexDobrushskiy/hommy/internal/version.Version=<v>".
package version

import "runtime/debug"

// Version is set at link time; empty falls back to the VCS revision.
var Version = ""

// String returns the link-time version, else the VCS revision, else "dev".
func String() string {
	if Version != "" {
		return Version
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		rev, dirty := "", false
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				rev = s.Value
			case "vcs.modified":
				dirty = s.Value == "true"
			}
		}
		if len(rev) > 7 {
			rev = rev[:7]
		}
		if rev != "" {
			if dirty {
				rev += "-dirty"
			}
			return rev
		}
	}
	return "dev"
}
