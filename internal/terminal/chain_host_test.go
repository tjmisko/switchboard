package terminal

import (
	"context"
	"errors"
	"testing"
)

// activationLog records Activate calls across several fakes, in order.
type activationLog struct{ calls []string }

// loggingBatch is a batch fake whose Activate appends to a shared log and can
// fail with a set error.
type loggingBatch struct {
	batchFake
	log         *activationLog
	activateErr error
}

func (f loggingBatch) Activate(_ context.Context, ref *PaneRef) error {
	f.log.calls = append(f.log.calls, f.name+" "+ref.TTY)
	return f.activateErr
}

func newLoggingBatch(name string, log *activationLog, panes map[string]PaneRef) loggingBatch {
	return loggingBatch{batchFake: newBatchFake(name, panes), log: log}
}

// A herdr pane on pts/7 whose attached client runs in WezTerm pane pts/3.
func hostedPanes() (inner, outer map[string]PaneRef) {
	inner = map[string]PaneRef{
		"/dev/pts/7": {Backend: "herdr", Handle: "w1:p2", MuxSocket: "/h.sock", TTY: "/dev/pts/7", HostTTY: "/dev/pts/3"},
	}
	outer = map[string]PaneRef{
		"/dev/pts/3": {Backend: "wezterm", Mux: 900, PaneID: 12, WindowID: 4, WindowTitle: "herdr", TTY: "/dev/pts/3"},
	}
	return inner, outer
}

func TestChainShouldGiveAHostedPaneItsClientsWindowJoinWhenTheOuterTerminalOwnsTheClientTTY(t *testing.T) {
	inner, outer := hostedPanes()
	c := NewChain(newBatchFake("herdr", inner), newBatchFake("wezterm", outer))

	want := PaneRef{
		Backend: "herdr", Handle: "w1:p2", MuxSocket: "/h.sock", TTY: "/dev/pts/7", HostTTY: "/dev/pts/3",
		Mux: 900, WindowID: 4, WindowTitle: "herdr",
	}
	single, err := c.Locate(context.Background(), "/dev/pts/7")
	if err != nil || single == nil || *single != want {
		t.Fatalf("Locate = %+v, %v; want %+v", single, err, want)
	}
	batch, err := chainSnapshot(t, c)
	if err != nil || batch["/dev/pts/7"] != want {
		t.Fatalf("Snapshot[pts/7] = %+v, %v; want %+v", batch["/dev/pts/7"], err, want)
	}
	if batch["/dev/pts/3"] != outer["/dev/pts/3"] {
		t.Fatalf("Snapshot[pts/3] = %+v, want the outer pane unchanged", batch["/dev/pts/3"])
	}
}

func TestChainShouldLeaveAHostedPaneWithoutAWindowWhenNoBackendOwnsTheClientTTY(t *testing.T) {
	inner, _ := hostedPanes()
	c := NewChain(newBatchFake("herdr", inner), newBatchFake("wezterm", map[string]PaneRef{}))

	single, _ := c.Locate(context.Background(), "/dev/pts/7")
	batch, _ := chainSnapshot(t, c)
	if single == nil || single.Mux != 0 || batch["/dev/pts/7"].Mux != 0 {
		t.Fatalf("Locate = %+v, Snapshot = %+v; want no window join", single, batch["/dev/pts/7"])
	}
}

// herdr client in a foot window, inside a herdr pane whose client runs in
// WezTerm: the window that shows the agent is WezTerm's.
func TestChainShouldFollowNestedHostsToTheOutermostWindowWhenClientsNest(t *testing.T) {
	inner := map[string]PaneRef{
		"/dev/pts/7": {Backend: "herdr", Handle: "w1:p1", TTY: "/dev/pts/7", HostTTY: "/dev/pts/5"},
		"/dev/pts/5": {Backend: "herdr", Handle: "w9:p9", TTY: "/dev/pts/5", HostTTY: "/dev/pts/3"},
	}
	_, outer := hostedPanes()
	c := NewChain(newBatchFake("herdr", inner), newBatchFake("wezterm", outer))

	single, _ := c.Locate(context.Background(), "/dev/pts/7")
	batch, _ := chainSnapshot(t, c)
	if single == nil || single.Mux != 900 || batch["/dev/pts/7"].Mux != 900 {
		t.Fatalf("Locate = %+v, Snapshot = %+v; want the WezTerm window (mux 900)", single, batch["/dev/pts/7"])
	}
}

func TestChainShouldTerminateWhenHostTTYsFormACycle(t *testing.T) {
	inner := map[string]PaneRef{
		"/dev/pts/7": {Backend: "herdr", TTY: "/dev/pts/7", HostTTY: "/dev/pts/8"},
		"/dev/pts/8": {Backend: "herdr", TTY: "/dev/pts/8", HostTTY: "/dev/pts/7"},
	}
	c := NewChain(newBatchFake("herdr", inner), newBatchFake("wezterm", map[string]PaneRef{}))

	if _, err := c.Locate(context.Background(), "/dev/pts/7"); err != nil {
		t.Fatalf("Locate: %v", err)
	}
	if _, err := chainSnapshot(t, c); err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if err := c.Activate(context.Background(), &PaneRef{Backend: "herdr", TTY: "/dev/pts/7", HostTTY: "/dev/pts/8"}); err != nil {
		t.Fatalf("Activate: %v", err)
	}
}

// herdr switches its clients to the pane's tab; WezTerm then has to show the
// tab running that client, or the raise lands on the right window, wrong tab.
func TestChainShouldActivateTheOuterPaneHostingTheClientAfterThePane(t *testing.T) {
	inner, outer := hostedPanes()
	log := &activationLog{}
	c := NewChain(newLoggingBatch("herdr", log, inner), newLoggingBatch("wezterm", log, outer))

	ref, _ := c.Locate(context.Background(), "/dev/pts/7")
	if err := c.Activate(context.Background(), ref); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	want := []string{"herdr /dev/pts/7", "wezterm /dev/pts/3"}
	if len(log.calls) != 2 || log.calls[0] != want[0] || log.calls[1] != want[1] {
		t.Fatalf("activations = %v, want %v", log.calls, want)
	}
}

func TestChainShouldSucceedWhenTheOuterTerminalHasNoStepFinerThanItsWindow(t *testing.T) {
	inner, outer := hostedPanes()
	log := &activationLog{}
	foot := newLoggingBatch("wezterm", log, outer)
	foot.activateErr = ErrUnsupported
	c := NewChain(newLoggingBatch("herdr", log, inner), foot)

	ref, _ := c.Locate(context.Background(), "/dev/pts/7")
	if err := c.Activate(context.Background(), ref); err != nil {
		t.Fatalf("Activate = %v, want nil when the host is window-only", err)
	}
}

func TestChainShouldReportTheOuterActivateErrorWhenTheHostPaneCannotBeFocused(t *testing.T) {
	inner, outer := hostedPanes()
	log := &activationLog{}
	wez := newLoggingBatch("wezterm", log, outer)
	wez.activateErr = errors.New("wezterm cli: no such pane")
	c := NewChain(newLoggingBatch("herdr", log, inner), wez)

	ref, _ := c.Locate(context.Background(), "/dev/pts/7")
	if err := c.Activate(context.Background(), ref); err == nil {
		t.Fatal("Activate = nil, want the host pane's error")
	}
}

func TestChainShouldNotTouchTheOuterPaneWhenThePaneItselfFailsToActivate(t *testing.T) {
	inner, outer := hostedPanes()
	log := &activationLog{}
	herdr := newLoggingBatch("herdr", log, inner)
	herdr.activateErr = errors.New("herdr: not_found")
	c := NewChain(herdr, newLoggingBatch("wezterm", log, outer))

	ref, _ := c.Locate(context.Background(), "/dev/pts/7")
	if err := c.Activate(context.Background(), ref); err == nil {
		t.Fatal("Activate = nil, want the pane's error")
	}
	if len(log.calls) != 1 {
		t.Fatalf("activations = %v, want only the failed herdr call", log.calls)
	}
}
