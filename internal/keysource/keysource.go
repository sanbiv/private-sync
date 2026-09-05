// Package keysource obtains the vault passphrase (spec §11).
package keysource

import (
	"context"
	"errors"
	"os"

	"github.com/sanbiv/private-sync/internal/config"
	"github.com/sanbiv/private-sync/internal/execx"
	"github.com/sanbiv/private-sync/internal/ui"
)

// EnvVar is the environment variable that overrides every source.
const EnvVar = "PRIVATE_SYNC_PASSPHRASE"

// Source produces a passphrase.
type Source interface {
	Name() string
	Passphrase(ctx context.Context, p ui.Prompter) ([]byte, error)
}

// ErrKeyFilePerms is returned when the key file is readable by others / not owned.
var ErrKeyFilePerms = errors.New("insecure key file permissions")

// envPassphrase is captured once by CaptureEnv.
var envPassphrase []byte

// CaptureEnv reads EnvVar once into memory and unsets it so children never see it.
// Call at process start.
func CaptureEnv() {
	if v, ok := os.LookupEnv(EnvVar); ok {
		envPassphrase = []byte(v)
		_ = os.Unsetenv(EnvVar)
	}
}

// FromConfig builds the configured source.
func FromConfig(cfg config.KeyConfig, r execx.Runner) (Source, error) {
	return nil, errors.New("keysource.FromConfig: not implemented")
}

// Obtain returns the passphrase: captured env first, then the configured source.
func Obtain(ctx context.Context, cfg config.KeyConfig, p ui.Prompter, r execx.Runner) ([]byte, error) {
	if len(envPassphrase) > 0 {
		out := make([]byte, len(envPassphrase))
		copy(out, envPassphrase)
		return out, nil
	}
	src, err := FromConfig(cfg, r)
	if err != nil {
		return nil, err
	}
	return src.Passphrase(ctx, p)
}

// WriteKeyFile creates a new passphrase file (O_EXCL, 0600) with a random passphrase.
func WriteKeyFile(path string) error { return errors.New("keysource.WriteKeyFile: not implemented") }
