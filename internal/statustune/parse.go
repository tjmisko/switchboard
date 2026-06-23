package statustune

import (
	"strconv"
	"strings"
	"time"
)

// logAnchor is the stable prefix every status-decision line carries. ParseDecision
// finds it anywhere in a line, so it works on a raw `journalctl` line (with its
// timestamp/host/unit prefix), a Go `log` line (with its date prefix), or the bare
// message — the caller need not strip anything first.
const logAnchor = "status: pid="

// Record is a parsed status-decision log line. It covers both shapes the daemon
// emits: the reconciler/gate decisions (Kind "reconciler", carrying Rule/Reason
// and the [S= pending= age=] tuple) and the plain hook edges (Kind "hook",
// carrying Agent/Event). Time is left zero by the parser; the source layer fills
// it from the journal/log prefix.
type Record struct {
	Time      time.Time
	PID       int
	Session   string
	From      string
	To        string
	Hold      bool // the verb was "==" (a deliberate no-change decision)
	Kind      string
	Rule      string
	Reason    string
	Subagents int
	Pending   string
	Age       time.Duration
	HasTuple  bool // the [S= pending= age=] tuple was present (reconciler/gate lines)
	Agent     string
	Event     string
	Raw       string
}

const (
	KindReconciler = "reconciler" // a rule-tagged decision (selfHeal* / clearsPermission)
	KindHook       = "hook"       // a plain hook-driven edge (agent=/event=)
)

// ParseDecision parses one log line into a Record. ok is false for any line that
// is not a status-decision line (no anchor, or unparseable transition), so a
// caller can feed it an entire journal and keep only the ones that parse.
func ParseDecision(line string) (Record, bool) {
	i := strings.Index(line, logAnchor)
	if i < 0 {
		return Record{}, false
	}
	rec := Record{Raw: line}
	msg := line[i+len(logAnchor):] // text after "status: pid="

	// pid is the leading number.
	pidTok, rest := splitToken(msg)
	pid, err := strconv.Atoi(pidTok)
	if err != nil {
		return Record{}, false
	}
	rec.PID = pid

	// session=<id> is the next token (the short id never contains a space).
	sessTok, rest := splitToken(rest)
	sid, ok := strings.CutPrefix(sessTok, "session=")
	if !ok {
		return Record{}, false
	}
	rec.Session = sid

	// Branch on the trailing marker: reconciler lines have " rule=", hook lines
	// have " (agent=". The transition token is the last whitespace-delimited token
	// of the segment before that marker (hook lines may carry a window-title/cwd
	// label between the session id and the transition).
	if idx := strings.Index(rest, " rule="); idx >= 0 {
		rec.Kind = KindReconciler
		if !parseTransition(lastToken(rest[:idx]), &rec) {
			return Record{}, false
		}
		parseReconcilerTail(rest[idx+1:], &rec) // skip the leading space
		return rec, true
	}
	if idx := strings.Index(rest, " (agent="); idx >= 0 {
		rec.Kind = KindHook
		if !parseTransition(lastToken(rest[:idx]), &rec) {
			return Record{}, false
		}
		parseHookTail(rest[idx+1:], &rec)
		return rec, true
	}
	return Record{}, false
}

// parseTransition splits a "FROM->TO" or "FROM==TO" token. FROM may be empty (a
// hook edge from a not-yet-known status renders as "->idle").
func parseTransition(tok string, rec *Record) bool {
	verb := "->"
	if strings.Contains(tok, "==") {
		verb = "=="
		rec.Hold = true
	}
	from, to, ok := strings.Cut(tok, verb)
	if !ok {
		return false
	}
	rec.From, rec.To = from, to
	return true
}

// parseReconcilerTail reads `rule=<id> reason=<quoted|token> [S=<n> pending=<quoted|token> age=<dur>]`.
func parseReconcilerTail(s string, rec *Record) {
	rec.Rule = valueToken(s, "rule=")
	rec.Reason = valueQuoted(s, "reason=")
	if open := strings.Index(s, "[S="); open >= 0 {
		if close := strings.Index(s[open:], "]"); close >= 0 {
			rec.HasTuple = true
			tuple := s[open+1 : open+close] // inside the brackets, e.g. S=2 pending="x" age=27s
			if n, err := strconv.Atoi(valueToken(tuple, "S=")); err == nil {
				rec.Subagents = n
			}
			rec.Pending = valueQuoted(tuple, "pending=")
			if d, err := time.ParseDuration(valueToken(tuple, "age=")); err == nil {
				rec.Age = d
			}
		}
	}
}

// parseHookTail reads `(agent=<a> event=<e>)`.
func parseHookTail(s string, rec *Record) {
	rec.Agent = strings.TrimRight(valueToken(s, "agent="), ")")
	rec.Event = strings.TrimRight(valueToken(s, "event="), ")")
}

// --- small field extractors (tolerant: a missing key yields "") ---

// splitToken returns the first whitespace-delimited token and the remainder
// (leading whitespace of the remainder trimmed).
func splitToken(s string) (tok, rest string) {
	s = strings.TrimLeft(s, " ")
	if i := strings.IndexByte(s, ' '); i >= 0 {
		return s[:i], s[i+1:]
	}
	return s, ""
}

// lastToken returns the last whitespace-delimited token of s.
func lastToken(s string) string {
	s = strings.TrimRight(s, " ")
	if i := strings.LastIndexByte(s, ' '); i >= 0 {
		return s[i+1:]
	}
	return s
}

// valueToken reads the unquoted token following key (up to the next space).
func valueToken(s, key string) string {
	i := strings.Index(s, key)
	if i < 0 {
		return ""
	}
	tok, _ := splitToken(s[i+len(key):])
	return tok
}

// valueQuoted reads the value following key: a Go-quoted string (unquoted) when it
// starts with a quote, else a bare token. Mirrors the `%q`/token forms the logger
// emits for reason=/pending=.
func valueQuoted(s, key string) string {
	i := strings.Index(s, key)
	if i < 0 {
		return ""
	}
	v := s[i+len(key):]
	if len(v) > 0 && v[0] == '"' {
		if unq, err := strconv.QuotedPrefix(v); err == nil {
			if out, err := strconv.Unquote(unq); err == nil {
				return out
			}
		}
	}
	tok, _ := splitToken(v)
	return tok
}
