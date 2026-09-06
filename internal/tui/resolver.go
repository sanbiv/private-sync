package tui

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/sanbiv/private-sync/internal/app"
	"github.com/sanbiv/private-sync/internal/merge"
	"github.com/sanbiv/private-sync/internal/sync"
)

// mergeTempSuffix names the scratch file written next to a conflicted file
// while it is open in $EDITOR (spec §2.2 item 5).
const mergeTempSuffix = ".psv-merge"

// Resolver is the conflict resolver screen (spec §2.2 item 5). It is exported
// as a reusable component: ResolveConflicts drives it as its own
// tea.Program, and syncView embeds one to resolve conflicts found mid-sync
// without leaving the enclosing program.
type Resolver struct {
	plan  *sync.Plan
	items []*sync.Item
	idx   int
	res   sync.Resolutions

	done       bool
	aborted    bool
	standalone bool

	vp            viewport.Model
	width, height int
	msg           string

	dotenv *dotenvState

	editing bool
}

// dotenvChoice tracks the user's answer for one dotenv hunk (spec §8, §2.2 "k").
type dotenvChoice struct {
	hunk   merge.Hunk
	side   merge.Side
	chosen bool
}

// dotenvState is the per-key resolution sub-mode for a dotenv conflict.
type dotenvState struct {
	hunks  []dotenvChoice
	cursor int
}

func newDotenvState(r *merge.Result) *dotenvState {
	ds := &dotenvState{}
	for _, h := range r.Hunks {
		ds.hunks = append(ds.hunks, dotenvChoice{hunk: h})
	}
	return ds
}

func (ds *dotenvState) allChosen() bool {
	for _, h := range ds.hunks {
		if !h.chosen {
			return false
		}
	}
	return true
}

// editDoneMsg reports that $EDITOR returned for a conflict-marker edit.
type editDoneMsg struct {
	err  error
	path string
	key  sync.ItemKey
}

// NewResolver builds a resolver over every item of plan that still needs a
// resolution not already present in res. res is mutated in place as the user
// answers; pass sync.Resolutions{} for a fresh run. A plan with nothing left
// to resolve yields a Resolver that is already Done().
func NewResolver(plan *sync.Plan, res sync.Resolutions) *Resolver {
	if res == nil {
		res = sync.Resolutions{}
	}
	m := &Resolver{
		plan: plan,
		res:  res,
		vp:   viewport.New(80, 16),
	}
	if plan != nil {
		for _, it := range plan.Items() {
			if !it.NeedsResolution {
				continue
			}
			if _, ok := res[it.Key]; ok {
				continue
			}
			m.items = append(m.items, it)
		}
	}
	if len(m.items) == 0 {
		m.done = true
	}
	m.updateViewport()
	return m
}

// Init satisfies tea.Model.
func (m *Resolver) Init() tea.Cmd { return nil }

// Done reports whether every item has an answer (or the run was aborted).
func (m *Resolver) Done() bool { return m.done }

// Aborted reports whether the user pressed A (nothing should be applied).
func (m *Resolver) Aborted() bool { return m.aborted }

// Resolutions returns the answers collected so far.
func (m *Resolver) Resolutions() sync.Resolutions { return m.res }

// SetSize resizes the diff viewport.
func (m *Resolver) SetSize(width, height int) {
	m.width, m.height = width, height
	m.vp.Width = width
	h := height - 6
	if h < 4 {
		h = 4
	}
	m.vp.Height = h
}

// updateViewport refreshes the diff/preview shown for the current item.
func (m *Resolver) updateViewport() {
	it := m.currentItem()
	if it == nil {
		m.vp.SetContent("")
		return
	}
	m.vp.SetContent(diffContent(it))
	m.vp.GotoTop()
}

// diffContent renders the body of the resolver's viewport for one item: the
// clean merge preview when one is available, a line diff of local vs. vault
// for a content conflict, or a plain description for conflicts that have no
// diff at all (spec §2.2 item 5).
func diffContent(it *sync.Item) string {
	switch {
	case it.Action == sync.ActionDeleteRemote:
		return "This file was removed locally.\n\nConfirm to delete it from the vault everywhere, or skip to keep it tracked there."
	case it.Conflict == sync.ConflictModifyDelete:
		return "Deleted in the vault, but present locally with different content.\n\nl = keep (re-upload)   r = delete (trash the local copy)"
	case it.Merge != nil && it.Merge.Clean:
		diff := renderDiff(merge.LineDiff(it.LocalText, it.HeadText))
		preview := styles.Success.Render("Clean automatic merge — press m to use it:") + "\n\n" + string(it.Merge.Merged)
		return diff + "\n" + styles.Muted.Render(strings.Repeat("─", 40)) + "\n\n" + preview
	case it.Merge != nil && it.Merge.Kind == merge.KindBinary:
		return fmt.Sprintf("Binary conflict: local %d bytes, vault %d bytes. Choose l or r.", len(it.LocalText), len(it.HeadText))
	case it.Merge != nil:
		return renderDiff(merge.LineDiff(it.LocalText, it.HeadText))
	default:
		return it.Reason
	}
}

// renderDiff colours a merge.LineDiff for the resolver's viewport (+ green,
// - red, context muted).
func renderDiff(ops []merge.DiffOp) string {
	var b strings.Builder
	for _, op := range ops {
		switch op.Kind {
		case '+':
			b.WriteString(styles.DiffAdd.Render("+ " + op.Text))
		case '-':
			b.WriteString(styles.DiffDel.Render("- " + op.Text))
		default:
			b.WriteString(styles.DiffCtx.Render("  " + op.Text))
		}
		b.WriteString("\n")
	}
	return b.String()
}

func (m *Resolver) currentItem() *sync.Item {
	if m.idx < 0 || m.idx >= len(m.items) {
		return nil
	}
	return m.items[m.idx]
}

// finishCmd returns tea.Quit once a standalone run has finished, so
// ResolveConflicts's own tea.Program exits; embedded runs (standalone
// false) never quit the enclosing program themselves.
func (m *Resolver) finishCmd() tea.Cmd {
	if m.done && m.standalone {
		return tea.Quit
	}
	return nil
}

// advance moves to the next item that still lacks a resolution (L/R can
// resolve several at once) and reports done when none remain.
func (m *Resolver) advance() tea.Cmd {
	m.idx++
	for m.idx < len(m.items) {
		if _, ok := m.res[m.items[m.idx].Key]; ok {
			m.idx++
			continue
		}
		break
	}
	if m.idx >= len(m.items) {
		m.done = true
	}
	m.dotenv = nil
	m.msg = ""
	m.updateViewport()
	return m.finishCmd()
}

// singleStrategy resolves just the current item as strategy s would (local:
// keep for modify/delete, remote: delete for modify/delete and for a
// concurrent head the other machine deleted, confirm for delete-remote),
// reusing sync.ApplyStrategy's own per-item rules instead of duplicating
// them: it runs the strategy over a throwaway one-item plan.
func (m *Resolver) singleStrategy(it *sync.Item, s sync.Strategy) bool {
	fake := &sync.Plan{Projects: []sync.ProjectPlan{{ID: it.Key.Project, Items: []sync.Item{*it}}}}
	sync.ApplyStrategy(fake, s, m.res)
	_, ok := m.res[it.Key]
	return ok
}

// Update satisfies tea.Model.
func (m *Resolver) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.SetSize(msg.Width, msg.Height)
		return m, nil
	case editDoneMsg:
		return m.handleEditDone(msg)
	}

	// Standalone use (ResolveConflicts) is this program's own tea.Model, so
	// nothing else intercepts ctrl+c the way rootModel does for every screen
	// embedding a Resolver inside the dashboard program; without this the
	// only way out of the standalone resolver is A (spec §2.2: global
	// ctrl+c = quit). An embedded Resolver never sees ctrl+c at all — the
	// enclosing rootModel.Update returns before dispatching to it — so the
	// standalone guard here cannot double-handle that case.
	if k, ok := msg.(tea.KeyMsg); ok && k.String() == "ctrl+c" && m.standalone {
		m.aborted = true
		m.done = true
		return m, tea.Quit
	}

	if m.done || m.editing {
		return m, nil
	}

	if m.dotenv != nil {
		return m.updateDotenv(msg)
	}

	key, ok := msg.(tea.KeyMsg)
	if !ok {
		var cmd tea.Cmd
		m.vp, cmd = m.vp.Update(msg)
		return m, cmd
	}

	it := m.currentItem()
	if it == nil {
		return m, nil
	}

	switch key.String() {
	case "A":
		m.aborted = true
		m.done = true
		return m, m.finishCmd()
	case "s":
		m.res[it.Key] = sync.Resolution{Kind: sync.ChooseSkip}
		return m, m.advance()
	case "L":
		sync.ApplyStrategy(m.plan, sync.StrategyLocal, m.res)
		return m, m.advance()
	case "R":
		sync.ApplyStrategy(m.plan, sync.StrategyRemote, m.res)
		return m, m.advance()
	case "c", "enter":
		if it.Action == sync.ActionDeleteRemote {
			m.res[it.Key] = sync.Resolution{Kind: sync.ChooseConfirm}
			return m, m.advance()
		}
		return m, nil
	case "m":
		if it.Merge != nil && it.Merge.Clean {
			m.res[it.Key] = sync.Resolution{Kind: sync.ChooseMerged}
			return m, m.advance()
		}
		m.msg = "no clean merge available for this file"
		return m, nil
	case "l":
		if it.Action == sync.ActionDeleteRemote {
			m.msg = "enter/c confirms the deletion, s keeps the file tracked"
			return m, nil
		}
		if m.singleStrategy(it, sync.StrategyLocal) {
			return m, m.advance()
		}
		m.msg = "no local copy to keep"
		return m, nil
	case "r":
		if it.Action == sync.ActionDeleteRemote {
			m.msg = "enter/c confirms the deletion, s keeps the file tracked"
			return m, nil
		}
		if m.singleStrategy(it, sync.StrategyRemote) {
			return m, m.advance()
		}
		m.msg = "no single remote version to take"
		return m, nil
	case "k":
		if it.Merge != nil && it.Merge.Kind == merge.KindDotenv && !it.Merge.Clean && len(it.Merge.Hunks) > 0 {
			m.dotenv = newDotenvState(it.Merge)
			m.msg = ""
		} else {
			// The len(Hunks) > 0 guard also covers a dotenv Result with
			// Clean=false but zero Hunks (conflicting non-dotenv sections of
			// an otherwise dotenv-shaped file): without it, newDotenvState
			// would build a dotenvState with an empty hunks slice and "l"/"r"
			// in updateDotenv would index ds.hunks[ds.cursor] out of bounds.
			m.msg = "per-key resolution is only available for dotenv conflicts"
		}
		return m, nil
	case "e":
		return m.startEdit(it)
	default:
		var cmd tea.Cmd
		m.vp, cmd = m.vp.Update(msg)
		return m, cmd
	}
}

func (m *Resolver) updateDotenv(msg tea.Msg) (tea.Model, tea.Cmd) {
	key, ok := msg.(tea.KeyMsg)
	if !ok {
		return m, nil
	}
	ds := m.dotenv
	switch key.String() {
	case "esc":
		m.dotenv = nil
		m.msg = ""
	case "up", "k":
		if ds.cursor > 0 {
			ds.cursor--
		}
	case "down", "j":
		if ds.cursor < len(ds.hunks)-1 {
			ds.cursor++
		}
	case "l":
		ds.hunks[ds.cursor].chosen = true
		ds.hunks[ds.cursor].side = merge.SideLocal
		if ds.cursor < len(ds.hunks)-1 {
			ds.cursor++
		}
	case "r":
		ds.hunks[ds.cursor].chosen = true
		ds.hunks[ds.cursor].side = merge.SideRemote
		if ds.cursor < len(ds.hunks)-1 {
			ds.cursor++
		}
	case "enter":
		if !ds.allChosen() {
			m.msg = "choose local or remote for every key first"
			return m, nil
		}
		it := m.currentItem()
		choices := make([]merge.Choice, len(ds.hunks))
		for i, h := range ds.hunks {
			choices[i] = merge.Choice{Side: h.side}
		}
		merged, err := merge.Resolve(it.Merge, choices)
		if err != nil {
			m.msg = fmt.Sprintf("merge: %v", err)
			return m, nil
		}
		m.res[it.Key] = sync.Resolution{Kind: sync.ChooseCustom, Content: merged}
		m.dotenv = nil
		return m, m.advance()
	}
	return m, nil
}

// startEdit writes the conflict-marker file next to the real file (inside
// the project directory) and opens $EDITOR on it via tea.ExecProcess.
func (m *Resolver) startEdit(it *sync.Item) (tea.Model, tea.Cmd) {
	if it.Merge == nil {
		m.msg = "no diff available to edit"
		return m, nil
	}
	editor := strings.TrimSpace(os.Getenv("EDITOR"))
	if editor == "" {
		m.msg = "set $EDITOR to edit conflicts"
		return m, nil
	}
	pp := findProjectPlan(m.plan, it.Key.Project)
	if pp == nil || pp.Path == "" {
		m.msg = "project path unavailable"
		return m, nil
	}
	full := filepath.Join(pp.Path, filepath.FromSlash(it.Key.Path))
	mergePath := full + mergeTempSuffix
	data := merge.RenderMarkers(it.Merge, "local", remoteLabelFor(it))
	if err := os.WriteFile(mergePath, data, 0o600); err != nil {
		m.msg = fmt.Sprintf("write merge file: %v", err)
		return m, nil
	}
	m.editing = true
	key := it.Key
	cmd := editorCommand(editor, mergePath)
	return m, tea.ExecProcess(cmd, func(err error) tea.Msg {
		return editDoneMsg{err: err, path: mergePath, key: key}
	})
}

// editorCommand builds the *exec.Cmd for $EDITOR against mergePath, splitting
// the value on whitespace first: a bare exec.Command(editor, mergePath)
// treats the whole $EDITOR string as the binary name, so common values that
// carry arguments (`code --wait`, `vim -u NONE`, `emacsclient -t`) fail with
// "executable file not found" instead of running the intended editor.
// editor must already be non-blank (checked by the caller), so Fields always
// yields at least one element.
func editorCommand(editor, mergePath string) *exec.Cmd {
	parts := strings.Fields(editor)
	args := append(append([]string(nil), parts[1:]...), mergePath)
	return exec.Command(parts[0], args...)
}

func (m *Resolver) handleEditDone(msg editDoneMsg) (tea.Model, tea.Cmd) {
	m.editing = false
	if msg.err != nil {
		os.Remove(msg.path)
		m.msg = fmt.Sprintf("editor failed: %v", msg.err)
		return m, nil
	}
	content, err := os.ReadFile(msg.path)
	os.Remove(msg.path)
	if err != nil {
		m.msg = fmt.Sprintf("read merge file: %v", err)
		return m, nil
	}
	if merge.HasMarkers(content) {
		m.msg = "still contains conflict markers; resolve them and press e again"
		return m, nil
	}
	m.res[msg.key] = sync.Resolution{Kind: sync.ChooseCustom, Content: content}
	return m, m.advance()
}

// remoteLabelFor names the "remote" side of the conflict markers.
func remoteLabelFor(it *sync.Item) string {
	if it.Conflict == sync.ConflictConcurrent && it.Remote != nil && it.Remote.Machine != "" {
		return shortMachineID(it.Remote.Machine)
	}
	if it.Head != nil && it.Head.Machine != "" {
		return shortMachineID(it.Head.Machine)
	}
	return "remote"
}

func shortMachineID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// findProjectPlan looks up a project's plan (for its local path) by id.
func findProjectPlan(p *sync.Plan, id string) *sync.ProjectPlan {
	if p == nil {
		return nil
	}
	for i := range p.Projects {
		if p.Projects[i].ID == id {
			return &p.Projects[i]
		}
	}
	return nil
}

// View satisfies tea.Model.
func (m *Resolver) View() string {
	if m.done {
		if m.aborted {
			return styles.Warning.Render("Sync aborted — nothing applied.")
		}
		return styles.Success.Render("All conflicts resolved.")
	}
	it := m.currentItem()
	if it == nil {
		return ""
	}
	header := styles.Title.Render(fmt.Sprintf("Conflict %d/%d — %s", m.idx+1, len(m.items), it.Key.Path))
	sub := styles.Subtitle.Render(conflictLabel(it) + ": " + it.Reason)
	var body string
	if m.dotenv != nil {
		body = m.dotenvView()
	} else {
		body = m.vp.View()
	}
	var msgLine string
	if m.msg != "" {
		msgLine = styles.Warning.Render(m.msg)
	}
	help := styles.Help.Render(helpLineFor(it, m.dotenv != nil))
	parts := []string{header, sub, body}
	if msgLine != "" {
		parts = append(parts, msgLine)
	}
	parts = append(parts, help)
	return strings.Join(parts, "\n")
}

func (m *Resolver) dotenvView() string {
	var b strings.Builder
	for i, h := range m.dotenv.hunks {
		cursor := "  "
		if i == m.dotenv.cursor {
			cursor = "> "
		}
		status := "?"
		if h.chosen {
			status = h.side.String()
		}
		fmt.Fprintf(&b, "%s%-24s local=%-20s remote=%-20s [%s]\n",
			cursor, h.hunk.Key, dotenvVal(h.hunk.Local), dotenvVal(h.hunk.Remote), status)
	}
	return b.String()
}

func dotenvVal(b []byte) string {
	if b == nil {
		return "(absent)"
	}
	s := string(b)
	if len(s) > 24 {
		s = s[:24] + "…"
	}
	return s
}

// conflictLabel names the kind of conflict for the header line.
func conflictLabel(it *sync.Item) string {
	switch {
	case it.Action == sync.ActionDeleteRemote:
		return "deleted locally"
	case it.Conflict == sync.ConflictModifyDelete:
		return "modified/deleted"
	case it.Conflict == sync.ConflictNoBase:
		return "no common base"
	case it.Conflict == sync.ConflictConcurrent:
		return "concurrent edits"
	case it.Merge != nil && it.Merge.Kind == merge.KindBinary:
		return "binary conflict"
	default:
		return "content conflict"
	}
}

// helpLineFor lists the keys valid for the current item.
func helpLineFor(it *sync.Item, inDotenv bool) string {
	if inDotenv {
		return "l local  r remote  up/down move  enter apply  esc cancel"
	}
	switch {
	case it.Action == sync.ActionDeleteRemote:
		return "enter/c confirm delete  s skip  A abort"
	case it.Conflict == sync.ConflictModifyDelete:
		return "l keep  r delete  s skip  A abort"
	default:
		keys := []string{"l local", "r remote"}
		if it.Merge != nil && it.Merge.Clean {
			keys = append(keys, "m merged")
		}
		if it.Merge != nil && it.Merge.Kind == merge.KindDotenv {
			keys = append(keys, "k per-key")
		}
		keys = append(keys, "e edit", "s skip", "L all-local", "R all-remote", "A abort")
		return strings.Join(keys, "  ")
	}
}

// ResolveConflicts runs the conflict resolver for a plan as its own
// tea.Program (spec §2.2 item 5). s is accepted for symmetry with the rest of
// the front end's entry points; the resolver only needs the plan's own
// decrypted contents and paths.
func ResolveConflicts(ctx context.Context, s *app.Session, p *sync.Plan) (res sync.Resolutions, aborted bool, err error) {
	initBackground()
	if ctx == nil {
		ctx = context.Background()
	}
	if p == nil {
		return nil, false, errors.New("tui.ResolveConflicts: nil plan")
	}
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	m := NewResolver(p, sync.Resolutions{})
	m.standalone = true
	if m.Done() {
		return m.Resolutions(), m.Aborted(), nil
	}
	prog := tea.NewProgram(m, tea.WithContext(ctx))
	final, runErr := prog.Run()
	fm, _ := final.(*Resolver)
	if fm == nil {
		fm = m
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return fm.Resolutions(), fm.Aborted(), ctxErr
	}
	if runErr != nil {
		return fm.Resolutions(), fm.Aborted(), runErr
	}
	return fm.Resolutions(), fm.Aborted(), nil
}
