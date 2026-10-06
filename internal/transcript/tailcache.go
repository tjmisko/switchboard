package transcript

import (
	"path/filepath"
	"strings"

	"github.com/tjmisko/switchboard/internal/tailcache"
)

// cached runs extract over path through the shared file-identity cache, so an
// unchanged transcript is stat'ed rather than re-read on every tick (#98). key
// names the extraction and every parameter its result depends on; the value
// is the extractor's small derived result, never transcript text.
func cached[V any](path, key string, extract func(string) (V, error)) (V, error) {
	if path == "" {
		return extract(path)
	}
	return tailcache.Load(tailcache.Default(), path, "transcript/"+key, extract)
}

// ForgetCached drops every cached extraction of a main transcript and of the
// files under its session directory (subagent transcripts and metas). A hook
// for the root, a session rotation and Forget call it: they announce changes
// the file identity may not show.
func ForgetCached(mainTranscript string) {
	if mainTranscript == "" {
		return
	}
	cache := tailcache.Default()
	cache.Invalidate(mainTranscript)
	if dir := subagentsDirForTranscript(mainTranscript); dir != "" {
		cache.InvalidatePrefix(strings.TrimSuffix(filepath.Dir(dir), string(filepath.Separator)) + string(filepath.Separator))
	}
}
