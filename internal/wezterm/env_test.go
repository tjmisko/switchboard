package wezterm

import (
	"strings"
	"testing"
)

func TestReplaceEnvRemovesConflictingSocketValues(t *testing.T) {
	env := replaceEnv([]string{
		"PATH=/bin", "WEZTERM_UNIX_SOCKET=/wrong/one", "HOME=/home/u", "WEZTERM_UNIX_SOCKET=/wrong/two",
	}, "WEZTERM_UNIX_SOCKET", "/right/gui-sock-42")
	var sockets []string
	for _, entry := range env {
		if strings.HasPrefix(entry, "WEZTERM_UNIX_SOCKET=") {
			sockets = append(sockets, entry)
		}
	}
	if len(sockets) != 1 || sockets[0] != "WEZTERM_UNIX_SOCKET=/right/gui-sock-42" {
		t.Fatalf("socket entries = %v", sockets)
	}
}
