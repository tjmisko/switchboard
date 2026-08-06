package projectname

// DirCache memoizes the dir -> (rule, base) resolution for the duration of ONE
// render. Its zero value is ready to use, and a nil *DirCache resolves exactly
// like the bare package functions, so a caller with no cache to hand can pass
// nil.
//
// # Why it exists
//
// Resolving a dir means ProjectRoot climbing the tree with an os.Stat per level
// until it finds a .git. A CPU profile of one switchboard-waybar emission at
// thirteen sessions put 47% of renderSlot inside this resolution, 78% of that in
// the stat syscalls — the single largest cost in an emission, ten slot processes
// deep, once a second.
//
// The saving is duplicate work, not repeat work: renderSlot names EVERY session
// to compute the shared barlayout.Fit budget, then the tooltip resolves its own
// session twice more. That is fifteen resolutions over the six DISTINCT project
// dirs a typical bar shows, because several sessions share a repo.
//
// # Why a render, and not the process
//
// A process-lifetime cache is the obvious next step and is deliberately not
// taken. It would have to answer what happens when a repo root moves under a
// live session — `git init` in a session's cwd makes the correct root deeper
// than the cached one — and the honest answer is that the chip would keep its
// old label until the bar restarted.
//
// Validating a process-lifetime entry does not rescue it, because of what the
// profile says: nearly every live session's cwd IS its repo root, which
// ProjectRoot answers in a single stat. Any cache that re-stats to stay honest
// therefore saves the arithmetic around the stat and not the stat itself —
// about a tenth of an emission, against the ~28% collapsing the duplicates
// gets. The cheaper option is also the one with no staleness to reason about.
//
// # Lifetime and concurrency
//
// NOT safe for concurrent use, unlike label.NameCache, and it does not need to
// be: it is created inside a render, used by that render's single goroutine, and
// dropped. Keeping one alive across renders would reintroduce exactly the
// staleness this design avoids, so don't — hold it in a local, never a field.
type DirCache struct {
	entries map[string]dirEntry
}

type dirEntry struct {
	rule ProjectRule
	base string
}

// ruleForDir returns the same (rule, base) as Config.ruleForDir, resolving each
// distinct dir at most once per cache.
func (c *DirCache) ruleForDir(cfg Config, dir string) (ProjectRule, string) {
	if c == nil {
		return cfg.ruleForDir(dir)
	}
	if e, ok := c.entries[dir]; ok {
		return e.rule, e.base
	}
	rule, base := cfg.ruleForDir(dir)
	if c.entries == nil {
		c.entries = make(map[string]dirEntry)
	}
	c.entries[dir] = dirEntry{rule: rule, base: base}
	return rule, base
}

// ResolveForDir is ResolveForDir served from the cache.
func (c *DirCache) ResolveForDir(cfg Config, dir, name string) string {
	r, _ := c.ruleForDir(cfg, dir)
	return r.Prefix(name)
}

// TaskForDir is TaskForDir served from the cache.
func (c *DirCache) TaskForDir(cfg Config, dir, name string) string {
	r, _ := c.ruleForDir(cfg, dir)
	return r.StripKnownPrefix(name)
}

// CanonicalForDir is CanonicalForDir served from the cache.
func (c *DirCache) CanonicalForDir(cfg Config, dir string) string {
	r, _ := c.ruleForDir(cfg, dir)
	return r.Canonical
}
