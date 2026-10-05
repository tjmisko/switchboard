package usagelimit

import (
	"os"
	"testing"
	"time"
)

var losAngeles *time.Location

// The Codex and Pi forms name a wall-clock time without a zone, which the
// provider rendered in the machine's zone; pin it so the tests mean one thing.
func TestMain(m *testing.M) {
	var err error
	if losAngeles, err = time.LoadLocation("America/Los_Angeles"); err != nil {
		panic(err)
	}
	time.Local = losAngeles
	os.Exit(m.Run())
}

// 12:28:05 PDT on 2026-10-05, the instant the functionary session was capped.
var at = time.Date(2026, 10, 5, 19, 28, 5, 0, time.UTC)

func la(month time.Month, day, hour, minute int) time.Time {
	return time.Date(2026, month, day, hour, minute, 0, 0, losAngeles)
}

func TestFromClaudeStopFailureShouldClassifyEveryObservedForm(t *testing.T) {
	for _, tc := range []struct {
		name, errorType, message string
		limited                  bool
		resetsAt                 *time.Time
	}{
		{"session limit, later today", "rate_limit", "You've hit your session limit · resets 4pm (America/Los_Angeles)", true, ptr(la(10, 5, 16, 0))},
		{"session limit with minutes", "rate_limit", "You've hit your session limit · resets 5:50pm (America/Los_Angeles)", true, ptr(la(10, 5, 17, 50))},
		{"session limit already past today rolls to tomorrow", "rate_limit", "You've hit your session limit · resets 2am (America/Los_Angeles)", true, ptr(la(10, 6, 2, 0))},
		{"weekly limit with a date", "rate_limit", "You've hit your weekly limit · resets Oct 8, 9pm (America/Los_Angeles)", true, ptr(la(10, 8, 21, 0))},
		{"weekly limit, time only", "rate_limit", "You've hit your weekly limit · resets 9pm (America/Los_Angeles)", true, ptr(la(10, 5, 21, 0))},
		{"progress-saved suffix", "rate_limit", "You've hit your session limit · resets 4pm (America/Los_Angeles) · progress saved", true, ptr(la(10, 5, 16, 0))},
		{"named zone differs from local", "rate_limit", "You've hit your session limit · resets 4pm (America/New_York)", true, ptr(time.Date(2026, 10, 5, 20, 0, 0, 0, time.UTC))},
		{"unknown zone falls back to local", "rate_limit", "You've hit your session limit · resets 4pm (Nowhere/Special)", true, ptr(la(10, 5, 16, 0))},
		{"truncated text keeps the verdict", "rate_limit", "You've hit your session limit · resets …", true, nil},
		{"credits exhausted waits on a human", "rate_limit", "You're out of usage credits. Run /usage-credits to keep using Fable 5.1 or /model to switch models.", true, nil},
		{"billing error", "billing_error", "Credit balance too low", true, nil},
		{"transient 429", "rate_limit", "API Error: 429 rate_limit_error: This request would exceed the rate limit", false, nil},
		{"transient 429 with no text", "rate_limit", "", false, nil},
		{"overloaded", "overloaded", "You've hit your session limit · resets 4pm (America/Los_Angeles)", false, nil},
		{"server error", "server_error", "API Error: 500", false, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			verdict, limited := FromClaudeStopFailure(tc.errorType, tc.message, at)
			if limited != tc.limited {
				t.Fatalf("limited = %v, want %v", limited, tc.limited)
			}
			assertReset(t, verdict.ResetsAt, tc.resetsAt)
		})
	}
}

func TestFromClaudeStopFailureShouldRollADatedResetIntoNextYear(t *testing.T) {
	december := time.Date(2026, 12, 30, 12, 0, 0, 0, losAngeles)
	verdict, _ := FromClaudeStopFailure("rate_limit", "You've hit your weekly limit · resets Jan 2, 9pm (America/Los_Angeles)", december)
	assertReset(t, verdict.ResetsAt, ptr(time.Date(2027, 1, 2, 21, 0, 0, 0, losAngeles)))
}

func TestFromPiErrorShouldClassifyEveryProviderForm(t *testing.T) {
	for _, tc := range []struct {
		name, message string
		limited       bool
		resetsAt      *time.Time
	}{
		{"pi's chatgpt rewrite", "You have hit your ChatGPT usage limit (plus plan). Try again in ~47 min.", true, ptr(at.Add(47 * time.Minute))},
		{"chatgpt rewrite without a reset", "You have hit your ChatGPT usage limit (plus plan).", true, nil},
		{"codex backend passthrough", "You’ve hit your usage limit. Upgrade to Pro (https://chatgpt.com/explore/pro), visit https://chatgpt.com/codex/settings/usage to purchase more credits or try again at 5:15 PM.", true, ptr(la(10, 5, 17, 15))},
		{"claude subscription text", "You've hit your session limit · resets 4pm (America/Los_Angeles)", true, ptr(la(10, 5, 16, 0))},
		{"openai api credits", "You have no credits remaining. Add credits to continue using the API at https://platform.openai.com/settings/organization/billing/.", true, nil},
		{"insufficient quota", "429 insufficient_quota: You exceeded your current quota", true, nil},
		{"hours reset", "Monthly usage limit reached. Try again in 3 hours.", true, ptr(at.Add(3 * time.Hour))},
		{"transient rate limit", "429 Too Many Requests: rate limit exceeded, retry after 20s", false, nil},
		{"overloaded", "529 overloaded_error", false, nil},
		{"empty", "", false, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			verdict, limited := FromPiError(tc.message, at)
			if limited != tc.limited {
				t.Fatalf("limited = %v, want %v", limited, tc.limited)
			}
			assertReset(t, verdict.ResetsAt, tc.resetsAt)
		})
	}
}

func TestCodexResetFromMessageShouldReadTheLocalClockTime(t *testing.T) {
	message := "You’ve hit your usage limit. Upgrade to Pro (https://chatgpt.com/explore/pro), visit https://chatgpt.com/codex/settings/usage to purchase more credits or try again at 5:15 PM."
	assertReset(t, CodexResetFromMessage(message, at), ptr(la(10, 5, 17, 15)))
	assertReset(t, CodexResetFromMessage("try again at 1:32 PM.", at), ptr(la(10, 5, 13, 32)))
	assertReset(t, CodexResetFromMessage("try again at 11:00 AM.", at), ptr(la(10, 6, 11, 0)))
	assertReset(t, CodexResetFromMessage("try again later.", at), nil)
}

func TestClockShouldRejectImpossibleReadings(t *testing.T) {
	for _, message := range []string{"resets 13pm (America/Los_Angeles)", "resets 0am", "resets 4:75pm", "resets Foo 3, 4pm", "resets Oct 40, 4pm"} {
		if got := parseResets(message, at); got != nil {
			t.Errorf("parseResets(%q) = %v, want nil", message, got)
		}
	}
}

func TestNextClockShouldKeepAResetInTheCurrentMinute(t *testing.T) {
	assertReset(t, nextClock(time.Date(2026, 10, 5, 17, 15, 40, 0, losAngeles), 17, 15, losAngeles), ptr(la(10, 5, 17, 15)))
}

func ptr(t time.Time) *time.Time { return &t }

func assertReset(t *testing.T, got, want *time.Time) {
	t.Helper()
	switch {
	case got == nil && want == nil:
	case got == nil || want == nil:
		t.Fatalf("resets_at = %v, want %v", got, want)
	case !got.Equal(*want):
		t.Fatalf("resets_at = %v, want %v", got.In(losAngeles), want.In(losAngeles))
	}
}
