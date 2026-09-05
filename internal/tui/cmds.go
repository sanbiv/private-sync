package tui

import (
	"context"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sanbiv/private-sync/internal/execx"
	"github.com/sanbiv/private-sync/internal/scan"
	"github.com/sanbiv/private-sync/internal/sync"
)

// This file holds the generic streaming machinery used to run the engine's
// long operations (Fetch/Plan/Apply/Push) and the scanner as Bubble Tea
// commands while forwarding their progress into the UI as discrete messages.
//
// The pattern: a "start" tea.Cmd launches the operation in a goroutine that
// writes progress into a buffered channel and, once finished (after closing
// that channel), writes a single result onto a done channel. The start
// command immediately delegates to a "wait" command that blocks (inside the
// goroutine tea already runs commands in) until either the next progress
// value or the final result is available, and the model re-issues the wait
// command after every progress message until the result arrives. This keeps
// events flowing into the view incrementally without ever running a
// tea.Program-driving goroutine of our own.

// opDone is the terminal outcome of one engine operation.
type opDone struct {
	Tag string
	// Gen distinguishes successive runs of the same stage in a model that
	// reuses one long-lived step across esc-and-retry (only stepFetch's
	// addModel usage sets this to anything but the zero value — see
	// startFetchCmd): a run cancelled by esc still delivers its final
	// message once its goroutine unwinds, and without this the stale
	// message could be applied to a fresh run of the same stage that
	// started in the meantime.
	Gen    int
	Err    error
	Plan   *sync.Plan
	Report *sync.Report
}

// opDoneMsg carries the terminal outcome of an engine operation into Update.
type opDoneMsg opDone

// opEventMsg carries one sync.Event plus the channels needed to keep listening.
type opEventMsg struct {
	Tag   string
	Gen   int
	Event sync.Event
	ch    <-chan sync.Event
	done  <-chan opDone
}

// waitOpCmd blocks for the next event or the final result of a running
// operation. The producer always closes ch only after every event has been
// sent, and only then sends on done, so reading ch until it is closed and
// only then reading done drains every event before the result is observed
// (a ready-select over both channels would let Go's random case choice drop
// a buffered event that raced the done send).
func waitOpCmd(tag string, ch <-chan sync.Event, done <-chan opDone, gen int) tea.Cmd {
	return func() tea.Msg {
		if ev, ok := <-ch; ok {
			return opEventMsg{Tag: tag, Gen: gen, Event: ev, ch: ch, done: done}
		}
		return opDoneMsg(<-done)
	}
}

// continueOpCmd resumes listening after an opEventMsg was handled.
func continueOpCmd(msg opEventMsg) tea.Cmd {
	return waitOpCmd(msg.Tag, msg.ch, msg.done, msg.Gen)
}

// eventSender returns a sync.Event progress callback that forwards into ch,
// giving up silently once ctx is cancelled so the producer goroutine never
// blocks forever on a channel nobody drains any more.
func eventSender(ctx context.Context, ch chan<- sync.Event) func(sync.Event) {
	return func(ev sync.Event) {
		select {
		case ch <- ev:
		case <-ctx.Done():
		}
	}
}

// startFetchCmd runs Engine.Fetch, streaming its log lines as sync.Events.
// gen is echoed back on every message so a caller that re-runs this stage in
// place (addModel's stepFetch) can tell a stale, already-cancelled run's
// message from the current one; callers with no such reentrancy (syncView)
// pass 0.
func startFetchCmd(ctx context.Context, e *sync.Engine, gen int) tea.Cmd {
	return func() tea.Msg {
		ch := make(chan sync.Event, 64)
		done := make(chan opDone, 1)
		send := eventSender(ctx, ch)
		go func() {
			err := e.Fetch(ctx, send)
			close(ch)
			done <- opDone{Tag: "fetch", Gen: gen, Err: err}
		}()
		return waitOpCmd("fetch", ch, done, gen)()
	}
}

// startPushCmd runs Engine.Push, streaming its log lines as sync.Events.
func startPushCmd(ctx context.Context, e *sync.Engine) tea.Cmd {
	return func() tea.Msg {
		ch := make(chan sync.Event, 64)
		done := make(chan opDone, 1)
		send := eventSender(ctx, ch)
		go func() {
			err := e.Push(ctx, send)
			close(ch)
			done <- opDone{Tag: "push", Err: err}
		}()
		return waitOpCmd("push", ch, done, 0)()
	}
}

// startPlanCmd runs Engine.Plan, streaming its per-project progress.
func startPlanCmd(ctx context.Context, e *sync.Engine, opts sync.Options) tea.Cmd {
	return func() tea.Msg {
		ch := make(chan sync.Event, 64)
		done := make(chan opDone, 1)
		opts.Progress = eventSender(ctx, ch)
		go func() {
			plan, err := e.Plan(ctx, opts)
			close(ch)
			done <- opDone{Tag: "plan", Err: err, Plan: plan}
		}()
		return waitOpCmd("plan", ch, done, 0)()
	}
}

// startApplyCmd runs Engine.Apply, streaming its per-item progress.
func startApplyCmd(ctx context.Context, e *sync.Engine, p *sync.Plan, res sync.Resolutions, opts sync.Options) tea.Cmd {
	return func() tea.Msg {
		ch := make(chan sync.Event, 64)
		done := make(chan opDone, 1)
		opts.Progress = eventSender(ctx, ch)
		go func() {
			report, err := e.Apply(ctx, p, res, opts)
			close(ch)
			done <- opDone{Tag: "apply", Err: err, Report: report}
		}()
		return waitOpCmd("apply", ch, done, 0)()
	}
}

// scanProgress is one walk/found tick from scan.Options.Progress.
type scanProgress struct{ Walked, Found int }

// scanDone is the terminal outcome of a scan.
type scanDone struct {
	// Gen distinguishes successive scans of the same addModel (esc cancels
	// and the wizard scans again later): see opDone.Gen for why this
	// matters.
	Gen    int
	Result *scan.Result
	Err    error
}

// scanDoneMsg carries the terminal outcome of a scan into Update.
type scanDoneMsg scanDone

// scanEventMsg carries one progress tick plus the channels needed to keep listening.
type scanEventMsg struct {
	Gen           int
	Walked, Found int
	ch            <-chan scanProgress
	done          <-chan scanDone
}

func waitScanCmd(ch <-chan scanProgress, done <-chan scanDone, gen int) tea.Cmd {
	return func() tea.Msg {
		if p, ok := <-ch; ok {
			return scanEventMsg{Gen: gen, Walked: p.Walked, Found: p.Found, ch: ch, done: done}
		}
		return scanDoneMsg(<-done)
	}
}

func continueScanCmd(msg scanEventMsg) tea.Cmd {
	return waitScanCmd(msg.ch, msg.done, msg.Gen)
}

// startScanCmd runs scan.Scan in the background, streaming its walk
// progress. gen is echoed back on every message (see opDone.Gen) so a scan
// cancelled by esc, whose goroutine still delivers a final message once it
// unwinds, can be told apart from whatever scan is running by the time that
// message arrives.
func startScanCmd(ctx context.Context, dir string, opts scan.Options, r execx.Runner, gen int) tea.Cmd {
	return func() tea.Msg {
		ch := make(chan scanProgress, 64)
		done := make(chan scanDone, 1)
		opts.Progress = func(walked, found int) {
			select {
			case ch <- scanProgress{Walked: walked, Found: found}:
			case <-ctx.Done():
			}
		}
		go func() {
			res, err := scan.Scan(ctx, dir, opts, r)
			close(ch)
			done <- scanDone{Gen: gen, Result: res, Err: err}
		}()
		return waitScanCmd(ch, done, gen)()
	}
}
