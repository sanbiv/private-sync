package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/sanbiv/private-sync/internal/sync"
)

// syncStage is where a syncView is in the Fetch -> Plan -> [resolve] -> Apply
// -> Push pipeline (spec §2.2 item 5, §9).
type syncStage int

const (
	stageFetch syncStage = iota
	stagePlan
	stageResolve
	stageApply
	stagePush
	stageDone
	stageAborted
	stageError
)

// terminal reports whether the pipeline has stopped producing more messages.
func (s syncStage) terminal() bool {
	return s == stageDone || s == stageAborted || s == stageError
}

func (s syncStage) label() string {
	switch s {
	case stageFetch:
		return "fetching"
	case stagePlan:
		return "planning"
	case stageResolve:
		return "resolving conflicts"
	case stageApply:
		return "applying"
	case stagePush:
		return "pushing"
	case stageDone:
		return "done"
	case stageAborted:
		return "aborted"
	case stageError:
		return "error"
	}
	return "?"
}

// syncView drives one run of the sync pipeline as a reusable embedded
// component (dashboard sync/push/pull/refresh, project detail sync/restore,
// the add-project wizard's apply step, and the dashboard's link+pull flow all
// build one of these instead of duplicating the plumbing).
type syncView struct {
	engine  *sync.Engine
	opts    sync.Options
	doApply bool // false = stop after planning (dashboard "r" refresh)
	doPush  bool

	ctx    context.Context
	cancel context.CancelFunc

	stage     syncStage
	log       []string
	vp        viewport.Model
	spin      spinner.Model
	plan      *sync.Plan
	res       sync.Resolutions
	report    *sync.Report
	err       error
	cancelled bool

	resolver *Resolver

	width, height int
}

// newSyncView builds a syncView ready to Init, starting at start (stageFetch,
// stagePlan or stagePush — whichever step the caller has not already done).
// parentCtx is the long-lived program/session context; the view derives its
// own cancellable child so esc can stop just this operation.
func newSyncView(parentCtx context.Context, e *sync.Engine, opts sync.Options, start syncStage, doApply, doPush bool) *syncView {
	if parentCtx == nil {
		parentCtx = context.Background()
	}
	ctx, cancel := context.WithCancel(parentCtx)
	vp := viewport.New(80, 12)
	return &syncView{
		engine:  e,
		opts:    opts,
		doApply: doApply,
		doPush:  doPush,
		ctx:     ctx,
		cancel:  cancel,
		stage:   start,
		spin:    newSpinner(),
		vp:      vp,
		res:     sync.Resolutions{},
	}
}

func newSpinner() spinner.Model {
	sp := spinner.New()
	sp.Spinner = spinner.Dot
	sp.Style = styles.Spinner
	return sp
}

// Init kicks off the first stage.
func (m *syncView) Init() tea.Cmd {
	return tea.Batch(m.spin.Tick, m.stageCmd())
}

// stageCmd returns the command that performs the current stage's work.
func (m *syncView) stageCmd() tea.Cmd {
	switch m.stage {
	case stageFetch:
		m.appendLog("fetching remote...")
		return startFetchCmd(m.ctx, m.engine, 0)
	case stagePlan:
		m.appendLog("planning...")
		return startPlanCmd(m.ctx, m.engine, m.opts)
	case stageApply:
		m.appendLog("applying...")
		return startApplyCmd(m.ctx, m.engine, m.plan, m.res, m.opts)
	case stagePush:
		m.appendLog("pushing...")
		return startPushCmd(m.ctx, m.engine)
	}
	return nil
}

func (m *syncView) appendLog(line string) {
	m.log = append(m.log, line)
	m.vp.SetContent(strings.Join(m.log, "\n"))
	m.vp.GotoBottom()
}

// Cancel stops the running operation (esc). Safe to call more than once, and
// safe once the pipeline is already terminal.
func (m *syncView) Cancel() {
	if m.cancel != nil {
		m.cancel()
	}
	if !m.stage.terminal() {
		m.cancelled = true
	}
}

// SetSize resizes the log viewport.
func (m *syncView) SetSize(width, height int) {
	m.width, m.height = width, height
	logHeight := height - 6
	if logHeight < 3 {
		logHeight = 3
	}
	m.vp.Width = width
	m.vp.Height = logHeight
	if m.resolver != nil {
		m.resolver.SetSize(width, height)
	}
}

// Update advances the pipeline. It never returns tea.Quit: the parent screen
// owns the program's lifetime and decides what to do once the view reaches a
// terminal stage.
func (m *syncView) Update(msg tea.Msg) (*syncView, tea.Cmd) {
	if m.stage == stageResolve && m.resolver != nil {
		next, cmd := m.resolver.Update(msg)
		if r, ok := next.(*Resolver); ok {
			m.resolver = r
		}
		if m.resolver.Done() {
			m.res = m.resolver.Resolutions()
			if m.resolver.Aborted() {
				m.stage = stageAborted
				m.appendLog("aborted: nothing applied")
				m.cancel()
				return m, cmd
			}
			m.stage = stageApply
			return m, tea.Batch(cmd, m.stageCmd())
		}
		return m, cmd
	}

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.SetSize(msg.Width, msg.Height)
		return m, nil
	case spinner.TickMsg:
		if m.stage.terminal() {
			return m, nil
		}
		var cmd tea.Cmd
		m.spin, cmd = m.spin.Update(msg)
		return m, cmd
	case tea.KeyMsg:
		if msg.String() == "esc" && !m.stage.terminal() {
			m.Cancel()
			m.appendLog("cancelling...")
			return m, nil
		}
		var cmd tea.Cmd
		m.vp, cmd = m.vp.Update(msg)
		return m, cmd
	case opEventMsg:
		m.appendLog(formatSyncEvent(msg.Event))
		return m, continueOpCmd(msg)
	case opDoneMsg:
		return m.handleDone(opDone(msg))
	}
	return m, nil
}

// handleDone advances the pipeline once a stage's Cmd delivers its result.
// Every path that lands on a terminal stage calls m.cancel(): the child
// context newSyncView derived from the caller's is otherwise only cancelled
// by esc, so a run that finishes or fails on its own would leak it (and its
// parent-context registration) for the rest of the enclosing program's life.
func (m *syncView) handleDone(d opDone) (*syncView, tea.Cmd) {
	if d.Err != nil {
		m.err = d.Err
		if errors.Is(d.Err, context.Canceled) {
			m.cancelled = true
			m.stage = stageAborted
			m.appendLog("cancelled")
		} else {
			m.stage = stageError
			m.appendLog("error: " + d.Err.Error())
		}
		m.cancel()
		return m, nil
	}
	switch d.Tag {
	case "fetch":
		m.appendLog("fetch complete")
		m.stage = stagePlan
		return m, m.stageCmd()
	case "plan":
		m.plan = d.Plan
		m.appendLog("plan complete")
		if !m.doApply {
			m.stage = stageDone
			m.cancel()
			return m, nil
		}
		unresolved := m.plan.Unresolved(m.res)
		if len(unresolved) > 0 {
			m.stage = stageResolve
			m.resolver = NewResolver(m.plan, m.res)
			return m, m.resolver.Init()
		}
		m.stage = stageApply
		return m, m.stageCmd()
	case "apply":
		m.report = d.Report
		m.appendLog("apply complete")
		if m.doPush {
			m.stage = stagePush
			return m, m.stageCmd()
		}
		m.stage = stageDone
		m.cancel()
		return m, nil
	case "push":
		m.appendLog("push complete")
		m.stage = stageDone
		m.cancel()
		return m, nil
	}
	return m, nil
}

// View renders the running operation: a spinner + stage while working, the
// resolver while resolving, or a summary once terminal.
func (m *syncView) View() string {
	if m.stage == stageResolve && m.resolver != nil {
		return m.resolver.View()
	}
	var b strings.Builder
	switch {
	case m.stage == stageDone:
		b.WriteString(styles.Success.Render("Done.") + "\n")
	case m.stage == stageAborted && m.cancelled:
		b.WriteString(styles.Warning.Render("Cancelled.") + "\n")
	case m.stage == stageAborted:
		b.WriteString(styles.Warning.Render("Aborted — nothing further was applied.") + "\n")
	case m.stage == stageError:
		b.WriteString(styles.Error.Render("Error: "+errString(m.err)) + "\n")
	default:
		fmt.Fprintf(&b, "%s %s\n", m.spin.View(), m.stage.label())
	}
	b.WriteString(m.vp.View())
	b.WriteString("\n")
	if m.report != nil {
		b.WriteString(styles.Muted.Render(reportSummary(m.report)) + "\n")
	}
	if m.stage.terminal() {
		b.WriteString(styles.Help.Render("enter/esc: continue"))
	} else {
		b.WriteString(styles.Help.Render("esc: cancel"))
	}
	return b.String()
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// formatSyncEvent renders one sync.Event as a log line.
func formatSyncEvent(ev sync.Event) string {
	line := fmt.Sprintf("[%s]", ev.Stage)
	if ev.Project != "" {
		line += " " + ev.Project
	}
	if ev.Path != "" {
		line += " " + ev.Path
	}
	if ev.Message != "" {
		line += " " + ev.Message
	}
	if ev.Total > 0 {
		line += fmt.Sprintf(" (%d/%d)", ev.Done, ev.Total)
	}
	if ev.Err != nil {
		line += ": " + ev.Err.Error()
	}
	return line
}

// reportSummary renders a one-line summary of a sync.Report. A nil report
// (the pipeline never reached Apply — e.g. fetch or plan failed, or the user
// aborted) renders as an explicit "nothing applied" rather than panicking.
func reportSummary(r *sync.Report) string {
	if r == nil {
		return "nothing applied"
	}
	parts := []string{
		fmt.Sprintf("uploaded %d", r.Uploaded),
		fmt.Sprintf("downloaded %d", r.Downloaded),
		fmt.Sprintf("converged %d", r.Converged),
		fmt.Sprintf("trashed %d", r.Trashed),
		fmt.Sprintf("deleted %d", r.Deleted),
		fmt.Sprintf("skipped %d", r.Skipped),
	}
	if r.Pending > 0 {
		parts = append(parts, fmt.Sprintf("pending %d", r.Pending))
	}
	if len(r.Unresolved) > 0 {
		parts = append(parts, fmt.Sprintf("unresolved %d", len(r.Unresolved)))
	}
	if len(r.Errors) > 0 {
		parts = append(parts, fmt.Sprintf("errors %d", len(r.Errors)))
	}
	return strings.Join(parts, " | ")
}
