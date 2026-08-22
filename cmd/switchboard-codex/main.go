// Command switchboard-codex launches one visible Codex TUI with its own
// app-server endpoint and stable Switchboard slot identity.
package main

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/tjmisko/switchboard/internal/rpc"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		log.Fatal(err)
	}
}

func run(tuiArgs []string) error {
	runtimeDir := os.Getenv("XDG_RUNTIME_DIR")
	if runtimeDir == "" || !filepath.IsAbs(runtimeDir) {
		return errors.New("XDG_RUNTIME_DIR must be an absolute path")
	}
	slotID, err := randomUUID()
	if err != nil {
		return fmt.Errorf("create slot id: %w", err)
	}
	slotDir := filepath.Join(runtimeDir, "switchboard", "codex", slotID)
	if err := os.MkdirAll(slotDir, 0o700); err != nil {
		return fmt.Errorf("create slot runtime directory: %w", err)
	}
	socketPath := filepath.Join(slotDir, "app-server.sock")
	endpoint := "unix://" + socketPath
	defer func() {
		_ = os.Remove(socketPath)
		_ = os.Remove(slotDir)
	}()

	binary := os.Getenv("SWITCHBOARD_CODEX_BIN")
	if binary == "" {
		binary = "codex"
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer cancel()

	server := exec.Command(binary, "app-server", "--listen", endpoint)
	server.Stderr = os.Stderr
	server.Env = os.Environ()
	if err := server.Start(); err != nil {
		return fmt.Errorf("start Codex app-server: %w", err)
	}
	serverDone := make(chan error, 1)
	go func() { serverDone <- server.Wait() }()
	if err := waitForSocket(ctx, socketPath, serverDone); err != nil {
		requestStop(server)
		return err
	}

	args := append([]string{"--remote", endpoint}, tuiArgs...)
	tui := exec.Command(binary, args...)
	tui.Stdin, tui.Stdout, tui.Stderr = os.Stdin, os.Stdout, os.Stderr
	tui.Env = replaceEnv(os.Environ(), map[string]string{
		"SWITCHBOARD_SLOT_ID":        slotID,
		"SWITCHBOARD_CODEX_ENDPOINT": endpoint,
	})
	startedAt := time.Now().UTC()
	if err := tui.Start(); err != nil {
		requestStop(server)
		waitForExit(server, serverDone)
		return fmt.Errorf("start Codex TUI: %w", err)
	}
	tuiDone := make(chan error, 1)
	go func() { tuiDone <- tui.Wait() }()

	registrationCtx, stopRegistration := context.WithCancel(ctx)
	registered := make(chan bool, 1)
	go func() {
		registered <- registerUntilReady(registrationCtx, rpc.Request{
			Cmd: "codex_slot_register", SlotID: slotID, Endpoint: endpoint,
			PID: tui.Process.Pid, StartedAt: startedAt,
		})
	}()

	var result error
	select {
	case err := <-tuiDone:
		result = normalizeExit(err)
		stopRegistration()
		requestStop(server)
		waitForExit(server, serverDone)
	case err := <-serverDone:
		stopRegistration()
		requestStop(tui)
		waitForExit(tui, tuiDone)
		if err != nil {
			result = fmt.Errorf("Codex app-server exited: %w", err)
		} else {
			result = errors.New("Codex app-server exited")
		}
	case <-ctx.Done():
		stopRegistration()
		requestStop(tui)
		requestStop(server)
		waitForExit(tui, tuiDone)
		waitForExit(server, serverDone)
	}
	wasRegistered := <-registered
	if wasRegistered {
		_ = sendControl(rpc.Request{Cmd: "codex_slot_unregister", SlotID: slotID})
	}
	return result
}

func waitForSocket(ctx context.Context, path string, serverDone <-chan error) error {
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-serverDone:
			return fmt.Errorf("Codex app-server exited before readiness: %w", err)
		case <-timer.C:
			return errors.New("timed out waiting for Codex app-server socket")
		case <-ticker.C:
			if info, err := os.Stat(path); err == nil && info.Mode()&os.ModeSocket != 0 {
				return nil
			}
		}
	}
}

func registerUntilReady(ctx context.Context, request rpc.Request) bool {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		if sendControl(request) == nil {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
		}
	}
}

func sendControl(request rpc.Request) error {
	client, err := rpc.Dial(defaultSocketPath())
	if err != nil {
		return err
	}
	defer client.Close()
	if err := client.SetDeadline(time.Now().Add(time.Second)); err != nil {
		return err
	}
	if err := client.Send(request); err != nil {
		return err
	}
	var response rpc.Response
	if err := client.Recv(&response); err != nil {
		return err
	}
	if response.Error != "" {
		return errors.New(response.Error)
	}
	return nil
}

func requestStop(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Signal(os.Interrupt)
}

func waitForExit(cmd *exec.Cmd, done <-chan error) {
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		if cmd != nil && cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		select {
		case <-done:
		case <-time.After(time.Second):
		}
	}
}

func normalizeExit(err error) error {
	var exitErr *exec.ExitError
	if err == nil || errors.As(err, &exitErr) {
		return err
	}
	return err
}

func replaceEnv(environment []string, values map[string]string) []string {
	out := make([]string, 0, len(environment)+len(values))
	for _, entry := range environment {
		key, _, _ := strings.Cut(entry, "=")
		if _, replaced := values[key]; !replaced {
			out = append(out, entry)
		}
	}
	for _, key := range []string{"SWITCHBOARD_SLOT_ID", "SWITCHBOARD_CODEX_ENDPOINT"} {
		if value, ok := values[key]; ok {
			out = append(out, key+"="+value)
		}
	}
	return out
}

func randomUUID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	value[6] = (value[6] & 0x0f) | 0x40
	value[8] = (value[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		value[0:4], value[4:6], value[6:8], value[8:10], value[10:16]), nil
}

func defaultSocketPath() string {
	if runtimeDir := os.Getenv("XDG_RUNTIME_DIR"); runtimeDir != "" {
		return filepath.Join(runtimeDir, "switchboard.sock")
	}
	return filepath.Join(os.TempDir(), fmt.Sprintf("switchboard-%d.sock", os.Getuid()))
}
