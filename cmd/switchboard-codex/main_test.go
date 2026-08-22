package main

import (
	"regexp"
	"slices"
	"testing"
)

func TestRandomUUIDIsVersion4AndUnique(t *testing.T) {
	pattern := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	first, err := randomUUID()
	if err != nil {
		t.Fatal(err)
	}
	second, err := randomUUID()
	if err != nil {
		t.Fatal(err)
	}
	if !pattern.MatchString(first) || !pattern.MatchString(second) || first == second {
		t.Fatalf("UUIDs = %q, %q", first, second)
	}
}

func TestReplaceEnvRemovesInheritedSlotIdentity(t *testing.T) {
	got := replaceEnv([]string{
		"PATH=/bin",
		"SWITCHBOARD_SLOT_ID=old-slot",
		"SWITCHBOARD_CODEX_ENDPOINT=unix:///old.sock",
	}, map[string]string{
		"SWITCHBOARD_SLOT_ID":        "new-slot",
		"SWITCHBOARD_CODEX_ENDPOINT": "unix:///new.sock",
	})
	for _, unwanted := range []string{"SWITCHBOARD_SLOT_ID=old-slot", "SWITCHBOARD_CODEX_ENDPOINT=unix:///old.sock"} {
		if slices.Contains(got, unwanted) {
			t.Fatalf("inherited value survived: %v", got)
		}
	}
	for _, want := range []string{"PATH=/bin", "SWITCHBOARD_SLOT_ID=new-slot", "SWITCHBOARD_CODEX_ENDPOINT=unix:///new.sock"} {
		if !slices.Contains(got, want) {
			t.Fatalf("environment missing %q: %v", want, got)
		}
	}
}
