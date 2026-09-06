package tui

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/sanbiv/private-sync/internal/app"
	"github.com/sanbiv/private-sync/internal/config"
	"github.com/sanbiv/private-sync/internal/identity"
	"github.com/sanbiv/private-sync/internal/paths"
	"github.com/sanbiv/private-sync/internal/sync"
	"github.com/sanbiv/private-sync/internal/vault"
)

// screenID selects which screen the root model is showing (spec §2.2).
type screenID int

const (
	screenDashboard screenID = iota
	screenDetail
	screenAddProject
	screenSettings
)

// rootModel is the top-level tea.Model for the dashboard program.
type rootModel struct {
	ctx context.Context
	s   *app.Session

	width, height int
	screen        screenID

	dash     *dashboardModel
	detail   *detailModel
	add      *addModel
	settings *settingsModel

	quitConfirm bool
	fatalErr    error
}

// Run launches the dashboard program on an opened session (spec §2.2 item 2).
func Run(ctx context.Context, s *app.Session) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if s == nil {
		return errors.New("tui.Run: nil session")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	m := &rootModel{ctx: ctx, s: s, screen: screenDashboard, dash: newDashboardModel(ctx, s)}
	p := tea.NewProgram(m, tea.WithContext(ctx), tea.WithAltScreen())
	_, err := p.Run()
	if err != nil {
		return err
	}
	if m.fatalErr != nil {
		return m.fatalErr
	}
	return ctx.Err()
}

func (m *rootModel) Init() tea.Cmd { return m.dash.Init() }

func (m *rootModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.dash.SetSize(msg.Width, msg.Height)
		if m.detail != nil {
			m.detail.SetSize(msg.Width, msg.Height)
		}
		if m.add != nil {
			m.add.SetSize(msg.Width, msg.Height)
		}
		if m.settings != nil {
			m.settings.SetSize(msg.Width, msg.Height)
		}
	case tea.KeyMsg:
		if msg.String() == "ctrl+c" {
			if m.applyRunning() {
				if !m.quitConfirm {
					m.quitConfirm = true
					return m, nil
				}
				m.cancelActiveOp()
			}
			return m, tea.Quit
		}
		m.quitConfirm = false
	}

	switch m.screen {
	case screenDashboard:
		return m.updateDashboard(msg)
	case screenDetail:
		return m.updateDetail(msg)
	case screenAddProject:
		return m.updateAdd(msg)
	case screenSettings:
		return m.updateSettings(msg)
	}
	return m, nil
}

// applyRunning reports whether the active screen is in the middle of writing
// changes (Apply or its finishing Push) — the only moment ctrl+c needs a
// second press to confirm (spec §2.2).
func (m *rootModel) applyRunning() bool {
	applying := func(sv *syncView) bool {
		return sv != nil && (sv.stage == stageApply || sv.stage == stagePush)
	}
	switch m.screen {
	case screenDashboard:
		return m.dash.svActive && applying(m.dash.sv)
	case screenDetail:
		return m.detail != nil && m.detail.svActive && applying(m.detail.sv)
	case screenAddProject:
		return m.add != nil && m.add.step == stepApply && applying(m.add.apply)
	case screenSettings:
		return m.settings != nil && m.settings.step == settingsStepRekeying && applying(m.settings.sv)
	}
	return false
}

// cancelActiveOp cancels whatever operation applyRunning found running.
func (m *rootModel) cancelActiveOp() {
	switch m.screen {
	case screenDashboard:
		if m.dash.sv != nil {
			m.dash.sv.Cancel()
		}
	case screenDetail:
		if m.detail != nil && m.detail.sv != nil {
			m.detail.sv.Cancel()
		}
	case screenAddProject:
		if m.add != nil && m.add.apply != nil {
			m.add.apply.Cancel()
		}
	case screenSettings:
		if m.settings != nil && m.settings.sv != nil {
			m.settings.sv.Cancel()
		}
	}
}

func (m *rootModel) updateDashboard(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd
	action, rest := m.dash.Update(msg)
	cmd = rest
	switch action.kind {
	case dashActionAdd:
		m.add = newAddModel(m.ctx, m.s, "")
		m.add.SetSize(m.width, m.height)
		m.screen = screenAddProject
		return m, m.add.Init()
	case dashActionDetail:
		dm, err := newDetailModel(m.ctx, m.s, action.projectID, action.projectName)
		if err != nil {
			m.dash.setError(err)
			return m, cmd
		}
		m.detail = dm
		m.detail.SetSize(m.width, m.height)
		m.screen = screenDetail
		return m, tea.Batch(cmd, m.detail.Init())
	case dashActionSettings:
		m.settings = newSettingsModel(m.ctx, m.s)
		m.settings.SetSize(m.width, m.height)
		m.screen = screenSettings
		return m, tea.Batch(cmd, m.settings.Init())
	case dashActionQuit:
		return m, tea.Quit
	}
	return m, cmd
}

func (m *rootModel) updateDetail(msg tea.Msg) (tea.Model, tea.Cmd) {
	back, cmd := m.detail.Update(msg)
	if back {
		m.screen = screenDashboard
		refresh := m.dash.Init()
		return m, tea.Batch(cmd, refresh)
	}
	return m, cmd
}

func (m *rootModel) updateAdd(msg tea.Msg) (tea.Model, tea.Cmd) {
	back, cmd := m.add.Update(msg)
	if back {
		m.screen = screenDashboard
		refresh := m.dash.Init()
		return m, tea.Batch(cmd, refresh)
	}
	return m, cmd
}

func (m *rootModel) updateSettings(msg tea.Msg) (tea.Model, tea.Cmd) {
	back, cmd := m.settings.Update(msg)
	if back {
		m.screen = screenDashboard
		return m, tea.Batch(cmd, m.dash.Init())
	}
	return m, cmd
}

func (m *rootModel) View() string {
	var body string
	switch m.screen {
	case screenDashboard:
		body = m.dash.View()
	case screenDetail:
		body = m.detail.View()
	case screenAddProject:
		body = m.add.View()
	case screenSettings:
		body = m.settings.View()
	}
	if m.quitConfirm {
		body += "\n" + styles.Warning.Render("an operation is running — press ctrl+c again to quit anyway")
	}
	return body
}

// --- dashboard screen -------------------------------------------------

// dashActionKind is what the dashboard's Update wants the root model to do.
type dashActionKind int

const (
	dashActionNone dashActionKind = iota
	dashActionAdd
	dashActionDetail
	dashActionSettings
	dashActionQuit
)

type dashAction struct {
	kind        dashActionKind
	projectID   string
	projectName string
}

// projectRow is one row of the dashboard's project list (spec §2.2 item 2).
type projectRow struct {
	linked       bool
	id           string
	name         string
	path         string
	badge        string
	badgeStyle   lipgloss.Style
	fingerprints []identity.Fingerprint
}

type dashboardModel struct {
	ctx context.Context
	s   *app.Session

	rows   []projectRow
	cursor int

	loading bool
	err     string
	status  string

	svActive bool
	sv       *syncView

	linking *linkPrompt

	width, height int
}

// linkPrompt is the small "path?" step shown when the user presses enter on
// an unlinked vault project (spec §2.2 item 2).
type linkPrompt struct {
	project vault.Project
	input   textinput.Model
	err     string
}

func newDashboardModel(ctx context.Context, s *app.Session) *dashboardModel {
	return &dashboardModel{ctx: ctx, s: s}
}

func (m *dashboardModel) SetSize(w, h int) {
	m.width, m.height = w, h
	if m.sv != nil {
		m.sv.SetSize(w, h)
	}
}

func (m *dashboardModel) setError(err error) {
	if err != nil {
		m.err = err.Error()
	}
}

// dashRowsMsg carries the freshly computed dashboard rows.
type dashRowsMsg struct {
	rows []projectRow
	err  error
}

// Init recomputes the dashboard's rows from a local plan (no network).
func (m *dashboardModel) Init() tea.Cmd {
	m.loading = true
	m.err = ""
	return m.loadRowsCmd()
}

func (m *dashboardModel) loadRowsCmd() tea.Cmd {
	s := m.s
	ctx := m.ctx
	return func() tea.Msg {
		rows, err := computeDashboardRows(ctx, s)
		return dashRowsMsg{rows: rows, err: err}
	}
}

// computeDashboardRows builds the dashboard's rows: linked projects (with
// badges from a local plan) followed by unlinked vault projects.
func computeDashboardRows(ctx context.Context, s *app.Session) ([]projectRow, error) {
	plan, err := s.Engine.Plan(ctx, sync.Options{Mode: sync.ModeSync})
	if err != nil {
		return nil, err
	}
	rows := make([]projectRow, 0, len(plan.Projects))
	for i := range plan.Projects {
		pp := &plan.Projects[i]
		label, style := dashboardBadge(pp)
		name := pp.Name
		if name == "" {
			name = pp.ID
		}
		rows = append(rows, projectRow{
			linked: true, id: pp.ID, name: name, path: pp.Path,
			badge: label, badgeStyle: style,
		})
	}
	unlinked, err := s.UnlinkedProjects()
	if err != nil {
		return rows, err
	}
	for _, p := range unlinked {
		name := p.Name
		if name == "" {
			name = p.ID
		}
		rows = append(rows, projectRow{
			linked: false, id: p.ID, name: name,
			badge: "not linked", badgeStyle: styles.BadgeMuted,
			fingerprints: p.Fingerprints,
		})
	}
	return rows, nil
}

// dashboardBadge computes the status badge for a linked project (spec §2.2
// item 2): synced, local changes, remote changes, conflicts, pending, path
// missing, or duplicate of <id>.
//
// "path missing" is reserved for pp.Missing — the project's whole local
// directory is gone. sum.Missing (ActionMissingLocal) is a much narrower
// thing: one tracked file was deleted locally while the vault head hasn't
// moved, a local-side change awaiting an explicit decision, not "go find
// your project directory" — so it is badged as "local changes" instead
// (BadgeWarn, since unlike an ordinary pending upload it needs the user to
// choose). Likewise sum.Rollback (the vault head moved backward relative to
// the base — someone rolled back or lost data) is a remote-side thing that
// needs attention, not a content conflict between two edits, so it is
// badged as "remote changes" (also BadgeWarn) rather than "conflicts".
func dashboardBadge(pp *sync.ProjectPlan) (string, lipgloss.Style) {
	if pp == nil {
		return "unknown", styles.BadgeMuted
	}
	if pp.Missing {
		return "path missing", styles.BadgeBad
	}
	if pp.DuplicateOf != "" {
		return "duplicate of " + shortMachineID(pp.DuplicateOf), styles.BadgeWarn
	}
	sum := sync.Summarize(pp)
	switch {
	case sum.Conflicts > 0:
		return "conflicts", styles.BadgeBad
	case sum.Missing > 0:
		return "local changes", styles.BadgeWarn
	case sum.Rollback > 0:
		return "remote changes", styles.BadgeWarn
	case sum.Pending > 0:
		return "pending", styles.BadgeWarn
	case sum.LocalChanges > 0:
		return "local changes", styles.BadgeInfo
	case sum.RemoteChanges > 0:
		return "remote changes", styles.BadgeInfo
	default:
		return "synced", styles.BadgeGood
	}
}

func (m *dashboardModel) Update(msg tea.Msg) (dashAction, tea.Cmd) {
	if m.linking != nil {
		return m.updateLinking(msg)
	}
	if m.svActive {
		return m.updateSyncView(msg)
	}

	switch msg := msg.(type) {
	case dashRowsMsg:
		m.loading = false
		if msg.err != nil {
			m.err = msg.err.Error()
			return dashAction{}, nil
		}
		m.rows = msg.rows
		if m.cursor >= len(m.rows) {
			m.cursor = 0
		}
		return dashAction{}, nil
	case linkDoneMsg:
		if msg.err != nil {
			m.err = msg.err.Error()
			return dashAction{}, nil
		}
		m.startSync(sync.Options{Mode: sync.ModePull, Projects: []string{msg.projectID}}, stagePlan, true, false)
		return dashAction{}, m.sv.Init()
	case tea.KeyMsg:
		return m.handleKey(msg)
	}
	return dashAction{}, nil
}

func (m *dashboardModel) handleKey(msg tea.KeyMsg) (dashAction, tea.Cmd) {
	switch msg.String() {
	case "q":
		return dashAction{kind: dashActionQuit}, nil
	case "up", "k":
		if m.cursor > 0 {
			m.cursor--
		}
	case "down", "j":
		if m.cursor < len(m.rows)-1 {
			m.cursor++
		}
	case "a":
		return dashAction{kind: dashActionAdd}, nil
	case "c":
		return dashAction{kind: dashActionSettings}, nil
	case "r":
		m.status = ""
		m.startSync(sync.Options{Mode: sync.ModeSync}, stageFetch, false, false)
		return dashAction{}, m.sv.Init()
	case "s":
		m.status = ""
		m.startSync(sync.Options{Mode: sync.ModeSync}, stageFetch, true, true)
		return dashAction{}, m.sv.Init()
	case "u":
		m.status = ""
		m.startSync(sync.Options{Mode: sync.ModePush}, stageFetch, true, true)
		return dashAction{}, m.sv.Init()
	case "d":
		m.status = ""
		m.startSync(sync.Options{Mode: sync.ModePull}, stageFetch, true, true)
		return dashAction{}, m.sv.Init()
	case "enter":
		return m.handleEnter()
	}
	return dashAction{}, nil
}

func (m *dashboardModel) handleEnter() (dashAction, tea.Cmd) {
	if m.cursor < 0 || m.cursor >= len(m.rows) {
		return dashAction{}, nil
	}
	row := m.rows[m.cursor]
	if row.linked {
		return dashAction{kind: dashActionDetail, projectID: row.id, projectName: row.name}, nil
	}
	ti := textinput.New()
	ti.Placeholder = "~/code/" + row.name
	ti.Focus()
	m.linking = &linkPrompt{
		project: vault.Project{ID: row.id, Name: row.name, Fingerprints: row.fingerprints},
		input:   ti,
	}
	return dashAction{}, textinput.Blink
}

func (m *dashboardModel) startSync(opts sync.Options, start syncStage, doApply, doPush bool) {
	m.sv = newSyncView(m.ctx, m.s.Engine, opts, start, doApply, doPush)
	m.sv.SetSize(m.width, m.height)
	m.svActive = true
}

func (m *dashboardModel) updateSyncView(msg tea.Msg) (dashAction, tea.Cmd) {
	if key, ok := msg.(tea.KeyMsg); ok && m.sv.stage.terminal() {
		if key.String() == "enter" || key.String() == "esc" {
			m.svActive = false
			m.sv = nil
			return dashAction{}, m.Init()
		}
	}
	sv, cmd := m.sv.Update(msg)
	m.sv = sv
	return dashAction{}, cmd
}

func (m *dashboardModel) updateLinking(msg tea.Msg) (dashAction, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "esc":
			m.linking = nil
			return dashAction{}, nil
		case "enter":
			return m.submitLink()
		}
	}
	var cmd tea.Cmd
	m.linking.input, cmd = m.linking.input.Update(msg)
	return dashAction{}, cmd
}

func (m *dashboardModel) submitLink() (dashAction, tea.Cmd) {
	raw := strings.TrimSpace(m.linking.input.Value())
	if raw == "" {
		m.linking.err = "enter a local path"
		return dashAction{}, nil
	}
	dir, err := paths.ExpandHome(raw)
	if err != nil {
		m.linking.err = err.Error()
		return dashAction{}, nil
	}
	if !dirExists(dir) {
		m.linking.err = "path does not exist: " + dir
		return dashAction{}, nil
	}
	proj := m.linking.project
	ctx, s := m.ctx, m.s
	m.linking = nil
	cmd := func() tea.Msg {
		err := s.LinkProject(ctx, proj.ID, proj.Name, dir, proj.Fingerprints)
		return linkDoneMsg{err: err, projectID: proj.ID}
	}
	return dashAction{}, cmd
}

// linkDoneMsg reports that LinkProject finished for the dashboard's
// unlinked-project flow.
type linkDoneMsg struct {
	err       error
	projectID string
}

func (m *dashboardModel) View() string {
	var b strings.Builder
	b.WriteString(styles.Title.Render("private-sync") + "\n")
	b.WriteString(styles.Muted.Render(vaultSummaryLine(m.s)) + "\n\n")
	if m.linking != nil {
		b.WriteString("Local path for " + m.linking.project.Name + ":\n")
		b.WriteString(m.linking.input.View() + "\n")
		if m.linking.err != "" {
			b.WriteString(styles.Error.Render(m.linking.err) + "\n")
		}
		b.WriteString(styles.Help.Render("enter: link and pull  esc: cancel"))
		return b.String()
	}
	if m.svActive {
		b.WriteString(m.sv.View())
		return b.String()
	}
	if m.loading && len(m.rows) == 0 {
		b.WriteString("loading...\n")
		return b.String()
	}
	if m.err != "" {
		b.WriteString(styles.Error.Render(m.err) + "\n")
	}
	if len(m.rows) == 0 {
		b.WriteString(styles.Muted.Render("no projects yet — press a to add one") + "\n")
	}
	for i, row := range m.rows {
		cursor := "  "
		if i == m.cursor {
			cursor = "> "
		}
		line := fmt.Sprintf("%s%-28s %s", cursor, row.name, row.badgeStyle.Render(row.badge))
		if row.linked {
			line += "  " + styles.Muted.Render(row.path)
		} else if len(row.fingerprints) > 0 {
			fps := make([]string, 0, len(row.fingerprints))
			for _, fp := range row.fingerprints {
				fps = append(fps, fp.String())
			}
			line += "  " + styles.Muted.Render(strings.Join(fps, ", "))
		}
		if i == m.cursor {
			line = styles.Selected.Render(line)
		}
		b.WriteString(line + "\n")
	}
	if m.status != "" {
		b.WriteString("\n" + styles.Success.Render(m.status) + "\n")
	}
	b.WriteString("\n" + styles.Help.Render("a add  s sync  u push  d pull  r refresh  enter open  c settings  q quit"))
	return b.String()
}

// vaultSummaryLine renders the dashboard's "vault + remote summary" header
// line (spec §2.2 item 2): the vault path, the configured remote and this
// machine's name.
func vaultSummaryLine(s *app.Session) string {
	if s == nil || s.Config == nil {
		return ""
	}
	return fmt.Sprintf("vault %s  remote %s  machine %s",
		paths.ContractHome(s.Config.Vault.Path),
		describeRemote(s.Config.Vault.Remote),
		s.Config.Machine.Name)
}

// describeRemote renders a one-line summary of the configured remote.
func describeRemote(r config.RemoteConfig) string {
	switch r.Type {
	case config.RemoteGit:
		return fmt.Sprintf("git %s (%s)", r.Git.URL, r.Git.Branch)
	case config.RemoteRclone:
		return fmt.Sprintf("rclone %s:%s", r.Rclone.Remote, r.Rclone.Path)
	default:
		return "none"
	}
}

func dirExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.IsDir()
}
