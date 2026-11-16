// Package release holds what identifies one MMX release.
package release

import "runtime/debug"

// DefaultVersion is the version of a build that is no release. The
// repository's official_extra_modules/shipped_with_mmx_version file holds
// the same value.
const DefaultVersion = "0.1.0-dev"

// Version is the version of this build. A release build sets it with
// -ldflags "-X mmx/internal/release.Version=<version>" and writes the same
// version into the official extra modules' shipped_with_mmx_version file.
var Version = DefaultVersion

// Commit is the Git commit that a build from a Git checkout was made from,
// which Go records in the build, followed by " with uncommitted changes"
// when the checkout had them; "" for a build without it, such as a release
// archive's, which its Version identifies. A lab can then tell whether a
// development build has a given commit.
func Commit() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	var revision string
	var modified bool
	for _, setting := range info.Settings {
		switch setting.Key {
		case "vcs.revision":
			revision = setting.Value
		case "vcs.modified":
			modified = setting.Value == "true"
		}
	}
	if revision != "" && modified {
		return revision + " with uncommitted changes"
	}
	return revision
}
