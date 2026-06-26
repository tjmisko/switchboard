package history

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// Config gates and bounds the activity log. It is OFF by default: history is
// opt-in because it records when and where you work. The on-disk form lives at
// $XDG_CONFIG_HOME/switchboard/history.json (sibling of projects.json), e.g.:
//
//	{ "enabled": true, "detail": "minimal", "retain_days": 90, "max_bytes": 104857600 }
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

// DefaultConfig is the off-by-default baseline: disabled, minimal detail, keep
// 90 days / 100 MB. A present-but-partial config file overrides only the fields
// it sets (zero retain_days/max_bytes mean "unlimited", which is a deliberate
// value, so we only backfill them when the file is absent).
func DefaultConfig() Config {
	return Config{
		Enabled:    false,
		Detail:     DetailMinimal,
		RetainDays: 90,
		MaxBytes:   100 * 1024 * 1024,
	}
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

func loadConfigFrom(path string) Config {
	cfg := DefaultConfig()
	data, err := os.ReadFile(path)
	if err != nil {
		return cfg // absent/unreadable → defaults (disabled)
	}
	// Decode over the defaults so an omitted field keeps its default, except the
	// "0 = unlimited" retention fields, which a present file is allowed to set to 0.
	_ = json.Unmarshal(data, &cfg)
	if cfg.Detail != DetailFull {
		cfg.Detail = DetailMinimal
	}
	return cfg
}
