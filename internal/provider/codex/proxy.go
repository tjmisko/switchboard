package codex

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

const minimumProxyVersion = "0.149.0"

// CommandConnector supervises only the disposable read-only proxy child. The
// installed capability was established for Codex 0.149.0, so a version
// preflight rejects older or unparseable CLIs.
type CommandConnector struct {
	Binary string
}

// StdioServerConnector starts an isolated app-server process. It is used only
// for ephemeral naming threads, never for visible TUI observation.
type StdioServerConnector struct{ Binary string }

func (c StdioServerConnector) Connect(ctx context.Context) (Connection, error) {
	binary := c.Binary
	if binary == "" {
		binary = "codex"
	}
	cmd := exec.CommandContext(ctx, binary, "app-server", "--listen", "stdio://")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		return nil, fmt.Errorf("start isolated Codex app-server: %w", err)
	}
	go func() { _, _ = io.Copy(io.Discard, stderr) }()
	return &commandConnection{reader: stdout, writer: stdin, cmd: cmd}, nil
}

func (c CommandConnector) Connect(ctx context.Context) (Connection, error) {
	binary := c.Binary
	if binary == "" {
		binary = "codex"
	}
	versionCmd := exec.CommandContext(ctx, binary, "--version")
	versionOutput, err := versionCmd.Output()
	if err != nil {
		return nil, fmt.Errorf("codex proxy capability check: %w", err)
	}
	if err := checkProxyVersion(string(versionOutput)); err != nil {
		return nil, err
	}

	cmd := exec.CommandContext(ctx, binary, "app-server", "proxy")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		return nil, fmt.Errorf("start codex app-server proxy: %w", err)
	}
	// stderr is never mixed with protocol stdout and raw payloads are never
	// logged. Draining it prevents a noisy proxy from blocking.
	go func() { _, _ = io.Copy(io.Discard, stderr) }()
	return &commandConnection{reader: stdout, writer: stdin, cmd: cmd}, nil
}

var versionPattern = regexp.MustCompile(`(?:^|[\s/])(\d+)\.(\d+)\.(\d+)(?:\s|$)`)

func checkProxyVersion(output string) error {
	match := versionPattern.FindStringSubmatch(strings.TrimSpace(output))
	if len(match) != 4 {
		return errors.New("codex proxy capability check: unrecognized CLI version")
	}
	got := [3]int{}
	for i := range got {
		got[i], _ = strconv.Atoi(match[i+1])
	}
	minimum := [3]int{0, 149, 0}
	for i := range got {
		if got[i] > minimum[i] {
			return nil
		}
		if got[i] < minimum[i] {
			return fmt.Errorf("codex app-server proxy requires CLI >= %s", minimumProxyVersion)
		}
	}
	return nil
}

type commandConnection struct {
	reader io.ReadCloser
	writer io.WriteCloser
	cmd    *exec.Cmd
	once   sync.Once
	err    error
}

func (c *commandConnection) Read(p []byte) (int, error)  { return c.reader.Read(p) }
func (c *commandConnection) Write(p []byte) (int, error) { return c.writer.Write(p) }

func (c *commandConnection) Close() error {
	c.once.Do(func() {
		_ = c.writer.Close()
		if c.cmd.Process != nil {
			_ = c.cmd.Process.Kill()
		}
		c.err = c.cmd.Wait()
		if errors.Is(c.err, context.Canceled) || strings.Contains(fmt.Sprint(c.err), "signal: killed") {
			c.err = nil
		}
	})
	return c.err
}
