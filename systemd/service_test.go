package systemd_test

import (
	"os"
	"strings"
	"testing"
)

func readUnit(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func requireUnitText(t *testing.T, unit, text string, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(text, want) {
			t.Errorf("%s is missing %q", unit, want)
		}
	}
}

func hasUnitDirective(text, directive string) bool {
	for _, line := range strings.Split(text, "\n") {
		if strings.TrimSpace(line) == directive {
			return true
		}
	}
	return false
}

func TestDaemonUnitKeepsMachineOverridesOutOfExecStart(t *testing.T) {
	const unit = "switchboard.service"
	text := readUnit(t, unit)
	requireUnitText(t, unit, text,
		"Environment=SWITCHBOARD_BIN=%h/go/bin/switchboard",
		"Environment=SWITCHBOARD_ARGS=",
		`exec "$$0" "$$@"`,
		"${SWITCHBOARD_BIN} $SWITCHBOARD_ARGS",
	)
	if strings.Contains(text, "exec %h/go/bin/switchboard") {
		t.Error("switchboard.service hard-codes its final binary instead of using the machine override seam")
	}
}

func TestRendererUnitsAreSeparateOptInProfiles(t *testing.T) {
	tests := []struct {
		unit        string
		profile     string
		condition   string
		environment string
		command     string
		other       string
	}{
		{
			unit:        "switchboard-waybar.service",
			profile:     "Waybar",
			condition:   "ConditionPathExists=%h/.config/waybar/claude.jsonc",
			environment: "Environment=SWITCHBOARD_CTL=%h/go/bin/switchboard-ctl",
			command:     "bottombar watch",
			other:       "polybar",
		},
		{
			unit:        "switchboard-polybar.service",
			profile:     "Polybar",
			condition:   "ConditionPathExists=%h/.config/polybar/switchboard.ini",
			environment: "Environment=SWITCHBOARD_POLYBAR_CONFIG=%h/.config/polybar/switchboard.ini",
			command:     `-c "$$1" switchboard`,
			other:       "waybar",
		},
	}

	for _, tt := range tests {
		t.Run(tt.profile, func(t *testing.T) {
			text := readUnit(t, tt.unit)
			requireUnitText(t, tt.unit, text,
				"PartOf=graphical-session.target",
				"Wants=switchboard.service",
				tt.condition,
				tt.environment,
				tt.command,
				"WantedBy=graphical-session.target",
			)
			if hasUnitDirective(text, "After=switchboard.service") {
				t.Error("renderer waits for switchboard.service, closing the graphical-session.target startup cycle")
			}
			if hasUnitDirective(text, "After=graphical-session.target") {
				t.Error("renderer ordered after the target that wants it, creating an activation cycle")
			}
			if strings.Contains(strings.ToLower(text), tt.other) {
				t.Errorf("%s profile also references %s", tt.profile, tt.other)
			}
			if strings.Contains(text, "WantedBy=default.target") {
				t.Error("renderer auto-activates outside a graphical host profile")
			}
		})
	}
}
