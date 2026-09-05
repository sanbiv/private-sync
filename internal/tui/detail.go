package tui

import (
	"context"
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/sanbiv/private-sync/internal/app"
	"github.com/sanbiv/private-sync/internal/sync"
)

// detailModel is the project detail screen (spec §2.2 item 4): a table of
// the project's files with per-file and per-project actions.
type detailModel struct {
	ctx context.Context
	s   *app.Session

	projectID   string
	projectName string
	projectPath string

	items []sync.Item
	tbl   table.Model
	err   string

	confirm *detailConfirm

	svActive bool
	sv       *syncView

	rescan     *addModel
	rescanning bool

	width, height int
}

// detailConfirm is a pending y/n confirmation for a destructive action.
type detailConfirm struct {
	message string
	run     func() tea.Cmd
}

// detailPlanMsg carries the freshly computed project plan.
type detailPlanMsg struct {
	plan *sync.Plan
	err  error
}

// newDetailModel resolves the project's local path and builds the screen;
// the file table itself is loaded asynchronously by Init.
func newDetailModel(ctx context.Context, s *app.Session, id, name string) (*detailModel, error) {
	path, err := s.Config.ProjectPath(id)
	if err != nil {
		return nil, err
	}
	tbl := table.New(
		table.WithColumns([]table.Column{
			{Title: "Path", Width: 40},
			{Title: "State", Width: 16},
			{Title: "Reason", Width: 40},
		}),
		table.WithFocused(true),
	)
	return &detailModel{ctx: ctx, s: s, projectID: id, projectName: name, projectPath: path, tbl: tbl}, nil
}

func (m *detailModel) SetSize(w, h int) {
	m.width, m.height = w, h
	m.tbl.SetWidth(w)
	m.tbl.SetHeight(h - 6)
	if m.sv != nil {
		m.sv.SetSize(w, h)
	}
	if m.rescan != nil {
		m.rescan.SetSize(w, h)
	}
}

func (m *detailModel) Init() tea.Cmd {
	return m.loadPlanCmd()
}

func (m *detailModel) loadPlanCmd() tea.Cmd {
	ctx, s, id := m.ctx, m.s, m.projectID
	return func() tea.Msg {
		p, err := s.Engine.Plan(ctx, sync.Options{Mode: sync.ModeSync, Projects: []string{id}})
		return detailPlanMsg{plan: p, err: err}
	}
}

// Update handles one message; the returned bool is true once the screen
// should be dismissed back to the dashboard.
func (m *detailModel) Update(msg tea.Msg) (bool, tea.Cmd) {
	if m.rescanning {
		return m.updateRescan(msg)
	}
	if m.svActive {
		return m.updateSyncView(msg)
	}
	if m.confirm != nil {
		return m.updateConfirm(msg)
	}

	switch msg := msg.(type) {
	case detailPlanMsg:
		if msg.err != nil {
			m.err = msg.err.Error()
			return false, nil
		}
		m.setPlan(msg.plan)
		return false, nil
	case unlinkedMsg:
		return true, nil
	case tea.KeyMsg:
		return m.handleKey(msg)
	}
	return false, nil
}

func (m *detailModel) setPlan(p *sync.Plan) {
	pp := findProjectPlan(p, m.projectID)
	if pp == nil {
		m.items = nil
		m.tbl.SetRows(nil)
		return
	}
	m.items = append([]sync.Item(nil), pp.Items...)
	rows := make([]table.Row, 0, len(m.items))
	for _, it := range m.items {
		rows = append(rows, table.Row{it.Key.Path, itemStateLabel(&it), it.Reason})
	}
	m.tbl.SetRows(rows)
}

// itemStateLabel renders an item's action for the detail table.
func itemStateLabel(it *sync.Item) string {
	a := it.Action
	if a == sync.ActionReportOnly {
		a = it.Original
	}
	return a.String()
}

func (m *detailModel) currentItem() *sync.Item {
	rows := m.tbl.Rows()
	if len(rows) == 0 {
		return nil
	}
	i := m.tbl.Cursor()
	if i < 0 || i >= len(m.items) {
		return nil
	}
	return &m.items[i]
}

func (m *detailModel) handleKey(msg tea.KeyMsg) (bool, tea.Cmd) {
	switch msg.String() {
	case "esc":
		return true, nil
	case "a":
		m.rescan = newRescanModel(m.ctx, m.s, m.projectID, m.projectName, m.projectPath)
		m.rescan.SetSize(m.width, m.height)
		m.rescanning = true
		return false, m.rescan.Init()
	case "s":
		m.startSync(sync.Options{Mode: sync.ModeSync, Projects: []string{m.projectID}}, stageFetch, true, true)
		return false, m.sv.Init()
	case "R":
		m.confirm = &detailConfirm{
			message: "Restore every tracked file in this project from the vault?",
			run: func() tea.Cmd {
				m.startSync(sync.Options{Mode: sync.ModeRestore, Projects: []string{m.projectID}}, stagePlan, true, true)
				return m.sv.Init()
			},
		}
		return false, nil
	case "-":
		m.confirm = &detailConfirm{
			message: "Unlink this project on this machine? (the vault is untouched)",
			run: func() tea.Cmd {
				err := m.s.UnlinkProject(m.projectID)
				if err != nil {
					m.err = err.Error()
					return nil
				}
				return func() tea.Msg { return unlinkedMsg{} }
			},
		}
		return false, nil
	case "x":
		it := m.currentItem()
		if it == nil {
			return false, nil
		}
		path := it.Key.Path
		m.confirm = &detailConfirm{
			message: fmt.Sprintf("Untrack %s? (kept on disk, dropped from the vault)", path),
			run: func() tea.Cmd {
				if err := m.s.Engine.Untrack(m.ctx, m.projectID, []string{path}); err != nil {
					m.err = err.Error()
					return nil
				}
				m.startSync(sync.Options{}, stagePush, false, true)
				return m.sv.Init()
			},
		}
		return false, nil
	case "D":
		it := m.currentItem()
		if it == nil {
			return false, nil
		}
		path := it.Key.Path
		m.confirm = &detailConfirm{
			message: fmt.Sprintf("Delete %s everywhere? (trashed locally, removed from the vault)", path),
			run: func() tea.Cmd {
				if err := m.s.Engine.DeleteEverywhere(m.ctx, m.projectID, []string{path}); err != nil {
					m.err = err.Error()
					return nil
				}
				m.startSync(sync.Options{}, stagePush, false, true)
				return m.sv.Init()
			},
		}
		return false, nil
	default:
		var cmd tea.Cmd
		m.tbl, cmd = m.tbl.Update(msg)
		return false, cmd
	}
}

// unlinkedMsg reports that UnlinkProject finished; the detail screen then
// leaves for the dashboard.
type unlinkedMsg struct{}

func (m *detailModel) updateConfirm(msg tea.Msg) (bool, tea.Cmd) {
	key, ok := msg.(tea.KeyMsg)
	if !ok {
		return false, nil
	}
	switch key.String() {
	case "y", "enter":
		run := m.confirm.run
		m.confirm = nil
		return false, run()
	case "n", "esc":
		m.confirm = nil
		return false, nil
	}
	return false, nil
}

func (m *detailModel) startSync(opts sync.Options, start syncStage, doApply, doPush bool) {
	m.sv = newSyncView(m.ctx, m.s.Engine, opts, start, doApply, doPush)
	m.sv.SetSize(m.width, m.height)
	m.svActive = true
}

func (m *detailModel) updateSyncView(msg tea.Msg) (bool, tea.Cmd) {
	if key, ok := msg.(tea.KeyMsg); ok && m.sv.stage.terminal() {
		if key.String() == "enter" || key.String() == "esc" {
			m.svActive = false
			m.sv = nil
			return false, m.loadPlanCmd()
		}
	}
	sv, cmd := m.sv.Update(msg)
	m.sv = sv
	return false, cmd
}

func (m *detailModel) updateRescan(msg tea.Msg) (bool, tea.Cmd) {
	done, cmd := m.rescan.Update(msg)
	if done {
		m.rescanning = false
		m.rescan = nil
		return false, m.loadPlanCmd()
	}
	return false, cmd
}

func (m *detailModel) View() string {
	if m.rescanning {
		return m.rescan.View()
	}
	if m.svActive {
		return m.sv.View()
	}
	var b strings.Builder
	name := m.projectName
	if name == "" {
		name = m.projectID
	}
	b.WriteString(styles.Title.Render(name) + "  " + styles.Muted.Render(m.projectPath) + "\n\n")
	if m.err != "" {
		b.WriteString(styles.Error.Render(m.err) + "\n")
	}
	b.WriteString(m.tbl.View() + "\n")
	if m.confirm != nil {
		b.WriteString("\n" + styles.Warning.Render(m.confirm.message+" (y/n)") + "\n")
	} else {
		b.WriteString("\n" + styles.Help.Render("a rescan  x untrack  D delete  R restore  - unlink  s sync  esc back"))
	}
	return b.String()
}
