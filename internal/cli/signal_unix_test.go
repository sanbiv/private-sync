//go:build !windows

package cli

import (
	"context"
	"errors"
	"syscall"
	"testing"
	"time"
)

// TestInterruptContextRealSignal sends one SIGUSR1 to the test process: the
// context must be cancelled. (Only one is sent: after it the handler is gone
// and the signal falls back to the runtime's default handling.)
func TestInterruptContextRealSignal(t *testing.T) {
	ctx, stop := interruptContext(context.Background(), syscall.SIGUSR1)
	defer stop()
	if err := syscall.Kill(syscall.Getpid(), syscall.SIGUSR1); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ctx.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("SIGUSR1 did not cancel the context")
	}
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatalf("err = %v", ctx.Err())
	}
}
