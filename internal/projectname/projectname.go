// Package projectname turns a project directory plus a desired session name
// into a project-prefixed, de-duplicated label. It is the shared core behind:
//
//   - `switchboard-ctl name resolve` — used by the claude launcher wrapper to
//     prefix `claude -n <name>` at startup;
//   - switchboard's chip/tooltip rendering — display-time prefixing, which is
//     what lets `/name` and our scheme coexist: switchboard always re-derives
//     the prefix from the session's cwd, so whatever you `/name` a session it
//     shows project-scoped and de-duplicated.
//
// Resolution layers a writable user config (abbreviations keyed by absolute
// git-root path, edited via the hover rename) in front of built-in defaults
// (matched by git-root basename). An unknown project falls back to its
// sanitized basename, so new repos get a sensible abbreviation with no prompt.
//
// The string logic (ruleForRoot/Prefix/StripKnownPrefix/sanitize) is pure and
// exhaustively unit-tested; only ProjectRoot, ConfigPath, Load, SetAbbrev and
// SetFull touch the filesystem.
package projectname

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
)

// ProjectRule maps a project identity to a canonical abbreviation (the form
// prepended to an un-prefixed name) and the aliases that already count as
// prefixed. MatchPaths holds absolute git-root paths (user overrides); Match
// holds git-root basenames (built-in defaults, compared case-insensitively).
// Canonical should also appear in Aliases. Full is the pretty, human-readable
// display name (e.g. "Switchboard") a dashboard renders; the bar-chip keeps
// using the terse Canonical. Full is optional — callers fall back to the
// Canonical or a title-cased basename when it is unset.
type ProjectRule struct {
	MatchPaths []string `json:"match_paths,omitempty"`
	Match      []string `json:"match,omitempty"`
	Canonical  string   `json:"canonical"`
	Full       string   `json:"full,omitempty"`
	Aliases    []string `json:"aliases,omitempty"`
}

// Config is an ordered rule list; the first matching rule wins. User rules are
// layered ahead of the defaults (see Merged), so a path override beats a
// basename default.
type Config struct {
	Rules []ProjectRule `json:"rules"`
}

// DefaultConfig returns the built-in seed rules, matched by basename.
func DefaultConfig() Config {
	return Config{Rules: []ProjectRule{
		{Match: []string{"arachne"}, Canonical: "arachne", Full: "Arachne", Aliases: []string{"arachne"}},
		{Match: []string{"sspi-data-webapp"}, Canonical: "sspi", Full: "SSPI Data Webapp", Aliases: []string{"sspi", "sspi-data", "sspi-data-webapp"}},
		{Match: []string{"switchboard"}, Canonical: "sb", Full: "Switchboard", Aliases: []string{"switchboard", "switch", "sb"}},
		{Match: []string{"switchboard-dashboard"}, Canonical: "sb-dash", Full: "Switchboard Dashboard", Aliases: []string{"switchboard-dashboard", "sb-dash"}},
	}}
}

// ruleForRoot returns the rule matching the git-root path or its basename,
// preferring a path match. When nothing matches it synthesizes a fallback rule
// whose canonical is the sanitized basename.
func (c Config) ruleForRoot(root, base string) ProjectRule {
	cleanRoot := filepath.Clean(root)
	for _, r := range c.Rules {
		for _, p := range r.MatchPaths {
			if filepath.Clean(p) == cleanRoot {
				return r
			}
		}
	}
	lowerBase := strings.ToLower(base)
	for _, r := range c.Rules {
		for _, m := range r.Match {
			if strings.ToLower(m) == lowerBase {
				return r
			}
		}
	}
	canon := sanitize(base)
	return ProjectRule{Match: []string{lowerBase}, Canonical: canon, Aliases: []string{canon}}
}

// alreadyPrefixed reports whether name already carries one of the rule's
// aliases as a prefix. The "alias" or "alias-" boundary keeps a name like
// "arachnophobia-notes" from matching the alias "arachne".
func (r ProjectRule) alreadyPrefixed(name string) bool {
	lower := strings.ToLower(name)
	for _, a := range r.Aliases {
		la := strings.ToLower(a)
		if la == "" {
			continue
		}
		if lower == la || strings.HasPrefix(lower, la+"-") {
			return true
		}
	}
	return false
}

// Prefix applies the canonical prefix to name unless it is already prefixed by
// any alias. An empty name is returned unchanged so callers decide whether to
// seed a bare canonical.
func (r ProjectRule) Prefix(name string) string {
	name = strings.TrimSpace(name)
	if name == "" || r.Canonical == "" || r.alreadyPrefixed(name) {
		return name
	}
	return r.Canonical + "-" + name
}

// StripKnownPrefix removes a matched alias prefix from name, yielding the bare
// task part for display (e.g. "arachne-foo" -> "foo"). Aliases are tried
// longest-first so "sspi-data-cleanup" strips "sspi-data-" to "cleanup" rather
// than "sspi-" to "data-cleanup". A name that is exactly an alias, or carries
// none, is returned unchanged.
func (r ProjectRule) StripKnownPrefix(name string) string {
	lower := strings.ToLower(strings.TrimSpace(name))
	aliases := append([]string(nil), r.Aliases...)
	sort.Slice(aliases, func(i, j int) bool { return len(aliases[i]) > len(aliases[j]) })
	for _, a := range aliases {
		la := strings.ToLower(a)
		if la == "" {
			continue
		}
		if strings.HasPrefix(lower, la+"-") {
			return name[len(la)+1:]
		}
	}
	return name
}

// ResolveForDir returns name prefixed for the project housing dir.
func ResolveForDir(cfg Config, dir, name string) string {
	return cfg.ruleForRoot(ProjectRoot(dir), ProjectBase(dir)).Prefix(name)
}

// TaskForDir returns name with any project-alias prefix stripped, for the
// tooltip's task line.
func TaskForDir(cfg Config, dir, name string) string {
	return cfg.ruleForRoot(ProjectRoot(dir), ProjectBase(dir)).StripKnownPrefix(name)
}

// CanonicalForDir returns the abbreviation that would be prepended for dir.
func CanonicalForDir(cfg Config, dir string) string {
	return cfg.ruleForRoot(ProjectRoot(dir), ProjectBase(dir)).Canonical
}

// FullForDir returns the pretty, human-readable display name for the project
// housing dir — the dashboard's title, as opposed to the bar-chip's terse
// abbreviation. It is the rule's Full when set, otherwise a sensible default: a
// title-cased project basename ("my-cool-repo" -> "My Cool Repo"), falling back
// to the Canonical when even that is empty.
func FullForDir(cfg Config, dir string) string {
	base := ProjectBase(dir)
	r := cfg.ruleForRoot(ProjectRoot(dir), base)
	if r.Full != "" {
		return r.Full
	}
	if t := titleize(base); t != "" {
		return t
	}
	return r.Canonical
}

// FullForAbbrev is the reverse lookup the timeline uses: it has only the stored
// abbreviation (the Canonical) and wants the pretty display name. It returns the
// Full of the first rule whose Canonical matches abbrev, falling back to abbrev
// itself when the project is unknown or its Full is unset. An empty abbrev maps
// to the empty string.
func FullForAbbrev(cfg Config, abbrev string) string {
	if abbrev == "" {
		return ""
	}
	for _, r := range cfg.Rules {
		if r.Canonical == abbrev {
			if r.Full != "" {
				return r.Full
			}
			return abbrev
		}
	}
	return abbrev
}

// ProjectRoot walks up from dir to the nearest git root (a .git directory or
// file — covers worktrees/submodules) and returns its absolute path, falling
// back to dir itself when no git root is found.
func ProjectRoot(dir string) string {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return ""
	}
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	cur := filepath.Clean(dir)
	for {
		if _, err := os.Stat(filepath.Join(cur, ".git")); err == nil {
			return cur
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			break
		}
		cur = parent
	}
	return filepath.Clean(dir)
}

// ProjectBase is the basename of ProjectRoot(dir).
func ProjectBase(dir string) string {
	return filepath.Base(ProjectRoot(dir))
}

// sanitize lowercases s and collapses runs of non-[a-z0-9] into single hyphens,
// trimming leading/trailing hyphens. It is the fallback abbreviation.
func sanitize(s string) string {
	var b strings.Builder
	prevHyphen := false
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			prevHyphen = false
			continue
		}
		if !prevHyphen {
			b.WriteByte('-')
			prevHyphen = true
		}
	}
	return strings.Trim(b.String(), "-")
}

// titleize turns a slug-ish basename ("my-cool-repo") into a display title
// ("My Cool Repo"): the separators a slug uses ('-', '_', '.', space) are word
// boundaries, and each word's first rune is upper-cased. It is the pretty-name
// fallback when a project has no explicit Full.
func titleize(s string) string {
	fields := strings.FieldsFunc(s, func(r rune) bool {
		return r == '-' || r == '_' || r == '.' || r == ' '
	})
	for i, w := range fields {
		runes := []rune(w)
		runes[0] = unicode.ToUpper(runes[0])
		fields[i] = string(runes)
	}
	return strings.Join(fields, " ")
}

// --- writable user config -------------------------------------------------

// fileConfig is the on-disk shape of the user config: abbreviations keyed by
// absolute git-root path.
type fileConfig struct {
	Projects map[string]fileEntry `json:"projects"`
}

type fileEntry struct {
	Canonical string   `json:"canonical,omitempty"`
	Full      string   `json:"full,omitempty"`
	Aliases   []string `json:"aliases,omitempty"`
}

// ConfigPath returns the user config path, honoring XDG_CONFIG_HOME.
func ConfigPath() string {
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "switchboard", "projects.json")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "switchboard", "projects.json")
}

// Load returns the merged config: user overrides (path-matched) layered ahead
// of the built-in defaults (basename-matched). A missing or unreadable user
// file degrades silently to the defaults.
func Load() Config {
	return loadFrom(ConfigPath())
}

func loadFrom(path string) Config {
	cfg := Config{}
	defaults := DefaultConfig()
	if data, err := os.ReadFile(path); err == nil {
		var fc fileConfig
		if json.Unmarshal(data, &fc) == nil {
			for root, e := range fc.Projects {
				canon, aliases, full := e.Canonical, e.Aliases, e.Full
				if canon == "" {
					// Only a Full (pretty name) was set — no abbrev override. Inherit
					// the canonical/aliases (and any default Full) the basename would
					// otherwise resolve to, so naming a project never erases its abbrev.
					base := filepath.Base(filepath.Clean(root))
					dr := defaults.ruleForRoot(root, base)
					canon = dr.Canonical
					if len(aliases) == 0 {
						aliases = dr.Aliases
					}
					if full == "" {
						full = dr.Full
					}
				}
				if len(aliases) == 0 {
					aliases = []string{canon}
				}
				cfg.Rules = append(cfg.Rules, ProjectRule{
					MatchPaths: []string{root},
					Canonical:  canon,
					Full:       full,
					Aliases:    aliases,
				})
			}
		}
	}
	cfg.Rules = append(cfg.Rules, defaults.Rules...)
	return cfg
}

// SetAbbrev persists the abbreviation for the project rooted at root (an
// absolute git-root path), upserting the user config file. Aliases default to
// the abbreviation itself. Any previously set Full (pretty name) is preserved.
func SetAbbrev(root, abbrev string) error {
	return setAbbrevIn(ConfigPath(), root, abbrev)
}

// SetFull persists the pretty display name (Full) for the project rooted at root
// (an absolute git-root path), upserting the user config file. Any previously
// set abbreviation/aliases are preserved — the two are edited independently.
func SetFull(root, full string) error {
	return setFullIn(ConfigPath(), root, full)
}

func setAbbrevIn(path, root, abbrev string) error {
	abbrev = sanitize(abbrev)
	return upsertEntry(path, root, func(e *fileEntry) {
		e.Canonical = abbrev
		e.Aliases = []string{abbrev}
	})
}

func setFullIn(path, root, full string) error {
	full = strings.TrimSpace(full)
	return upsertEntry(path, root, func(e *fileEntry) {
		e.Full = full
	})
}

// upsertEntry reads the user config, applies mod to the entry for root (a fresh
// zero entry when absent — preserving the sibling fields mod leaves untouched),
// and writes it back atomically via a temp-file rename.
func upsertEntry(path, root string, mod func(*fileEntry)) error {
	root = filepath.Clean(root)
	fc := fileConfig{Projects: map[string]fileEntry{}}
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &fc)
		if fc.Projects == nil {
			fc.Projects = map[string]fileEntry{}
		}
	}
	e := fc.Projects[root]
	mod(&e)
	fc.Projects[root] = e
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(fc, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
