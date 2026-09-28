// Package herdr talks to herdr (herdr.dev) servers over their local JSON socket
// API: one-shot requests (Call) for the terminal backend, and a long-lived
// Watcher that follows every pane's agent status through event subscriptions.
//
// herdr serves newline-delimited JSON, one request per connection; a
// subscription keeps its connection open and streams events after an ack.
package herdr

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"time"
)

// Pane is the subset of herdr's PaneInfo Switchboard reads.
type Pane struct {
	PaneID        string `json:"pane_id"`
	TerminalID    string `json:"terminal_id"`
	CWD           string `json:"cwd"`
	ForegroundCWD string `json:"foreground_cwd"`
	TerminalTitle string `json:"terminal_title"`
	// Agent is the agent herdr detected in the pane ("claude", "pi", …); nil
	// (JSON null) when none.
	Agent       *string `json:"agent"`
	AgentStatus string  `json:"agent_status"`
	// Focused is true for the one pane herdr's focus is on: the focused pane
	// of the active tab of the active workspace. At most one per server.
	Focused bool `json:"focused"`
}

// AgentName returns the detected agent's label, or "" when none.
func (p Pane) AgentName() string {
	if p.Agent == nil {
		return ""
	}
	return *p.Agent
}

// Agent status values as herdr reports them. Done is idle that no one has
// looked at yet; Unknown means an agent is present but unclassified, or none.
const (
	StatusWorking = "working"
	StatusBlocked = "blocked"
	StatusDone    = "done"
	StatusIdle    = "idle"
	StatusUnknown = "unknown"
)

// Caller performs one API request against a server socket and decodes its
// result into result (skipped when result is nil). Call is the real one; tests
// substitute fakes.
type Caller func(ctx context.Context, socket, method string, params, result any) error

// CallTimeout bounds one API round trip. herdr answers from memory, so a slow
// reply means a wedged server, and no caller should wait on it.
const CallTimeout = time.Second

// Call sends one request and reads its one-line response.
func Call(ctx context.Context, socket, method string, params, result any) error {
	ctx, cancel := context.WithTimeout(ctx, CallTimeout)
	defer cancel()
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", socket)
	if err != nil {
		return err
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	if err := writeRequest(conn, method, params); err != nil {
		return err
	}
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		return err
	}
	return DecodeResponse(line, method, result)
}

func writeRequest(conn net.Conn, method string, params any) error {
	request, err := json.Marshal(struct {
		ID     string `json:"id"`
		Method string `json:"method"`
		Params any    `json:"params"`
	}{ID: "switchboard", Method: method, Params: params})
	if err != nil {
		return err
	}
	_, err = conn.Write(append(request, '\n'))
	return err
}

// APIError is an error response from a herdr server.
type APIError struct {
	Method  string
	Code    string
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("herdr %s: %s: %s", e.Method, e.Code, e.Message)
}

// DecodeResponse decodes one response line, returning the server's error as an
// *APIError.
func DecodeResponse(line []byte, method string, result any) error {
	var response struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(line, &response); err != nil {
		return fmt.Errorf("herdr %s: decode response: %w", method, err)
	}
	if response.Error != nil {
		return &APIError{Method: method, Code: response.Error.Code, Message: response.Error.Message}
	}
	if response.Result == nil {
		return errors.New("herdr " + method + ": response has no result")
	}
	if result == nil {
		return nil
	}
	return json.Unmarshal(response.Result, result)
}

// ListPanes returns every pane of the server at socket.
func ListPanes(ctx context.Context, call Caller, socket string) ([]Pane, error) {
	var list struct {
		Panes []Pane `json:"panes"`
	}
	if err := call(ctx, socket, "pane.list", struct{}{}, &list); err != nil {
		return nil, err
	}
	return list.Panes, nil
}
