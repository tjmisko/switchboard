package wezterm

import (
	"context"
	"errors"
	"testing"

	"github.com/tjmisko/switchboard/internal/testsupport"
)

func TestListGUIRejectsSocketWhosePeerDoesNotMatchFilenamePID(t *testing.T) {
	runtime := testsupport.NewWeztermRuntime(t)
	pid := testsupport.LivePID()
	runtime.AddMux(t, pid)
	_, err := listGUI(t.Context(), pid, func(context.Context, string) (int, error) { return pid + 1, nil })
	if !errors.Is(err, ErrGUISocketPeer) {
		t.Fatalf("ListGUI error = %v, want ErrGUISocketPeer", err)
	}
}

func TestListGUIPreservesTransientPeerInspectionError(t *testing.T) {
	runtime := testsupport.NewWeztermRuntime(t)
	pid := testsupport.LivePID()
	runtime.AddMux(t, pid)
	want := errors.New("temporary dial failure")
	_, err := listGUI(t.Context(), pid, func(context.Context, string) (int, error) { return 0, want })
	if !errors.Is(err, want) || errors.Is(err, ErrGUISocketPeer) {
		t.Fatalf("ListGUI error = %v, want preserved transient error only", err)
	}
}
