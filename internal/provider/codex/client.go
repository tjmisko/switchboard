package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
)

var (
	ErrDisconnected     = errors.New("codex app-server connection lost")
	ErrMethodNotAllowed = errors.New("codex app-server method is not read-only")
)

var allowedRequests = map[string]struct{}{
	"initialize":         {},
	"account/read":       {},
	"account/usage/read": {},
	"thread/read":        {},
	"thread/list":        {},
	"thread/turns/list":  {},
}

var displayNameRequests = map[string]struct{}{
	"initialize":   {},
	"thread/start": {},
	"turn/start":   {},
}

var allowedNotifications = map[string]struct{}{
	"initialized": {},
}

// Connection is one stdio-compatible app-server connection.
type Connection interface {
	io.Reader
	io.Writer
	io.Closer
}

// Connector creates a connection to a disposable standalone app-server.
// Production uses CommandConnector; tests should inject a fake and must not
// invoke Codex.
type Connector interface {
	Connect(context.Context) (Connection, error)
}

type rpcEnvelope struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code int `json:"code"`
}

type rpcResponse struct {
	result json.RawMessage
	err    error
}

type rpcNotification struct {
	Generation uint64
	ID         json.RawMessage
	Method     string
	Params     json.RawMessage
}

// rpcClient has one reader goroutine. Every response waiter belongs to this
// immutable connection generation; closing the client releases all waiters.
type rpcClient struct {
	conn       Connection
	generation uint64
	enc        *json.Encoder

	writeMu sync.Mutex
	mu      sync.Mutex
	nextID  uint64
	waiters map[uint64]chan rpcResponse
	closed  bool
	done    chan error
	onNote  func(rpcNotification)
	close   sync.Once
	allowed map[string]struct{}
}

func newRPCClient(conn Connection, generation uint64, onNote func(rpcNotification)) *rpcClient {
	return newRPCClientWithRequests(conn, generation, onNote, allowedRequests)
}

func newRPCClientWithRequests(conn Connection, generation uint64, onNote func(rpcNotification), allowed map[string]struct{}) *rpcClient {
	c := &rpcClient{
		conn: conn, generation: generation, enc: json.NewEncoder(conn),
		waiters: make(map[uint64]chan rpcResponse), done: make(chan error, 1), onNote: onNote, allowed: allowed,
	}
	go c.readLoop()
	return c
}

func (c *rpcClient) Done() <-chan error { return c.done }

func (c *rpcClient) request(ctx context.Context, method string, params any, out any) error {
	if _, ok := c.allowed[method]; !ok {
		return fmt.Errorf("%w: %s", ErrMethodNotAllowed, method)
	}

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return ErrDisconnected
	}
	c.nextID++
	id := c.nextID
	wait := make(chan rpcResponse, 1)
	c.waiters[id] = wait
	c.mu.Unlock()

	c.writeMu.Lock()
	err := c.enc.Encode(struct {
		ID     uint64 `json:"id"`
		Method string `json:"method"`
		Params any    `json:"params,omitempty"`
	}{ID: id, Method: method, Params: params})
	c.writeMu.Unlock()
	if err != nil {
		c.removeWaiter(id)
		c.fail(err)
		_ = c.conn.Close()
		return fmt.Errorf("codex app-server request write: %w", err)
	}

	select {
	case <-ctx.Done():
		c.removeWaiter(id)
		return ctx.Err()
	case response := <-wait:
		if response.err != nil {
			return response.err
		}
		if out == nil || len(response.result) == 0 {
			return nil
		}
		if err := json.Unmarshal(response.result, out); err != nil {
			return fmt.Errorf("codex app-server response decode: %w", err)
		}
		return nil
	}
}

func (c *rpcClient) notify(method string, params any) error {
	if _, ok := allowedNotifications[method]; !ok {
		return fmt.Errorf("%w: %s", ErrMethodNotAllowed, method)
	}
	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()
	if closed {
		return ErrDisconnected
	}
	c.writeMu.Lock()
	err := c.enc.Encode(struct {
		Method string `json:"method"`
		Params any    `json:"params,omitempty"`
	}{Method: method, Params: params})
	c.writeMu.Unlock()
	if err != nil {
		c.fail(err)
		_ = c.conn.Close()
	}
	return err
}

func (c *rpcClient) removeWaiter(id uint64) {
	c.mu.Lock()
	delete(c.waiters, id)
	c.mu.Unlock()
}

func (c *rpcClient) readLoop() {
	decoder := json.NewDecoder(c.conn)
	for {
		var envelope rpcEnvelope
		if err := decoder.Decode(&envelope); err != nil {
			if !errors.Is(err, io.EOF) {
				err = fmt.Errorf("codex app-server read: %w", err)
			} else {
				err = ErrDisconnected
			}
			c.fail(err)
			return
		}
		if envelope.Method != "" {
			if c.onNote != nil {
				c.onNote(rpcNotification{
					Generation: c.generation,
					ID:         append(json.RawMessage(nil), envelope.ID...),
					Method:     envelope.Method,
					Params:     append(json.RawMessage(nil), envelope.Params...),
				})
			}
			continue
		}
		var id uint64
		if err := json.Unmarshal(envelope.ID, &id); err != nil || id == 0 {
			continue
		}
		c.mu.Lock()
		wait := c.waiters[id]
		delete(c.waiters, id)
		c.mu.Unlock()
		if wait == nil {
			continue
		}
		if envelope.Error != nil {
			// Server error messages can contain request-derived detail. Retain the
			// protocol code but never propagate raw server text into logs.
			wait <- rpcResponse{err: fmt.Errorf("codex app-server rpc error %d", envelope.Error.Code)}
		} else {
			wait <- rpcResponse{result: append(json.RawMessage(nil), envelope.Result...)}
		}
	}
}

func (c *rpcClient) fail(err error) {
	c.close.Do(func() {
		c.mu.Lock()
		c.closed = true
		waiters := c.waiters
		c.waiters = make(map[uint64]chan rpcResponse)
		c.mu.Unlock()
		for _, wait := range waiters {
			wait <- rpcResponse{err: err}
		}
		select {
		case c.done <- err:
		default:
		}
	})
}

func (c *rpcClient) Close() error {
	err := c.conn.Close()
	c.fail(ErrDisconnected)
	return err
}
