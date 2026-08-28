// Package buildinfo reports the source revision a binary was built from.
//
// The revision is a deployment's identity. Without it, "is the process I am
// looking at the one I just built?" can only be answered with proxies — a path,
// an mtime, a file size, or a behavioural A/B — and every one of those proxies
// reports success for a deploy that silently did nothing.
//
// The failure this exists to catch is a build from the wrong source tree. More
// than one clone of this module can exist on a machine, each with the same
// module path and the same command names, so `go install ./cmd/...` in the
// wrong directory overwrites the right binaries with no warning and exit 0. A
// binary that can name its own revision makes that mistake visible; one that
// cannot makes it undetectable.
package buildinfo

import (
	"runtime/debug"
)

// Version is set at link time with
// -X github.com/tjmisko/switchboard/internal/buildinfo.Version=<value>.
//
// It is authoritative when non-empty: the deploy script knows the revision it
// intends to ship and whether the tree was dirty, and it can express both
// correctly. When it is empty — any plain `go build` or `go test` — Get falls
// back to the VCS stamp the Go toolchain embeds automatically, so even a
// hand-built binary still names its true source tree.
var Version string

// shortRevisionLen matches the abbreviation git uses in log output, which is
// what a human comparing a deployed binary against `git log` will have on
// screen.
const shortRevisionLen = 7

// Info describes the build behind the running binary.
type Info struct {
	// Version is the link-time value, empty when it was not injected.
	Version string
	// Revision is the short VCS revision, empty when the binary was built
	// outside a VCS tree (a release tarball, or `go build` on extracted
	// sources).
	Revision string
	// Modified reports the toolchain's view of whether the tree carried
	// uncommitted changes.
	//
	// Treat it as advisory only. Go reports it as true for a *clean* linked
	// git worktree, so under a worktree-per-branch workflow it is true almost
	// always and cannot gate a deploy. Anything that needs to refuse a dirty
	// tree must ask git directly (`git status --porcelain`) rather than trust
	// this field.
	Modified bool
}

// Get returns the build behind the running binary.
func Get() Info { return get(debug.ReadBuildInfo) }

func get(read func() (*debug.BuildInfo, bool)) Info {
	info := Info{Version: Version}
	build, ok := read()
	if !ok {
		return info
	}
	for _, setting := range build.Settings {
		switch setting.Key {
		case "vcs.revision":
			info.Revision = shortRevision(setting.Value)
		case "vcs.modified":
			info.Modified = setting.Value == "true"
		}
	}
	return info
}

func shortRevision(revision string) string {
	if len(revision) <= shortRevisionLen {
		return revision
	}
	return revision[:shortRevisionLen]
}

// String renders the build for a human or for a deploy script's equality check.
//
// The injected Version wins when present. Otherwise the bare revision is used:
// Modified is deliberately not appended, because a worktree build would then
// report "-dirty" every time and train the reader to ignore the suffix that is
// supposed to mean something.
func (i Info) String() string {
	if i.Version != "" {
		return i.Version
	}
	if i.Revision != "" {
		return i.Revision
	}
	return "unknown"
}
