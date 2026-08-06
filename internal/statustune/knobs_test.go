package statustune

import (
	"go/ast"
	"go/parser"
	"go/token"
	"reflect"
	"sort"
	"strconv"
	"testing"
)

// knobsSourceFile is parsed by the coverage tests below. `go test` runs with the
// package directory as its working directory, so the bare name resolves.
const knobsSourceFile = "knobs.go"

// declaredRule is one `Rule*` constant as it is actually written in knobs.go.
type declaredRule struct {
	Name  string // the Go identifier, e.g. RuleIdleTitle
	Value string // the string it expands to, e.g. "case6-idle-title"
	Line  int    // where it is declared, so a failure points at the source
}

// declaredRules recovers every Rule* constant from the source text of knobs.go.
//
// Why the AST and not reflection: Go's untyped string constants have no runtime
// representation at all — reflect can enumerate a package's types, funcs and
// vars, but never its constants, so a reflective check here would silently
// inspect nothing and pass forever. That is the exact failure mode this test
// exists to end. A `go:generate` step could emit a slice of the constants, but a
// generated file is only as fresh as the last person to run `go generate`, which
// reintroduces the human step. Parsing knobs.go is self-contained, has no build
// step to forget, and reads the same bytes the compiler does.
func declaredRules(t *testing.T) []declaredRule {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, knobsSourceFile, nil, 0)
	if err != nil {
		t.Fatalf("parsing %s: %v", knobsSourceFile, err)
	}
	var rules []declaredRule
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, name := range vs.Names {
				if !isRuleConstName(name.Name) {
					continue
				}
				pos := fset.Position(name.Pos())
				if i >= len(vs.Values) {
					t.Fatalf("%s:%d: %s has no value; this test can only verify Rule* constants declared as string literals",
						knobsSourceFile, pos.Line, name.Name)
				}
				lit, ok := vs.Values[i].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					t.Fatalf("%s:%d: %s is not a string literal; this test can only verify Rule* constants declared as string literals",
						knobsSourceFile, pos.Line, name.Name)
				}
				value, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatalf("%s:%d: unquoting %s: %v", knobsSourceFile, pos.Line, name.Name, err)
				}
				rules = append(rules, declaredRule{Name: name.Name, Value: value, Line: pos.Line})
			}
		}
	}
	if len(rules) == 0 {
		t.Fatalf("found no Rule* constants in %s — the parse is broken, not the map", knobsSourceFile)
	}
	return rules
}

// isRuleConstName reports whether an identifier is an exported Rule* constant
// (RuleIdleTitle yes, Rule or Ruleset no).
func isRuleConstName(name string) bool {
	const prefix = "Rule"
	if len(name) <= len(prefix) || name[:len(prefix)] != prefix {
		return false
	}
	next := name[len(prefix)]
	return next >= 'A' && next <= 'Z'
}

// The map documents itself as exhaustive over the Rule* constants. This is the
// check that makes that true: it reads the constants out of knobs.go rather than
// out of a slice someone has to remember to extend, so adding a rule without a
// knob entry fails here instead of surfacing months later as `diagnose` printing
// "unrecognized rule id (daemon version skew?)" at a user.
func TestRuleKnobCoverage(t *testing.T) {
	rules := declaredRules(t)

	t.Run("should name a knob or explain its absence when a Rule constant is declared", func(t *testing.T) {
		for _, r := range rules {
			hint, ok := ruleKnobs[r.Value]
			if !ok {
				t.Errorf("%s:%d: %s (%q) has no ruleKnobs entry.\n"+
					"\tAdd one to ruleKnobs in %s naming the Tuning field that governs this rule,\n"+
					"\tor Field: \"\" with a What explaining why there is no knob.\n"+
					"\tWithout it `switchboard-ctl diagnose` reports this rule as\n"+
					"\t\"unrecognized rule id (daemon version skew?)\" and the rule= field in the\n"+
					"\tdecision log stops pointing a complaint at anything actionable.",
					knobsSourceFile, r.Line, r.Name, r.Value, knobsSourceFile)
				continue
			}
			if hint.What == "" {
				t.Errorf("%s:%d: %s (%q) has a ruleKnobs entry with an empty What.\n"+
					"\tEvery entry must explain the trade — that text is what `diagnose` prints.",
					knobsSourceFile, r.Line, r.Name, r.Value)
			}
		}
	})

	t.Run("should reject a knob entry when its key is not a declared Rule constant", func(t *testing.T) {
		declared := map[string]string{} // rule value -> constant name
		for _, r := range rules {
			declared[r.Value] = r.Name
		}
		var stale []string
		for key := range ruleKnobs {
			if _, ok := declared[key]; !ok {
				stale = append(stale, key)
			}
		}
		sort.Strings(stale)
		for _, key := range stale {
			t.Errorf("ruleKnobs has an entry for %q, which no Rule* constant in %s declares.\n"+
				"\tEither the constant was renamed/removed and this entry is dead, or the entry\n"+
				"\twas keyed by a bare string that has since drifted from the constant's value.",
				key, knobsSourceFile)
		}
	})

	t.Run("should name a real Tuning field when a knob hint sets Field", func(t *testing.T) {
		tuning := reflect.TypeOf(Tuning{})
		for _, r := range rules {
			hint, ok := ruleKnobs[r.Value]
			if !ok || hint.Field == "" {
				continue // absence is the case above; "" is a documented no-knob rule
			}
			if _, found := tuning.FieldByName(hint.Field); !found {
				t.Errorf("%s:%d: %s points at Tuning.%s, which does not exist.\n"+
					"\t`diagnose` prints \"Tuning.%s\" verbatim as the thing to change, so this\n"+
					"\tsends the user hunting for a field that was renamed or removed.",
					knobsSourceFile, r.Line, r.Name, hint.Field, hint.Field)
			}
		}
	})

	t.Run("should degrade gracefully when the rule id is unknown", func(t *testing.T) {
		if got := RuleKnob("bogus-rule"); got.Field != "" || got.What == "" {
			t.Errorf("unknown rule should yield an empty Field with an explanatory What, got %+v", got)
		}
	})
}
