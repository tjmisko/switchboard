package statustune

// Rule ids tag every status decision (the `rule=` field in the decision log).
// They are the single authority for the strings: rpc and the reconciler emit
// them, ParseDecision reads them back, and RuleKnob maps each to the Tuning field
// that governs it. They mirror the case table in docs/status-color-state-model.md
// §5, so a complaint's `rule=` points straight at both the doc row and the knob.
const (
	// RuleApproveToolMatch — red cleared at hook speed because a PostToolUse's
	// tool_name matched the tool the prompt was raised for (the approved tool
	// completed). rpc.clearsPermission.
	RuleApproveToolMatch = "case9-approve-toolmatch"
	// RuleApproveTranscript — red cleared because the transcript showed the turn
	// resumed (an assistant message), the fallback when tool_name was not forwarded.
	RuleApproveTranscript = "case9-approve-transcript"
	// RuleHoldBareResult — a PostToolUse was NOT allowed to clear red: it was a
	// sibling/Task completion, not the prompt resolving. The missed-RED guard.
	RuleHoldBareResult = "case12-hold-bare-result"

	// RuleApproveResume — reconciler exit: an assistant message advanced past the
	// prompt → working (green), directly, no orange bounce.
	RuleApproveResume = "case9-approve-resume"
	// RuleDeclineIdle — reconciler exit: interrupt/decline with no teammates → idle.
	RuleDeclineIdle = "case10-decline-idle"
	// RuleDeclineDelegating — reconciler exit: interrupt/decline but subagents are
	// still in flight → delegating (green).
	RuleDeclineDelegating = "case11-decline-delegating"
	// RuleTTLBackstop — reconciler exit: the transcript was unreadable and the TTL
	// elapsed, so red is released as a last resort.
	RuleTTLBackstop = "case15-ttl-backstop"

	// RuleDelegating — idle promoted to delegating (green) because subagents are in
	// flight.
	RuleDelegating = "case5-delegating"
	// RuleDrained — delegating reverted to idle because the last teammate finished.
	RuleDrained = "case4-drained"
	// RuleResumeActivity — idle promoted to working because the transcript showed
	// fresh activity (an orchestrator woken by a teammate, etc.).
	RuleResumeActivity = "resume-activity"
	// RuleInterrupt — working demoted to idle because the turn was Esc-interrupted
	// (no Stop hook fires).
	RuleInterrupt = "case6-interrupt"
)

// KnobHint names the Tuning field that governs a rule's outcome, with a one-line
// description of what moving it does. Field is "" for a rule that has no knob —
// either an intentional guard (the missed-RED hold) or a pure transcript-signal
// edge — in which case What explains why there is nothing to tune.
type KnobHint struct {
	Field string
	What  string
}

// ruleKnobs maps every rule id to the Tuning field that decides its color. Kept
// exhaustive over the Rule* constants (TestRuleKnobCoverage enforces it) so the
// diagnose command can always answer "what do I change?".
var ruleKnobs = map[string]KnobHint{
	RuleApproveToolMatch:  {"EarlyClearApproveByToolName", "set false to require the transcript to confirm resume before clearing red (slower, but no tool-name guessing)"},
	RuleApproveTranscript: {"ResumeExitStatus", "the color a red chip exits to when the turn resumes (default working/green)"},
	RuleApproveResume:     {"ResumeExitStatus", "the color a red chip exits to when the turn resumes (default working/green)"},
	RuleDeclineIdle:       {"InterruptExitStatus", "the color a red chip exits to when interrupted/declined with no teammates (default idle/orange)"},
	RuleDeclineDelegating: {"EscWithTeammatesStatus", "the color when interrupted/declined while teammates are in flight (default delegating/green)"},
	RuleTTLBackstop:       {"PermissionDecayTTL", "how long an unreadable-transcript red chip waits before the backstop releases it (default 30s)"},
	RuleDelegating:        {"DelegatingEnabled", "set false to keep an idle-with-teammates chip orange instead of green"},
	RuleDrained:           {"DelegatingEnabled", "set false to disable the delegating state entirely"},
	RuleHoldBareResult:    {"", "intentional missed-RED guard: a bare/Task PostToolUse must not clear a pending prompt — there is no knob, and loosening it risks the worst error (a blocked agent shown not-red)"},
	RuleResumeActivity:    {"", "pure transcript-signal edge (idle→working on fresh activity); not tunable — adjust upstream signal classification in package transcript if it misfires"},
	RuleInterrupt:         {"", "pure transcript-signal edge (working→idle on an interrupt notice); not tunable — see package transcript"},
}

// RuleKnob returns the tuning hint for a rule id. An unknown rule yields a zero
// KnobHint with a generic note, so the diagnose command degrades gracefully on a
// log line written by a newer/older daemon.
func RuleKnob(rule string) KnobHint {
	if h, ok := ruleKnobs[rule]; ok {
		return h
	}
	return KnobHint{What: "unrecognized rule id (daemon version skew?) — see docs/status-color-state-model.md §5"}
}
