package main

import (
	"path/filepath"
	"testing"

	"github.com/tjmisko/switchboard/internal/history"
	codexprovider "github.com/tjmisko/switchboard/internal/provider/codex"
)

func TestProductionCodexConfigWiresDurableRolloutOnlyWithHistory(t *testing.T) {
	dir := t.TempDir()
	enabled := history.NewSink(history.Config{Enabled: true, Detail: history.DetailMinimal, Dir: dir})
	t.Cleanup(enabled.Close)
	config := configureCodexUsagePersistence(codexprovider.Config{}, enabled)
	if config.UsageRecorder == nil || config.RolloutStateDir != filepath.Join(dir, ".codex-usage-cursors") {
		t.Fatalf("enabled production config = %#v", config)
	}

	disabled := history.NewSink(history.Config{Enabled: false, Dir: filepath.Join(dir, "off")})
	config = configureCodexUsagePersistence(codexprovider.Config{}, disabled)
	if config.UsageRecorder != nil || config.RolloutStateDir != "" {
		t.Fatalf("disabled history unexpectedly persisted usage = %#v", config)
	}
}
