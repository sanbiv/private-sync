package sync

import (
	"context"
	"errors"
	"fmt"

	"github.com/sanbiv/private-sync/internal/state"
	"github.com/sanbiv/private-sync/internal/vault"
)

// Untrack writes a KindUntracked tombstone for paths (files rm): the vault
// stops syncing them everywhere, local copies are never touched. The base
// entries are dropped. Every path must be tracked (in the vault or in this
// machine's base store), otherwise ErrNotTracked and nothing is written.
func (e *Engine) Untrack(ctx context.Context, projectID string, paths []string) error {
	return e.writeTombstones(ctx, projectID, paths, vault.KindUntracked)
}

// DeleteEverywhere writes a KindDeleted tombstone for paths (files delete).
// The local file is left in place here (the caller may remove it); the base
// becomes the tombstone, so the next Plan reports a surviving local copy
// (row 9) instead of re-uploading it, and other machines trash their copies.
func (e *Engine) DeleteEverywhere(ctx context.Context, projectID string, paths []string) error {
	return e.writeTombstones(ctx, projectID, paths, vault.KindDeleted)
}

// writeTombstones is the shared implementation of Untrack/DeleteEverywhere.
func (e *Engine) writeTombstones(ctx context.Context, projectID string, paths []string, kind vault.Kind) error {
	op := "sync.Untrack"
	if kind == vault.KindDeleted {
		op = "sync.DeleteEverywhere"
	}
	if err := e.ready(); err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if projectID == "" {
		return fmt.Errorf("%s: empty project id", op)
	}
	// Dedupe and validate first: nothing is written when any path is bad.
	var list []string
	seen := map[string]bool{}
	for _, p := range paths {
		if !ValidPath(p) {
			return fmt.Errorf("%s: %w: %q", op, ErrBadPath, p)
		}
		if !seen[p] {
			seen[p] = true
			list = append(list, p)
		}
	}
	if len(list) == 0 {
		return nil
	}
	journals, _, err := e.vault.ReadJournals(projectID)
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	heads := vault.ResolveHeads(journals)
	for _, p := range list {
		if _, ok := heads[p]; ok {
			continue
		}
		if _, ok := e.store.Base(projectID, p); ok {
			continue
		}
		return fmt.Errorf("%s: %w: %s", op, ErrNotTracked, p)
	}
	j := journals[e.machine.ID]
	if j == nil {
		j = &vault.Journal{Machine: e.machine.ID}
	}
	if j.Entries == nil {
		j.Entries = map[string]vault.Entry{}
	}
	now := e.now()
	type pending struct {
		path  string
		clock vault.Clock
	}
	var done []pending
	for _, p := range list {
		clock := vault.Clock{}
		var parents []string
		if h, ok := heads[p]; ok {
			for _, c := range h.Candidates {
				clock = clock.Merge(c.Clock)
				parents = appendUnique(parents, c.Blob)
			}
		}
		if b, ok := e.store.Base(projectID, p); ok {
			clock = clock.Merge(b.Clock)
			if len(parents) == 0 {
				parents = appendUnique(parents, b.Blob)
			}
		}
		if own, ok := j.Entries[p]; ok {
			clock = clock.Merge(own.Clock)
		}
		clock = clock.Tick(e.machine.ID)
		en := vault.Entry{
			Path:      p,
			Kind:      kind,
			Clock:     clock,
			UpdatedAt: now,
			Machine:   e.machine.ID,
		}
		if kind == vault.KindDeleted {
			en.Parents = parents
		}
		j.Entries[p] = en
		done = append(done, pending{path: p, clock: clock})
	}
	if err := e.vault.WriteJournal(projectID, j); err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	for _, d := range done {
		if kind == vault.KindDeleted {
			e.store.SetBase(projectID, d.path, state.BaseEntry{Kind: vault.KindDeleted, Clock: d.clock})
		} else {
			e.store.DeleteBase(projectID, d.path)
		}
	}
	e.store.SetJournalSeq(e.machine.ID, j.Seq)
	if err := e.store.Save(); err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	return nil
}

// TrackedPaths returns the paths whose vault head is a file (for scan/restore).
// A concurrent head counts as tracked when any candidate is a file.
func (e *Engine) TrackedPaths(projectID string) (map[string]bool, error) {
	if err := e.ready(); err != nil {
		return nil, fmt.Errorf("sync.TrackedPaths: %w", err)
	}
	if projectID == "" {
		return nil, errors.New("sync.TrackedPaths: empty project id")
	}
	journals, _, err := e.vault.ReadJournals(projectID)
	if err != nil {
		return nil, fmt.Errorf("sync.TrackedPaths: %w", err)
	}
	out := map[string]bool{}
	for p, h := range vault.ResolveHeads(journals) {
		if h.Entry != nil {
			if h.Entry.Kind == vault.KindFile {
				out[p] = true
			}
			continue
		}
		for _, c := range h.Candidates {
			if c.Kind == vault.KindFile {
				out[p] = true
				break
			}
		}
	}
	return out, nil
}
