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
	// RuleHoldTeammateCollision — a PostToolUse whose tool_name DID match the tool
	// the chip's red is reported under was still refused, because the writer that
	// fired it does not own that prompt — or, with an empty agent_id and teammates in
	// flight, because nothing identifies the writer at all (T2's surviving floor).
	// tool_name is a tool *kind*, not a tool *identity*: a teammate running the same
	// kind of tool (Bash, constantly) satisfies the match. The 2026-08-05 lost-RED
	// edge (docs/subagent-permission-oscillation.md §3.1).
	RuleHoldTeammateCollision = "case12-hold-teammate-collision"
	// RuleHoldInputMismatch — the PostToolUse came from the prompt's OWN writer and
	// named the pending tool, but its tool_input hash differs and that writer's own
	// transcript does not yet show it resumed. Ambiguous by construction: PostToolUse
	// reports the input AFTER the decision, and the approval paths rewrite it
	// (docs/claude-code-hook-schema.md §2), so a mismatch is equally "same call,
	// rewritten on approval" and "a sibling call by the same writer". Held, because
	// only one of those is safe to guess at (plan T7).
	RuleHoldInputMismatch = "case12-hold-input-mismatch"
	// RuleHoldOtherWriter — case 18: the event DID resolve the prompt of the writer
	// that fired it, and the chip stayed red because a DIFFERENT writer is still
	// waiting. Not a refusal — a partial resolution, logged under its own id so a
	// permission==permission line never carries an approve rule.
	RuleHoldOtherWriter = "case18-hold-other-writer"
	// RuleHoldNonToolEvent — a non-tool hook event (Stop / UserPromptSubmit /
	// SessionStart) tried to move the chip off permission and was held: it carries
	// no evidence about the prompt at all, and the transcript did not show the turn
	// resumed. Defect 5 of the same incident (§3.5) — the hold gate used to guard
	// only PostToolUse, so the main thread merely finishing its turn discarded a
	// teammate's red.
	RuleHoldNonToolEvent = "case12-hold-nontool-event"

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
	// RuleHoldOtherWriters — reconciler HOLD: one writer's prompt was proven
	// resolved and dropped, but another writer is still blocked, so the chip stays
	// red. Case 18 — the case the old whole-session ClearPending could not express,
	// and the reason resolution is routed per writer at all.
	RuleHoldOtherWriters = "case18-hold-other-writers"
	// RuleStaleWriterBackstop — reconciler exit: a writer's own transcript has been
	// quiescent past PendingWriterStaleCap (or the writer is gone entirely), so its
	// prompt is unanswerable and is dropped rather than latched. Case 19, the
	// per-writer analogue of case 15 for a file that reads fine and never moves.
	RuleStaleWriterBackstop = "case19-stale-writer-backstop"

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
	// RuleIdleTitle — working demoted to idle because the pane title showed the
	// agent's static idle glyph (waiting at the prompt) on a fresh sample. The
	// recovery for the silent abort: an interrupt before the first token fires no
	// hook AND writes no marker (docs/timing-hazards.md H9).
	RuleIdleTitle = "case6-idle-title"
)

// KnobHint names the Tuning field that governs a rule's outcome, with a one-line
// description of what moving it does. Field is "" for a rule that has no knob —
// either an intentional guard (the missed-RED hold) or a pure transcript-signal
// edge — in which case What explains why there is nothing to tune.
type KnobHint struct {
	Field string
	What  string
}

// ruleKnobs maps every rule id to the Tuning field that decides its color, so the
// diagnose command can always answer "what do I change?". Exhaustive over the
// Rule* constants above, and TestRuleKnobCoverage enforces that by parsing THIS
// file for `Rule*` declarations — adding a constant without adding a row here
// fails the test, naming the constant and its line.
var ruleKnobs = map[string]KnobHint{
	RuleApproveToolMatch:      {"EarlyClearApproveByToolName", "set false to require the transcript to confirm resume before clearing red (slower, but no correlator guessing). The fast path now matches on (agent_id, tool_name, tool_input hash), so it clears only for the writer that RAISED the prompt; an unidentifiable writer (empty agent_id) with subagents in flight still cannot clear on tool_name alone whatever this says (case12-hold-teammate-collision)"},
	RuleApproveTranscript:     {"", "no knob decides this one. It is the hook path's fallback clear, and the color it exits to is the hook event's own status (PostToolUse→working, Stop→idle), not a policy field — ResumeExitStatus governs the reconciler's case9-approve-resume, not this. What you CAN change is how often it fires: EarlyClearApproveByToolName=false routes every approve clear through here"},
	RuleApproveResume:         {"ResumeExitStatus", "the color a red chip exits to when the turn resumes (default working/green)"},
	RuleDeclineIdle:           {"InterruptExitStatus", "the color a red chip exits to when interrupted/declined with no teammates (default idle/orange)"},
	RuleDeclineDelegating:     {"EscWithTeammatesStatus", "the color when interrupted/declined while teammates are in flight (default delegating/green)"},
	RuleTTLBackstop:           {"PermissionDecayTTL", "how long an unreadable-transcript red chip waits before the backstop releases it (default 30s)"},
	RuleStaleWriterBackstop:   {"PendingWriterStaleCap", "how long one writer's transcript may sit quiescent before its prompt is dropped as unanswerable (default 30m, matching the fanout Observer's force-close of the same subagent). Raise it to let a crashed teammate's red nag longer; lowering it starts cutting real human decision windows short, which is a missed RED"},
	RuleHoldOtherWriters:      {"", "intentional missed-RED guard, and the point of keying red by writer: one prompt resolving says nothing about another writer's, so the chip stays red until every entry is gone. There is no knob — a session with two blocked writers needs two answers"},
	RuleDelegating:            {"DelegatingEnabled", "set false to keep an idle-with-teammates chip orange instead of green"},
	RuleDrained:               {"DelegatingEnabled", "set false to disable the delegating state entirely"},
	RuleHoldBareResult:        {"", "intentional missed-RED guard: a bare/Task PostToolUse must not clear a pending prompt — there is no knob, and loosening it risks the worst error (a blocked agent shown not-red)"},
	RuleHoldTeammateCollision: {"", "intentional missed-RED guard: the tool_name matched but the WRITER did not — the hook came from a teammate that owns no pending prompt, or from an unidentifiable writer (empty agent_id) while teammates are in flight. A tool_name match is a tool KIND collision, not the approved tool completing, and there is no knob (EarlyClearApproveByToolName only turns the fast path further off). The blocked writer's own next event, or its own transcript on the reconcile tick, is what clears it"},
	RuleHoldInputMismatch:     {"", "intentional missed-RED guard, and the price of the T7 fast path: the prompt's own writer completed the pending tool with a DIFFERENT tool_input hash, which is equally 'the approved call, rewritten on approval' and 'a sibling call while the prompt still waits'. No knob — the ambiguity is in Claude Code's payload (PostToolUse reports the input after the decision), not in a threshold. It costs one transcript check of latency on rewrite-path approvals (bare output redirection, permission-root relocation, a user-edited call); a missed RED is the worse error"},
	RuleHoldOtherWriter:       {"", "not a guard and not tunable: a prompt WAS resolved, and the chip is red because another writer is still blocked — the fold `len(Pending) > 0 → RED` outranks every other rule (docs/subagent-permission-plan.md §3.3). The reason= field names who is still waiting; the chip greens when the last of them answers"},
	RuleHoldNonToolEvent:      {"", "intentional missed-RED guard: Stop/UserPromptSubmit/SessionStart carry no evidence about the pending prompt, so they may not repaint the chip — there is no knob. A queued message during a pending prompt is common, which is why UserPromptSubmit is not exempt (plan Q6)"},
	RuleResumeActivity:        {"", "pure transcript-signal edge (idle→working on fresh activity); not tunable — adjust upstream signal classification in package transcript if it misfires"},
	RuleInterrupt:             {"", "pure transcript-signal edge (working→idle on an interrupt notice); not tunable — see package transcript"},
	RuleIdleTitle:             {"IdleTitleDemotionEnabled", "set false to never demote a green chip on an idle pane title; IdleTitleGrace delays it, IdleTitleGlyphs names the glyphs that count as idle"},
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
