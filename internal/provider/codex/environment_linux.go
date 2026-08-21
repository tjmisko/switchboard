//go:build linux

package codex

import (
	"context"
	"fmt"
	"os"

	"github.com/tjmisko/switchboard/internal/provider"
)

type procEnvironmentReader struct{}

func defaultEnvironmentReader() EnvironmentReader { return procEnvironmentReader{} }

func (procEnvironmentReader) Environ(ctx context.Context, key provider.RootKey) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// RootKey is supplied by discovery and scopes all retained state to the
	// observed process lifetime. The procfs read itself is intentionally small
	// and one-shot; a vanished or inaccessible process is an ordinary miss.
	return os.ReadFile(fmt.Sprintf("/proc/%d/environ", key.PID))
}
