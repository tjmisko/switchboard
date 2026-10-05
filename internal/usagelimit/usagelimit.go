// Package usagelimit classifies provider error text as a subscription usage
// limit and extracts the reset time it names. It runs at the hook privacy
// boundary (switchboard-ctl) and on rollout reads: the message text never
// leaves it, only the verdict and an optional reset instant.
//
// The forms below were observed in local transcripts and rollouts; see
// docs/usage-limit-status-plan.md for provenance.
package usagelimit

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Verdict is a classified usage-limit stop. ResetsAt is nil when the provider
// named no reset (credits exhausted, billing): the session waits on a human.
type Verdict struct {
	ResetsAt *time.Time
}

// FromClaudeStopFailure classifies a Claude Code StopFailure. errorType is the
// hook's `error` field and message its `last_assistant_message`; at is when the
// turn failed. A bare rate_limit is a transient 429 unless the text names a
// subscription limit or exhausted usage credits.
//
//	You've hit your session limit · resets 4pm (America/Los_Angeles)
//	You've hit your weekly limit · resets Aug 25, 9pm (America/Los_Angeles)
//	You're out of usage credits. Run /usage-credits to keep using …
func FromClaudeStopFailure(errorType, message string, at time.Time) (Verdict, bool) {
	switch errorType {
	case "rate_limit":
		lower := strings.ToLower(message)
		switch {
		case claudeLimitPattern.MatchString(lower):
			return Verdict{ResetsAt: parseResets(message, at)}, true
		case strings.Contains(lower, "out of usage"):
			return Verdict{}, true
		}
		return Verdict{}, false
	case "billing_error":
		return Verdict{}, true
	}
	return Verdict{}, false
}

// FromPiError classifies the errorMessage of a Pi assistant message that ended
// with stopReason "error". Pi rewrites a ChatGPT-subscription 429 as "You have
// hit your ChatGPT usage limit (plus plan). Try again in ~47 min.", passes the
// Codex backend's own "You've hit your usage limit … try again at 5:15 PM."
// through, and treats the provider-limit patterns below as non-retryable.
// Transient rate limits are retried by Pi itself and are not limits here.
func FromPiError(message string, at time.Time) (Verdict, bool) {
	lower := strings.ToLower(message)
	switch {
	case strings.Contains(lower, "usage limit") || claudeLimitPattern.MatchString(lower):
		return Verdict{ResetsAt: firstReset(message, at)}, true
	case strings.Contains(lower, "out of usage"):
		return Verdict{}, true
	}
	for _, pattern := range piHardLimitPatterns {
		if strings.Contains(lower, pattern) {
			return Verdict{ResetsAt: firstReset(message, at)}, true
		}
	}
	return Verdict{}, false
}

// CodexResetFromMessage reads "try again at 5:15 PM." from a Codex
// usage_limit_exceeded error, in the local zone, at or after at. Callers prefer
// the rollout's rate_limits epoch and use this only without one.
func CodexResetFromMessage(message string, at time.Time) *time.Time {
	return parseTryAgain(message, at)
}

var (
	claudeLimitPattern = regexp.MustCompile(`hit your [a-z0-9 ]*limit`)
	// resets 4pm (Area/City) | resets at 8:20pm | resets Aug 25, 9pm (Area/City)
	resetsPattern = regexp.MustCompile(`(?i)\bresets\s+(?:at\s+)?(?:([A-Z][a-z]{2})[a-z]*\.?\s+(\d{1,2}),?\s+(?:at\s+)?)?(\d{1,2})(?::(\d{2}))?\s*([ap])\.?m\.?(?:\s*\(([^)]+)\))?`)
	// try again at 5:15 PM | try again at 1pm | try again at Oct 7th, 2026 5:15 PM
	tryAgainAtPattern = regexp.MustCompile(`(?i)\btry again at\s+(?:([A-Z][a-z]{2})[a-z]*\.?\s+(\d{1,2})(?:st|nd|rd|th)?,?\s+(?:(\d{4})\s+)?(?:at\s+)?)?(\d{1,2})(?::(\d{2}))?\s*([ap])\.?m\b`)
	// Try again in ~47 min | try again in 2 hours
	tryAgainInPattern = regexp.MustCompile(`(?i)\btry again in\s+~?\s*(\d+)\s*(min|minute|minutes|h|hr|hrs|hour|hours)\b`)

	// Pi's own non-retryable provider-limit vocabulary (pi-coding-agent
	// NON_RETRYABLE_PROVIDER_LIMIT_ERROR_PATTERN), lower-cased, plus the
	// OpenAI API's "no credits remaining".
	piHardLimitPatterns = []string{
		"gousagelimiterror", "freeusagelimiterror", "monthly usage limit reached",
		"available balance", "insufficient_quota", "out of budget", "quota exceeded",
		"no credits remaining",
	}
)

func firstReset(message string, at time.Time) *time.Time {
	for _, parse := range []func(string, time.Time) *time.Time{parseResets, parseTryAgain, parseTryAgainIn} {
		if resetsAt := parse(message, at); resetsAt != nil {
			return resetsAt
		}
	}
	return nil
}

func parseResets(message string, at time.Time) *time.Time {
	match := resetsPattern.FindStringSubmatch(message)
	if match == nil {
		return nil
	}
	loc := time.Local
	if zone := strings.TrimSpace(match[6]); zone != "" {
		if named, err := time.LoadLocation(zone); err == nil {
			loc = named
		}
	}
	hour, minute, ok := clock(match[3], match[4], match[5])
	if !ok {
		return nil
	}
	if match[1] == "" {
		return nextClock(at, hour, minute, loc)
	}
	return datedClock(match[1], match[2], "", hour, minute, loc, at)
}

// datedClock builds "<Mon> <d>[, <yyyy>] h:mm" in loc. A date without a year
// is the next one: "Jan 2" read on Dec 30.
func datedClock(monthText, dayText, yearText string, hour, minute int, loc *time.Location, at time.Time) *time.Time {
	month, ok := monthByAbbrev[strings.ToLower(monthText)]
	day, err := strconv.Atoi(dayText)
	if !ok || err != nil || day < 1 || day > 31 {
		return nil
	}
	year := at.In(loc).Year()
	if yearText != "" {
		if year, err = strconv.Atoi(yearText); err != nil {
			return nil
		}
	}
	resetsAt := time.Date(year, month, day, hour, minute, 0, 0, loc)
	if yearText == "" && resetsAt.Before(at.Add(-24*time.Hour)) {
		resetsAt = resetsAt.AddDate(1, 0, 0)
	}
	return &resetsAt
}

func parseTryAgain(message string, at time.Time) *time.Time {
	match := tryAgainAtPattern.FindStringSubmatch(message)
	if match == nil {
		return nil
	}
	hour, minute, ok := clock(match[4], match[5], match[6])
	if !ok {
		return nil
	}
	if match[1] == "" {
		return nextClock(at, hour, minute, time.Local)
	}
	return datedClock(match[1], match[2], match[3], hour, minute, time.Local, at)
}

func parseTryAgainIn(message string, at time.Time) *time.Time {
	match := tryAgainInPattern.FindStringSubmatch(message)
	if match == nil {
		return nil
	}
	count, err := strconv.Atoi(match[1])
	if err != nil {
		return nil
	}
	unit := time.Minute
	if strings.HasPrefix(strings.ToLower(match[2]), "h") {
		unit = time.Hour
	}
	resetsAt := at.Add(time.Duration(count) * unit)
	return &resetsAt
}

// clock converts a 12-hour h[:mm] a|p reading to 24-hour fields.
func clock(hourText, minuteText, meridiem string) (hour, minute int, ok bool) {
	hour, err := strconv.Atoi(hourText)
	if err != nil || hour < 1 || hour > 12 {
		return 0, 0, false
	}
	if minuteText != "" {
		if minute, err = strconv.Atoi(minuteText); err != nil || minute > 59 {
			return 0, 0, false
		}
	}
	hour %= 12
	if strings.EqualFold(meridiem, "p") {
		hour += 12
	}
	return hour, minute, true
}

// nextClock is the first hour:minute in loc at or after at.
func nextClock(at time.Time, hour, minute int, loc *time.Location) *time.Time {
	local := at.In(loc)
	resetsAt := time.Date(local.Year(), local.Month(), local.Day(), hour, minute, 0, 0, loc)
	if resetsAt.Before(at.Truncate(time.Minute)) {
		resetsAt = resetsAt.AddDate(0, 0, 1)
	}
	return &resetsAt
}

var monthByAbbrev = map[string]time.Month{
	"jan": time.January, "feb": time.February, "mar": time.March, "apr": time.April,
	"may": time.May, "jun": time.June, "jul": time.July, "aug": time.August,
	"sep": time.September, "oct": time.October, "nov": time.November, "dec": time.December,
}
