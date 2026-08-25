package main

import (
	"errors"
	"testing"

	"github.com/tjmisko/switchboard/internal/rpc"
)

type announceClient struct {
	request  rpc.Request
	sendErr  error
	recvErr  error
	response rpc.Response
}

func (c *announceClient) Send(request rpc.Request) error {
	c.request = request
	return c.sendErr
}

func (c *announceClient) Recv(response *rpc.Response) error {
	*response = c.response
	return c.recvErr
}

func TestAnnounceRemoteBindingsUsesRPCAndTreatsRejectionAsObserveOnly(t *testing.T) {
	client := &announceClient{response: rpc.Response{Error: "pane binding is not configured"}}
	if err := announceRemoteBindings(client); err != nil {
		t.Fatalf("application rejection should be non-fatal: %v", err)
	}
	if client.request.Cmd != "announce-bindings" {
		t.Fatalf("request cmd = %q, want announce-bindings", client.request.Cmd)
	}
}

func TestAnnounceRemoteBindingsPropagatesConnectionFailure(t *testing.T) {
	want := errors.New("connection failed")
	for _, client := range []*announceClient{
		{sendErr: want},
		{response: rpc.Response{OK: true}, recvErr: want},
	} {
		if err := announceRemoteBindings(client); !errors.Is(err, want) {
			t.Fatalf("announceRemoteBindings error = %v, want wrapped connection failure", err)
		}
	}
}
