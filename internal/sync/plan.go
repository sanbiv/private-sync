package sync

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/sanbiv/private-sync/internal/config"
	"github.com/sanbiv/private-sync/internal/fsutil"
	"github.com/sanbiv/private-sync/internal/identity"
	"github.com/sanbiv/private-sync/internal/merge"
	"github.com/sanbiv/private-sync/internal/vault"
)

// Plan computes the items for every selected project without touching anything.
//
// It is pure: it reads the local vault copy, the base store and the project
// directories, and decrypts every blob an item needs (head for downloads and
// merges, base for merges) up to the configured max file size. A blob that
// cannot be read makes its item ActionPending.
func (e *Engine) Plan(ctx context.Context, opts Options) (*Plan, error) {
	if err := e.ready(); err != nil {
		return nil, fmt.Errorf("sync.Plan: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	plan := &Plan{Mode: opts.Mode}
	selected := e.selectProjects(opts, plan)
	dups := e.duplicates(selected, plan)
	for i, pc := range selected {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		name := pc.Name
		if name == "" {
			name = pc.ID
		}
		emit(opts.Progress, Event{Stage: "plan", Project: pc.ID, Message: "planning " + name, Done: i, Total: len(selected)})
		pp := e.planProject(pc, opts)
		if d := dups[pc.ID]; d != "" {
			pp.DuplicateOf = d
			pp.Warnings = append(pp.Warnings, fmt.Sprintf("duplicate of project %s (both share a strong fingerprint; fix with projects link)", d))
		}
		plan.Projects = append(plan.Projects, pp)
	}
	emit(opts.Progress, Event{Stage: "plan", Message: "plan complete", Done: len(selected), Total: len(selected)})
	return plan, nil
}

// selectProjects returns the linked projects to plan, honouring opts.Projects
// (ids; unknown ids become plan warnings).
func (e *Engine) selectProjects(opts Options, plan *Plan) []config.ProjectConfig {
	if len(opts.Projects) == 0 {
		return append([]config.ProjectConfig(nil), e.cfg.Projects...)
	}
	var out []config.ProjectConfig
	seen := map[string]bool{}
	for _, id := range opts.Projects {
		pc, ok := e.cfg.Project(id)
		if !ok {
			plan.Warnings = append(plan.Warnings, fmt.Sprintf("project %s is not linked on this machine", id))
			continue
		}
		if seen[pc.ID] {
			continue
		}
		seen[pc.ID] = true
		out = append(out, *pc)
	}
	return out
}

// duplicates detects distinct vault projects sharing a level 1-2 fingerprint
// with a selected project (spec §5) and returns selectedID -> other id.
func (e *Engine) duplicates(selected []config.ProjectConfig, plan *Plan) map[string]string {
	out := map[string]string{}
	if len(selected) == 0 {
		return out
	}
	projects, warnings, err := e.vault.ListProjects()
	plan.Warnings = append(plan.Warnings, warnings...)
	if err != nil {
		plan.Warnings = append(plan.Warnings, "duplicate detection skipped: "+err.Error())
		return out
	}
	fps := make(map[string][]identity.Fingerprint, len(projects))
	for _, p := range projects {
		fps[p.ID] = p.Fingerprints
	}
	for _, pc := range selected {
		mine, ok := fps[pc.ID]
		if !ok || len(mine) == 0 {
			continue
		}
		others := make(map[string][]identity.Fingerprint, len(fps))
		for id, f := range fps {
			if id != pc.ID {
				others[id] = f
			}
		}
		for _, m := range identity.MatchProjects(mine, others) {
			if m.Strength == identity.StrengthStrong {
				out[pc.ID] = m.ProjectID
				break
			}
		}
	}
	return out
}

// planner holds the per-project planning context.
type planner struct {
	e      *Engine
	pp     *ProjectPlan
	opts   Options
	dir    string
	rolled map[string]bool // machines whose journal Seq went backwards
}

// planProject plans one linked project.
func (e *Engine) planProject(pc config.ProjectConfig, opts Options) ProjectPlan {
	pp := ProjectPlan{ID: pc.ID, Name: pc.Name, journalSeqs: map[string]uint64{}}
	dir, err := e.cfg.ProjectPath(pc.ID)
	if err != nil {
		pp.Missing = true
		pp.Warnings = append(pp.Warnings, "path missing: "+err.Error())
		return pp
	}
	pp.Path = dir
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		pp.Missing = true
		pp.Warnings = append(pp.Warnings, "path missing: "+dir)
		return pp
	}
	journals, warnings, err := e.vault.ReadJournals(pc.ID)
	pp.Warnings = append(pp.Warnings, warnings...)
	if err != nil {
		pp.Unreadable = true
		if errors.Is(err, vault.ErrUnreadableJournal) {
			pp.Warnings = append(pp.Warnings, "unreadable journal, project skipped: "+err.Error())
		} else {
			pp.Warnings = append(pp.Warnings, "journals could not be read, project skipped: "+err.Error())
		}
		return pp
	}
	pl := &planner{e: e, pp: &pp, opts: opts, dir: dir, rolled: map[string]bool{}}
	for mid, j := range journals {
		if j == nil {
			continue
		}
		pp.journalSeqs[mid] = j.Seq
		if stored := e.store.JournalSeq(mid); stored > j.Seq {
			pl.rolled[mid] = true
			pp.Warnings = append(pp.Warnings, fmt.Sprintf("journal of %s rolled back (seq %d, previously seen %d)", mid, j.Seq, stored))
		}
	}
	heads := vault.ResolveHeads(journals)
	bases := e.store.Bases(pc.ID)

	set := map[string]struct{}{}
	for p := range heads {
		set[p] = struct{}{}
	}
	for p := range bases {
		set[p] = struct{}{}
	}
	for k, on := range opts.Track {
		if on && k.Project == pc.ID {
			set[k.Path] = struct{}{}
		}
	}
	paths := make([]string, 0, len(set))
	for p := range set {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	for _, p := range paths {
		if !ValidPath(p) {
			pp.Warnings = append(pp.Warnings, fmt.Sprintf("%s: skipped, invalid path", p))
			continue
		}
		local, localText, ok := pl.readLocal(p)
		if !ok {
			continue
		}
		var base *FileRef
		if b, ok := bases[p]; ok {
			base = &FileRef{Blob: b.Blob, Kind: b.Kind, Clock: b.Clock}
		}
		head, hasHead := heads[p]
		if it, ok := pl.decide(p, local, localText, base, head, hasHead); ok {
			pp.Items = append(pp.Items, it)
		}
	}
	return pp
}

// ValidPath reports whether p is a clean, relative, slash separated path with
// no "." or ".." segments — the only shape accepted for project files.
func ValidPath(p string) bool {
	if p == "" || strings.HasPrefix(p, "/") || strings.Contains(p, "\\") || strings.ContainsRune(p, 0) {
		return false
	}
	if path.Clean(p) != p {
		return false
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
	}
	return !filepath.IsAbs(filepath.FromSlash(p))
}

// readLocal reads the local copy of p: (nil, nil, true) when absent, ok=false
// (with a project warning) when the path must be skipped entirely.
func (pl *planner) readLocal(p string) (*FileRef, []byte, bool) {
	abs := filepath.Join(pl.dir, filepath.FromSlash(p))
	st, err := os.Lstat(abs)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil, true
		}
		pl.pp.Warnings = append(pl.pp.Warnings, fmt.Sprintf("%s: skipped: %v", p, err))
		return nil, nil, false
	}
	if st.Mode()&fs.ModeSymlink != 0 {
		pl.pp.Warnings = append(pl.pp.Warnings, fmt.Sprintf("%s: skipped, symbolic link", p))
		return nil, nil, false
	}
	if !st.Mode().IsRegular() {
		pl.pp.Warnings = append(pl.pp.Warnings, fmt.Sprintf("%s: skipped, not a regular file", p))
		return nil, nil, false
	}
	if pl.e.maxFileSize > 0 && st.Size() > pl.e.maxFileSize {
		// Present but not comparable: the path stays in the plan (Blob "")
		// so a vault-side change is still reported instead of vanishing.
		pl.pp.Warnings = append(pl.pp.Warnings, fmt.Sprintf("%s: larger than the %s limit (%d bytes), left in place", p, config.FormatSize(pl.e.maxFileSize), st.Size()))
		return &FileRef{
			Kind:    vault.KindFile,
			Size:    st.Size(),
			Mode:    uint32(st.Mode().Perm()),
			ModTime: st.ModTime(),
			Machine: pl.e.machine.ID,
		}, nil, true
	}
	content, err := fsutil.ReadFileMax(abs, pl.e.maxFileSize)
	if err != nil {
		pl.pp.Warnings = append(pl.pp.Warnings, fmt.Sprintf("%s: skipped: %v", p, err))
		return nil, nil, false
	}
	ref := &FileRef{
		Blob:    pl.e.vault.BlobID(content),
		Kind:    vault.KindFile,
		Size:    int64(len(content)),
		Mode:    uint32(st.Mode().Perm()),
		ModTime: st.ModTime(),
		Machine: pl.e.machine.ID,
	}
	return ref, content, true
}

// refFromEntry converts a journal entry into a FileRef.
func refFromEntry(en *vault.Entry) *FileRef {
	if en == nil {
		return nil
	}
	return &FileRef{
		Blob:    en.Blob,
		Kind:    en.Kind,
		Size:    en.Size,
		Mode:    en.Mode,
		ModTime: en.ModTime,
		Clock:   en.Clock.Copy(),
		Machine: en.Machine,
	}
}

// decide applies the decision table (spec §9.2) to one path. ok=false means
// no item (nothing to do or report).
func (pl *planner) decide(p string, local *FileRef, localText []byte, base *FileRef, head vault.Head, hasHead bool) (Item, bool) {
	it := Item{Key: ItemKey{Project: pl.pp.ID, Path: p}, Local: local, Base: base}
	track := pl.opts.Track[it.Key]
	restore := pl.opts.Mode == ModeRestore
	var single *vault.Entry
	if hasHead {
		it.Candidates = head.Candidates
		single = head.Entry
	}
	note := ""

	if local != nil && local.Blob == "" {
		return pl.tooLarge(it, base, single, hasHead)
	}

	// Row 1: untracked head.
	if single != nil && single.Kind == vault.KindUntracked {
		if restore {
			return it, false
		}
		it.Head = refFromEntry(single)
		if track {
			if local == nil {
				pl.pp.Warnings = append(pl.pp.Warnings, fmt.Sprintf("%s: not found locally, cannot re-add", p))
				return it, false
			}
			it.Action = ActionUpload
			it.LocalText = localText
			it.Reason = "re-added: uploading (was untracked in the vault)"
			return pl.finish(it, note)
		}
		if base == nil {
			return it, false
		}
		it.Action = ActionUntrack
		it.Reason = "untracked in the vault: base dropped, local file left in place"
		return pl.finish(it, note)
	}

	// Row 2: concurrent heads → pre-merge.
	if hasHead && single == nil {
		if !pl.premerge(&it, p, head, local, localText) {
			return pl.finish(it, "")
		}
		note = "concurrent vault heads merged cleanly"
	} else if single != nil {
		it.Head = refFromEntry(single)
	}

	// Rows 3-4: no head.
	if it.Head == nil {
		if restore {
			return it, false
		}
		if local != nil {
			if track || base != nil {
				it.Action = ActionUpload
				it.LocalText = localText
				if base != nil && !track {
					it.Reason = "no longer in the vault: re-publishing the local copy"
				} else {
					it.Reason = "new file: uploading"
				}
				return pl.finish(it, note)
			}
			return it, false
		}
		if track {
			pl.pp.Warnings = append(pl.pp.Warnings, fmt.Sprintf("%s: not found locally, cannot add", p))
			return it, false
		}
		if base != nil {
			it.Action = ActionUntrack
			it.Reason = "not in the vault and absent locally: base dropped"
			return pl.finish(it, note)
		}
		return it, false
	}

	h := it.Head
	// Rows 5-9: deleted head.
	if h.Kind == vault.KindDeleted {
		if restore {
			return it, false
		}
		if local == nil { // row 5
			if base != nil && base.Kind == vault.KindDeleted {
				return it, false
			}
			it.Action = ActionConverge
			it.Reason = "deleted in the vault and absent locally: recording the deletion"
			return pl.finish(it, note)
		}
		it.LocalText = localText
		if track { // files add on a deleted path: re-publish the local copy
			it.Action = ActionUpload
			it.Reason = "re-added: uploading (was deleted in the vault)"
			return pl.finish(it, note)
		}
		switch {
		case base == nil: // row 8
			it.Action = ActionConflict
			it.Conflict = ConflictModifyDelete
			it.NeedsResolution = true
			it.Reason = "deleted in the vault, present locally with no base: keep (re-upload) or delete (trash)"
		case base.Kind == vault.KindDeleted: // row 9
			// Nothing applies in any mode: the deletion was already recorded
			// here and the local copy is deliberately left alone. Original is
			// ActionMissingLocal so status counts it as "tracked, report only".
			it.Action = ActionReportOnly
			it.Original = ActionMissingLocal
			it.Reason = "deleted in the vault, local copy left in place (re-track with files add, or remove it)"
		case base.Kind == vault.KindFile && local.Blob == base.Blob: // row 6
			it.Action = ActionTrashLocal
			it.Reason = "deleted in the vault, unchanged locally: moving the local copy to the trash"
		default: // row 7
			it.Action = ActionConflict
			it.Conflict = ConflictModifyDelete
			it.NeedsResolution = true
			it.Reason = "deleted in the vault but modified locally: keep (re-upload) or delete (trash)"
		}
		return pl.finish(it, note)
	}

	// Rows 10-14: file head (single or synthetic).
	if restore {
		if local != nil && local.Blob == h.Blob && h.Mode != 0 && local.Mode != h.Mode&uint32(fs.ModePerm) {
			// Same bytes, different mode: restore reapplies the recorded mode
			// (spec §13); download() only chmods when the content matches.
			if !pl.loadHeadText(&it) {
				return pl.finish(it, note)
			}
			it.LocalText = localText
			it.Action = ActionDownload
			it.Reason = fmt.Sprintf("restore: content matches, restoring mode %04o", h.Mode&uint32(fs.ModePerm))
			return pl.finish(it, note)
		}
		if local != nil && local.Blob == h.Blob {
			if base != nil && base.Kind == vault.KindFile && base.Blob == h.Blob && !it.Synthetic {
				it.Action = ActionInSync
				it.Reason = "restore: local copy already matches the vault"
			} else {
				it.Action = ActionConverge
				it.Reason = "restore: local copy already matches the vault, recording base"
			}
			return pl.finish(it, note)
		}
		if !pl.loadHeadText(&it) {
			return pl.finish(it, note)
		}
		it.LocalText = localText
		it.Action = ActionDownload
		if local != nil {
			it.Reason = "restore: writing the vault version (local copy goes to the trash)"
		} else {
			it.Reason = "restore: writing the vault version"
		}
		return pl.finish(it, note)
	}

	if local != nil {
		it.LocalText = localText
		if base == nil || base.Kind != vault.KindFile { // row 10
			if local.Blob == h.Blob {
				it.Action = ActionConverge
				it.Reason = "same content locally and in the vault: recording base"
				return pl.finish(it, note)
			}
			if !pl.loadHeadText(&it) {
				return pl.finish(it, note)
			}
			it.Action = ActionConflict
			it.Conflict = ConflictNoBase
			it.Merge = merge.ThreeWay(p, nil, localText, it.HeadText)
			it.NeedsResolution = !it.Merge.Clean
			it.Reason = "differs locally and in the vault with no common base: choose a side"
			return pl.finish(it, note)
		}
		// Row 11.
		lb, hb := local.Blob == base.Blob, h.Blob == base.Blob
		switch {
		case lb && hb:
			it.Action = ActionInSync
			it.LocalText = nil
			it.Reason = "in sync"
		case !lb && hb:
			it.Action = ActionUpload
			it.Reason = "modified locally: uploading"
		case lb && !hb:
			if !pl.loadHeadText(&it) {
				return pl.finish(it, note)
			}
			it.Action = ActionDownload
			it.Reason = "modified in the vault: downloading"
			pl.rollbackCheck(&it, base, h)
		case local.Blob == h.Blob:
			it.Action = ActionConverge
			it.Reason = "same change locally and in the vault: recording base"
		default:
			if !pl.loadHeadText(&it) {
				return pl.finish(it, note)
			}
			baseText, err := pl.e.vault.ReadBlob(base.Blob)
			if err != nil {
				pl.pending(&it, base.Blob, err)
				return pl.finish(it, note)
			}
			it.BaseText = baseText
			it.Action = ActionConflict
			it.Conflict = ConflictContent
			it.Merge = merge.ThreeWay(p, baseText, localText, it.HeadText)
			if it.Merge.Clean {
				it.NeedsResolution = false
				it.Reason = "modified locally and in the vault: merged automatically"
				if it.Merge.Note != "" {
					it.Reason += " (" + it.Merge.Note + ")"
				}
			} else {
				it.NeedsResolution = true
				it.Reason = fmt.Sprintf("modified locally and in the vault: %d conflicting %s", len(it.Merge.Hunks), plural(len(it.Merge.Hunks), "hunk", "hunks"))
			}
		}
		return pl.finish(it, note)
	}

	// Local absent, file head.
	if base == nil || base.Kind != vault.KindFile { // row 12
		if !pl.loadHeadText(&it) {
			return pl.finish(it, note)
		}
		it.Action = ActionDownload
		it.Reason = "new in the vault: downloading"
		pl.rollbackCheck(&it, base, h)
		return pl.finish(it, note)
	}
	if h.Blob == base.Blob { // row 13
		if pl.opts.PropagateDeletes {
			it.Action = ActionDeleteRemote
			it.NeedsResolution = true
			it.Reason = "deleted locally: propagating the deletion to the vault"
		} else {
			it.Action = ActionMissingLocal
			it.Reason = "tracked file missing locally (use --delete or files delete to remove it everywhere, restore to get it back)"
		}
		return pl.finish(it, note)
	}
	// Row 14.
	if !pl.loadHeadText(&it) {
		return pl.finish(it, note)
	}
	it.Action = ActionDownload
	it.Reason = "missing locally, newer version in the vault: downloading"
	pl.rollbackCheck(&it, base, h)
	return pl.finish(it, note)
}

// tooLarge decides a path whose local file exceeds max_file_size (Local.Blob
// is ""): it can be neither hashed, uploaded, compared nor replaced, so the
// vault side is reported only (Original names the action it would imply) and
// the base is kept. Only an untracked head still drops the base, which needs
// no content.
func (pl *planner) tooLarge(it Item, base *FileRef, single *vault.Entry, hasHead bool) (Item, bool) {
	track := pl.opts.Track[it.Key]
	why := fmt.Sprintf("local file larger than the %s limit (%d bytes)", config.FormatSize(pl.e.maxFileSize), it.Local.Size)
	report := func(orig Action, what string) (Item, bool) {
		it.Action = ActionReportOnly
		it.Original = orig
		it.NeedsResolution = false
		it.Reason = what + ": " + why + ", left in place"
		return it, true
	}
	if single != nil {
		it.Head = refFromEntry(single)
	}
	if pl.opts.Mode == ModeRestore {
		if single == nil || single.Kind != vault.KindFile {
			return it, false // restore ignores tombstones, untracked and concurrent heads
		}
		return report(ActionDownload, "restore: local copy cannot be replaced")
	}
	switch {
	case single != nil && single.Kind == vault.KindUntracked: // row 1
		if track {
			return report(ActionUpload, "cannot re-add")
		}
		if base == nil {
			return it, false
		}
		it.Action = ActionUntrack
		it.Reason = "untracked in the vault: base dropped, local file left in place"
		return pl.finish(it, "")
	case !hasHead: // rows 3-4
		if track || base != nil {
			return report(ActionUpload, "cannot upload")
		}
		return it, false
	case single == nil: // row 2
		return report(ActionConflict, "concurrent vault heads ("+describeCandidates(it.Candidates)+") cannot be resolved here")
	case single.Kind == vault.KindDeleted: // rows 6-9
		if base != nil && base.Kind == vault.KindDeleted {
			return report(ActionMissingLocal, "deleted in the vault")
		}
		return report(ActionTrashLocal, "deleted in the vault, local copy cannot be compared with its base")
	case base != nil && base.Kind == vault.KindFile && base.Blob == single.Blob: // row 11, vault unchanged
		return report(ActionUpload, "vault unchanged, local changes (if any) cannot be uploaded")
	}
	return report(ActionDownload, "modified in the vault, local copy cannot be compared or replaced")
}

// plural picks the singular or plural noun.
func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// loadHeadText reads the head blob into it.HeadText; on failure the item
// becomes ActionPending and false is returned.
func (pl *planner) loadHeadText(it *Item) bool {
	if it.Head == nil {
		return false
	}
	if it.HeadText != nil { // synthetic head
		return true
	}
	if it.Head.Blob == "" {
		it.HeadText = []byte{}
		return true
	}
	text, err := pl.e.vault.ReadBlob(it.Head.Blob)
	if err != nil {
		pl.pending(it, it.Head.Blob, err)
		return false
	}
	it.HeadText = text
	return true
}

// pending marks an item as waiting for a blob.
func (pl *planner) pending(it *Item, blob string, err error) {
	it.Action = ActionPending
	it.Conflict = ConflictNone
	it.Merge = nil
	it.NeedsResolution = false
	it.Reason = fmt.Sprintf("blob %s not available yet (remote still in flight?): %v", shortID(blob), err)
}

// shortID abbreviates a blob id for messages.
func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// rollbackCheck flags a download whose single head is older than (or
// concurrent with) the base this machine already converged to.
func (pl *planner) rollbackCheck(it *Item, base, h *FileRef) {
	if base == nil || len(base.Clock) == 0 || len(h.Clock) == 0 {
		return
	}
	cmp := h.Clock.Compare(base.Clock)
	if cmp != vault.Before && cmp != vault.Concurrent {
		return
	}
	msg := "vault head is older than the version last synced here (a journal was replaced by an older copy?)"
	if pl.opts.AcceptRollback || pl.opts.Mode == ModeRestore {
		it.AcceptedRollback = true
		it.Reason = "accepting rollback: " + it.Reason
		return
	}
	it.Original = it.Action
	it.Action = ActionRollback
	it.NeedsResolution = false
	it.Reason = msg + "; apply with --accept-rollback or restore"
}

// premerge handles decision table row 2: pairwise ThreeWay of the concurrent
// candidates against Head.Base. Returns true when the fold was clean (it.Head
// is then the synthetic merged head); otherwise it is already a conflict or
// pending item.
func (pl *planner) premerge(it *Item, p string, head vault.Head, local *FileRef, localText []byte) bool {
	cands := head.Candidates
	var baseText []byte
	if head.Base != "" {
		// The common base is needed to merge: an unreadable one is a blob
		// still in flight (spec §9.1: any read failure ⇒ pending), not "no
		// base" — that would turn a mergeable pair into a conflict.
		b, err := pl.e.vault.ReadBlob(head.Base)
		if err != nil {
			pl.pending(it, head.Base, err)
			return false
		}
		baseText = b
	}
	texts := make([][]byte, len(cands))
	tombstones := 0
	firstFile := -1
	for i, c := range cands {
		if c.Kind != vault.KindFile {
			tombstones++
			continue
		}
		if firstFile < 0 {
			firstFile = i
		}
		t, err := pl.e.vault.ReadBlob(c.Blob)
		if err != nil {
			pl.pending(it, c.Blob, err)
			return false
		}
		texts[i] = t
	}
	fail := func(r *merge.Result, reason string) {
		it.Action = ActionConflict
		it.Conflict = ConflictConcurrent
		it.Merge = r
		it.NeedsResolution = true
		it.LocalText = localText
		it.BaseText = baseText
		it.Reason = reason
		remote, ambiguous := pl.remoteSide(cands)
		if remote >= 0 {
			it.Remote = refFromEntry(&cands[remote])
		}
		// HeadText is the other machine's file version; when every other
		// machine deleted the path it falls back to the vault's surviving
		// file version (the one that differs from the local copy if any).
		switch {
		case remote >= 0 && cands[remote].Kind == vault.KindFile:
			it.HeadText = texts[remote]
		default:
			shown := firstFile
			for i, c := range cands {
				if c.Kind == vault.KindFile && c.Machine != pl.e.machine.ID {
					shown = i
					break
				}
			}
			if shown == firstFile && local != nil {
				for i, c := range cands {
					if c.Kind == vault.KindFile && c.Blob != local.Blob {
						shown = i
						break
					}
				}
			}
			if shown >= 0 {
				it.HeadText = texts[shown]
			}
		}
		switch {
		case ambiguous:
			it.Reason += "; no single remote side (several other machines disagree)"
		case remote >= 0 && cands[remote].Kind == vault.KindFile:
			it.Reason += "; remote = " + shortMachine(cands[remote].Machine)
		case remote >= 0:
			it.Reason += "; remote = deleted on " + shortMachine(cands[remote].Machine)
		}
	}
	if tombstones > 0 || firstFile < 0 {
		fail(nil, fmt.Sprintf("concurrent vault heads: %s", describeCandidates(cands)))
		return false
	}
	cur := texts[0]
	for i := 1; i < len(cands); i++ {
		r := merge.ThreeWay(p, baseText, cur, texts[i])
		if !r.Clean {
			why := "no common base"
			if baseText != nil {
				why = fmt.Sprintf("%d conflicting %s", len(r.Hunks), plural(len(r.Hunks), "hunk", "hunks"))
			}
			fail(r, fmt.Sprintf("concurrent vault heads (%s): %s", why, describeCandidates(cands)))
			return false
		}
		cur = r.Merged
	}
	clock := vault.Clock{}
	var mode uint32
	for _, c := range cands {
		clock = clock.Merge(c.Clock)
		if mode == 0 {
			mode = c.Mode
		}
	}
	it.Head = &FileRef{
		Blob:  pl.e.vault.BlobID(cur),
		Kind:  vault.KindFile,
		Size:  int64(len(cur)),
		Mode:  mode,
		Clock: clock,
	}
	it.HeadText = cur
	it.Synthetic = true
	return true
}

// remoteSide picks the candidate that "remote" means for this machine: the
// side written by another machine. It returns its index and false when the
// other machines agree on one state (all the same blob, or all deletions),
// -1 and true when they disagree ("remote" then names no single side), and
// -1, false when every candidate is this machine's own (cannot happen for a
// concurrent head, kept for safety).
func (pl *planner) remoteSide(cands []vault.Entry) (int, bool) {
	own := pl.e.machine.ID
	first := -1
	for i, c := range cands {
		if c.Machine == own {
			continue
		}
		if first < 0 {
			first = i
			continue
		}
		f := cands[first]
		if c.Kind != f.Kind || (c.Kind == vault.KindFile && c.Blob != f.Blob) {
			return -1, true
		}
	}
	return first, false
}

// describeCandidates renders "modified on <m>, deleted on <m>" for messages.
func describeCandidates(cands []vault.Entry) string {
	parts := make([]string, 0, len(cands))
	for _, c := range cands {
		verb := "modified"
		switch c.Kind {
		case vault.KindDeleted:
			verb = "deleted"
		case vault.KindUntracked:
			verb = "untracked"
		}
		parts = append(parts, verb+" on "+shortMachine(c.Machine))
	}
	return strings.Join(parts, ", ")
}

// shortMachine abbreviates a machine id for messages.
func shortMachine(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	if id == "" {
		return "?"
	}
	return id
}

// finish applies the cross-cutting rules to a decided item: synthetic heads
// always converge (so the merge entry is written), journal-sequence rollback,
// and the push/pull mode filters.
func (pl *planner) finish(it Item, note string) (Item, bool) {
	if it.Synthetic && it.Action == ActionInSync {
		it.Action = ActionConverge
		it.Reason = "recording the merged version"
	}
	if note != "" && it.Action != ActionPending {
		it.Reason = note + ": " + it.Reason
	}

	// Journal sequence rollback: any item whose head reflects a rolled-back
	// journal and differs from what this machine converged to is suspect.
	if len(pl.rolled) > 0 && it.Head != nil && it.Action != ActionInSync && it.Action != ActionPending {
		var from []string
		for _, c := range it.Candidates {
			if pl.rolled[c.Machine] {
				from = append(from, shortMachine(c.Machine))
			}
		}
		agrees := it.Base != nil && it.Base.Kind == it.Head.Kind && it.Base.Blob == it.Head.Blob
		if len(from) > 0 && !agrees {
			msg := fmt.Sprintf("journal of %s rolled back", strings.Join(from, ", "))
			switch {
			case it.Action == ActionRollback:
				it.Reason = msg + "; " + it.Reason
			case pl.opts.AcceptRollback || pl.opts.Mode == ModeRestore:
				it.AcceptedRollback = true
				if !strings.HasPrefix(it.Reason, "accepting rollback") {
					it.Reason = "accepting rollback: " + it.Reason
				}
				it.Reason = msg + "; " + it.Reason
			default:
				it.Original = it.Action
				it.Action = ActionRollback
				it.NeedsResolution = false
				it.Reason = msg + "; apply with --accept-rollback or restore"
			}
		}
	}

	switch pl.opts.Mode {
	case ModePush:
		if it.Action == ActionDownload || it.Action == ActionTrashLocal {
			it.Original = it.Action
			it.Action = ActionReportOnly
			it.NeedsResolution = false
			it.Reason += " (not applied in push mode)"
		}
	case ModePull:
		if it.Action == ActionUpload || it.Action == ActionDeleteRemote {
			it.Original = it.Action
			it.Action = ActionReportOnly
			it.NeedsResolution = false
			it.Reason += " (not applied in pull mode)"
		}
	}
	return it, true
}
