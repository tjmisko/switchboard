package history

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Config gates and bounds the activity log. It is OFF by default: history is
// opt-in because it records when and where you work. The on-disk form lives at
// $XDG_CONFIG_HOME/switchboard/history.json (sibling of projects.json), e.g.:
//
//	{ "enabled": true, "detail": "minimal", "memory": true, "retain_days": 90, "max_bytes": 2147483648 }
//
// Dir and ResolveProject are not part of the file — the daemon sets them (Dir
// from a flag, ResolveProject from the project-name resolver) so history stays a
// dependency-light leaf.
type Config struct {
	Enabled    bool   `json:"enabled"`
	Detail     string `json:"detail"`      // minimal (default) | full
	RetainDays int    `json:"retain_days"` // delete day-files older than this; 0 = unlimited
	MaxBytes   int64  `json:"max_bytes"`   // trim oldest day-files past this total; 0 = unlimited

	// Memory gates per-session memory sampling — a memory_sample per live session
	// per reconcile tick. On by default when history is on: the samples are byte
	// counts with nothing content-shaped in them, so they carry no privacy cost
	// the tier system needs to weigh. It is separately switchable because it does
	// carry a VOLUME cost — roughly 12x the line rate of everything else the log
	// records — and someone who wants the timeline without that should not have to
	// turn off history to get it.
	Memory bool `json:"memory"`

	// Dir overrides the storage directory (default DefaultDir). Not read from the
	// file; set by the daemon's -history-dir flag.
	Dir string `json:"-"`
	// ResolveProject maps a session cwd to its project abbreviation for the
	// `project` field. Optional; nil leaves project empty. Set by the daemon so
	// the resolution runs off the hot path (in the writer goroutine).
	ResolveProject func(cwd string) string `json:"-"`
}

// Default size caps, chosen for what is being recorded.
//
// Memory sampling writes one line per live session per reconcile tick. Measured
// against the busiest recorded day (2026-07-22: 66 lanes, 54.8 live
// session-hours, ~39.5k ticks at 5 s), that is ~11.8 MB/day — about 12x the line
// volume of everything else the log records put together, which wrote 1.0 MB
// that same day. The whole store today is 4.95 MB across 33 day-files.
//
// The cap has to move with it, because retention is size-bounded BEFORE it is
// age-bounded: pruneDir trims oldest-first on total size, and RetainDays only
// gets a say over whatever survives that. Left at 100 MB, a configured 90-day
// retention would quietly become about ten days, with nothing logged and nothing
// to notice. That corpus is load-bearing rather than nice-to-have — the suspect
// caps in suspect.go were calibrated by replaying a month of it, a method that
// stops being possible the moment the store self-trims to ten days. 90 days at
// the measured rate is ~1.1 GB; 2 GB leaves headroom for a busier month.
const (
	defaultMaxBytes       = 100 * 1024 * 1024      // transitions, usage, fanout, focus
	defaultMemoryMaxBytes = 2 * 1024 * 1024 * 1024 // …plus a sample per session per tick
)

// DefaultConfig is the off-by-default baseline: disabled, minimal detail, memory
// sampling on, keep 90 days / defaultMemoryMaxBytes. A present-but-partial config
// file overrides only the fields it sets (zero retain_days/max_bytes mean
// "unlimited", which is a deliberate value, so we only backfill them when the
// file is absent).
func DefaultConfig() Config {
	return Config{
		Enabled:    false,
		Detail:     DetailMinimal,
		RetainDays: 90,
		MaxBytes:   defaultMemoryMaxBytes,
		Memory:     true,
	}
}

// HumanBytes renders a byte count for the startup log line — "2.0 GB",
// "100.0 MB", and "unlimited" for the 0 that means no cap. Powers of 1024, which
// is how the caps above are written.
func HumanBytes(n int64) string {
	if n <= 0 {
		return "unlimited"
	}
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

// ConfigPath returns the user history-config path, honoring XDG_CONFIG_HOME.
func ConfigPath() string {
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "switchboard", "history.json")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "switchboard", "history.json")
}

// LoadConfig reads the user history config, falling back to DefaultConfig when
// the file is absent or unreadable. Detail is normalized to a known tier.
func LoadConfig() Config {
	return loadConfigFrom(ConfigPath())
}

// configFile is the on-disk form, decoded through pointers so an ABSENT key is
// distinguishable from one explicitly set to a zero value. Decoding straight
// over the defaults cannot tell those apart, and the difference is load-bearing
// twice: 0 means "unlimited" for both retention fields, and the max_bytes
// default depends on whether memory sampling is on — which can only be applied
// when we know the file did not set one itself.
type configFile struct {
	Enabled    *bool   `json:"enabled"`
	Detail     *string `json:"detail"`
	Memory     *bool   `json:"memory"`
	RetainDays *int    `json:"retain_days"`
	MaxBytes   *int64  `json:"max_bytes"`
}

func loadConfigFrom(path string) Config {
	cfg := DefaultConfig()
	data, err := os.ReadFile(path)
	if err != nil {
		return cfg // absent/unreadable → defaults (disabled)
	}
	var file configFile
	if json.Unmarshal(data, &file) != nil {
		return cfg // malformed → defaults (disabled)
	}
	if file.Enabled != nil {
		cfg.Enabled = *file.Enabled
	}
	if file.Detail != nil {
		cfg.Detail = *file.Detail
	}
	if file.Memory != nil {
		cfg.Memory = *file.Memory
	}
	if file.RetainDays != nil {
		cfg.RetainDays = *file.RetainDays
	}
	// An explicitly configured cap always wins, including an explicit 0
	// (unlimited). Only an omitted one takes the default for what is recorded,
	// which is the whole reason presence is tracked above.
	switch {
	case file.MaxBytes != nil:
		cfg.MaxBytes = *file.MaxBytes
	case !cfg.Memory:
		cfg.MaxBytes = defaultMaxBytes
	}
	if cfg.Detail != DetailFull {
		cfg.Detail = DetailMinimal
	}
	return cfg
}
