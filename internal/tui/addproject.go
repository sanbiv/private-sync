package tui

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/sanbiv/private-sync/internal/app"
	"github.com/sanbiv/private-sync/internal/config"
	"github.com/sanbiv/private-sync/internal/execx"
	"github.com/sanbiv/private-sync/internal/identity"
	"github.com/sanbiv/private-sync/internal/paths"
	"github.com/sanbiv/private-sync/internal/scan"
	"github.com/sanbiv/private-sync/internal/sync"
	"github.com/sanbiv/private-sync/internal/vault"
)

// addStep is one page of the add-project wizard (spec §2.2 item 3: identity
// -> scanning -> candidate multi-select -> name -> confirm).
type addStep int

const (
	stepPath addStep = iota
	stepFetch
	stepIdentify
	stepAssociate
	stepScan
	stepSelect
	stepNewName
	stepConfirm
	stepApply
	stepSummary
)

// addModel is the add-project wizard, used both as its own program
// (RunAddProject) and embedded by the dashboard ("a") and project detail
// ("a", to re-scan) screens.
type addModel struct {
	ctx context.Context
	s   *app.Session

	step    addStep
	history []addStep
	err     string

	// stepPath
	pathInput textinput.Model

	dir string

	// stepFetch. fetchGen is bumped every time stepFetch is (re)entered and
	// carried by its opEventMsg/opDoneMsg: a fetch cancelled by esc still
	// delivers its final message once the goroutine unwinds, and without the
	// generation check that stale message would be applied to whatever fetch
	// is running by the time it arrives (spec §2.2 item 3, "esc cancels").
	fetchCtx    context.Context
	fetchCancel context.CancelFunc
	fetchSpin   spinner.Model
	fetchGen    int
	fetchErr    error // set on a failed fetch; "enter" re-runs enterCmd(stepFetch)

	// stepIdentify / stepAssociate. identGen is the same kind of guard as
	// fetchGen/scanGen above, for identifyMsg: without it, esc back to
	// stepPath followed by a different directory could let a stale
	// identifyMsg for the OLD directory land after the new identify starts,
	// applying the wrong fingerprints/matches to the wizard.
	identCtx    context.Context
	identCancel context.CancelFunc
	identGen    int
	fps         []identity.Fingerprint
	matches     []identity.Match
	vaultByID   map[string]vault.Project
	assocIdx    int // index into matches; len(matches) means "create new"

	// resolved outcome of identify/associate
	creatingNew bool
	forceRandom bool // true when the user chose "create new" despite a match
	projectID   string
	projectName string
	willRestore []string

	// stepNewName
	nameInput textinput.Model

	// stepScan. scanGen is the same kind of guard as fetchGen above, for the
	// scan's scanEventMsg/scanDoneMsg.
	scanCtx    context.Context
	scanCancel context.CancelFunc
	scanSpin   spinner.Model
	scanGen    int
	walked     int
	found      int
	scanErr    error

	// stepSelect
	cand *candidateList

	// stepApply
	untrack []string
	apply   *syncView

	// stepSummary
	summary string

	// finished is set once the wizard reaches stepSummary — from that point
	// leaving the program is a normal finish (whether or not Apply itself
	// succeeded), not a cancel. RunAddProject reports ErrAborted only when
	// the wizard is dismissed before this happens (e.g. esc at the path
	// step): spec §2.2 item 3's "esc = back one step" must not let `add`
	// silently exit 0 after the user backed all the way out.
	finished bool

	width, height int
}

// newAddModel builds the wizard. dir, when non-empty, is the directory the
// caller already knows (RunAddProject's positional argument): the wizard
// starts at the fetch step instead of asking for a path again.
func newAddModel(ctx context.Context, s *app.Session, dir string) *addModel {
	pi := textinput.New()
	pi.Placeholder = "~/code/my-project"
	pi.Focus()
	m := &addModel{ctx: ctx, s: s, pathInput: pi}
	if strings.TrimSpace(dir) == "" {
		m.step = stepPath
		return m
	}
	expanded, err := paths.ExpandHome(dir)
	if err != nil || !dirExists(expanded) {
		m.step = stepPath
		m.pathInput.SetValue(dir)
		if err != nil {
			m.err = err.Error()
		} else {
			m.err = "path does not exist: " + expanded
		}
		return m
	}
	m.dir = expanded
	m.pathInput.SetValue(expanded)
	m.step = stepFetch
	return m
}

// newRescanModel builds the wizard scoped to an already-linked project,
// jumping straight to the scan step with its tracked files marked (spec §2.2
// item 4, "a" on the project detail screen).
func newRescanModel(ctx context.Context, s *app.Session, projectID, name, dir string) *addModel {
	m := &addModel{
		ctx:         ctx,
		s:           s,
		dir:         dir,
		projectID:   projectID,
		projectName: name,
		creatingNew: false,
		step:        stepScan,
	}
	return m
}

func (m *addModel) SetSize(w, h int) {
	m.width, m.height = w, h
	if m.apply != nil {
		m.apply.SetSize(w, h)
	}
}

// Init kicks off the current step's side effect, if any.
func (m *addModel) Init() tea.Cmd {
	return m.enterCmd(m.step)
}

// enterCmd starts whatever background work a step needs as it becomes active
// for the first time (goTo, moving forward). Re-entering a step via esc uses
// resumeCmd instead, which keeps state enterCmd would otherwise discard or
// re-run (spec §2.2: "esc = back one step, keeping state").
func (m *addModel) enterCmd(step addStep) tea.Cmd {
	m.err = ""
	switch step {
	case stepFetch:
		m.fetchCtx, m.fetchCancel = context.WithCancel(m.ctx)
		m.fetchSpin = newSpinner()
		m.fetchGen++
		m.fetchErr = nil
		return tea.Batch(m.fetchSpin.Tick, startFetchCmd(m.fetchCtx, m.s.Engine, m.fetchGen))
	case stepIdentify:
		m.identCtx, m.identCancel = context.WithCancel(m.ctx)
		m.identGen++
		return identifyCmd(m.identCtx, m.s, m.dir, m.identGen)
	case stepNewName:
		def := filepath.Base(m.dir)
		ni := textinput.New()
		ni.Placeholder = def
		ni.SetValue(def)
		ni.Focus()
		m.nameInput = ni
		return textinput.Blink
	case stepScan:
		m.scanCtx, m.scanCancel = context.WithCancel(m.ctx)
		m.scanSpin = newSpinner()
		m.scanGen++
		m.walked, m.found = 0, 0
		// A brand new project has nothing tracked yet, so trackID is only
		// ever non-empty here when associating with an existing match
		// (m.projectID is set before this step runs); ScanOptions("") skips
		// the tracked-paths lookup entirely for the create-new case.
		trackID := m.projectID
		opts, err := m.s.ScanOptions(trackID)
		if err != nil {
			m.err = err.Error()
			return nil
		}
		return tea.Batch(m.scanSpin.Tick, startScanCmd(m.scanCtx, m.dir, opts, runnerFor(m.s), m.scanGen))
	case stepConfirm:
		if m.cand != nil {
			m.untrack = m.cand.untracked()
		}
		return nil
	case stepApply:
		return m.startApply()
	}
	return nil
}

// resumeCmd re-enters a step reached by going back (esc), preserving
// whatever the user already did there instead of enterCmd's "start fresh"
// behaviour:
//   - stepNewName only refocuses the input, keeping the typed name instead
//     of resetting it to the directory's basename.
//   - stepFetch, stepIdentify and stepScan are never resumed directly:
//     goBack always skips over them while unwinding (see goBack), so every
//     other (interactive) step falls through to enterCmd's ordinary
//     behaviour, which for those steps is a no-op — their state (matches,
//     candidate list, ...) is already sitting in the model.
func (m *addModel) resumeCmd(step addStep) tea.Cmd {
	m.err = ""
	if step == stepNewName {
		m.nameInput.Focus()
		return textinput.Blink
	}
	return m.enterCmd(step)
}

// runnerFor returns the session's configured subprocess runner, defaulting
// to execx.Real when the session was built without one.
func runnerFor(s *app.Session) execx.Runner {
	if s != nil && s.Runner != nil {
		return s.Runner
	}
	return execx.Real()
}

// goTo pushes the current step onto the history stack and moves forward.
func (m *addModel) goTo(step addStep) tea.Cmd {
	m.history = append(m.history, m.step)
	m.step = step
	return m.enterCmd(step)
}

// goBack pops the history stack (esc = back one step, keeping state). It
// reports whether the wizard should be left entirely (history empty).
//
// stepFetch, stepIdentify and stepScan are always skipped while unwinding:
// none of them render a screen the user interacts with (spec §2.2 item 3
// lists them as the identity -> scan -> select pipeline, not pages with
// their own esc target) — they only run a background operation and then
// immediately advance on completion. Treating any of them as an esc landing
// spot means resumeCmd's enterCmd re-runs that operation, which on
// completion calls goTo again and lands right back where esc was pressed,
// so the step can never actually be left (e.g. stepAssociate -esc-> re-run
// identify -no match found-> back to stepAssociate). Skipping all three
// unconditionally means esc always lands on a genuinely interactive step
// (stepPath, stepAssociate, stepSelect, stepNewName, stepConfirm) or, once
// none remain in the history (e.g. the wizard was started with a directory
// already known, so stepPath was never pushed), leaves the wizard.
func (m *addModel) goBack() (bool, tea.Cmd) {
	for len(m.history) > 0 {
		prev := m.history[len(m.history)-1]
		m.history = m.history[:len(m.history)-1]
		if prev == stepFetch || prev == stepIdentify || prev == stepScan {
			continue
		}
		m.step = prev
		return false, m.resumeCmd(prev)
	}
	return true, nil
}

// identifyMsg carries s.Identify's result plus the vault's project list (for
// displaying match names). gen ties it back to the identGen that was current
// when the identify started, so a stale message from a cancelled/superseded
// run (esc back to stepPath, a different directory, identify started again)
// is ignored instead of being applied to the wrong directory.
type identifyMsg struct {
	gen       int
	fps       []identity.Fingerprint
	matches   []identity.Match
	vaultByID map[string]vault.Project
	err       error
}

func identifyCmd(ctx context.Context, s *app.Session, dir string, gen int) tea.Cmd {
	return func() tea.Msg {
		fps, matches, err := s.Identify(ctx, dir)
		if err != nil {
			return identifyMsg{gen: gen, err: err}
		}
		projs, _, err := s.VaultProjects()
		if err != nil {
			return identifyMsg{gen: gen, fps: fps, matches: matches, err: err}
		}
		byID := make(map[string]vault.Project, len(projs))
		for _, p := range projs {
			byID[p.ID] = p
		}
		return identifyMsg{gen: gen, fps: fps, matches: matches, vaultByID: byID}
	}
}

// Update handles one message. The returned bool is true once the wizard
// should be dismissed (cancelled or finished).
func (m *addModel) Update(msg tea.Msg) (bool, tea.Cmd) {
	if wsz, ok := msg.(tea.WindowSizeMsg); ok {
		m.SetSize(wsz.Width, wsz.Height)
	}

	switch m.step {
	case stepPath:
		return m.updatePath(msg)
	case stepFetch:
		return m.updateFetch(msg)
	case stepIdentify:
		return m.updateIdentify(msg)
	case stepAssociate:
		return m.updateAssociate(msg)
	case stepNewName:
		return m.updateNewName(msg)
	case stepScan:
		return m.updateScan(msg)
	case stepSelect:
		return m.updateSelect(msg)
	case stepConfirm:
		return m.updateConfirm(msg)
	case stepApply:
		return m.updateApply(msg)
	case stepSummary:
		return m.updateSummary(msg)
	}
	return false, nil
}

func (m *addModel) updatePath(msg tea.Msg) (bool, tea.Cmd) {
	if key, ok := msg.(tea.KeyMsg); ok {
		switch key.String() {
		case "esc":
			return true, nil
		case "enter":
			raw := strings.TrimSpace(m.pathInput.Value())
			if raw == "" {
				m.err = "enter a directory"
				return false, nil
			}
			dir, err := paths.ExpandHome(raw)
			if err != nil {
				m.err = err.Error()
				return false, nil
			}
			if !dirExists(dir) {
				m.err = "path does not exist: " + dir
				return false, nil
			}
			m.dir = dir
			return false, m.goTo(stepFetch)
		}
	}
	var cmd tea.Cmd
	m.pathInput, cmd = m.pathInput.Update(msg)
	return false, cmd
}

func (m *addModel) updateFetch(msg tea.Msg) (bool, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "esc":
			if m.fetchCancel != nil {
				m.fetchCancel()
			}
			done, cmd := m.goBack()
			return done, cmd
		case "enter":
			if m.fetchErr != nil {
				return false, m.enterCmd(stepFetch)
			}
		}
	case spinner.TickMsg:
		if m.fetchErr != nil {
			return false, nil // fetch already failed: stop animating
		}
		var cmd tea.Cmd
		m.fetchSpin, cmd = m.fetchSpin.Update(msg)
		return false, cmd
	case opEventMsg:
		if msg.Gen != m.fetchGen {
			return false, nil // stale: from a fetch esc already cancelled
		}
		return false, continueOpCmd(msg)
	case opDoneMsg:
		if msg.Gen != m.fetchGen {
			return false, nil // stale: from a fetch esc already cancelled
		}
		// The fetch (success or failure) is over: release the child context
		// enterCmd derived from m.ctx now rather than only on esc, or it
		// leaks its parent-context registration for the rest of the
		// program's life (syncView.handleDone does the same on every path).
		if m.fetchCancel != nil {
			m.fetchCancel()
		}
		if msg.Err != nil {
			m.fetchErr = msg.Err
			return false, nil
		}
		return false, m.goTo(stepIdentify)
	}
	return false, nil
}

func (m *addModel) updateIdentify(msg tea.Msg) (bool, tea.Cmd) {
	msgv, ok := msg.(identifyMsg)
	if !ok {
		if key, ok := msg.(tea.KeyMsg); ok && key.String() == "esc" {
			if m.identCancel != nil {
				m.identCancel()
			}
			return m.goBack()
		}
		return false, nil
	}
	if msgv.gen != m.identGen {
		return false, nil // stale: from an identify esc already cancelled
	}
	// The identify (success or failure) is over: release the child context
	// enterCmd derived from m.ctx now rather than only on esc, or it leaks
	// its parent-context registration for the rest of the program's life
	// (the fetch and scan handlers do the same on every path).
	if m.identCancel != nil {
		m.identCancel()
	}
	if msgv.err != nil {
		m.err = msgv.err.Error()
		return false, nil
	}
	m.fps = msgv.fps
	m.matches = msgv.matches
	m.vaultByID = msgv.vaultByID
	if len(m.matches) == 0 {
		m.creatingNew = true
		return false, m.goTo(stepScan)
	}
	// MatchProjects (spec §7) returns strong matches first: a weak (dir-only)
	// match is "proposed with a warning, never default" (spec §2.2 item 3),
	// so the cursor starts on "create new" instead of the top row unless
	// that top row is a strong match.
	m.assocIdx = 0
	if m.matches[0].Strength != identity.StrengthStrong {
		m.assocIdx = len(m.matches)
	}
	m.refreshWillRestore()
	return false, m.goTo(stepAssociate)
}

func (m *addModel) updateAssociate(msg tea.Msg) (bool, tea.Cmd) {
	key, ok := msg.(tea.KeyMsg)
	if !ok {
		return false, nil
	}
	total := len(m.matches) + 1 // + "create new"
	switch key.String() {
	case "esc":
		return m.goBack()
	case "up", "k":
		if m.assocIdx > 0 {
			m.assocIdx--
			m.refreshWillRestore()
		}
	case "down", "j":
		if m.assocIdx < total-1 {
			m.assocIdx++
			m.refreshWillRestore()
		}
	case "enter":
		if m.assocIdx == len(m.matches) {
			m.creatingNew = true
			m.forceRandom = true
			return false, m.goTo(stepScan)
		}
		match := m.matches[m.assocIdx]
		m.creatingNew = false
		m.projectID = match.ProjectID
		if p, ok := m.vaultByID[match.ProjectID]; ok {
			m.projectName = p.Name
		}
		m.refreshWillRestore()
		return false, m.goTo(stepScan)
	}
	return false, nil
}

// refreshWillRestore recomputes willRestore for whichever match is currently
// highlighted on the associate screen (spec §2.2 item 3: on a strong match
// the wizard "lists the vault-tracked files that are missing locally as will
// be restored") — called both as the associate step is entered and again
// every time the cursor moves, so the list is accurate for what is on screen
// rather than only for whatever was last chosen.
func (m *addModel) refreshWillRestore() {
	if m.assocIdx >= len(m.matches) {
		m.willRestore = nil
		return
	}
	m.computeWillRestore(m.matches[m.assocIdx].ProjectID)
}

// computeWillRestore lists projectID's vault-tracked files missing from the
// local directory (they will come back as downloads once the plan is
// applied).
func (m *addModel) computeWillRestore(projectID string) {
	m.willRestore = nil
	if projectID == "" {
		return
	}
	tracked, err := m.s.Engine.TrackedPaths(projectID)
	if err != nil {
		return
	}
	relPaths := make([]string, 0, len(tracked))
	for p := range tracked {
		relPaths = append(relPaths, p)
	}
	sort.Strings(relPaths)
	for _, p := range relPaths {
		full := filepath.Join(m.dir, filepath.FromSlash(p))
		if _, err := os.Stat(full); err != nil {
			m.willRestore = append(m.willRestore, p)
		}
	}
}

// updateNewName runs after the candidate selection (spec order: identity ->
// scan -> select -> name -> confirm), so the project id is only minted once
// the user has actually decided what to track — a random id would otherwise
// be wasted every time the wizard is backed out of before confirming.
func (m *addModel) updateNewName(msg tea.Msg) (bool, tea.Cmd) {
	if key, ok := msg.(tea.KeyMsg); ok {
		switch key.String() {
		case "esc":
			return m.goBack()
		case "enter":
			name := strings.TrimSpace(m.nameInput.Value())
			if name == "" {
				name = filepath.Base(m.dir)
			}
			m.projectName = name
			id, err := m.s.ProjectID(m.fps, m.forceRandom)
			if err != nil {
				m.err = err.Error()
				return false, nil
			}
			m.projectID = id
			return false, m.goTo(stepConfirm)
		}
	}
	var cmd tea.Cmd
	m.nameInput, cmd = m.nameInput.Update(msg)
	return false, cmd
}

func (m *addModel) updateScan(msg tea.Msg) (bool, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		if msg.String() == "esc" {
			if m.scanCancel != nil {
				m.scanCancel()
			}
			return m.goBack()
		}
	case spinner.TickMsg:
		var cmd tea.Cmd
		m.scanSpin, cmd = m.scanSpin.Update(msg)
		return false, cmd
	case scanEventMsg:
		if msg.Gen != m.scanGen {
			return false, nil // stale: from a scan esc already cancelled
		}
		m.walked, m.found = msg.Walked, msg.Found
		return false, continueScanCmd(msg)
	case scanDoneMsg:
		if msg.Gen != m.scanGen {
			return false, nil // stale: from a scan esc already cancelled
		}
		// The scan (success or failure) is over: release the child context
		// enterCmd derived from m.ctx now rather than only on esc, or it
		// leaks its parent-context registration for the rest of the
		// program's life (syncView.handleDone does the same on every path).
		if m.scanCancel != nil {
			m.scanCancel()
		}
		if msg.Err != nil {
			m.scanErr = msg.Err
			return false, nil
		}
		m.cand = newCandidateList(msg.Result)
		return false, m.goTo(stepSelect)
	}
	return false, nil
}

func (m *addModel) updateSelect(msg tea.Msg) (bool, tea.Cmd) {
	if key, ok := msg.(tea.KeyMsg); ok && key.String() == "esc" && !m.cand.filtering && !m.cand.adding {
		return m.goBack()
	}
	if key, ok := msg.(tea.KeyMsg); ok && key.String() == "enter" && !m.cand.filtering && !m.cand.adding {
		if m.creatingNew {
			return false, m.goTo(stepNewName)
		}
		return false, m.goTo(stepConfirm)
	}
	cmd := m.cand.update(msg, m.dir)
	return false, cmd
}

func (m *addModel) updateConfirm(msg tea.Msg) (bool, tea.Cmd) {
	key, ok := msg.(tea.KeyMsg)
	if !ok {
		return false, nil
	}
	switch key.String() {
	case "esc":
		return m.goBack()
	case "enter", "y":
		return false, m.goTo(stepApply)
	}
	return false, nil
}

func (m *addModel) startApply() tea.Cmd {
	track := map[sync.ItemKey]bool{}
	for _, r := range m.cand.rows {
		if r.checked {
			track[sync.ItemKey{Project: m.projectID, Path: r.c.Path}] = true
		}
	}
	if len(m.untrack) > 0 {
		if err := m.s.Engine.Untrack(m.ctx, m.projectID, m.untrack); err != nil {
			m.err = err.Error()
		}
	}
	name := m.projectName
	if err := m.s.LinkProject(m.ctx, m.projectID, mapName(m.creatingNew, name), m.dir, m.fps); err != nil {
		m.err = err.Error()
		// Undo the goTo(stepApply) push that got us here, so esc from
		// stepConfirm goes back to stepSelect rather than to itself.
		if len(m.history) > 0 {
			m.history = m.history[:len(m.history)-1]
		}
		m.step = stepConfirm
		return nil
	}
	opts := sync.Options{Mode: sync.ModeSync, Projects: []string{m.projectID}, Track: track}
	m.apply = newSyncView(m.ctx, m.s.Engine, opts, stagePlan, true, true)
	m.apply.SetSize(m.width, m.height)
	return m.apply.Init()
}

// mapName returns name for a new project, or "" (keep the existing vault
// project's name) when associating.
func mapName(creatingNew bool, name string) string {
	if creatingNew {
		return name
	}
	return ""
}

func (m *addModel) updateApply(msg tea.Msg) (bool, tea.Cmd) {
	if key, ok := msg.(tea.KeyMsg); ok && m.apply.stage.terminal() {
		if key.String() == "enter" || key.String() == "esc" {
			m.summary = reportSummary(m.apply.report)
			m.finished = true
			return false, m.goTo(stepSummary)
		}
	}
	sv, cmd := m.apply.Update(msg)
	m.apply = sv
	return false, cmd
}

func (m *addModel) updateSummary(msg tea.Msg) (bool, tea.Cmd) {
	if key, ok := msg.(tea.KeyMsg); ok {
		if key.String() == "enter" || key.String() == "esc" {
			return true, nil
		}
	}
	return false, nil
}

func (m *addModel) View() string {
	var b strings.Builder
	b.WriteString(styles.Title.Render("Add project") + "\n\n")
	if m.err != "" {
		b.WriteString(styles.Error.Render(m.err) + "\n\n")
	}
	switch m.step {
	case stepPath:
		b.WriteString("Project directory:\n" + m.pathInput.View() + "\n\n")
		b.WriteString(styles.Help.Render("enter: continue  esc: cancel"))
	case stepFetch:
		if m.fetchErr != nil {
			b.WriteString(styles.Error.Render("fetch failed: "+m.fetchErr.Error()) + "\n\n")
			b.WriteString(styles.Help.Render("enter: retry  esc: back"))
		} else {
			fmt.Fprintf(&b, "%s fetching remote...\n\n", m.fetchSpin.View())
			b.WriteString(styles.Help.Render("esc: cancel"))
		}
	case stepIdentify:
		b.WriteString("identifying project...\n")
	case stepAssociate:
		b.WriteString(m.viewAssociate())
	case stepNewName:
		b.WriteString("Project name:\n" + m.nameInput.View() + "\n\n")
		b.WriteString(styles.Help.Render("enter: continue  esc: back"))
	case stepScan:
		if m.scanErr != nil {
			b.WriteString(styles.Error.Render(m.scanErr.Error()) + "\n\n")
			b.WriteString(styles.Help.Render("esc: back"))
			break
		}
		fmt.Fprintf(&b, "%s walked %d files, %d candidates — esc to stop\n", m.scanSpin.View(), m.walked, m.found)
	case stepSelect:
		b.WriteString(m.cand.View(m.width))
	case stepConfirm:
		b.WriteString(m.viewConfirm())
	case stepApply:
		b.WriteString(m.apply.View())
	case stepSummary:
		b.WriteString(summaryHeadline(m.apply) + "\n\n")
		b.WriteString(m.summary + "\n\n")
		b.WriteString(styles.Help.Render("enter: back to dashboard"))
	}
	return b.String()
}

// summaryHeadline renders the stepSummary heading. Reaching stepSummary only
// means the pipeline ran to completion, not that it succeeded: an Apply that
// ended in stageError or stageAborted must not read as "Project added."
func summaryHeadline(apply *syncView) string {
	if apply != nil && apply.stage == stageDone {
		return styles.Success.Render("Project added.")
	}
	return styles.Warning.Render("Apply failed or was cancelled — nothing was pushed.")
}

func (m *addModel) viewAssociate() string {
	var b strings.Builder
	b.WriteString("This looks like a project already in the vault:\n\n")
	for i, match := range m.matches {
		cursor := "  "
		if i == m.assocIdx {
			cursor = "> "
		}
		name := match.ProjectID
		if p, ok := m.vaultByID[match.ProjectID]; ok && p.Name != "" {
			name = p.Name
		}
		line := fmt.Sprintf("%sAssociate with %s (%s)", cursor, name, shortMachineID(match.ProjectID))
		if match.Strength == identity.StrengthWeak {
			line += "  " + styles.Warning.Render("(weak match: only the directory name matches)")
		}
		b.WriteString(line + "\n")
	}
	cursor := "  "
	if m.assocIdx == len(m.matches) {
		cursor = "> "
	}
	b.WriteString(cursor + "Create new project anyway\n")
	if m.assocIdx < len(m.matches) && len(m.willRestore) > 0 {
		b.WriteString("\n" + styles.Muted.Render("will be restored:") + "\n")
		b.WriteString(renderWillRestoreList(m.willRestore))
	}
	b.WriteString("\n" + styles.Help.Render("up/down move  enter select  esc back"))
	return b.String()
}

// willRestoreShownMax caps how many of the highlighted match's missing
// tracked files viewAssociate lists individually before collapsing the rest
// into a "+N more" tail.
const willRestoreShownMax = 10

// renderWillRestoreList renders one muted line per path in paths, capped to
// willRestoreShownMax with a "+N more" tail (spec §2.2 item 3).
func renderWillRestoreList(paths []string) string {
	var b strings.Builder
	shown := paths
	more := 0
	if len(shown) > willRestoreShownMax {
		more = len(shown) - willRestoreShownMax
		shown = shown[:willRestoreShownMax]
	}
	for _, p := range shown {
		b.WriteString(styles.Muted.Render("  "+p) + "\n")
	}
	if more > 0 {
		b.WriteString(styles.Muted.Render(fmt.Sprintf("  +%d more", more)) + "\n")
	}
	return b.String()
}

func (m *addModel) viewConfirm() string {
	var b strings.Builder
	n, size := m.cand.selectedStats()
	fmt.Fprintf(&b, "Project: %s\n", m.projectName)
	fmt.Fprintf(&b, "Directory: %s\n", m.dir)
	fmt.Fprintf(&b, "Tracking %d file(s), %s\n", n, formatSize(size))
	if len(m.untrack) > 0 {
		fmt.Fprintf(&b, "Untracking %d previously tracked file(s)\n", len(m.untrack))
	}
	b.WriteString("\n" + styles.Help.Render("enter: apply  esc: back"))
	return b.String()
}

// --- candidate multi-select (spec §2.2 item 3: "custom list") -------------

type candidateRow struct {
	c       scan.Candidate
	checked bool
}

// candidateList is the hand-rolled checklist for scanned candidates: a
// bubbles/list delegate would not give us the checkbox/score-badge/reasons
// columns the spec calls for, so this renders its own rows directly.
type candidateList struct {
	rows    []candidateRow
	cursor  int
	showLow bool

	filtering   bool
	filterInput textinput.Model
	filter      string

	adding   bool
	addInput textinput.Model
	addErr   string

	nested []string
}

func newCandidateList(res *scan.Result) *candidateList {
	cl := &candidateList{}
	if res != nil {
		cl.nested = res.NestedRepos
		for _, c := range res.Candidates {
			cl.rows = append(cl.rows, candidateRow{c: c, checked: c.Preselected || c.Tracked})
		}
	}
	fi := textinput.New()
	fi.Prompt = "/"
	ai := textinput.New()
	ai.Placeholder = "relative/path/to/file"
	cl.filterInput = fi
	cl.addInput = ai
	return cl
}

// visible returns the indices into rows that pass the current filter and
// low-score visibility setting.
func (cl *candidateList) visible() []int {
	var out []int
	for i, r := range cl.rows {
		if !cl.showLow && r.c.Score == scan.ScoreLow && !r.c.Tracked {
			continue
		}
		if cl.filter != "" && !strings.Contains(strings.ToLower(r.c.Path), strings.ToLower(cl.filter)) {
			continue
		}
		out = append(out, i)
	}
	return out
}

func (cl *candidateList) selectedStats() (count int, size int64) {
	for _, r := range cl.rows {
		if r.checked {
			count++
			size += r.c.Size
		}
	}
	return
}

// untracked returns the paths that were tracked in the vault but are no
// longer checked (the user wants them dropped).
func (cl *candidateList) untracked() []string {
	var out []string
	for _, r := range cl.rows {
		if r.c.Tracked && !r.checked {
			out = append(out, r.c.Path)
		}
	}
	return out
}

func (cl *candidateList) update(msg tea.Msg, dir string) tea.Cmd {
	if cl.filtering {
		return cl.updateFiltering(msg)
	}
	if cl.adding {
		return cl.updateAdding(msg, dir)
	}
	key, ok := msg.(tea.KeyMsg)
	if !ok {
		return nil
	}
	vis := cl.visible()
	switch key.String() {
	case "up", "k":
		if cl.cursor > 0 {
			cl.cursor--
		}
	case "down", "j":
		if cl.cursor < len(vis)-1 {
			cl.cursor++
		}
	case " ":
		if cl.cursor >= 0 && cl.cursor < len(vis) {
			i := vis[cl.cursor]
			cl.rows[i].checked = !cl.rows[i].checked
		}
	case "t":
		cl.showLow = !cl.showLow
		if cl.cursor >= len(cl.visible()) {
			cl.cursor = 0
		}
	case "/":
		cl.filtering = true
		cl.filterInput.SetValue(cl.filter)
		cl.filterInput.Focus()
		return textinput.Blink
	case "+":
		cl.adding = true
		cl.addInput.SetValue("")
		cl.addInput.Focus()
		cl.addErr = ""
		return textinput.Blink
	}
	return nil
}

func (cl *candidateList) updateFiltering(msg tea.Msg) tea.Cmd {
	if key, ok := msg.(tea.KeyMsg); ok {
		switch key.String() {
		case "esc":
			cl.filtering = false
			cl.filter = ""
			cl.filterInput.SetValue("")
			cl.cursor = 0
			return nil
		case "enter":
			cl.filtering = false
			cl.cursor = 0
			return nil
		}
	}
	var cmd tea.Cmd
	cl.filterInput, cmd = cl.filterInput.Update(msg)
	cl.filter = cl.filterInput.Value()
	return cmd
}

func (cl *candidateList) updateAdding(msg tea.Msg, dir string) tea.Cmd {
	if key, ok := msg.(tea.KeyMsg); ok {
		switch key.String() {
		case "esc":
			cl.adding = false
			return nil
		case "enter":
			rel := strings.TrimSpace(cl.addInput.Value())
			clean, ok := cleanRelPath(rel)
			if !ok {
				cl.addErr = "enter a path relative to the project, without .."
				return nil
			}
			info, err := os.Stat(filepath.Join(dir, filepath.FromSlash(clean)))
			if err != nil || info.IsDir() {
				cl.addErr = "file not found: " + clean
				return nil
			}
			cl.addExtra(clean, info.Size())
			cl.adding = false
			return nil
		}
	}
	var cmd tea.Cmd
	cl.addInput, cmd = cl.addInput.Update(msg)
	return cmd
}

func (cl *candidateList) addExtra(rel string, size int64) {
	for i := range cl.rows {
		if cl.rows[i].c.Path == rel {
			cl.rows[i].checked = true
			return
		}
	}
	cl.rows = append(cl.rows, candidateRow{
		c: scan.Candidate{
			Path:    rel,
			Size:    size,
			Score:   scan.ScoreMedium,
			Reasons: []string{"added manually"},
		},
		checked: true,
	})
}

// cleanRelPath validates and slash-normalises a user-typed relative path.
func cleanRelPath(rel string) (string, bool) {
	if rel == "" {
		return "", false
	}
	rel = filepath.ToSlash(filepath.Clean(rel))
	if rel == "." || rel == ".." || strings.HasPrefix(rel, "../") || filepath.IsAbs(rel) {
		return "", false
	}
	// filepath.IsAbs is volume-relative on Windows: "/etc/passwd" has no
	// drive letter, so IsAbs reports false there even though the leading
	// slash still roots it at the current drive, and a "C:foo" / "C:\foo"
	// volume-name form would otherwise slip through too. Reject both
	// explicitly so a rooted path never gets joined under the project dir.
	if strings.HasPrefix(rel, "/") || filepath.VolumeName(rel) != "" {
		return "", false
	}
	return rel, true
}

func (cl *candidateList) View(width int) string {
	var b strings.Builder
	b.WriteString("Select files to track:\n\n")
	vis := cl.visible()
	for i, idx := range vis {
		row := cl.rows[idx]
		b.WriteString(renderCandidateRow(row.c, row.checked, i == cl.cursor, width) + "\n")
	}
	if len(vis) == 0 {
		b.WriteString(styles.Muted.Render("(no candidates match)") + "\n")
	}
	for _, n := range cl.nested {
		b.WriteString(styles.Muted.Render(fmt.Sprintf("nested repo (add separately): %s", n)) + "\n")
	}
	b.WriteString("\n")
	if cl.filtering {
		b.WriteString("filter: " + cl.filterInput.View() + "\n")
	} else if cl.adding {
		b.WriteString("add path: " + cl.addInput.View() + "\n")
		if cl.addErr != "" {
			b.WriteString(styles.Error.Render(cl.addErr) + "\n")
		}
	} else {
		low := "show low"
		if cl.showLow {
			low = "hide low"
		}
		b.WriteString(styles.Help.Render(fmt.Sprintf("space toggle  + add path  / filter  t %s  enter continue  esc back", low)))
	}
	return b.String()
}

// renderCandidateRow renders one candidate row: checkbox, path, size, score
// badge and reasons (spec §2.2 item 3).
func renderCandidateRow(c scan.Candidate, checked, cursor bool, width int) string {
	box := "[ ]"
	if checked {
		box = "[x]"
	}
	badge, style := scoreBadge(c.Score)
	reasons := strings.Join(c.Reasons, ", ")
	line := fmt.Sprintf("%s %-40s %10s %s  %s", box, truncatePath(c.Path, 40), formatSize(c.Size), style.Render(badge), styles.Muted.Render(reasons))
	if cursor {
		line = styles.Selected.Render(line)
	}
	return line
}

// truncatePath keeps the last n runes of p, prefixed with an ellipsis when it
// had to cut anything. Slicing by rune (rather than byte, as a naive
// p[len(p)-n:] would) avoids splitting a multi-byte character in half and
// printing garbage for a non-ASCII path.
func truncatePath(p string, n int) string {
	r := []rune(p)
	if len(r) <= n {
		return p
	}
	if n <= 1 {
		return string(r[len(r)-n:])
	}
	return "…" + string(r[len(r)-(n-1):])
}

// scoreBadge renders a scan.Score as a short label and style.
func scoreBadge(s scan.Score) (string, lipgloss.Style) {
	switch s {
	case scan.ScoreHigh:
		return "high", styles.BadgeBad
	case scan.ScoreMedium:
		return "medium", styles.BadgeWarn
	default:
		return "low", styles.BadgeMuted
	}
}

// formatSize renders a byte count for the candidate list and confirm step.
func formatSize(n int64) string { return config.FormatSize(n) }

// ErrAborted is returned by RunAddProject when the wizard is left before it
// reaches its summary step (e.g. esc back out at the path step, or ctrl+c),
// as opposed to running to completion (whether or not Apply itself
// succeeded — see summaryHeadline). It wraps context.Canceled so that a
// caller already mapping a cancelled interactive step to a non-zero exit
// code needs no extra wiring; errors.Is(err, ErrAborted) still identifies it
// precisely.
var ErrAborted = fmt.Errorf("add-project wizard cancelled: %w", context.Canceled)

// finishErr is RunAddProject's decision, once its program has exited
// cleanly, of what to report: nil once the wizard actually finished
// (finished is set on entering stepSummary, regardless of Apply's outcome),
// ErrAborted when it was left earlier.
func finishErr(finished bool) error {
	if finished {
		return nil
	}
	return ErrAborted
}

// RunAddProject runs the add-project wizard as its own program (spec §2.2
// item 3; used by the `add` CLI command).
func RunAddProject(ctx context.Context, s *app.Session, dir string) error {
	initBackground()
	if ctx == nil {
		ctx = context.Background()
	}
	if s == nil {
		return errors.New("tui.RunAddProject: nil session")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	m := newAddModel(ctx, s, dir)
	p := tea.NewProgram(&addProgram{m: m}, tea.WithContext(ctx), tea.WithAltScreen())
	_, err := p.Run()
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return finishErr(m.finished)
}

// addProgram adapts addModel (whose Update returns (bool, tea.Cmd)) to the
// tea.Model interface for standalone use.
type addProgram struct {
	m *addModel

	quitConfirm bool
}

func (p *addProgram) Init() tea.Cmd { return p.m.Init() }

func (p *addProgram) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if k, ok := msg.(tea.KeyMsg); ok && k.String() == "ctrl+c" {
		if p.applyRunning() {
			if !p.quitConfirm {
				p.quitConfirm = true
				return p, nil
			}
			p.m.apply.Cancel()
		}
		return p, tea.Quit
	}
	if _, ok := msg.(tea.KeyMsg); ok {
		p.quitConfirm = false
	}
	done, cmd := p.m.Update(msg)
	if done {
		return p, tea.Quit
	}
	return p, cmd
}

// applyRunning reports whether the wizard is in the middle of writing
// changes (Apply or its finishing Push) — the only moment ctrl+c needs a
// second press to confirm (spec §2.2), matching rootModel.applyRunning.
func (p *addProgram) applyRunning() bool {
	if p.m.step != stepApply || p.m.apply == nil {
		return false
	}
	return p.m.apply.stage == stageApply || p.m.apply.stage == stagePush
}

func (p *addProgram) View() string {
	v := p.m.View()
	if p.quitConfirm {
		v += "\n" + styles.Warning.Render("an operation is running — press ctrl+c again to quit anyway")
	}
	return v
}
