package codex

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"reflect"
	"testing"
	"time"
)

func TestRPCMethodAllowlistRejectsEveryMutation(t *testing.T) {
	clientSide, serverSide := net.Pipe()
	defer serverSide.Close()
	client := newRPCClient(clientSide, 1, nil)
	defer client.Close()
	for _, method := range []string{"initialize", "account/read", "account/usage/read", "thread/read", "thread/list", "thread/turns/list"} {
		if _, allowed := allowedRequests[method]; !allowed {
			t.Errorf("read-only method %q is not allowlisted", method)
		}
	}

	mutations := []string{
		"thread/start", "thread/resume", "thread/fork", "thread/archive", "thread/delete",
		"turn/start", "turn/steer", "turn/interrupt", "item/commandExecution/requestApproval",
		"collab/spawn", "collab/sendInput", "collab/wait", "collab/closeAgent",
	}
	for _, method := range mutations {
		if err := client.request(context.Background(), method, map[string]any{}, nil); !errors.Is(err, ErrMethodNotAllowed) {
			t.Errorf("request(%q) error = %v", method, err)
		}
	}
	for _, method := range []string{"thread/start", "turn/start", "item/tool/requestUserInput"} {
		if err := client.notify(method, map[string]any{}); !errors.Is(err, ErrMethodNotAllowed) {
			t.Errorf("notify(%q) error = %v", method, err)
		}
	}

	serverSide.SetReadDeadline(time.Now().Add(20 * time.Millisecond))
	var envelope map[string]any
	if err := json.NewDecoder(serverSide).Decode(&envelope); err == nil {
		t.Fatalf("mutation wrote protocol envelope %#v", envelope)
	}
}

func TestRPCGenerationPreservesServerRequestIDAndCloseReleasesWaiter(t *testing.T) {
	clientSide, serverSide := net.Pipe()
	notes := make(chan rpcNotification, 1)
	client := newRPCClient(clientSide, 7, func(note rpcNotification) { notes <- note })

	go func() {
		decoder := json.NewDecoder(serverSide)
		var request map[string]any
		_ = decoder.Decode(&request)
		_, _ = serverSide.Write([]byte(`{"id":"approval-42","method":"item/commandExecution/requestApproval","params":{"threadId":"x","turnId":"t","itemId":"i"}}` + "\n"))
	}()
	errCh := make(chan error, 1)
	go func() {
		errCh <- client.request(context.Background(), "thread/read", map[string]string{"threadId": "x"}, nil)
	}()
	select {
	case note := <-notes:
		if note.Generation != 7 {
			t.Fatalf("notification generation = %d", note.Generation)
		}
		if string(note.ID) != `"approval-42"` {
			t.Fatalf("server-request id = %s, want preserved string id", note.ID)
		}
	case <-time.After(time.Second):
		t.Fatal("notification not delivered")
	}
	_ = client.Close()
	_ = serverSide.Close()
	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("waiter succeeded after generation loss")
		}
	case <-time.After(time.Second):
		t.Fatal("Close did not release response waiter")
	}
}

func TestAppServerVersionCapabilityChecks(t *testing.T) {
	for _, test := range []struct {
		value string
		ok    bool
	}{
		{"codex-cli 0.149.0", true},
		{"codex-cli 0.150.1", true},
		{"codex_app_server/0.149.0", true},
		{"codex-cli 0.148.9", false},
		{"unknown", false},
	} {
		err := checkAppServerCLIVersion(test.value)
		if (err == nil) != test.ok {
			t.Errorf("checkAppServerCLIVersion(%q) = %v", test.value, err)
		}
	}
}

func TestCommandConnectorUsesStandaloneStdioTransport(t *testing.T) {
	command := standaloneAppServerCommand(context.Background(), "codex-test")
	want := []string{"codex-test", "app-server", "--stdio"}
	if !reflect.DeepEqual(command.Args, want) {
		t.Fatalf("command args = %q, want %q", command.Args, want)
	}
}
