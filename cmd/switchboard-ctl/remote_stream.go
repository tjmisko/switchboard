package main

import (
	"context"
	"os"

	"github.com/tjmisko/switchboard/internal/remotestate"
	"github.com/tjmisko/switchboard/internal/rpc"
)

// cmdRemoteStream exposes only the daemon attached to this machine. The SSH
// destination is selected by the client-side source manager; it never crosses
// this local RPC connection and remote-stream offers no action channel back.
func cmdRemoteStream(client *rpc.Client) {
	options := remotestate.StreamOptions{
		OnAttach: func(context.Context) error { return announceRemoteBindings(client) },
	}
	if err := remotestate.StreamLocal(context.Background(), client, os.Stdout, options); err != nil {
		fail("remote-stream: %v", err)
	}
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
