package tui

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sanbiv/private-sync/internal/execx"
	"github.com/sanbiv/private-sync/internal/merge"
	"github.com/sanbiv/private-sync/internal/sync"
)

func textConflictItem() sync.Item {
	base := []byte("line1\nline2\nline3\n")
	local := []byte("line1\nLOCAL-CHANGE\nline3\n")
	remote := []byte("line1\nREMOTE-CHANGE\nline3\n")
	r := merge.ThreeWay("file.txt", base, local, remote)
	return sync.Item{
		Key:             sync.ItemKey{Project: "p1", Path: "file.txt"},
		Action:          sync.ActionConflict,
		Conflict:        sync.ConflictContent,
		Local:           &sync.FileRef{Blob: "localblob"},
		Head:            &sync.FileRef{Blob: "headblob"},
		Merge:           r,
		LocalText:       local,
		HeadText:        remote,
		NeedsResolution: true,
		Reason:          "modified locally and in the vault",
	}
}

func modifyDeleteItem(path string) sync.Item {
	return sync.Item{
		Key:             sync.ItemKey{Project: "p1", Path: path},
		Action:          sync.ActionConflict,
		Conflict:        sync.ConflictModifyDelete,
		Local:           &sync.FileRef{Blob: "localblob"},
		LocalText:       []byte("still here"),
		NeedsResolution: true,
		Reason:          "deleted in the vault but modified locally",
	}
}

func deleteRemoteItem(path string) sync.Item {
	return sync.Item{
		Key:             sync.ItemKey{Project: "p1", Path: path},
		Action:          sync.ActionDeleteRemote,
		NeedsResolution: true,
		Reason:          "deleted locally: propagating the deletion to the vault",
	}
}

func planWith(items ...sync.Item) *sync.Plan {
	return &sync.Plan{Projects: []sync.ProjectPlan{{ID: "p1", Path: "/does/not/matter", Items: items}}}
}

func TestNewResolverSkipsAlreadyResolved(t *testing.T) {
	it := textConflictItem()
	res := sync.Resolutions{it.Key: {Kind: sync.ChooseLocal}}
	m := NewResolver(planWith(it), res)
	if !m.Done() {
		t.Fatalf("Resolver should be Done() when every item already has a resolution")
	}
	if len(m.items) != 0 {
		t.Fatalf("items = %v, want none (already resolved)", m.items)
	}
}

func TestNewResolverNothingToResolve(t *testing.T) {
	plan := &sync.Plan{Projects: []sync.ProjectPlan{{ID: "p1", Items: []sync.Item{{Action: sync.ActionInSync}}}}}
	m := NewResolver(plan, nil)
	if !m.Done() {
		t.Fatalf("Resolver over a plan with nothing to resolve should be immediately Done()")
	}
	if m.Aborted() {
		t.Fatalf("an empty resolver is not an abort")
	}
}

func key(s string) tea.KeyMsg {
	if len(s) == 1 {
		switch s {
		case " ":
			return tea.KeyMsg{Type: tea.KeySpace}
		}
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
	}
	switch s {
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "up":
		return tea.KeyMsg{Type: tea.KeyUp}
	case "down":
		return tea.KeyMsg{Type: tea.KeyDown}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
}

func TestResolverContentConflictLocalAndRemote(t *testing.T) {
	it := textConflictItem()
	m := NewResolver(planWith(it), sync.Resolutions{})

	next, _ := m.Update(key("l"))
	m = next.(*Resolver)
	if !m.Done() {
		t.Fatalf("resolver should be done after resolving its only item")
	}
	if m.Aborted() {
		t.Fatalf("resolving with l should not count as aborting")
	}
	got := m.Resolutions()[it.Key]
	if got.Kind != sync.ChooseLocal {
		t.Fatalf("Resolutions()[key] = %v, want ChooseLocal", got.Kind)
	}
}

func TestResolverContentConflictRemote(t *testing.T) {
	it := textConflictItem()
	m := NewResolver(planWith(it), sync.Resolutions{})

	next, _ := m.Update(key("r"))
	m = next.(*Resolver)
	got := m.Resolutions()[it.Key]
	if got.Kind != sync.ChooseRemote {
		t.Fatalf("Resolutions()[key] = %v, want ChooseRemote", got.Kind)
	}
}

func TestResolverSkip(t *testing.T) {
	it := textConflictItem()
	m := NewResolver(planWith(it), sync.Resolutions{})

	next, _ := m.Update(key("s"))
	m = next.(*Resolver)
	if !m.Done() {
		t.Fatalf("resolver should be done after skipping its only item")
	}
	got := m.Resolutions()[it.Key]
	if got.Kind != sync.ChooseSkip {
		t.Fatalf("Resolutions()[key] = %v, want ChooseSkip", got.Kind)
	}
}

func TestResolverAbort(t *testing.T) {
	items := []sync.Item{textConflictItem(), modifyDeleteItem("other.txt")}
	m := NewResolver(planWith(items...), sync.Resolutions{})

	next, _ := m.Update(key("A"))
	m = next.(*Resolver)
	if !m.Done() || !m.Aborted() {
		t.Fatalf("A should abort: Done()=%v Aborted()=%v, want true/true", m.Done(), m.Aborted())
	}
	if len(m.Resolutions()) != 0 {
		t.Fatalf("Resolutions() after abort = %v, want empty (nothing decided)", m.Resolutions())
	}
}

func TestResolverModifyDeleteKeepAndDelete(t *testing.T) {
	keep := modifyDeleteItem("keep.txt")
	del := modifyDeleteItem("del.txt")

	m := NewResolver(planWith(keep, del), sync.Resolutions{})
	next, _ := m.Update(key("l"))
	m = next.(*Resolver)
	next, _ = m.Update(key("r"))
	m = next.(*Resolver)

	if !m.Done() {
		t.Fatalf("resolver should be done after answering both items")
	}
	res := m.Resolutions()
	if got := res[keep.Key].Kind; got != sync.ChooseKeep {
		t.Errorf("Resolutions()[keep] = %v, want ChooseKeep", got)
	}
	if got := res[del.Key].Kind; got != sync.ChooseDelete {
		t.Errorf("Resolutions()[del] = %v, want ChooseDelete", got)
	}
}

func TestResolverDeleteRemoteConfirmAndSkip(t *testing.T) {
	confirmed := deleteRemoteItem("gone.txt")
	skipped := deleteRemoteItem("kept.txt")

	m := NewResolver(planWith(confirmed, skipped), sync.Resolutions{})
	next, _ := m.Update(key("enter"))
	m = next.(*Resolver)
	next, _ = m.Update(key("s"))
	m = next.(*Resolver)

	res := m.Resolutions()
	if got := res[confirmed.Key].Kind; got != sync.ChooseConfirm {
		t.Errorf("Resolutions()[confirmed] = %v, want ChooseConfirm", got)
	}
	if got := res[skipped.Key].Kind; got != sync.ChooseSkip {
		t.Errorf("Resolutions()[skipped] = %v, want ChooseSkip", got)
	}
}

func TestResolverMergedOnlyWhenClean(t *testing.T) {
	it := textConflictItem() // not clean: pressing m must refuse
	m := NewResolver(planWith(it), sync.Resolutions{})
	next, _ := m.Update(key("m"))
	m = next.(*Resolver)
	if m.Done() {
		t.Fatalf("m on an unclean merge should not resolve the item")
	}
	if _, ok := m.Resolutions()[it.Key]; ok {
		t.Fatalf("m on an unclean merge should not record a resolution")
	}
	if m.msg == "" {
		t.Fatalf("expected a message explaining why m was refused")
	}
}

func TestResolverMergedWhenClean(t *testing.T) {
	it := textConflictItem()
	it.Merge = &merge.Result{Kind: merge.KindText, Clean: true, Merged: []byte("merged content\n")}
	m := NewResolver(planWith(it), sync.Resolutions{})

	next, _ := m.Update(key("m"))
	m = next.(*Resolver)
	if !m.Done() {
		t.Fatalf("m on a clean merge should resolve the only item")
	}
	got := m.Resolutions()[it.Key]
	if got.Kind != sync.ChooseMerged {
		t.Fatalf("Resolutions()[key] = %v, want ChooseMerged", got.Kind)
	}
}

// TestDiffContentCleanMergeShowsDiffAndPreview is the regression for spec
// §2.2 item 5 ("plus the auto-merge if clean"): a clean auto-merge used to
// show only the merged preview, hiding the local-vs-remote diff that every
// other conflict kind gets.
func TestDiffContentCleanMergeShowsDiffAndPreview(t *testing.T) {
	it := textConflictItem()
	it.Merge = &merge.Result{Kind: merge.KindText, Clean: true, Merged: []byte("merged content\n")}

	content := diffContent(&it)
	if !strings.Contains(content, "LOCAL-CHANGE") || !strings.Contains(content, "REMOTE-CHANGE") {
		t.Fatalf("diffContent for a clean merge should still show the local-vs-remote diff, got %q", content)
	}
	if !strings.Contains(content, "merged content") {
		t.Fatalf("diffContent for a clean merge should also show the merged preview, got %q", content)
	}
	if !strings.Contains(content, "press m to use it") {
		t.Fatalf("diffContent should keep the existing merged-preview call to action, got %q", content)
	}
}

func TestResolverBulkLocalAndRemote(t *testing.T) {
	a := textConflictItem()
	a.Key.Path = "a.txt"
	b := textConflictItem()
	b.Key.Path = "b.txt"
	m := NewResolver(planWith(a, b), sync.Resolutions{})

	next, _ := m.Update(key("L"))
	m = next.(*Resolver)
	if !m.Done() {
		t.Fatalf("L should resolve every remaining item at once")
	}
	res := m.Resolutions()
	if res[a.Key].Kind != sync.ChooseLocal || res[b.Key].Kind != sync.ChooseLocal {
		t.Fatalf("Resolutions() = %+v, want both ChooseLocal", res)
	}
}

func TestResolverBulkRemoteMidway(t *testing.T) {
	a := textConflictItem()
	a.Key.Path = "a.txt"
	b := textConflictItem()
	b.Key.Path = "b.txt"
	c := textConflictItem()
	c.Key.Path = "c.txt"
	m := NewResolver(planWith(a, b, c), sync.Resolutions{})

	// Answer the first item individually, then let R sweep the rest.
	next, _ := m.Update(key("l"))
	m = next.(*Resolver)
	next, _ = m.Update(key("R"))
	m = next.(*Resolver)

	if !m.Done() {
		t.Fatalf("resolver should be done once R answers everything left")
	}
	res := m.Resolutions()
	if res[a.Key].Kind != sync.ChooseLocal {
		t.Errorf("Resolutions()[a] = %v, want the individually chosen ChooseLocal", res[a.Key].Kind)
	}
	if res[b.Key].Kind != sync.ChooseRemote || res[c.Key].Kind != sync.ChooseRemote {
		t.Errorf("Resolutions() = %+v, want b and c ChooseRemote", res)
	}
}

// --- dotenv per-key resolution ("k") ----------------------------------------

func dotenvConflictItem() sync.Item {
	base := []byte("A=1\nB=2\n")
	local := []byte("A=2\nB=2\n")
	remote := []byte("A=3\nB=2\n")
	r := merge.ThreeWay(".env", base, local, remote)
	return sync.Item{
		Key:             sync.ItemKey{Project: "p1", Path: ".env"},
		Action:          sync.ActionConflict,
		Conflict:        sync.ConflictContent,
		Local:           &sync.FileRef{Blob: "l"},
		Head:            &sync.FileRef{Blob: "h"},
		Merge:           r,
		LocalText:       local,
		HeadText:        remote,
		NeedsResolution: true,
	}
}

func TestResolverDotenvPerKey(t *testing.T) {
	it := dotenvConflictItem()
	if it.Merge.Kind != merge.KindDotenv || it.Merge.Clean {
		t.Fatalf("fixture is not an unclean dotenv conflict: kind=%v clean=%v", it.Merge.Kind, it.Merge.Clean)
	}
	if len(it.Merge.Hunks) != 1 || it.Merge.Hunks[0].Key != "A" {
		t.Fatalf("fixture should have one hunk for key A, got %+v", it.Merge.Hunks)
	}

	m := NewResolver(planWith(it), sync.Resolutions{})
	next, _ := m.Update(key("k"))
	m = next.(*Resolver)
	if m.dotenv == nil {
		t.Fatalf("k should open the dotenv per-key sub-mode")
	}

	next, _ = m.Update(key("l")) // choose local (A=2) for the only hunk
	m = next.(*Resolver)
	next, _ = m.Update(key("enter"))
	m = next.(*Resolver)

	if !m.Done() {
		t.Fatalf("resolver should be done once the dotenv hunk is answered")
	}
	got := m.Resolutions()[it.Key]
	if got.Kind != sync.ChooseCustom {
		t.Fatalf("Resolutions()[key].Kind = %v, want ChooseCustom", got.Kind)
	}
	if !strings.Contains(string(got.Content), "A=2") {
		t.Errorf("merged dotenv content = %q, want it to contain the locally chosen A=2", got.Content)
	}
}

func TestResolverDotenvRefusesUntilAllChosen(t *testing.T) {
	base := []byte("A=1\nB=1\n")
	local := []byte("A=2\nB=2\n")
	remote := []byte("A=3\nB=3\n")
	it := sync.Item{
		Key:             sync.ItemKey{Project: "p1", Path: ".env"},
		Action:          sync.ActionConflict,
		Merge:           merge.ThreeWay(".env", base, local, remote),
		LocalText:       local,
		HeadText:        remote,
		NeedsResolution: true,
	}
	if len(it.Merge.Hunks) != 2 {
		t.Fatalf("fixture should conflict on both keys, got %+v", it.Merge.Hunks)
	}
	m := NewResolver(planWith(it), sync.Resolutions{})
	next, _ := m.Update(key("k"))
	m = next.(*Resolver)
	next, _ = m.Update(key("l")) // only the first hunk
	m = next.(*Resolver)
	next, _ = m.Update(key("enter"))
	m = next.(*Resolver)

	if m.Done() {
		t.Fatalf("enter should refuse while a hunk is still unanswered")
	}
	if m.msg == "" {
		t.Fatalf("expected a message explaining the refusal")
	}
}

// TestResolverKeyGuardsAgainstEmptyDotenvHunks is the regression for a
// dotenv-shaped merge.Result with Clean=false but zero Hunks (e.g. every
// conflicting section fell outside what the dotenv merger tracks as a key):
// without a length guard, "k" would open the per-key sub-mode with an empty
// hunks slice, and updateDotenv's "l"/"r" handlers index
// ds.hunks[ds.cursor] with no bounds check, which panics on the very first
// keypress.
func TestResolverKeyGuardsAgainstEmptyDotenvHunks(t *testing.T) {
	it := textConflictItem()
	it.Merge = &merge.Result{Kind: merge.KindDotenv, Clean: false} // Hunks is nil

	m := NewResolver(planWith(it), sync.Resolutions{})
	next, _ := m.Update(key("k"))
	m = next.(*Resolver)

	if m.dotenv != nil {
		t.Fatalf("k should not open the per-key sub-mode when there are no hunks to choose between")
	}
	if m.msg == "" {
		t.Fatalf("expected a message explaining why per-key resolution was refused")
	}
	if !strings.Contains(m.msg, "per-key resolution") {
		t.Fatalf("msg = %q, want the usual per-key-unavailable message", m.msg)
	}
}

// --- edit ($EDITOR) message handling, without ever spawning a process ------

func TestResolverEditRefusesWhileMarkersRemain(t *testing.T) {
	it := textConflictItem()
	dir := t.TempDir()
	plan := &sync.Plan{Projects: []sync.ProjectPlan{{ID: "p1", Path: dir, Items: []sync.Item{it}}}}
	m := NewResolver(plan, sync.Resolutions{})

	mergePath := filepath.Join(dir, "file.txt"+mergeTempSuffix)
	markers := merge.RenderMarkers(it.Merge, "local", "remote")
	if !merge.HasMarkers(markers) {
		t.Fatalf("RenderMarkers should itself contain markers")
	}
	if err := os.WriteFile(mergePath, markers, 0o600); err != nil {
		t.Fatalf("write merge file: %v", err)
	}

	next, _ := m.handleEditDone(editDoneMsg{path: mergePath, key: it.Key})
	m = next.(*Resolver)
	if m.Done() {
		t.Fatalf("resolver should not resolve the item while markers remain")
	}
	if _, ok := m.Resolutions()[it.Key]; ok {
		t.Fatalf("no resolution should be recorded while markers remain")
	}
	if m.msg == "" {
		t.Fatalf("expected a refusal message")
	}
	// The refusal must not destroy the file: it holds whatever the user
	// already merged by hand, and the message tells them to press e again.
	if _, err := os.Stat(mergePath); err != nil {
		t.Fatalf("the scratch merge file must survive a refusal: %v", err)
	}
	if m.pendingEditPath != mergePath || m.pendingEditKey != it.Key {
		t.Fatalf("the resolver should remember the kept scratch file, got %q for %+v", m.pendingEditPath, m.pendingEditKey)
	}
}

func TestResolverEditAcceptsCleanEdit(t *testing.T) {
	it := textConflictItem()
	dir := t.TempDir()
	plan := &sync.Plan{Projects: []sync.ProjectPlan{{ID: "p1", Path: dir, Items: []sync.Item{it}}}}
	m := NewResolver(plan, sync.Resolutions{})

	mergePath := filepath.Join(dir, "file.txt"+mergeTempSuffix)
	edited := []byte("line1\nresolved by hand\nline3\n")
	if err := os.WriteFile(mergePath, edited, 0o600); err != nil {
		t.Fatalf("write merge file: %v", err)
	}

	next, _ := m.handleEditDone(editDoneMsg{path: mergePath, key: it.Key})
	m = next.(*Resolver)
	if !m.Done() {
		t.Fatalf("resolver should be done: the edit resolved the only item")
	}
	got := m.Resolutions()[it.Key]
	if got.Kind != sync.ChooseCustom || string(got.Content) != string(edited) {
		t.Fatalf("Resolutions()[key] = %+v, want ChooseCustom with the edited content", got)
	}
	if _, err := os.Stat(mergePath); !os.IsNotExist(err) {
		t.Fatalf("the scratch merge file should be removed after a successful edit")
	}
}

func TestResolverStartEditWritesMarkersNextToTheFile(t *testing.T) {
	t.Setenv("EDITOR", "true")
	it := textConflictItem()
	dir := t.TempDir()
	plan := &sync.Plan{Projects: []sync.ProjectPlan{{ID: "p1", Path: dir, Items: []sync.Item{it}}}}
	m := NewResolver(plan, sync.Resolutions{})

	next, cmd := m.startEdit(&it)
	m = next.(*Resolver)
	if !m.editing {
		t.Fatalf("startEdit should mark the resolver as editing")
	}
	if cmd == nil {
		t.Fatalf("startEdit should return the tea.ExecProcess command")
	}
	mergePath := filepath.Join(dir, "file.txt"+mergeTempSuffix)
	data, err := os.ReadFile(mergePath)
	if err != nil {
		t.Fatalf("expected the merge scratch file next to the real file: %v", err)
	}
	if !merge.HasMarkers(data) {
		t.Fatalf("the scratch file should contain conflict markers")
	}
	os.Remove(mergePath)
}

func TestResolverStartEditRequiresEditor(t *testing.T) {
	t.Setenv("EDITOR", "")
	it := textConflictItem()
	dir := t.TempDir()
	plan := &sync.Plan{Projects: []sync.ProjectPlan{{ID: "p1", Path: dir, Items: []sync.Item{it}}}}
	m := NewResolver(plan, sync.Resolutions{})

	_, cmd := m.startEdit(&it)
	if cmd != nil {
		t.Fatalf("startEdit without $EDITOR should not return a command")
	}
	if m.editing {
		t.Fatalf("startEdit without $EDITOR should not enter editing mode")
	}
	if m.msg == "" {
		t.Fatalf("expected a message asking to set $EDITOR")
	}
}

// --- small pure helpers ------------------------------------------------------

func TestConflictLabel(t *testing.T) {
	tests := []struct {
		it   sync.Item
		want string
	}{
		{sync.Item{Action: sync.ActionDeleteRemote}, "deleted locally"},
		{sync.Item{Conflict: sync.ConflictModifyDelete}, "modified/deleted"},
		{sync.Item{Conflict: sync.ConflictNoBase}, "no common base"},
		{sync.Item{Conflict: sync.ConflictConcurrent}, "concurrent edits"},
		{sync.Item{Merge: &merge.Result{Kind: merge.KindBinary}}, "binary conflict"},
		{sync.Item{}, "content conflict"},
	}
	for _, tt := range tests {
		if got := conflictLabel(&tt.it); got != tt.want {
			t.Errorf("conflictLabel(%+v) = %q, want %q", tt.it, got, tt.want)
		}
	}
}

func TestFindProjectPlan(t *testing.T) {
	p := planWith(textConflictItem())
	if got := findProjectPlan(p, "p1"); got == nil || got.ID != "p1" {
		t.Fatalf("findProjectPlan(p1) = %v, want the p1 project", got)
	}
	if got := findProjectPlan(p, "missing"); got != nil {
		t.Fatalf("findProjectPlan(missing) = %v, want nil", got)
	}
}

// --- ctrl+c (spec §2.2 global quit) -----------------------------------------

func TestResolverCtrlCAbortsWhenStandalone(t *testing.T) {
	it := textConflictItem()
	m := NewResolver(planWith(it), sync.Resolutions{})
	m.standalone = true

	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	m = next.(*Resolver)
	if !m.Done() || !m.Aborted() {
		t.Fatalf("ctrl+c on a standalone resolver should abort: Done()=%v Aborted()=%v, want true/true", m.Done(), m.Aborted())
	}
	if cmd == nil {
		t.Fatalf("ctrl+c on a standalone resolver should return tea.Quit")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Fatalf("expected a tea.QuitMsg")
	}
	if len(m.Resolutions()) != 0 {
		t.Fatalf("Resolutions() after ctrl+c abort = %v, want empty", m.Resolutions())
	}
}

func TestResolverCtrlCDoesNothingWhenEmbedded(t *testing.T) {
	it := textConflictItem()
	m := NewResolver(planWith(it), sync.Resolutions{})
	// m.standalone left false: this Resolver is embedded in a syncView, and
	// the enclosing rootModel already intercepts ctrl+c before it ever
	// reaches an embedded screen — the resolver itself must not react to it.

	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	m = next.(*Resolver)
	if m.Done() || m.Aborted() {
		t.Fatalf("ctrl+c on an embedded resolver must not abort it: Done()=%v Aborted()=%v", m.Done(), m.Aborted())
	}
	if cmd != nil {
		t.Fatalf("ctrl+c on an embedded resolver should return no command")
	}
}

// --- ActionDeleteRemote guards l/r (they alias enter/c today) ---------------

func TestResolverDeleteRemoteLRDoNotSilentlyConfirmOrSkip(t *testing.T) {
	it := deleteRemoteItem("gone.txt")
	m := NewResolver(planWith(it), sync.Resolutions{})

	next, _ := m.Update(key("l"))
	m = next.(*Resolver)
	if _, ok := m.Resolutions()[it.Key]; ok {
		t.Fatalf("l on a delete-remote item must not silently confirm the deletion")
	}
	if m.Done() {
		t.Fatalf("l on a delete-remote item must not resolve it")
	}
	if m.msg == "" {
		t.Fatalf("expected a message explaining l does not apply here")
	}

	next, _ = m.Update(key("r"))
	m = next.(*Resolver)
	if _, ok := m.Resolutions()[it.Key]; ok {
		t.Fatalf("r on a delete-remote item must not silently confirm the deletion")
	}
	if m.Done() {
		t.Fatalf("r on a delete-remote item must not resolve it")
	}
}

// --- $EDITOR values that carry arguments ------------------------------------

func TestEditorCommandSplitsArguments(t *testing.T) {
	cmd := editorCommand("code --wait", "/tmp/x.psv-merge")
	want := []string{"code", "--wait", "/tmp/x.psv-merge"}
	if len(cmd.Args) != len(want) {
		t.Fatalf("editorCommand args = %v, want %v", cmd.Args, want)
	}
	for i := range want {
		if cmd.Args[i] != want[i] {
			t.Fatalf("editorCommand args = %v, want %v", cmd.Args, want)
		}
	}
}

func TestEditorCommandSingleWord(t *testing.T) {
	cmd := editorCommand("vim", "/tmp/x.psv-merge")
	want := []string{"vim", "/tmp/x.psv-merge"}
	if len(cmd.Args) != len(want) || cmd.Args[0] != want[0] || cmd.Args[1] != want[1] {
		t.Fatalf("editorCommand args = %v, want %v", cmd.Args, want)
	}
}

func TestResolverStartEditSplitsEditorArguments(t *testing.T) {
	t.Setenv("EDITOR", "true --some-flag")
	it := textConflictItem()
	dir := t.TempDir()
	plan := &sync.Plan{Projects: []sync.ProjectPlan{{ID: "p1", Path: dir, Items: []sync.Item{it}}}}
	m := NewResolver(plan, sync.Resolutions{})

	next, cmd := m.startEdit(&it)
	m = next.(*Resolver)
	if !m.editing {
		t.Fatalf("startEdit with a multi-word $EDITOR should still enter editing mode")
	}
	if cmd == nil {
		t.Fatalf("startEdit should return the tea.ExecProcess command")
	}
	mergePath := filepath.Join(dir, "file.txt"+mergeTempSuffix)
	os.Remove(mergePath)
}

// --- regressions -------------------------------------------------------------

// TestEditorCommandSanitisesEnvironment is the regression for the TUI's
// $EDITOR inheriting the process's full os.Environ(): the conflict editor
// (and every plugin, LSP server or format-on-save hook it spawns) must not be
// handed BW_SESSION, BW_PASSWORD & co., exactly as `config edit` does on the
// CLI side.
func TestEditorCommandSanitisesEnvironment(t *testing.T) {
	for _, name := range execx.Denylist() {
		t.Setenv(name, "super-secret-"+name)
	}
	t.Setenv("PATH_MARKER_FOR_TEST", "kept")

	cmd := editorCommand("vim -u NONE", "/tmp/x"+mergeTempSuffix)
	if cmd.Env == nil {
		t.Fatalf("cmd.Env is nil: the editor would inherit the whole environment, secrets included")
	}
	denied := map[string]bool{}
	for _, name := range execx.Denylist() {
		denied[name] = true
	}
	kept := false
	for _, kv := range cmd.Env {
		k, _, _ := strings.Cut(kv, "=")
		if denied[k] {
			t.Fatalf("editor environment still carries %s", k)
		}
		if k == "PATH_MARKER_FOR_TEST" {
			kept = true
		}
	}
	if !kept {
		t.Fatalf("the sanitised environment dropped ordinary variables too")
	}
	// The editor's own arguments must survive the change.
	if len(cmd.Args) != 4 || cmd.Args[1] != "-u" || cmd.Args[2] != "NONE" || !strings.HasSuffix(cmd.Args[3], mergeTempSuffix) {
		t.Fatalf("cmd.Args = %q, want the split editor arguments plus the merge path", cmd.Args)
	}
}

// TestResolverEditKeepsPartialMergeForTheNextEdit is the regression for the
// scratch file being deleted before the marker check: a partially merged file
// was thrown away and the next `e` restarted from pristine markers, so every
// pass over a big conflict had to be redone from zero.
func TestResolverEditKeepsPartialMergeForTheNextEdit(t *testing.T) {
	t.Setenv("EDITOR", "true")
	it := textConflictItem()
	dir := t.TempDir()
	plan := &sync.Plan{Projects: []sync.ProjectPlan{{ID: "p1", Path: dir, Items: []sync.Item{it}}}}
	m := NewResolver(plan, sync.Resolutions{})

	next, _ := m.startEdit(&it) // user presses e
	m = next.(*Resolver)
	mergePath := filepath.Join(dir, "file.txt"+mergeTempSuffix)

	rendered, err := os.ReadFile(mergePath)
	if err != nil {
		t.Fatalf("read scratch file: %v", err)
	}
	// The user merges part of the file and leaves the rest conflicted.
	partial := append([]byte("HAND-MERGED HEADER\n"), rendered...)
	if err := os.WriteFile(mergePath, partial, 0o600); err != nil {
		t.Fatalf("write scratch file: %v", err)
	}

	next, _ = m.handleEditDone(editDoneMsg{path: mergePath, key: it.Key})
	m = next.(*Resolver)
	if !strings.Contains(m.msg, "conflict markers") {
		t.Fatalf("msg = %q, want the refusal that asks for another e", m.msg)
	}

	next, _ = m.startEdit(&it) // user follows the advice and presses e again
	m = next.(*Resolver)
	after, err := os.ReadFile(mergePath)
	if err != nil {
		t.Fatalf("read scratch file on the second edit: %v", err)
	}
	if !bytes.Contains(after, []byte("HAND-MERGED HEADER")) {
		t.Fatalf("the second edit restarted from pristine markers; the user's work was lost")
	}

	// Leaving the item must not leave the scratch file behind.
	next, _ = m.handleEditDone(editDoneMsg{path: mergePath, key: it.Key})
	m = next.(*Resolver)
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'s'}})
	if _, err := os.Stat(mergePath); !os.IsNotExist(err) {
		t.Fatalf("the scratch file must not outlive the item, stat err = %v", err)
	}
}

// TestResolverBulkStrategyStaysOnUndecidableItem is the regression for L/R
// stepping straight over the item on screen when sync.ApplyStrategy declines
// it (StrategyLocal with no local copy): the resolver used to report Done()
// with that item silently unresolved.
func TestResolverBulkStrategyStaysOnUndecidableItem(t *testing.T) {
	a := sync.Item{
		Key: sync.ItemKey{Project: "p1", Path: "a.env"}, Action: sync.ActionConflict,
		Conflict: sync.ConflictConcurrent, Local: nil, NeedsResolution: true,
	}
	b := sync.Item{
		Key: sync.ItemKey{Project: "p1", Path: "b.env"}, Action: sync.ActionConflict,
		Conflict: sync.ConflictContent, Local: &sync.FileRef{Blob: "x"}, NeedsResolution: true,
	}
	plan := &sync.Plan{Projects: []sync.ProjectPlan{{ID: "p1", Items: []sync.Item{a, b}}}}
	m := NewResolver(plan, sync.Resolutions{})

	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'L'}})
	m = next.(*Resolver)

	if m.Done() {
		t.Fatalf("L must not finish the resolver while a.env has no resolution")
	}
	cur := m.currentItem()
	if cur == nil || cur.Key.Path != "a.env" {
		t.Fatalf("resolver moved off the undecidable item, now on %v", cur)
	}
	if _, ok := m.Resolutions()[a.Key]; ok {
		t.Fatalf("a.env should still be unresolved")
	}
	if m.msg == "" {
		t.Fatalf("expected a message explaining why L could not decide this file")
	}
	if len(plan.Unresolved(m.Resolutions())) == 0 {
		t.Fatalf("a.env must still count as unresolved")
	}
	// The rest of the plan is still resolved by the bulk strategy.
	if _, ok := m.Resolutions()[b.Key]; !ok {
		t.Fatalf("L should still have resolved b.env")
	}
}
