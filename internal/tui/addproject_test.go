package tui

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/sanbiv/private-sync/internal/app"
	"github.com/sanbiv/private-sync/internal/identity"
	"github.com/sanbiv/private-sync/internal/scan"
	"github.com/sanbiv/private-sync/internal/vault"
)

func TestFormatSize(t *testing.T) {
	tests := []struct {
		n    int64
		want string
	}{
		{0, "0"},
		{512, "512"},
		{1024, "1KiB"},
		{1024 * 1024, "1MiB"},
	}
	for _, tt := range tests {
		if got := formatSize(tt.n); got != tt.want {
			t.Errorf("formatSize(%d) = %q, want %q", tt.n, got, tt.want)
		}
	}
}

func TestTruncatePath(t *testing.T) {
	if got := truncatePath("short.env", 40); got != "short.env" {
		t.Errorf("truncatePath(short) = %q, want unchanged", got)
	}
	long := "a/b/c/d/e/f/g/h/verylongpathname.env"
	got := truncatePath(long, 10)
	if !strings.HasPrefix(got, "…") {
		t.Errorf("truncatePath(%q, 10) = %q, want an ellipsis prefix", long, got)
	}
	if !strings.HasSuffix(long, got[len("…"):]) {
		t.Errorf("truncatePath(%q, 10) = %q, want it to keep the path's tail", long, got)
	}
}

func TestScoreBadge(t *testing.T) {
	tests := []struct {
		s    scan.Score
		want string
	}{
		{scan.ScoreHigh, "high"},
		{scan.ScoreMedium, "medium"},
		{scan.ScoreLow, "low"},
	}
	for _, tt := range tests {
		label, style := scoreBadge(tt.s)
		if label != tt.want {
			t.Errorf("scoreBadge(%v) = %q, want %q", tt.s, label, tt.want)
		}
		if style.Render("x") == "" {
			t.Errorf("scoreBadge(%v) returned a zero style", tt.s)
		}
	}
}

func TestRenderCandidateRow(t *testing.T) {
	c := scan.Candidate{Path: ".env", Size: 2048, Score: scan.ScoreHigh, Reasons: []string{"ignored by git"}}

	checked := renderCandidateRow(c, true, false, 80)
	if !strings.Contains(checked, "[x]") {
		t.Errorf("renderCandidateRow(checked) = %q, want it to contain \"[x]\"", checked)
	}
	unchecked := renderCandidateRow(c, false, false, 80)
	if !strings.Contains(unchecked, "[ ]") {
		t.Errorf("renderCandidateRow(unchecked) = %q, want it to contain \"[ ]\"", unchecked)
	}
	if !strings.Contains(unchecked, ".env") {
		t.Errorf("renderCandidateRow() = %q, want it to contain the path", unchecked)
	}
	if !strings.Contains(unchecked, formatSize(c.Size)) {
		t.Errorf("renderCandidateRow() = %q, want it to contain the formatted size", unchecked)
	}
	if !strings.Contains(unchecked, "high") {
		t.Errorf("renderCandidateRow() = %q, want it to contain the score badge", unchecked)
	}
	if !strings.Contains(unchecked, "ignored by git") {
		t.Errorf("renderCandidateRow() = %q, want it to contain the reasons", unchecked)
	}
}

func TestCleanRelPath(t *testing.T) {
	tests := []struct {
		in   string
		want string
		ok   bool
	}{
		{"config/secret.env", "config/secret.env", true},
		{"./config/secret.env", "config/secret.env", true},
		{"", "", false},
		{".", "", false},
		{"..", "", false},
		{"../escape.env", "", false},
		{"/etc/passwd", "", false},
	}
	for _, tt := range tests {
		got, ok := cleanRelPath(tt.in)
		if ok != tt.ok {
			t.Errorf("cleanRelPath(%q) ok = %v, want %v", tt.in, ok, tt.ok)
			continue
		}
		if ok && got != tt.want {
			t.Errorf("cleanRelPath(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// TestCleanRelPathRejectsWindowsRootedPaths is the regression for a rooted
// path slipping past cleanRelPath on Windows: filepath.IsAbs is
// volume-relative there, so "/etc/passwd" (no drive letter) reports false
// even though the leading slash still roots it at the current drive, and a
// bare "C:foo" volume-name form isn't caught by IsAbs either. Both
// filepath.IsAbs and filepath.VolumeName are no-ops on every other OS (a
// leading "/" is already absolute there, and VolumeName always returns ""),
// so this only exercises anything on Windows and is skipped elsewhere.
func TestCleanRelPathRejectsWindowsRootedPaths(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("filepath.IsAbs/VolumeName only differ from every other OS on windows")
	}
	for _, in := range []string{"/etc/passwd", `C:\Windows\System32\config`, "C:foo"} {
		if _, ok := cleanRelPath(in); ok {
			t.Errorf("cleanRelPath(%q) ok = true, want false (rooted path)", in)
		}
	}
}

func newTestCandidateList() *candidateList {
	return newCandidateList(&scan.Result{
		Candidates: []scan.Candidate{
			{Path: ".env", Score: scan.ScoreHigh, Preselected: true},
			{Path: "config.yaml", Score: scan.ScoreMedium},
			{Path: "README.md", Score: scan.ScoreLow},
			{Path: "old-tracked.env", Score: scan.ScoreLow, Tracked: true, Preselected: false},
		},
		NestedRepos: []string{"vendor/lib"},
	})
}

func TestCandidateListPreselection(t *testing.T) {
	cl := newTestCandidateList()
	want := map[string]bool{".env": true, "config.yaml": false, "README.md": false, "old-tracked.env": true}
	for _, r := range cl.rows {
		if r.checked != want[r.c.Path] {
			t.Errorf("row %q checked = %v, want %v", r.c.Path, r.checked, want[r.c.Path])
		}
	}
}

func TestCandidateListVisibleHidesLowByDefault(t *testing.T) {
	cl := newTestCandidateList()
	for _, i := range cl.visible() {
		if cl.rows[i].c.Path == "README.md" {
			t.Errorf("visible() unexpectedly includes low-score README.md before pressing t")
		}
	}
	cl.update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("t")}, "")
	if !cl.showLow {
		t.Fatalf("t should toggle showLow on")
	}
	found := false
	for _, i := range cl.visible() {
		if cl.rows[i].c.Path == "README.md" {
			found = true
		}
	}
	if !found {
		t.Errorf("visible() should include README.md once showLow is toggled on")
	}
}

func TestCandidateListToggleAndFilter(t *testing.T) {
	cl := newTestCandidateList()
	cl.cursor = 0 // first visible row: .env

	cl.update(tea.KeyMsg{Type: tea.KeySpace}, "")
	if cl.rows[0].checked {
		t.Fatalf(".env should be unchecked after toggling a preselected row")
	}
	cl.update(tea.KeyMsg{Type: tea.KeySpace}, "")
	if !cl.rows[0].checked {
		t.Fatalf(".env should be checked again after toggling twice")
	}

	cl.update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/")}, "")
	if !cl.filtering {
		t.Fatalf("expected filtering mode after /")
	}
	cl.update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("config")}, "")
	if cl.filter != "config" {
		t.Fatalf("filter = %q, want %q", cl.filter, "config")
	}
	vis := cl.visible()
	if len(vis) != 1 || cl.rows[vis[0]].c.Path != "config.yaml" {
		t.Fatalf("visible() with filter %q = %v, want just config.yaml", cl.filter, vis)
	}
	cl.update(tea.KeyMsg{Type: tea.KeyEsc}, "")
	if cl.filtering || cl.filter != "" {
		t.Fatalf("esc while filtering should clear the filter")
	}
}

func TestCandidateListUntracked(t *testing.T) {
	cl := newTestCandidateList()
	for i := range cl.rows {
		if cl.rows[i].c.Path == "old-tracked.env" {
			cl.rows[i].checked = false
		}
	}
	got := cl.untracked()
	if len(got) != 1 || got[0] != "old-tracked.env" {
		t.Fatalf("untracked() = %v, want [old-tracked.env]", got)
	}
}

func TestCandidateListAddExtra(t *testing.T) {
	dir := t.TempDir()
	extra := filepath.Join(dir, "extra.pem")
	if err := os.WriteFile(extra, []byte("secret"), 0o600); err != nil {
		t.Fatalf("write extra file: %v", err)
	}
	cl := newTestCandidateList()
	cl.update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("+")}, dir)
	if !cl.adding {
		t.Fatalf("expected adding mode after +")
	}
	cl.update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("extra.pem")}, dir)
	cl.update(tea.KeyMsg{Type: tea.KeyEnter}, dir)
	if cl.adding {
		t.Fatalf("adding mode should end after enter")
	}
	found := false
	for _, r := range cl.rows {
		if r.c.Path == "extra.pem" && r.checked {
			found = true
		}
	}
	if !found {
		t.Fatalf("addExtra did not add extra.pem as checked: %+v", cl.rows)
	}
}

// --- add-wizard step navigation with esc -----------------------------------

// The steps exercised here (path, fetch, identify, associate) never call a
// method on the session — they only close over it for a later tea.Cmd that
// this test never runs — so a zero-value *app.Session is safe to use.
func newNavTestModel(t *testing.T) *addModel {
	t.Helper()
	dir := t.TempDir() // a real directory, so the path step's dirExists check passes
	m := &addModel{ctx: context.Background(), s: &app.Session{}, dir: dir}
	m.step = stepPath
	m.pathInput.SetValue(dir)
	return m
}

func TestAddWizardForwardAndBackNavigation(t *testing.T) {
	m := newNavTestModel(t)

	// path -> fetch
	if done, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter}); done {
		t.Fatalf("enter on a valid path should not leave the wizard")
	}
	if m.step != stepFetch {
		t.Fatalf("step = %v, want stepFetch", m.step)
	}

	// fetch -> identify, once the (faked) fetch completes. Gen must match
	// what entering stepFetch bumped it to (opDoneMsg from a stale,
	// already-superseded fetch is otherwise ignored — see
	// TestUpdateFetchIgnoresStaleGeneration).
	if done, _ := m.Update(opDoneMsg{Tag: "fetch", Gen: m.fetchGen}); done {
		t.Fatalf("fetch completing should not leave the wizard")
	}
	if m.step != stepIdentify {
		t.Fatalf("step = %v, want stepIdentify", m.step)
	}

	// identify -> associate, when matches are found. gen must match what
	// entering stepIdentify bumped it to, exactly like the fetch step above.
	matches := []identity.Match{{ProjectID: "existing1", Strength: identity.StrengthStrong}}
	if done, _ := m.Update(identifyMsg{gen: m.identGen, matches: matches}); done {
		t.Fatalf("identify completing should not leave the wizard")
	}
	if m.step != stepAssociate {
		t.Fatalf("step = %v, want stepAssociate (matches were found)", m.step)
	}

	// esc skips the non-interactive stepFetch/stepIdentify (goBack never
	// lands on either — see goBack's doc comment) and goes straight back to
	// stepPath in one press; a second esc then leaves the wizard.
	if done, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc}); done {
		t.Fatalf("esc from stepAssociate left the wizard early, expected to land on stepPath")
	}
	if m.step != stepPath {
		t.Fatalf("step after esc = %v, want stepPath", m.step)
	}
	done, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if !done {
		t.Fatalf("esc on the first step should leave the wizard")
	}
}

// TestAddWizardNoMatchesGoesToScan checks the spec §2.2 item 3 step order
// (identity -> scan -> select -> name -> confirm): with no matches at all,
// identify goes straight to scanning, not to naming the project yet.
func TestAddWizardNoMatchesGoesToScan(t *testing.T) {
	m := &addModel{ctx: context.Background(), s: &app.Session{}, dir: "/tmp/example", step: stepIdentify}
	if done, _ := m.Update(identifyMsg{}); done {
		t.Fatalf("identify with no matches should not leave the wizard")
	}
	if m.step != stepScan {
		t.Fatalf("step = %v, want stepScan", m.step)
	}
	if !m.creatingNew {
		t.Fatalf("creatingNew should be true when there are no matches")
	}
	if m.forceRandom {
		t.Fatalf("forceRandom should stay false when there was nothing to bypass")
	}
}

// TestAddWizardWeakMatchDefaultsToCreateNew is the regression for spec §7/§2.2:
// a weak (dir-only) match must be "proposed with a warning, never default".
func TestAddWizardWeakMatchDefaultsToCreateNew(t *testing.T) {
	matches := []identity.Match{{ProjectID: "existing1", Strength: identity.StrengthWeak}}
	m := &addModel{ctx: context.Background(), s: &app.Session{}, dir: "/tmp/example", step: stepIdentify}
	if done, _ := m.Update(identifyMsg{matches: matches}); done {
		t.Fatalf("identify with a weak match should not leave the wizard")
	}
	if m.step != stepAssociate {
		t.Fatalf("step = %v, want stepAssociate", m.step)
	}
	if m.assocIdx != len(m.matches) {
		t.Fatalf("assocIdx = %d, want %d (the \"create new\" row) for a weak-only match", m.assocIdx, len(m.matches))
	}
}

// TestAddWizardStrongMatchDefaultsToAssociate is the counterpart: a strong
// match is still proposed as the default choice.
func TestAddWizardStrongMatchDefaultsToAssociate(t *testing.T) {
	matches := []identity.Match{{ProjectID: "existing1", Strength: identity.StrengthStrong}}
	m := &addModel{ctx: context.Background(), s: &app.Session{}, dir: "/tmp/example", step: stepIdentify}
	m.Update(identifyMsg{matches: matches})
	if m.assocIdx != 0 {
		t.Fatalf("assocIdx = %d, want 0 (the strong match) as the default", m.assocIdx)
	}
}

func TestAddWizardCreateNewDespiteMatchForcesRandom(t *testing.T) {
	matches := []identity.Match{{ProjectID: "existing1", Strength: identity.StrengthStrong}}
	m := &addModel{ctx: context.Background(), s: &app.Session{}, dir: "/tmp/example", step: stepAssociate, matches: matches, assocIdx: len(matches)}
	if done, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter}); done {
		t.Fatalf("choosing create-new should not leave the wizard")
	}
	if m.step != stepScan {
		t.Fatalf("step = %v, want stepScan", m.step)
	}
	if !m.creatingNew || !m.forceRandom {
		t.Fatalf("creatingNew=%v forceRandom=%v, want both true", m.creatingNew, m.forceRandom)
	}
}

// TestAddWizardSelectRoutesToNewNameOnlyWhenCreatingNew mirrors the new step
// order: stepSelect's enter goes to stepNewName only for a brand new
// project, straight to stepConfirm when associating with an existing one.
func TestAddWizardSelectRoutesToNewNameOnlyWhenCreatingNew(t *testing.T) {
	newCand := func() *candidateList { return newCandidateList(&scan.Result{}) }

	creating := &addModel{ctx: context.Background(), s: &app.Session{}, step: stepSelect, creatingNew: true, cand: newCand()}
	if done, _ := creating.Update(tea.KeyMsg{Type: tea.KeyEnter}); done {
		t.Fatalf("enter on stepSelect should not leave the wizard")
	}
	if creating.step != stepNewName {
		t.Fatalf("creating-new: step = %v, want stepNewName", creating.step)
	}

	associating := &addModel{ctx: context.Background(), s: &app.Session{}, step: stepSelect, creatingNew: false, cand: newCand()}
	if done, _ := associating.Update(tea.KeyMsg{Type: tea.KeyEnter}); done {
		t.Fatalf("enter on stepSelect should not leave the wizard")
	}
	if associating.step != stepConfirm {
		t.Fatalf("associating: step = %v, want stepConfirm", associating.step)
	}
}

func TestComputeWillRestore(t *testing.T) {
	s := newTestSession(t)
	dir := t.TempDir()
	const projectID = "proj0000"

	writeTrackedFiles(t, s, projectID, map[string]string{
		"present.txt": "here",
		"missing.env": "SECRET=1",
	})

	if err := os.WriteFile(filepath.Join(dir, "present.txt"), []byte("here"), 0o600); err != nil {
		t.Fatalf("write present.txt: %v", err)
	}

	m := &addModel{ctx: context.Background(), s: s, dir: dir, projectID: projectID}
	m.computeWillRestore(projectID)

	if len(m.willRestore) != 1 || m.willRestore[0] != "missing.env" {
		t.Fatalf("willRestore = %v, want [missing.env]", m.willRestore)
	}
}

// writeTrackedFiles publishes a single journal (this session's machine) with
// one entry per file, so Engine.TrackedPaths reports them as tracked heads
// (spec §9.1). All entries must go in one WriteJournal call: it replaces the
// machine's whole journal document rather than merging into it.
func writeTrackedFiles(t *testing.T, s *app.Session, projectID string, files map[string]string) {
	t.Helper()
	now := time.Now().UTC()
	entries := make(map[string]vault.Entry, len(files))
	for relPath, content := range files {
		id, _, err := s.Vault.WriteBlob([]byte(content))
		if err != nil {
			t.Fatalf("WriteBlob(%s): %v", relPath, err)
		}
		entries[relPath] = vault.Entry{
			Path:      relPath,
			Kind:      vault.KindFile,
			Blob:      id,
			Clock:     vault.Clock{}.Tick(s.Machine.ID),
			Mode:      0o600,
			Size:      int64(len(content)),
			ModTime:   now,
			UpdatedAt: now,
			Machine:   s.Machine.ID,
		}
	}
	j := &vault.Journal{Machine: s.Machine.ID, Entries: entries}
	if err := s.Vault.WriteJournal(projectID, j); err != nil {
		t.Fatalf("WriteJournal: %v", err)
	}
}

// --- esc = back one step, keeping state (spec §2.2) -------------------------

// TestGoBackSkipsScanKeepingSelectionState is the regression for esc from
// stepSelect discarding the candidate list: the scan step is not itself
// interactive, so backing out of stepSelect must skip it (not re-scan) and
// land on whichever step started the scan, without touching m.cand.
//
// The scan here is started from stepAssociate (a strong match found, then
// the user picks "create new anyway") rather than stepIdentify's no-match
// path: stepIdentify is itself always skipped while unwinding (see goBack's
// doc comment — re-entering it would just re-run identify and, on finding
// the same match again, bounce straight back to stepAssociate), so a
// meaningful test of "land on whichever step started the scan" needs that
// step to be one goBack actually stops on. stepAssociate is genuinely
// interactive and is not skipped.
func TestGoBackSkipsScanKeepingSelectionState(t *testing.T) {
	matches := []identity.Match{{ProjectID: "existing1", Strength: identity.StrengthStrong}}
	m := &addModel{ctx: context.Background(), s: &app.Session{}, dir: "/tmp/example", step: stepIdentify}
	if done, _ := m.Update(identifyMsg{matches: matches}); done {
		t.Fatalf("identify with a match should not leave the wizard")
	}
	if m.step != stepAssociate {
		t.Fatalf("setup: step = %v, want stepAssociate", m.step)
	}

	// Choose "create new project anyway" (the row past the last match).
	m.assocIdx = len(m.matches)
	if done, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter}); done {
		t.Fatalf("choosing create-new should not leave the wizard")
	}
	if m.step != stepScan {
		t.Fatalf("setup: step = %v, want stepScan", m.step)
	}

	res := &scan.Result{Candidates: []scan.Candidate{
		{Path: "a.env", Score: scan.ScoreHigh, Preselected: true},
	}}
	if done, _ := m.Update(scanDoneMsg{Gen: m.scanGen, Result: res}); done {
		t.Fatalf("scan completing should not leave the wizard")
	}
	if m.step != stepSelect {
		t.Fatalf("setup: step = %v, want stepSelect", m.step)
	}

	m.cand.rows[0].checked = false // the user's edit that must survive esc
	cand := m.cand

	if done, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc}); done {
		t.Fatalf("esc from stepSelect should not leave the wizard")
	}
	if m.step != stepAssociate {
		t.Fatalf("esc from stepSelect should skip the non-interactive scan step and land on stepAssociate, got %v", m.step)
	}
	if m.cand != cand {
		t.Fatalf("esc from stepSelect must not discard the candidate list built by the scan")
	}
	if m.cand.rows[0].checked {
		t.Fatalf("esc from stepSelect must keep the user's checkbox change")
	}
}

// TestGoBackToNewNamePreservesTypedName is the regression for esc rebuilding
// stepNewName's input from the directory's basename, discarding whatever the
// user had typed.
func TestGoBackToNewNamePreservesTypedName(t *testing.T) {
	s := newTestSession(t)
	m := &addModel{ctx: context.Background(), s: s, dir: "/tmp/example", step: stepSelect, creatingNew: true, cand: newCandidateList(&scan.Result{})}
	if done, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter}); done {
		t.Fatalf("enter on stepSelect should not leave the wizard")
	}
	if m.step != stepNewName {
		t.Fatalf("setup: step = %v, want stepNewName", m.step)
	}
	m.nameInput.SetValue("my-typed-name")

	if done, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter}); done {
		t.Fatalf("enter on stepNewName should not leave the wizard")
	}
	if m.step != stepConfirm {
		t.Fatalf("setup: step = %v, want stepConfirm (err=%q)", m.step, m.err)
	}

	if done, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc}); done {
		t.Fatalf("esc from stepConfirm should not leave the wizard")
	}
	if m.step != stepNewName {
		t.Fatalf("step = %v, want stepNewName", m.step)
	}
	if m.nameInput.Value() != "my-typed-name" {
		t.Fatalf("nameInput.Value() = %q, want the previously typed name preserved", m.nameInput.Value())
	}
}

// --- stale scan/fetch messages after esc (generation guards) ---------------

func TestUpdateScanIgnoresStaleGeneration(t *testing.T) {
	m := &addModel{ctx: context.Background(), s: &app.Session{}, step: stepScan, scanGen: 2}

	stale := scanDoneMsg{Gen: 1, Err: errors.New("stale failure from a cancelled run")}
	if done, _ := m.Update(stale); done {
		t.Fatalf("a stale scanDoneMsg should not leave the wizard")
	}
	if m.scanErr != nil {
		t.Fatalf("a stale scanDoneMsg must not set scanErr, got %v", m.scanErr)
	}
	if m.step != stepScan {
		t.Fatalf("a stale scanDoneMsg must not change the step, got %v", m.step)
	}

	staleEvent := scanEventMsg{Gen: 1, Walked: 999}
	m.Update(staleEvent)
	if m.walked == 999 {
		t.Fatalf("a stale scanEventMsg must not update the progress counters")
	}

	current := scanDoneMsg{Gen: 2, Result: &scan.Result{}}
	if done, _ := m.Update(current); done {
		t.Fatalf("a current scanDoneMsg should not leave the wizard")
	}
	if m.step != stepSelect {
		t.Fatalf("a current scanDoneMsg should advance to stepSelect, got %v", m.step)
	}
}

func TestUpdateFetchIgnoresStaleGeneration(t *testing.T) {
	m := &addModel{ctx: context.Background(), s: &app.Session{}, step: stepFetch, fetchGen: 2}

	stale := opDoneMsg{Tag: "fetch", Gen: 1, Err: errors.New("stale failure from a cancelled run")}
	if done, _ := m.Update(stale); done {
		t.Fatalf("a stale opDoneMsg should not leave the wizard")
	}
	if m.err != "" {
		t.Fatalf("a stale opDoneMsg must not set err, got %q", m.err)
	}
	if m.step != stepFetch {
		t.Fatalf("a stale opDoneMsg must not change the step, got %v", m.step)
	}

	current := opDoneMsg{Tag: "fetch", Gen: 2}
	if done, _ := m.Update(current); done {
		t.Fatalf("a current opDoneMsg should not leave the wizard")
	}
	if m.step != stepIdentify {
		t.Fatalf("a current opDoneMsg should advance to stepIdentify, got %v", m.step)
	}
}

// --- RunAddProject's aborted-vs-finished reporting --------------------------

func TestFinishErr(t *testing.T) {
	if err := finishErr(true); err != nil {
		t.Fatalf("finishErr(true) = %v, want nil", err)
	}
	err := finishErr(false)
	if err == nil {
		t.Fatalf("finishErr(false) should return an error")
	}
	if !errors.Is(err, ErrAborted) {
		t.Fatalf("finishErr(false) = %v, want errors.Is(err, ErrAborted)", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("finishErr(false) should also satisfy errors.Is(err, context.Canceled)")
	}
}

func TestAddModelFinishedSetOnReachingStepSummary(t *testing.T) {
	sv := &syncView{stage: stageDone}
	m := &addModel{ctx: context.Background(), s: &app.Session{}, step: stepApply, apply: sv}
	if m.finished {
		t.Fatalf("finished should start false")
	}
	if done, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter}); done {
		t.Fatalf("reaching stepSummary should not itself leave the wizard")
	}
	if !m.finished {
		t.Fatalf("reaching stepSummary should set finished")
	}
	if m.step != stepSummary {
		t.Fatalf("step = %v, want stepSummary", m.step)
	}
}

func TestSummaryHeadline(t *testing.T) {
	if got := summaryHeadline(&syncView{stage: stageDone}); !strings.Contains(got, "Project added") {
		t.Errorf("summaryHeadline(stageDone) = %q, want it to report success", got)
	}
	for _, apply := range []*syncView{{stage: stageError}, {stage: stageAborted}, nil} {
		got := summaryHeadline(apply)
		if strings.Contains(got, "Project added") {
			t.Errorf("summaryHeadline(%+v) = %q, must not report success", apply, got)
		}
	}
}

// --- ctrl+c handling in the standalone add-project program ------------------

func TestAddProgramCtrlCQuitsImmediatelyWhenNotApplying(t *testing.T) {
	m := &addModel{ctx: context.Background(), s: &app.Session{}, step: stepPath}
	p := &addProgram{m: m}
	_, cmd := p.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if cmd == nil {
		t.Fatalf("ctrl+c with nothing applying should quit immediately")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Fatalf("expected a tea.QuitMsg")
	}
}

func TestAddProgramCtrlCRequiresSecondPressWhileApplying(t *testing.T) {
	sv := &syncView{stage: stageApply}
	cancelled := false
	sv.cancel = func() { cancelled = true }

	m := &addModel{ctx: context.Background(), s: &app.Session{}, step: stepApply, apply: sv}
	p := &addProgram{m: m}

	_, cmd := p.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if cmd != nil {
		t.Fatalf("first ctrl+c while applying should not quit yet")
	}
	if !p.quitConfirm {
		t.Fatalf("first ctrl+c while applying should arm the confirmation")
	}
	if cancelled {
		t.Fatalf("first ctrl+c must not cancel the running apply yet")
	}

	_, cmd = p.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if cmd == nil {
		t.Fatalf("second ctrl+c should quit")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Fatalf("expected a tea.QuitMsg")
	}
	if !cancelled {
		t.Fatalf("second ctrl+c should cancel the running apply")
	}
}

func TestAddProgramCtrlCConfirmResetsOnOtherKey(t *testing.T) {
	sv := &syncView{stage: stageApply}
	m := &addModel{ctx: context.Background(), s: &app.Session{}, step: stepApply, apply: sv}
	p := &addProgram{m: m}

	p.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if !p.quitConfirm {
		t.Fatalf("setup: expected quitConfirm armed")
	}
	p.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
	if p.quitConfirm {
		t.Fatalf("any other key should disarm the quit confirmation")
	}
}

func TestAddProgramViewShowsQuitConfirmWarning(t *testing.T) {
	m := &addModel{ctx: context.Background(), s: &app.Session{}, step: stepPath}
	p := &addProgram{m: m, quitConfirm: true}
	if !strings.Contains(p.View(), "ctrl+c again") {
		t.Fatalf("View() should show the quit-confirmation warning while armed")
	}
}
