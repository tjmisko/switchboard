package waybar_test

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

func TestBottomConfigUsesDistinctSignalTriggeredSlotFiles(t *testing.T) {
	b, err := os.ReadFile("claude.jsonc")
	if err != nil {
		t.Fatal(err)
	}
	text := string(b)
	if strings.Contains(text, "switchboard-waybar --slot") {
		t.Fatal("live example still launches a Go renderer per slot")
	}
	if strings.Contains(text, "/usr/bin/cat") {
		t.Fatal("slot reader should use the shell already launched by Waybar")
	}
	if got := strings.Count(text, `IFS= read -r line`); got != 10 {
		t.Fatalf("one-shot shell read count = %d, want 10", got)
	}
	if strings.Contains(text, `"interval"`) || strings.Contains(text, `"restart-interval"`) {
		t.Fatal("signal modules must not poll or keep a continuous reader")
	}
	if got := strings.Count(text, `"exec-on-event": false`); got != 10 {
		t.Fatalf("exec-on-event=false count = %d, want 10", got)
	}
	if got := strings.Count(text, `"on-click-right": "$HOME/.config/scripts/claude-picker"`); got != 10 {
		t.Fatalf("picker binding count = %d, want 10", got)
	}
	if got := strings.Count(text, `"on-click-middle": "$HOME/.config/scripts/claude-abbrev-edit `); got != 10 {
		t.Fatalf("rename binding count = %d, want 10", got)
	}
	for slot := 0; slot < 10; slot++ {
		for _, want := range []string{
			fmt.Sprintf("slot-%d.json", slot),
			fmt.Sprintf(`"signal": %d`, slot+1),
		} {
			if !strings.Contains(text, want) {
				t.Errorf("config is missing %q", want)
			}
		}
	}
	for _, want := range []string{"bottom-waybar.ready", "$PPID", `/proc/$PPID/stat`} {
		if !strings.Contains(text, want) {
			t.Errorf("startup readiness handshake is missing %q", want)
		}
	}
}
