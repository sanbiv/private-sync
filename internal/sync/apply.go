package sync

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/sanbiv/private-sync/internal/fsutil"
	"github.com/sanbiv/private-sync/internal/state"
	"github.com/sanbiv/private-sync/internal/vault"
)

// Apply executes a plan with the given resolutions.
//
// Per project (Missing/Unreadable ones are skipped): the items are applied in
// order, then this machine's journal is written (only when entries changed),
// the base store updated and saved, and the machine info refreshed. Base
// store changes of a project are committed only after its journal was
// written, so a failure never leaves a base pointing at an entry the vault
// does not have. Context cancellation between items is safe: the next Plan
// recomputes everything from disk.
func (e *Engine) Apply(ctx context.Context, p *Plan, res Resolutions, opts Options) (*Report, error) {
	if err := e.ready(); err != nil {
		return nil, fmt.Errorf("sync.Apply: %w", err)
	}
	if p == nil {
		return nil, errors.New("sync.Apply: nil plan")
	}
	rep := &Report{}
	if err := ctx.Err(); err != nil {
		return rep, err
	}

	if n, err := e.store.TrashPurge(TrashRetention); err != nil {
		emit(opts.Progress, Event{Stage: "apply", Message: "trash purge failed", Err: err})
	} else if n > 0 {
		emit(opts.Progress, Event{Stage: "apply", Message: fmt.Sprintf("purged %d trash %s older than 30 days", n, plural(n, "entry", "entries"))})
	}
	_ = fsutil.CleanupTemp(e.vault.Dir(), e.suffix())

	total := 0
	for i := range p.Projects {
		total += len(p.Projects[i].Items)
	}
	done := 0
	touched := false
	for pi := range p.Projects {
		pp := &p.Projects[pi]
		if pp.Missing || pp.Unreadable {
			continue
		}
		if err := ctx.Err(); err != nil {
			return rep, err
		}
		if st, err := os.Stat(pp.Path); err != nil || !st.IsDir() {
			rep.Errors = append(rep.Errors, ItemError{Key: ItemKey{Project: pp.ID}, Err: fmt.Errorf("project path missing: %s", pp.Path)})
			done += len(pp.Items)
			continue
		}
		j, err := e.vault.ReadJournal(pp.ID, e.machine.ID)
		if err != nil {
			return rep, fmt.Errorf("sync.Apply: %s: %w", pp.ID, err)
		}
		if j == nil {
			j = &vault.Journal{Machine: e.machine.ID}
		}
		if j.Entries == nil {
			j.Entries = map[string]vault.Entry{}
		}
		if pp.journalSeqs == nil {
			pp.journalSeqs = map[string]uint64{}
		}
		ap := &applier{e: e, pp: pp, j: j, rep: rep, res: res, opts: opts, mode: p.Mode, bases: map[string]*state.BaseEntry{}}
		for i := range pp.Items {
			if err := ctx.Err(); err != nil {
				return rep, err
			}
			it := &pp.Items[i]
			ap.cleanupTemp(it.Key.Path)
			emit(opts.Progress, Event{Stage: "apply", Project: pp.ID, Path: it.Key.Path, Message: it.Action.String() + ": " + it.Key.Path, Done: done, Total: total})
			if err := ap.apply(it); err != nil {
				rep.Errors = append(rep.Errors, ItemError{Key: it.Key, Err: err})
				emit(opts.Progress, Event{Stage: "apply", Project: pp.ID, Path: it.Key.Path, Message: "failed: " + it.Key.Path, Done: done, Total: total, Err: err})
			}
			done++
		}
		if ap.changed {
			if err := e.vault.WriteJournal(pp.ID, j); err != nil {
				return rep, fmt.Errorf("sync.Apply: %s: %w", pp.ID, err)
			}
			pp.journalSeqs[e.machine.ID] = j.Seq
		}
		for path, b := range ap.bases {
			if b == nil {
				e.store.DeleteBase(pp.ID, path)
			} else {
				e.store.SetBase(pp.ID, path, *b)
			}
		}
		for mid, seq := range pp.journalSeqs {
			if seq > e.store.JournalSeq(mid) || opts.AcceptRollback || p.Mode == ModeRestore {
				e.store.SetJournalSeq(mid, seq)
			}
		}
		if err := e.store.Save(); err != nil {
			return rep, fmt.Errorf("sync.Apply: %w", err)
		}
		touched = true
	}
	if touched {
		name := ""
		if e.cfg != nil {
			name = e.cfg.Machine.Name
		}
		info := vault.MachineInfo{ID: e.machine.ID, Name: name, Hostname: e.hostname(), LastSeen: e.now()}
		if err := e.vault.WriteMachine(info); err != nil {
			return rep, fmt.Errorf("sync.Apply: %w", err)
		}
	}
	emit(opts.Progress, Event{Stage: "apply", Message: "apply complete", Done: done, Total: total})
	return rep, nil
}

// applier holds the per-project state while applying.
type applier struct {
	e       *Engine
	pp      *ProjectPlan
	j       *vault.Journal
	rep     *Report
	res     Resolutions
	opts    Options
	mode    Mode                        // the plan's mode
	bases   map[string]*state.BaseEntry // pending base changes (nil = delete)
	changed bool
}

func (ap *applier) abs(p string) string {
	return filepath.Join(ap.pp.Path, filepath.FromSlash(p))
}

// cleanupTemp removes a leftover temp file of this machine next to p.
func (ap *applier) cleanupTemp(p string) {
	tmp := ap.abs(p) + fsutil.TempPrefix + ap.e.suffix()
	if fsutil.Exists(tmp) {
		_ = os.Remove(tmp)
	}
}

func (ap *applier) setBase(p string, b *state.BaseEntry) { ap.bases[p] = b }

func (ap *applier) putEntry(en vault.Entry) {
	ap.j.Entries[en.Path] = en
	ap.changed = true
}

// apply dispatches one item.
func (ap *applier) apply(it *Item) error {
	switch it.Action {
	case ActionInSync:
		return nil
	case ActionPending:
		ap.rep.Pending++
		return nil
	case ActionReportOnly, ActionRollback, ActionMissingLocal:
		ap.rep.Skipped++
		return nil
	case ActionUntrack:
		ap.setBase(it.Key.Path, nil)
		ap.rep.Untracked++
		return nil
	case ActionUpload:
		return ap.upload(it)
	case ActionDownload:
		return ap.download(it)
	case ActionConverge:
		return ap.converge(it)
	case ActionTrashLocal:
		return ap.trashLocal(it)
	case ActionDeleteRemote:
		return ap.deleteRemote(it)
	case ActionConflict:
		return ap.conflict(it)
	}
	return fmt.Errorf("unknown action %v", it.Action)
}

// readCurrent reads the local file as it is now.
func (ap *applier) readCurrent(p string) (content []byte, info fs.FileInfo, exists bool, err error) {
	abs := ap.abs(p)
	info, err = os.Lstat(abs)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil, false, nil
		}
		return nil, nil, false, err
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		return nil, nil, true, fmt.Errorf("%s: is a symbolic link", p)
	}
	if !info.Mode().IsRegular() {
		return nil, nil, true, fmt.Errorf("%s: not a regular file", p)
	}
	content, err = fsutil.ReadFileMax(abs, ap.e.maxFileSize)
	if err != nil {
		return nil, nil, true, err
	}
	return content, info, true, nil
}

// checkStale compares the current local state with what Plan saw.
func (ap *applier) checkStale(it *Item, content []byte, exists bool) error {
	if exists != (it.Local != nil) {
		return fmt.Errorf("%w: %s", ErrStale, it.Key.Path)
	}
	if exists && ap.e.vault.BlobID(content) != it.Local.Blob {
		return fmt.Errorf("%w: %s", ErrStale, it.Key.Path)
	}
	return nil
}

// nextClock is merge(all candidate clocks, head, base, own entry).Tick(self).
func (ap *applier) nextClock(it *Item) vault.Clock {
	c := vault.Clock{}
	for _, cand := range it.Candidates {
		c = c.Merge(cand.Clock)
	}
	if it.Head != nil {
		c = c.Merge(it.Head.Clock)
	}
	if it.Base != nil {
		c = c.Merge(it.Base.Clock)
	}
	if own, ok := ap.j.Entries[it.Key.Path]; ok {
		c = c.Merge(own.Clock)
	}
	return c.Tick(ap.e.machine.ID)
}

// headParents returns the non-empty blobs of the head candidates (or the
// head itself), falling back to the base blob.
func (ap *applier) headParents(it *Item) []string {
	var out []string
	for _, cand := range it.Candidates {
		out = appendUnique(out, cand.Blob)
	}
	if len(out) == 0 && it.Head != nil {
		out = appendUnique(out, it.Head.Blob)
	}
	if len(out) == 0 && it.Base != nil {
		out = appendUnique(out, it.Base.Blob)
	}
	return out
}

// allParents returns the non-empty blobs of {head candidates, base} deduped.
func (ap *applier) allParents(it *Item) []string {
	var out []string
	for _, cand := range it.Candidates {
		out = appendUnique(out, cand.Blob)
	}
	if len(it.Candidates) == 0 && it.Head != nil {
		out = appendUnique(out, it.Head.Blob)
	}
	if it.Base != nil {
		out = appendUnique(out, it.Base.Blob)
	}
	return out
}

func appendUnique(ids []string, id string) []string {
	if id == "" {
		return ids
	}
	for _, x := range ids {
		if x == id {
			return ids
		}
	}
	return append(ids, id)
}

// writeEntry writes the blob for content, records a KindFile entry for it and
// updates the pending base. mode/size/mtime describe the local file.
func (ap *applier) writeEntry(it *Item, content []byte, parents []string, info fs.FileInfo) (string, error) {
	id, _, err := ap.e.vault.WriteBlob(content)
	if err != nil {
		return "", err
	}
	ap.recordEntry(it, id, content, parents, info)
	return id, nil
}

// recordEntry records a KindFile entry for an already written blob and
// updates the pending base (spec §9.3 order: blob, local file, then entry).
func (ap *applier) recordEntry(it *Item, id string, content []byte, parents []string, info fs.FileInfo) {
	clock := ap.nextClock(it)
	en := vault.Entry{
		Path:      it.Key.Path,
		Kind:      vault.KindFile,
		Blob:      id,
		Clock:     clock,
		Parents:   parents,
		Size:      int64(len(content)),
		UpdatedAt: ap.e.now(),
		Machine:   ap.e.machine.ID,
	}
	if info != nil {
		en.Mode = uint32(info.Mode().Perm())
		en.ModTime = info.ModTime()
	} else if it.Head != nil {
		en.Mode = it.Head.Mode
	}
	ap.putEntry(en)
	ap.setBase(it.Key.Path, &state.BaseEntry{Blob: id, Kind: vault.KindFile, Clock: clock})
}

// tombstoneEntry records a KindDeleted entry with the next clock and the
// given parents, and makes the tombstone the pending base.
func (ap *applier) tombstoneEntry(it *Item, parents []string) vault.Clock {
	clock := ap.nextClock(it)
	ap.putEntry(vault.Entry{
		Path:      it.Key.Path,
		Kind:      vault.KindDeleted,
		Clock:     clock,
		Parents:   parents,
		UpdatedAt: ap.e.now(),
		Machine:   ap.e.machine.ID,
	})
	ap.setBase(it.Key.Path, &state.BaseEntry{Kind: vault.KindDeleted, Clock: clock})
	return clock
}

// upload re-reads the local file (ErrStale when it changed) and publishes it.
func (ap *applier) upload(it *Item) error {
	content, info, exists, err := ap.readCurrent(it.Key.Path)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("%w: %s vanished", ErrStale, it.Key.Path)
	}
	if err := ap.checkStale(it, content, exists); err != nil {
		return err
	}
	if _, err := ap.writeEntry(it, content, ap.headParents(it), info); err != nil {
		return err
	}
	ap.rep.Uploaded++
	return nil
}

// headContent returns the bytes of the head (HeadText, else the blob).
func (ap *applier) headContent(it *Item) ([]byte, error) {
	if it.HeadText != nil {
		return it.HeadText, nil
	}
	if it.Head == nil {
		return nil, errors.New("no head content")
	}
	if it.Head.Blob == "" {
		return []byte{}, nil
	}
	return ap.e.vault.ReadBlob(it.Head.Blob)
}

// trashPreimage stores the current local content in the trash when it is not
// the base (or force is set).
func (ap *applier) trashPreimage(it *Item, content []byte, info fs.FileInfo, force bool) error {
	if !force && it.Base != nil && it.Local != nil && it.Base.Kind == vault.KindFile && it.Base.Blob == it.Local.Blob {
		return nil
	}
	var mode uint32
	if info != nil {
		mode = uint32(info.Mode().Perm())
	}
	if _, err := ap.e.store.TrashPut(ap.e.vault.Keys(), ap.pp.ID, it.Key.Path, content, mode); err != nil {
		return fmt.Errorf("trash: %w", err)
	}
	return nil
}

// writeLocal writes content atomically with the mode of the head, else the
// existing file's, else 0600.
func (ap *applier) writeLocal(it *Item, content []byte, info fs.FileInfo) (fs.FileInfo, error) {
	var mode fs.FileMode
	if it.Head != nil {
		mode = fs.FileMode(it.Head.Mode) & fs.ModePerm
	}
	if mode == 0 && info != nil {
		mode = info.Mode().Perm()
	}
	if mode == 0 {
		mode = 0o600
	}
	abs := ap.abs(it.Key.Path)
	if err := fsutil.WriteFileAtomic(abs, content, mode, ap.e.suffix()); err != nil {
		return nil, err
	}
	return os.Lstat(abs)
}

// download writes the head to the local file (pre-image to the trash).
func (ap *applier) download(it *Item) error {
	if it.Head == nil {
		return errors.New("download without a head")
	}
	content, info, exists, err := ap.readCurrent(it.Key.Path)
	if err != nil {
		return err
	}
	if err := ap.checkStale(it, content, exists); err != nil {
		return err
	}
	text, err := ap.headContent(it)
	if err != nil {
		return err
	}
	// Spec §9.3: the blob first, then the local file, then the entry. Only a
	// synthetic (pre-merged) head has a blob that is not in the vault yet.
	var merged string
	if it.Synthetic {
		if merged, _, err = ap.e.vault.WriteBlob(text); err != nil {
			return err
		}
	}
	var written fs.FileInfo
	if exists && bytes.Equal(content, text) {
		// Same bytes: reconcile only the mode with the head's (a mode-only
		// change in the vault would otherwise never propagate).
		if written, err = ap.reconcileMode(it, info); err != nil {
			return err
		}
	} else {
		if exists {
			force := ap.mode == ModeRestore || it.AcceptedRollback
			if err := ap.trashPreimage(it, content, info, force); err != nil {
				return err
			}
		}
		if written, err = ap.writeLocal(it, text, info); err != nil {
			return err
		}
	}
	if it.Synthetic {
		ap.recordEntry(it, merged, text, ap.headParents(it), written)
	} else {
		ap.setBase(it.Key.Path, &state.BaseEntry{Blob: it.Head.Blob, Kind: it.Head.Kind, Clock: it.Head.Clock.Copy()})
	}
	ap.rep.Downloaded++
	return nil
}

// reconcileMode applies the head's mode to an existing local file whose
// content already matches, returning the fresh file info.
func (ap *applier) reconcileMode(it *Item, info fs.FileInfo) (fs.FileInfo, error) {
	if it.Head == nil || it.Head.Mode == 0 {
		return info, nil
	}
	want := fs.FileMode(it.Head.Mode) & fs.ModePerm
	if info.Mode().Perm() == want {
		return info, nil
	}
	abs := ap.abs(it.Key.Path)
	if err := os.Chmod(abs, want); err != nil {
		return nil, err
	}
	return os.Lstat(abs)
}

// converge records the head as base (writing the merge entry for a
// synthetic head).
func (ap *applier) converge(it *Item) error {
	if it.Head == nil {
		return errors.New("converge without a head")
	}
	if it.Synthetic {
		text, err := ap.headContent(it)
		if err != nil {
			return err
		}
		_, info, _, err := ap.readCurrent(it.Key.Path)
		if err != nil {
			return err
		}
		if _, err := ap.writeEntry(it, text, ap.headParents(it), info); err != nil {
			return err
		}
	} else {
		ap.setBase(it.Key.Path, &state.BaseEntry{Blob: it.Head.Blob, Kind: it.Head.Kind, Clock: it.Head.Clock.Copy()})
	}
	ap.rep.Converged++
	return nil
}

// trashLocal moves the unchanged local copy to the trash and records the
// tombstone as base.
func (ap *applier) trashLocal(it *Item) error {
	content, info, exists, err := ap.readCurrent(it.Key.Path)
	if err != nil {
		return err
	}
	if exists {
		if err := ap.checkStale(it, content, exists); err != nil {
			return err
		}
		if err := ap.trashPreimage(it, content, info, true); err != nil {
			return err
		}
		if err := os.Remove(ap.abs(it.Key.Path)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	var clock vault.Clock
	if it.Head != nil {
		clock = it.Head.Clock.Copy()
	}
	ap.setBase(it.Key.Path, &state.BaseEntry{Kind: vault.KindDeleted, Clock: clock})
	ap.rep.Trashed++
	return nil
}

// unresolved records an item that needed an answer and got none.
func (ap *applier) unresolved(it *Item) {
	ap.rep.Skipped++
	ap.rep.Unresolved = append(ap.rep.Unresolved, it.Key)
}

// deleteRemote writes a KindDeleted entry once confirmed.
func (ap *applier) deleteRemote(it *Item) error {
	r, ok := ap.res[it.Key]
	if !ok {
		ap.unresolved(it)
		return nil
	}
	switch r.Kind {
	case ChooseSkip:
		ap.rep.Skipped++
		return nil
	case ChooseConfirm:
	default:
		return fmt.Errorf("resolution %v not applicable to delete-remote", r.Kind)
	}
	content, _, exists, err := ap.readCurrent(it.Key.Path)
	if err != nil {
		return err
	}
	if err := ap.checkStale(it, content, exists); err != nil {
		return err
	}
	ap.tombstoneEntry(it, ap.headParents(it))
	ap.rep.Deleted++
	return nil
}

// conflict applies the resolution of a conflict item.
func (ap *applier) conflict(it *Item) error {
	r, ok := ap.res[it.Key]
	if !ok {
		if it.Merge != nil && it.Merge.Clean {
			r = Resolution{Kind: ChooseMerged}
		} else {
			ap.unresolved(it)
			return nil
		}
	}
	if it.Conflict == ConflictModifyDelete {
		switch r.Kind {
		case ChooseLocal:
			r.Kind = ChooseKeep
		case ChooseRemote:
			r.Kind = ChooseDelete
		}
	}
	if it.Conflict == ConflictConcurrent && r.Kind == ChooseRemote && it.Remote != nil && it.Remote.Kind == vault.KindDeleted {
		r.Kind = ChooseDelete // the other side is a deletion: taking it means deleting
	}
	var content []byte
	switch r.Kind {
	case ChooseSkip:
		ap.rep.Skipped++
		return nil
	case ChooseKeep:
		if err := ap.upload(it); err != nil {
			return err
		}
		ap.rep.Resolved++
		return nil
	case ChooseDelete:
		if err := ap.trashLocal(it); err != nil {
			return err
		}
		if it.Conflict == ConflictConcurrent {
			// The vault head is still concurrent: record the deletion with
			// every candidate as parent so the other machines see it resolved.
			ap.tombstoneEntry(it, ap.allParents(it))
			ap.rep.Deleted++
		}
		ap.rep.Resolved++
		return nil
	case ChooseMerged:
		if it.Merge == nil || !it.Merge.Clean {
			return errors.New("no clean merge available")
		}
		content = it.Merge.Merged
	case ChooseLocal:
		if it.Local == nil {
			return errors.New("no local copy to keep")
		}
		content = it.LocalText
	case ChooseRemote:
		if it.HeadText == nil {
			return errors.New("no vault content to take")
		}
		content = it.HeadText
	case ChooseCustom:
		if r.Content == nil {
			return errors.New("custom resolution without content")
		}
		content = r.Content
	default:
		return fmt.Errorf("resolution %v not applicable to a conflict", r.Kind)
	}
	if content == nil {
		content = []byte{}
	}
	current, info, exists, err := ap.readCurrent(it.Key.Path)
	if err != nil {
		return err
	}
	if err := ap.checkStale(it, current, exists); err != nil {
		return err
	}
	if r.Kind == ChooseLocal && exists {
		content = current
	}
	// Spec §9.3 order: blob, then the local file, then the journal entry.
	id, _, err := ap.e.vault.WriteBlob(content)
	if err != nil {
		return err
	}
	written := info
	if !exists || !bytes.Equal(current, content) {
		if exists {
			if err := ap.trashPreimage(it, current, info, false); err != nil {
				return err
			}
		}
		if written, err = ap.writeLocal(it, content, info); err != nil {
			return err
		}
	}
	ap.recordEntry(it, id, content, ap.allParents(it), written)
	ap.rep.Uploaded++
	ap.rep.Resolved++
	return nil
}
