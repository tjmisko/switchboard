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
//	{ "enabled": true, "detail": "minimal", "retain_days": 90, "max_bytes": 104857600 }
//
// A `memory` key from the retired per-session memory sampler (removed
// 2026-08-26, docs/seed-replay-memory-plan.md) is ignored if present.
//
// Dir and ResolveProject are not part of the file — the daemon sets them (Dir
// from a flag, ResolveProject from the project-name resolver) so history stays a
// dependency-light leaf.
type Config struct {
	Enabled    bool   `json:"enabled"`
	Detail     string `json:"detail"`      // minimal (default) | full
	RetainDays int    `json:"retain_days"` // delete day-files older than this; 0 = unlimited
	MaxBytes   int64  `json:"max_bytes"`   // trim oldest day-files past this total; 0 = unlimited

	// Dir overrides the storage directory (default DefaultDir). Not read from the
	// file; set by the daemon's -history-dir flag.
	Dir string `json:"-"`
	// ResolveProject maps a session cwd to its project abbreviation for the
	// `project` field. Optional; nil leaves project empty. Set by the daemon so
	// the resolution runs off the hot path (in the writer goroutine).
	ResolveProject func(cwd string) string `json:"-"`
}

// defaultMaxBytes bounds the store for what is recorded: transitions, usage,
// fanout, focus. Measured across the pre-sampling corpus that volume is
// ~1 MB/day busy, so 100 MB comfortably holds the 90-day default retention.
// (The retired memory sampler wrote ~12x that line rate and carried a
// gigabytes-scale default of its own; when it went, so did the split default.)
const defaultMaxBytes = 100 * 1024 * 1024

// DefaultConfig is the off-by-default baseline: disabled, minimal detail, keep
// 90 days / defaultMaxBytes. A present-but-partial config file overrides only
// the fields it sets (zero retain_days/max_bytes mean "unlimited", which is a
// deliberate value, so we only backfill them when the file is absent).
func DefaultConfig() Config {
	return Config{
		Enabled:    false,
		Detail:     DetailMinimal,
		RetainDays: 90,
		MaxBytes:   defaultMaxBytes,
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
// over the defaults cannot tell those apart, and the difference is load-bearing:
// 0 means "unlimited" for both retention fields.
type configFile struct {
	Enabled    *bool   `json:"enabled"`
	Detail     *string `json:"detail"`
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
	if file.RetainDays != nil {
		cfg.RetainDays = *file.RetainDays
	}
	// An explicitly configured cap always wins, including an explicit 0
	// (unlimited); an omitted one keeps the default.
	if file.MaxBytes != nil {
		cfg.MaxBytes = *file.MaxBytes
	}
	if cfg.Detail != DetailFull {
		cfg.Detail = DetailMinimal
	}
	return cfg
}
