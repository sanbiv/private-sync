package tui

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/sanbiv/private-sync/internal/sync"
)

func TestSyncStageTerminal(t *testing.T) {
	tests := []struct {
		s    syncStage
		want bool
	}{
		{stageFetch, false},
		{stagePlan, false},
		{stageResolve, false},
		{stageApply, false},
		{stagePush, false},
		{stageDone, true},
		{stageAborted, true},
		{stageError, true},
	}
	for _, tt := range tests {
		if got := tt.s.terminal(); got != tt.want {
			t.Errorf("stage %v terminal() = %v, want %v", tt.s, got, tt.want)
		}
	}
}

func TestFormatSyncEvent(t *testing.T) {
	ev := sync.Event{Stage: "apply", Project: "proj1", Path: "a.env", Message: "uploading", Done: 2, Total: 5}
	line := formatSyncEvent(ev)
	for _, want := range []string{"[apply]", "proj1", "a.env", "uploading", "2/5"} {
		if !strings.Contains(line, want) {
			t.Errorf("formatSyncEvent(%+v) = %q, want it to contain %q", ev, line, want)
		}
	}

	errEv := sync.Event{Stage: "fetch", Message: "fetch failed", Err: errors.New("boom")}
	line = formatSyncEvent(errEv)
	if !strings.Contains(line, "boom") {
		t.Errorf("formatSyncEvent(%+v) = %q, want it to contain the error", errEv, line)
	}
}

func TestReportSummary(t *testing.T) {
	r := &sync.Report{Uploaded: 1, Downloaded: 2, Pending: 1, Unresolved: []sync.ItemKey{{Project: "p", Path: "x"}}}
	s := reportSummary(r)
	for _, want := range []string{"uploaded 1", "downloaded 2", "pending 1", "unresolved 1"} {
		if !strings.Contains(s, want) {
			t.Errorf("reportSummary() = %q, want it to contain %q", s, want)
		}
	}
	if strings.Contains(s, "errors") {
		t.Errorf("reportSummary() = %q, want no errors segment when there are none", s)
	}
}

// --- Update-driven stage transitions (spec §2.2 item 5, §9) -----------------

func newTestSyncView() *syncView {
	return newSyncView(context.Background(), nil, sync.Options{}, stageFetch, true, true)
}

func TestSyncViewPlanWithConflictsGoesToResolve(t *testing.T) {
	m := newTestSyncView()
	m.stage = stagePlan
	plan := planWith(textConflictItem())
	next, _ := m.handleDone(opDone{Tag: "plan", Plan: plan})
	if next.stage != stageResolve {
		t.Fatalf("stage = %v, want stageResolve", next.stage)
	}
	if next.resolver == nil {
		t.Fatalf("expected a resolver to be created")
	}
	if next.resolver.Done() {
		t.Fatalf("the resolver should not already be done: the plan has an unresolved conflict")
	}
}

func TestSyncViewPlanWithoutConflictsGoesToApply(t *testing.T) {
	m := newTestSyncView()
	m.stage = stagePlan
	plan := planWith(sync.Item{Action: sync.ActionInSync})
	next, cmd := m.handleDone(opDone{Tag: "plan", Plan: plan})
	if next.stage != stageApply {
		t.Fatalf("stage = %v, want stageApply", next.stage)
	}
	if cmd == nil {
		t.Fatalf("expected the apply stage's command")
	}
}

func TestSyncViewPlanOnlyStopsAfterPlan(t *testing.T) {
	m := newSyncView(context.Background(), nil, sync.Options{}, stagePlan, false, false)
	m.stage = stagePlan
	next, _ := m.handleDone(opDone{Tag: "plan", Plan: planWith(sync.Item{Action: sync.ActionInSync})})
	if next.stage != stageDone {
		t.Fatalf("stage = %v, want stageDone (doApply=false)", next.stage)
	}
}

func TestSyncViewFetchCancelledSetsAbortedAndCancelled(t *testing.T) {
	m := newTestSyncView()
	next, _ := m.Update(opDoneMsg{Tag: "fetch", Err: context.Canceled})
	if next.stage != stageAborted {
		t.Fatalf("stage = %v, want stageAborted", next.stage)
	}
	if !next.cancelled {
		t.Fatalf("cancelled should be true for a context.Canceled error")
	}
}

func TestSyncViewFetchErrorSetsStageError(t *testing.T) {
	m := newTestSyncView()
	next, _ := m.Update(opDoneMsg{Tag: "fetch", Err: errors.New("boom")})
	if next.stage != stageError {
		t.Fatalf("stage = %v, want stageError", next.stage)
	}
	if next.cancelled {
		t.Fatalf("a real error should not be reported as cancelled")
	}
}

func TestSyncViewApplyThenPushThenDone(t *testing.T) {
	m := newTestSyncView() // doApply=true doPush=true
	m.stage = stageApply
	next, cmd := m.handleDone(opDone{Tag: "apply", Report: &sync.Report{Uploaded: 1}})
	if next.stage != stagePush {
		t.Fatalf("stage = %v, want stagePush", next.stage)
	}
	if cmd == nil {
		t.Fatalf("expected the push stage's command")
	}
	next, _ = next.handleDone(opDone{Tag: "push"})
	if next.stage != stageDone {
		t.Fatalf("stage = %v, want stageDone", next.stage)
	}
}

func TestSyncViewEscCancelsRunningStage(t *testing.T) {
	m := newTestSyncView()
	cancelled := false
	orig := m.cancel
	m.cancel = func() { cancelled = true; orig() }
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if !next.cancelled {
		t.Fatalf("esc should mark the view cancelled")
	}
	if !cancelled {
		t.Fatalf("esc should call the context's CancelFunc")
	}
}

func TestSyncViewEscOnTerminalStageDoesNotMarkCancelled(t *testing.T) {
	m := newTestSyncView()
	m.stage = stageDone
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if next.cancelled {
		t.Fatalf("esc on a terminal stage should not mark cancelled")
	}
}

// TestSyncViewCancelsContextOnTerminalCompletion is the regression for the
// syncView context leak: every path that reaches a terminal stage on its own
// (not via esc) must still cancel the child context newSyncView derived, or
// it (and its parent-context registration) leaks for the program's life.
func TestSyncViewCancelsContextOnTerminalCompletion(t *testing.T) {
	m := newTestSyncView()
	cancelled := false
	orig := m.cancel
	m.cancel = func() { cancelled = true; orig() }
	m.stage = stagePush
	m.handleDone(opDone{Tag: "push"})
	if !cancelled {
		t.Fatalf("reaching stageDone should cancel the syncView's context")
	}
}

func TestSyncViewCancelsContextOnError(t *testing.T) {
	m := newTestSyncView()
	cancelled := false
	orig := m.cancel
	m.cancel = func() { cancelled = true; orig() }
	m.handleDone(opDone{Tag: "fetch", Err: errors.New("boom")})
	if !cancelled {
		t.Fatalf("reaching stageError should cancel the syncView's context")
	}
}

func TestSyncViewResolverAbortCancelsContext(t *testing.T) {
	it := textConflictItem()
	m := newTestSyncView()
	m.stage = stageResolve
	m.plan = planWith(it)
	m.resolver = NewResolver(m.plan, m.res)
	cancelled := false
	orig := m.cancel
	m.cancel = func() { cancelled = true; orig() }

	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("A")})
	if next.stage != stageAborted {
		t.Fatalf("stage = %v, want stageAborted", next.stage)
	}
	if !cancelled {
		t.Fatalf("aborting the resolver should cancel the syncView's context")
	}
}

// TestSyncViewResolverResolveGoesToApply is the counterpart to
// TestSyncViewResolverAbortCancelsContext: only the abort path used to be
// exercised, leaving the ordinary "answer the last conflict" hand-off from
// stageResolve to stageApply untested.
func TestSyncViewResolverResolveGoesToApply(t *testing.T) {
	it := textConflictItem()
	m := newTestSyncView()
	m.stage = stageResolve
	m.plan = planWith(it)
	m.resolver = NewResolver(m.plan, m.res)

	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("l")})
	if next.stage != stageApply {
		t.Fatalf("stage = %v, want stageApply", next.stage)
	}
	if cmd == nil {
		t.Fatalf("expected the apply stage's command once the resolver hands off")
	}
	if _, ok := next.res[it.Key]; !ok {
		t.Fatalf("resolving the item should have recorded a resolution before handing off")
	}
}

// TestSyncViewSpinnerTickSurvivesResolveStage is the regression for the
// spinner's tick chain dying at the conflict resolver: stageResolve routed
// every message (spinner.TickMsg included) to the Resolver, which drops it,
// and nothing ever re-armed the chain — so the apply and push spinner stayed
// frozen on one frame for the whole write-heavy part of the run.
func TestSyncViewSpinnerTickSurvivesResolveStage(t *testing.T) {
	m := newTestSyncView()
	m.plan = planWith(textConflictItem())
	m.stage = stageResolve
	m.resolver = NewResolver(m.plan, m.res)

	frameBefore := m.spin.View()
	next, cmd := m.Update(spinner.TickMsg{})
	m = next
	if cmd == nil {
		t.Fatalf("a spinner tick during stageResolve must re-arm the tick chain")
	}
	if m.spin.View() == frameBefore {
		t.Fatalf("the spinner should have advanced a frame during stageResolve")
	}
	if m.stage != stageResolve || m.resolver == nil {
		t.Fatalf("handling the tick must not disturb the resolve stage (stage=%v)", m.stage)
	}

	// The chain is still alive once the resolver hands off to apply.
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'l'}})
	m = next
	if m.stage != stageApply {
		t.Fatalf("stage = %v, want stageApply after the last conflict is answered", m.stage)
	}
	if _, cmd := m.Update(spinner.TickMsg{}); cmd == nil {
		t.Fatalf("the spinner must keep ticking during stageApply")
	}
}
