package projectname

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
)

// ProjectRoot first identifies a direct child of a configured project parent.
// This also covers non-Git projects and nested worktrees. Outside those parents,
// it uses the nearest Git root, then the directory itself.
func (c Config) ProjectRoot(dir string) string {
	if strings.TrimSpace(dir) == "" {
		return ""
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return ProjectRoot(dir)
	}
	parents := c.ProjectDirs
	if parents == nil {
		parents = DefaultConfig().ProjectDirs
	}
	var root string
	for _, parent := range parents {
		if strings.HasPrefix(parent, "~/") {
			home, _ := os.UserHomeDir()
			parent = filepath.Join(home, parent[2:])
		}
		if !filepath.IsAbs(parent) {
			continue
		}
		rel, err := filepath.Rel(parent, abs)
		if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		candidate := filepath.Join(parent, strings.Split(rel, string(filepath.Separator))[0])
		if len(candidate) > len(root) {
			root = candidate
		}
	}
	if root != "" {
		return root
	}
	return ProjectRoot(dir)
}

func (c Config) RuleForDir(dir string) ProjectRule {
	root := c.ProjectRoot(dir)
	return c.ruleForRoot(root, filepath.Base(root))
}

// DisplayName preserves the directory's spelling unless an alias is configured.
func (r ProjectRule) DisplayName() string {
	if r.Display != "" {
		return r.Display
	}
	return r.Canonical
}

// TaskTitle removes a project prefix before applying the shared five-word
// display limit. Names and aliases are compared as words, allowing either the
// legacy dashed form or human-readable spaces. Stored values stay untouched.
func (r ProjectRule) TaskTitle(name string) string {
	words := titleWords(name)
	aliases := append([]string{r.Canonical, r.Display, r.Full}, r.Aliases...)
	aliases = append(aliases, r.Match...)
	sort.SliceStable(aliases, func(i, j int) bool { return len(titleWords(aliases[i])) > len(titleWords(aliases[j])) })
	for _, alias := range aliases {
		prefix := titleWords(alias)
		if len(prefix) == 0 || len(prefix) > len(words) {
			continue
		}
		match := true
		for i := range prefix {
			if !strings.EqualFold(prefix[i], words[i]) {
				match = false
				break
			}
		}
		if match {
			words = words[len(prefix):]
			break
		}
	}
	if len(words) > 5 {
		words = words[:5]
	}
	for i, word := range words {
		runes := []rune(strings.ToLower(word))
		runes[0] = unicode.ToUpper(runes[0])
		words[i] = string(runes)
	}
	return strings.Join(words, " ")
}

func titleWords(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r) && !unicode.IsMark(r)
	})
}
