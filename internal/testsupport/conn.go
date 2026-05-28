package testsupport

import (
	"io"
	"net"
	"strings"
	"sync"
	"time"
)

// LineReader returns an io.Reader that yields the given lines, each terminated
// by '\n', then EOF. Use it to drive a line-oriented stream parser (e.g. a
// socket2-style event loop refactored to take an io.Reader) deterministically.
func LineReader(lines ...string) io.Reader {
	if len(lines) == 0 {
		return strings.NewReader("")
	}
	return strings.NewReader(strings.Join(lines, "\n") + "\n")
}

// ScriptedConn is a fake net.Conn for stream-parser tests. Reads drain a
// scripted byte sequence; once drained, Read blocks until Close (then returns
// io.EOF), modelling a socket that pushed some lines and then stayed open until
// the peer hung up. Writes are captured and readable via Written.
type ScriptedConn struct {
	mu      sync.Mutex
	script  []byte
	written []byte
	closed  chan struct{}
}

// NewScriptedConn returns a ScriptedConn that will serve script on Read. Joining
// helper: pass LineReader-style input by building the string with "\n"s.
func NewScriptedConn(script string) *ScriptedConn {
	return &ScriptedConn{script: []byte(script), closed: make(chan struct{})}
}

// ScriptedLines builds a ScriptedConn whose script is the lines joined by '\n'
// with a trailing newline — the common case for line-oriented parsers.
func ScriptedLines(lines ...string) *ScriptedConn {
	return NewScriptedConn(strings.Join(lines, "\n") + "\n")
}

func (c *ScriptedConn) Read(p []byte) (int, error) {
	c.mu.Lock()
	if len(c.script) > 0 {
		n := copy(p, c.script)
		c.script = c.script[n:]
		c.mu.Unlock()
		return n, nil
	}
	c.mu.Unlock()
	// Script drained: behave like an open socket awaiting more data, until the
	// peer (the test, or a ctx-cancel goroutine) closes us.
	<-c.closed
	return 0, io.EOF
}

func (c *ScriptedConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.written = append(c.written, p...)
	return len(p), nil
}

// Written returns everything written to the conn so far.
func (c *ScriptedConn) Written() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return string(c.written)
}

func (c *ScriptedConn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	select {
	case <-c.closed:
	default:
		close(c.closed)
	}
	return nil
}

func (c *ScriptedConn) LocalAddr() net.Addr              { return fakeAddr{} }
func (c *ScriptedConn) RemoteAddr() net.Addr             { return fakeAddr{} }
func (c *ScriptedConn) SetDeadline(time.Time) error      { return nil }
func (c *ScriptedConn) SetReadDeadline(time.Time) error  { return nil }
func (c *ScriptedConn) SetWriteDeadline(time.Time) error { return nil }

type fakeAddr struct{}

func (fakeAddr) Network() string { return "scripted" }
func (fakeAddr) String() string  { return "scripted" }

// compile-time assertion that ScriptedConn satisfies net.Conn.
var _ net.Conn = (*ScriptedConn)(nil)
