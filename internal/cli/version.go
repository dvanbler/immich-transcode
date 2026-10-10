package cli

import "runtime/debug"

// resolveVersion returns the version stamped in at build time, or else the
// git commit Go records in the binary, or "dev".
func resolveVersion(stamped string) string {
	if stamped != "" {
		return stamped
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "dev"
	}
	if v := info.Main.Version; v != "" && v != "(devel)" {
		return v
	}
	var rev string
	var dirty bool
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if rev == "" {
		return "dev"
	}
	if len(rev) > 12 {
		rev = rev[:12]
	}
	v := "dev-" + rev
	if dirty {
		v += "-dirty"
	}
	return v
}
