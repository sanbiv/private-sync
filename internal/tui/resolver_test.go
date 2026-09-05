package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
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
	if _, err := os.Stat(mergePath); !os.IsNotExist(err) {
		t.Fatalf("the scratch merge file should be removed even on refusal")
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
