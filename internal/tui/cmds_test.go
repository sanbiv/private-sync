package tui

import (
	"testing"

	"github.com/sanbiv/private-sync/internal/scan"
	"github.com/sanbiv/private-sync/internal/sync"
)

// TestWaitOpCmdDrainsAllBufferedEventsBeforeDone is the regression for the
// old select-based waitOpCmd: with ch buffered and closed, and done already
// sent, a select over both channels could pick done first and drop the
// remaining buffered events. Draining ch to closed before ever reading done
// makes the order deterministic.
func TestWaitOpCmdDrainsAllBufferedEventsBeforeDone(t *testing.T) {
	ch := make(chan sync.Event, 4)
	done := make(chan opDone, 1)
	ch <- sync.Event{Message: "one"}
	ch <- sync.Event{Message: "two"}
	close(ch)
	done <- opDone{Tag: "fetch", Gen: 7}

	cmd := waitOpCmd("fetch", ch, done, 7)
	msg := cmd()
	ev1, ok := msg.(opEventMsg)
	if !ok || ev1.Event.Message != "one" {
		t.Fatalf("first message = %#v, want event %q", msg, "one")
	}
	if ev1.Gen != 7 {
		t.Fatalf("first message Gen = %d, want 7", ev1.Gen)
	}

	msg = continueOpCmd(ev1)()
	ev2, ok := msg.(opEventMsg)
	if !ok || ev2.Event.Message != "two" {
		t.Fatalf("second message = %#v, want event %q", msg, "two")
	}

	msg = continueOpCmd(ev2)()
	final, ok := msg.(opDoneMsg)
	if !ok {
		t.Fatalf("third message = %#v, want the final opDoneMsg", msg)
	}
	if final.Tag != "fetch" || final.Gen != 7 {
		t.Fatalf("final opDoneMsg = %+v, want Tag=fetch Gen=7", final)
	}
}

// TestWaitScanCmdDrainsAllBufferedEventsBeforeDone is the scan-side
// counterpart of the above.
func TestWaitScanCmdDrainsAllBufferedEventsBeforeDone(t *testing.T) {
	ch := make(chan scanProgress, 4)
	done := make(chan scanDone, 1)
	ch <- scanProgress{Walked: 1, Found: 0}
	ch <- scanProgress{Walked: 2, Found: 1}
	close(ch)
	done <- scanDone{Gen: 3, Result: &scan.Result{}}

	cmd := waitScanCmd(ch, done, 3)
	msg := cmd()
	ev1, ok := msg.(scanEventMsg)
	if !ok || ev1.Walked != 1 {
		t.Fatalf("first message = %#v, want Walked=1", msg)
	}

	msg = continueScanCmd(ev1)()
	ev2, ok := msg.(scanEventMsg)
	if !ok || ev2.Walked != 2 {
		t.Fatalf("second message = %#v, want Walked=2", msg)
	}

	msg = continueScanCmd(ev2)()
	final, ok := msg.(scanDoneMsg)
	if !ok {
		t.Fatalf("third message = %#v, want the final scanDoneMsg", msg)
	}
	if final.Gen != 3 {
		t.Fatalf("final scanDoneMsg.Gen = %d, want 3", final.Gen)
	}
}

// TestWaitOpCmdDoneOnly covers the (still valid) case where ch is closed
// with nothing ever sent on it.
func TestWaitOpCmdDoneOnly(t *testing.T) {
	ch := make(chan sync.Event)
	done := make(chan opDone, 1)
	close(ch)
	done <- opDone{Tag: "push"}

	msg := waitOpCmd("push", ch, done, 0)()
	final, ok := msg.(opDoneMsg)
	if !ok || final.Tag != "push" {
		t.Fatalf("message = %#v, want the opDoneMsg", msg)
	}
}
