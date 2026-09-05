package cli

import (
	"context"
	"errors"
	"os"
	"sync/atomic"
	"testing"
	"time"
)

// TestInterruptContextReleasesHandlerOnFirstSignal: the first signal cancels
// the context and removes the handler (so a second ctrl+c terminates the
// process even while a prompt is blocked); stop without a signal cancels too.
func TestInterruptContextReleasesHandlerOnFirstSignal(t *testing.T) {
	type fakeSignals struct {
		ch       chan<- os.Signal
		notified []os.Signal
		stops    atomic.Int32 // written by the handler goroutine, read by the test
	}
	newFake := func() (*fakeSignals, func(chan<- os.Signal, ...os.Signal), func(chan<- os.Signal)) {
		f := &fakeSignals{}
		notify := func(ch chan<- os.Signal, sigs ...os.Signal) { f.ch, f.notified = ch, sigs }
		stop := func(ch chan<- os.Signal) {
			if ch != f.ch {
				t.Errorf("stop called with another channel")
			}
			f.stops.Add(1)
		}
		return f, notify, stop
	}
	wait := func(t *testing.T, ctx context.Context) {
		t.Helper()
		select {
		case <-ctx.Done():
		case <-time.After(10 * time.Second):
			t.Fatal("context not cancelled")
		}
		if !errors.Is(ctx.Err(), context.Canceled) {
			t.Fatalf("err = %v", ctx.Err())
		}
	}

	t.Run("signal", func(t *testing.T) {
		f, notify, stop := newFake()
		ctx, cancel := interruptContextWith(context.Background(), notify, stop, os.Interrupt)
		defer cancel()
		if len(f.notified) != 1 || f.notified[0] != os.Interrupt || f.ch == nil {
			t.Fatalf("notify registered %v on %v", f.notified, f.ch)
		}
		if ctx.Err() != nil {
			t.Fatalf("fresh context err = %v", ctx.Err())
		}
		f.ch <- os.Interrupt
		wait(t, ctx)
		// The goroutine stops the handler before cancelling: visible once Done is closed.
		if n := f.stops.Load(); n != 1 {
			t.Fatalf("stop calls after the signal = %d, want 1", n)
		}
		cancel()
		if n := f.stops.Load(); n != 1 {
			t.Fatalf("stop calls after cancel = %d, want 1 (idempotent)", n)
		}
	})
	t.Run("stop without a signal", func(t *testing.T) {
		f, notify, stop := newFake()
		ctx, cancel := interruptContextWith(context.Background(), notify, stop, os.Interrupt)
		cancel()
		wait(t, ctx)
		if n := f.stops.Load(); n != 1 {
			t.Fatalf("stop calls = %d, want 1", n)
		}
		cancel()
		if n := f.stops.Load(); n != 1 {
			t.Fatalf("stop calls after a second cancel = %d, want 1", n)
		}
	})
	t.Run("parent cancellation releases the handler", func(t *testing.T) {
		f, notify, stop := newFake()
		parent, cancelParent := context.WithCancel(context.Background())
		ctx, cancel := interruptContextWith(parent, notify, stop, os.Interrupt)
		defer cancel()
		cancelParent()
		wait(t, ctx)
		deadline := time.Now().Add(5 * time.Second)
		for f.stops.Load() == 0 && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
		if n := f.stops.Load(); n != 1 {
			t.Fatalf("stop calls = %d, want 1", n)
		}
	})
}
