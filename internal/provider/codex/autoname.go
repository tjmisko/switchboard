package codex

import (
	"hash/fnv"
	"regexp"
	"strings"
	"unicode"
)

var repeatedHyphen = regexp.MustCompile(`-+`)

// NormalizeGeneratedName enforces the public lowercase 2-5 word, kebab-case,
// 40-character contract. It repairs harmless punctuation/spacing but rejects
// content that cannot become a useful title.
func NormalizeGeneratedName(value string) (string, bool) {
	value = strings.Trim(strings.TrimSpace(value), "`'\"")
	var b strings.Builder
	for _, r := range strings.ToLower(value) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
		case unicode.IsSpace(r) || r == '-' || r == '_':
			b.WriteByte('-')
		}
	}
	name := strings.Trim(repeatedHyphen.ReplaceAllString(b.String(), "-"), "-")
	words := compactWords(strings.Split(name, "-"))
	if len(words) < 2 {
		return "", false
	}
	if len(words) > 5 {
		words = words[:5]
	}
	for len(words) >= 2 {
		name = strings.Join(words, "-")
		if len([]rune(name)) <= 40 {
			return name, true
		}
		words = words[:len(words)-1]
	}
	return "", false
}

func compactWords(words []string) []string {
	out := words[:0]
	for _, word := range words {
		if word != "" {
			out = append(out, word)
		}
	}
	return out
}

var fallbackStopwords = map[string]struct{}{
	"a": {}, "an": {}, "and": {}, "for": {}, "in": {}, "of": {}, "on": {},
	"please": {}, "the": {}, "to": {}, "with": {},
}

// FallbackName is deterministic and content-local; it performs no I/O and is
// used after the single transient retry is exhausted.
func FallbackName(prompt string) string {
	var words []string
	var current strings.Builder
	flush := func() {
		word := strings.ToLower(current.String())
		current.Reset()
		if word == "" {
			return
		}
		if _, skip := fallbackStopwords[word]; !skip {
			words = append(words, word)
		}
	}
	for _, r := range prompt {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			current.WriteRune(r)
		} else {
			flush()
		}
		if len(words) == 4 {
			break
		}
	}
	flush()
	if len(words) == 0 {
		h := fnv.New32a()
		_, _ = h.Write([]byte(prompt))
		return "codex-task-" + base36(uint64(h.Sum32()))
	}
	if len(words) == 1 {
		words = append(words, "task")
	}
	if name, ok := NormalizeGeneratedName(strings.Join(words, "-")); ok {
		return name
	}
	return "codex-task"
}

func base36(value uint64) string {
	const alphabet = "0123456789abcdefghijklmnopqrstuvwxyz"
	if value == 0 {
		return "0"
	}
	var buf [16]byte
	i := len(buf)
	for value > 0 {
		i--
		buf[i] = alphabet[value%36]
		value /= 36
	}
	return string(buf[i:])
}
