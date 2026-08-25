package weztermintegration

import (
	"os/exec"
	"testing"
)

func TestLuaModuleCallbacksAndDeduplication(t *testing.T) {
	lua, err := exec.LookPath("lua")
	if err != nil {
		t.Skip("lua interpreter is unavailable")
	}
	cmd := exec.Command(lua, "switchboard_test.lua", "switchboard.lua")
	cmd.Dir = "."
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("lua integration test: %v\n%s", err, out)
	}
}
