package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/tjmisko/switchboard/internal/remotestate"
	"github.com/tjmisko/switchboard/internal/rpc"
)

// cmdRemoteStream exposes only the daemon attached to this machine. The SSH
// destination is selected by the client-side source manager; it never crosses
// this local RPC connection and remote-stream offers no action channel back.
func cmdRemoteStream(client *rpc.Client) {
	closeout, stopSignals := watchForTeardown()
	defer stopSignals()
	options := remotestate.StreamOptions{
		OnAttach: func(context.Context) error { return announceRemoteBindings(client) },
		Closeout: closeout,
	}
	if err := remotestate.StreamLocal(context.Background(), client, os.Stdout, options); err != nil {
		fail("remote-stream: %v", err)
	}
}

// watchForTeardown turns a stop signal into a closeout reason.
//
// Only a signal qualifies. A signal means something DECIDED this stream should
// end — the host is going down, or an operator stopped it — and the client
// should believe the last snapshot rather than hold rows for a machine that is
// leaving. Every other way this process can end is ambiguous, and the loudest
// example is the local daemon socket dropping under a `systemctl restart`: the
// agent sessions on this machine keep running throughout, so a closeout there
// would blank the client's chips for exactly the restart the hold exists to
// cover. That path deliberately exits without one.
//
// The channel is buffered so the notifier never blocks, and only the first
// signal is acted on: a second one during the final write should kill the
// process, which is the default disposition once the handler is reset.
func watchForTeardown() (<-chan string, func()) {
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	closeout := make(chan string, 1)
	go func() {
		if _, ok := <-signals; !ok {
			return
		}
		signal.Stop(signals)
		closeout <- remotestate.CloseoutSignal
	}()
	return closeout, func() { signal.Stop(signals) }
}

// announceRemoteBindings completes before subscribe starts because subscribe
// owns the connection for the rest of its lifetime. The daemon implements the
// actual bounded/non-blocking TTY announcements; ctl only requests them.
func announceRemoteBindings(client remotestate.SubscriptionClient) error {
	if err := client.Send(rpc.Request{Cmd: "announce-bindings"}); err != nil {
		return err
	}
	var response rpc.Response
	if err := client.Recv(&response); err != nil {
		return err
	}
	// Binding announcement is enrichment: a daemon with no emitter, or one
	// whose bounded TTY writes fail, still has authoritative live snapshots.
	// The response must be consumed before subscribe takes over the connection,
	// but an application-level rejection leaves the rows observe-only.
	return nil
}
