package codex

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"testing"
	"time"
)

type oneConnection struct{ connection Connection }

func (c oneConnection) Connect(context.Context) (Connection, error) { return c.connection, nil }

func TestEphemeralNamerUsesIsolatedEphemeralThread(t *testing.T) {
	client, server := net.Pipe()
	defer server.Close()
	requests := make(chan rpcEnvelope, 4)
	go func() {
		decoder, encoder := json.NewDecoder(server), json.NewEncoder(server)
		for {
			var request rpcEnvelope
			if decoder.Decode(&request) != nil {
				return
			}
			if request.Method == "initialized" {
				continue
			}
			requests <- request
			var result any = struct{}{}
			switch request.Method {
			case "initialize":
				result = initializeResult{UserAgent: "codex_app_server/0.149.0"}
			case "thread/start":
				result = map[string]any{"thread": map[string]string{"id": "ephemeral-name-thread"}}
			}
			_ = encoder.Encode(struct {
				ID     json.RawMessage `json:"id"`
				Result any             `json:"result"`
			}{ID: request.ID, Result: result})
			if request.Method == "turn/start" {
				_ = encoder.Encode(map[string]any{
					"method": "item/completed",
					"params": map[string]any{"item": map[string]string{"type": "agentMessage", "text": "completed-turn-label"}},
				})
				_ = encoder.Encode(map[string]any{"method": "turn/completed", "params": map[string]any{}})
			}
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	input := NamingContext{CWDBase: "switchboard", UserPrompt: "fix rotation", AssistantResponse: "implemented and verified the hook lifecycle"}
	got, err := (EphemeralNamer{Connector: oneConnection{connection: client}}).Generate(ctx, input, "test-model")
	if err != nil || got != "completed-turn-label" {
		t.Fatalf("Generate = %q, %v", got, err)
	}

	seen := make(map[string]json.RawMessage)
	for range 3 {
		request := <-requests
		seen[request.Method] = request.Params
	}
	var start struct {
		Model          string `json:"model"`
		Ephemeral      bool   `json:"ephemeral"`
		ApprovalPolicy string `json:"approvalPolicy"`
		Sandbox        string `json:"sandbox"`
	}
	if err := json.Unmarshal(seen["thread/start"], &start); err != nil {
		t.Fatal(err)
	}
	if start.Model != "test-model" || !start.Ephemeral || start.ApprovalPolicy != "never" || start.Sandbox != "read-only" {
		t.Fatalf("thread/start = %+v", start)
	}
	var turn struct {
		ThreadID string `json:"threadId"`
		Input    []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"input"`
	}
	if err := json.Unmarshal(seen["turn/start"], &turn); err != nil {
		t.Fatal(err)
	}
	if turn.ThreadID != "ephemeral-name-thread" || len(turn.Input) != 1 || turn.Input[0].Type != "text" || turn.Input[0].Text == "" {
		t.Fatalf("turn/start = %+v", turn)
	}
	for _, want := range []string{input.CWDBase, input.UserPrompt, input.AssistantResponse} {
		if !strings.Contains(turn.Input[0].Text, want) {
			t.Errorf("naming prompt omitted %q: %q", want, turn.Input[0].Text)
		}
	}
}

func TestEphemeralNamerHonorsCancellation(t *testing.T) {
	client, server := net.Pipe()
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	go func() {
		var request rpcEnvelope
		_ = json.NewDecoder(server).Decode(&request)
		<-ctx.Done()
	}()

	_, err := (EphemeralNamer{Connector: oneConnection{connection: client}}).Generate(ctx, NamingContext{
		CWDBase: "switchboard", UserPrompt: "cancel naming", AssistantResponse: "still pending",
	}, "test-model")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Generate error = %v, want deadline exceeded", err)
	}
}
