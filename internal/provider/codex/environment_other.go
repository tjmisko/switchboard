//go:build !linux

package codex

import (
	"context"
	"errors"

	"github.com/tjmisko/switchboard/internal/provider"
)

type unsupportedEnvironmentReader struct{}

func defaultEnvironmentReader() EnvironmentReader { return unsupportedEnvironmentReader{} }

func (unsupportedEnvironmentReader) Environ(context.Context, provider.RootKey) ([]byte, error) {
	return nil, errors.New("codex: process environment binding unsupported on this platform")
}
