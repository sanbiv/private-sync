// Package sync is the three-way synchronisation engine (spec §9). It has no UI,
// reads no flags and no environment: Options and Resolutions are its only inputs.
package sync

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/sanbiv/private-sync/internal/config"
	"github.com/sanbiv/private-sync/internal/merge"
	"github.com/sanbiv/private-sync/internal/remote"
	"github.com/sanbiv/private-sync/internal/state"
	"github.com/sanbiv/private-sync/internal/vault"
)

// Mode selects the direction.
type Mode int

const (
	ModeSync Mode = iota
	ModePush
	ModePull
	ModeRestore
)

// String renders the mode for logs and test output.
func (m Mode) String() string {
	switch m {
	case ModeSync:
		return "sync"
	case ModePush:
		return "push"
	case ModePull:
		return "pull"
	case ModeRestore:
		return "restore"
	}
	return fmt.Sprintf("Mode(%d)", int(m))
}

// Strategy resolves conflicts non-interactively.
type Strategy int

const (
	StrategyAsk Strategy = iota
	StrategyLocal
	StrategyRemote
	StrategyAbort
)

// Action is what the engine wants to do with a path.
type Action int

const (
	ActionInSync       Action = iota
	ActionUpload              // local -> vault
	ActionDownload            // vault -> local
	ActionConverge            // same content: update base only
	ActionTrashLocal          // head deleted, local unchanged: local -> trash
	ActionUntrack             // head untracked: drop base, leave file
	ActionMissingLocal        // tracked file absent locally, vault unchanged: report only
	ActionDeleteRemote        // write a KindDeleted entry
	ActionConflict            // needs a Resolution
	ActionPending             // blob not readable yet: skip
	ActionRollback            // head older than base: report only
	ActionReportOnly          // would apply in another mode
)

// String renders the action for logs and test output.
func (a Action) String() string {
	switch a {
	case ActionInSync:
		return "in-sync"
	case ActionUpload:
		return "upload"
	case ActionDownload:
		return "download"
	case ActionConverge:
		return "converge"
	case ActionTrashLocal:
		return "trash-local"
	case ActionUntrack:
		return "untrack"
	case ActionMissingLocal:
		return "missing-local"
	case ActionDeleteRemote:
		return "delete-remote"
	case ActionConflict:
		return "conflict"
	case ActionPending:
		return "pending"
	case ActionRollback:
		return "rollback"
	case ActionReportOnly:
		return "report-only"
	}
	return fmt.Sprintf("Action(%d)", int(a))
}

// ConflictKind refines ActionConflict.
type ConflictKind int

const (
	ConflictNone ConflictKind = iota
	ConflictContent
	ConflictNoBase
	ConflictModifyDelete // local modified, vault deleted
	ConflictConcurrent   // several vault heads
)

// String renders the conflict kind for logs and test output.
func (c ConflictKind) String() string {
	switch c {
	case ConflictNone:
		return "none"
	case ConflictContent:
		return "content"
	case ConflictNoBase:
		return "no-base"
	case ConflictModifyDelete:
		return "modify-delete"
	case ConflictConcurrent:
		return "concurrent"
	}
	return fmt.Sprintf("ConflictKind(%d)", int(c))
}

// ItemKey identifies a file.
type ItemKey struct {
	Project string
	Path    string
}

// FileRef describes one side of an item.
type FileRef struct {
	Blob    string
	Kind    vault.Kind
	Size    int64
	Mode    uint32
	ModTime time.Time
	Clock   vault.Clock
	Machine string
}

// Item is one planned file operation.
type Item struct {
	Key             ItemKey
	Action          Action
	Conflict        ConflictKind
	Local           *FileRef // nil = absent
	Base            *FileRef
	Head            *FileRef // resolved (or synthetic merged) head
	Candidates      []vault.Entry
	Merge           *merge.Result
	LocalText       []byte
	BaseText        []byte
	HeadText        []byte
	Reason          string
	NeedsResolution bool

	// Synthetic is set when Head is the clean pre-merge of several concurrent
	// vault heads (decision table row 2): Apply writes the merged blob and a
	// journal entry whose Parents are every candidate blob, so the other
	// machines see the divergence resolved.
	Synthetic bool
	// Original is the action that would have applied in another mode when
	// Action is ActionReportOnly (push/pull), or the action a rollback would
	// have applied when Action is ActionRollback.
	Original Action
	// AcceptedRollback marks an item whose head is older than the base and
	// that is applied anyway (Options.AcceptRollback or restore): Apply then
	// keeps the local pre-image in the trash even when it equals the base,
	// because the vault entry that referenced it is gone.
	AcceptedRollback bool
	// Remote is the other machine's side of a concurrent vault head
	// (ConflictConcurrent) and is never this machine's own entry, so
	// ChooseRemote always takes the other side. Kind KindFile: HeadText
	// carries its content and ChooseRemote writes it. Kind KindDeleted: the
	// other machine deleted the path; ChooseRemote (and StrategyRemote)
	// accept the deletion like ChooseDelete, HeadText only shows the vault's
	// surviving file version. nil while Head is resolved, and when several
	// other machines disagree: "remote" then names no single side, HeadText
	// carries the first foreign file version and StrategyRemote leaves the
	// item unresolved (an explicit ChooseRemote still writes HeadText).
	Remote *FileRef
}

// ProjectPlan groups the items of one project.
type ProjectPlan struct {
	ID          string
	Name        string
	Path        string
	Missing     bool // local path missing
	Unreadable  bool // a journal could not be decrypted
	DuplicateOf string
	Items       []Item
	Warnings    []string

	// journalSeqs records the Seq of every journal read while planning, so
	// Apply can update the base store's high-water marks.
	journalSeqs map[string]uint64
}

// Plan is the output of Engine.Plan.
type Plan struct {
	Mode     Mode
	Projects []ProjectPlan
	Warnings []string
}

// ChoiceKind is the user's answer for an item.
type ChoiceKind int

const (
	ChooseNone ChoiceKind = iota
	ChooseMerged
	ChooseLocal
	ChooseRemote
	ChooseCustom
	ChooseKeep    // modify/delete: keep the file (re-upload)
	ChooseDelete  // modify/delete: accept deletion (trash)
	ChooseConfirm // generic confirmation (delete remote, rollback)
	ChooseSkip
)

// String renders the choice for logs and test output.
func (c ChoiceKind) String() string {
	switch c {
	case ChooseNone:
		return "none"
	case ChooseMerged:
		return "merged"
	case ChooseLocal:
		return "local"
	case ChooseRemote:
		return "remote"
	case ChooseCustom:
		return "custom"
	case ChooseKeep:
		return "keep"
	case ChooseDelete:
		return "delete"
	case ChooseConfirm:
		return "confirm"
	case ChooseSkip:
		return "skip"
	}
	return fmt.Sprintf("ChoiceKind(%d)", int(c))
}

// Resolution answers one item.
type Resolution struct {
	Kind    ChoiceKind
	Content []byte // ChooseCustom
}

// Resolutions maps items to answers.
type Resolutions map[ItemKey]Resolution

// Event is a progress message.
type Event struct {
	Stage   string // fetch | plan | apply | push
	Project string
	Path    string
	Message string
	Done    int
	Total   int
	Err     error
}

// Options configure Plan/Apply.
type Options struct {
	Mode             Mode
	Projects         []string // ids; empty = all linked projects
	Strategy         Strategy
	PropagateDeletes bool
	AcceptRollback   bool
	Track            map[ItemKey]bool // paths explicitly (re)added this run
	Progress         func(Event)
}

// Report summarises Apply.
type Report struct {
	Uploaded, Downloaded, Converged, Trashed, Untracked, Deleted, Skipped, Pending int
	Unresolved                                                                     []ItemKey
	Errors                                                                         []ItemError

	// Resolved counts conflicts that were applied with a resolution (they are
	// also counted under Uploaded/Trashed according to what was done).
	Resolved int
}

// ItemError records a per-item failure.
type ItemError struct {
	Key ItemKey
	Err error
}

// ErrStale is returned when a local file changed between Plan and Apply.
var ErrStale = errors.New("local file changed since planning")

// ErrNotTracked is returned by Untrack/DeleteEverywhere for a path that is
// neither in the vault nor in this machine's base store.
var ErrNotTracked = errors.New("path is not tracked")

// ErrBadPath is returned for a path that is not a clean, relative, slash
// separated path inside the project (spec §5: paths are relative to the root).
var ErrBadPath = errors.New("invalid project-relative path")

// TrashRetention is how long trash pre-images are kept; Apply purges older
// rows first (spec §9.4).
const TrashRetention = 30 * 24 * time.Hour

// Engine performs planning and application.
type Engine struct {
	vault   *vault.Vault
	store   *state.Store
	cfg     *config.Config
	remote  remote.Remote
	machine state.Machine

	maxFileSize int64
	now         func() time.Time
	hostname    func() string
}

// New creates an engine.
func New(v *vault.Vault, st *state.Store, cfg *config.Config, r remote.Remote, m state.Machine) *Engine {
	e := &Engine{vault: v, store: st, cfg: cfg, remote: r, machine: m}
	e.maxFileSize = config.DefaultMaxFileSize
	if cfg != nil {
		if n, err := cfg.MaxFileSize(); err == nil && n > 0 {
			e.maxFileSize = n
		}
	}
	e.now = func() time.Time { return time.Now().UTC() }
	e.hostname = func() string {
		h, err := os.Hostname()
		if err != nil {
			return ""
		}
		return h
	}
	return e
}

// MachineID returns the id this engine writes as.
func (e *Engine) MachineID() string {
	if e == nil {
		return ""
	}
	return e.machine.ID
}

// MaxFileSize returns the per-file size limit the planner applies.
func (e *Engine) MaxFileSize() int64 {
	if e == nil {
		return 0
	}
	return e.maxFileSize
}

// ready reports whether the engine can plan/apply.
func (e *Engine) ready() error {
	switch {
	case e == nil:
		return errors.New("nil engine")
	case e.vault == nil:
		return errors.New("no vault")
	case e.store == nil:
		return errors.New("no state store")
	case e.cfg == nil:
		return errors.New("no config")
	case !vault.ValidMachineID(e.machine.ID):
		return fmt.Errorf("%w: %q", vault.ErrBadMachineID, e.machine.ID)
	}
	return nil
}

// suffix is the temp-file suffix identifying this machine's writes.
func (e *Engine) suffix() string {
	if len(e.machine.ID) > 8 {
		return e.machine.ID[:8]
	}
	return e.machine.ID
}

// emit sends an event when a progress callback is set.
func emit(progress func(Event), ev Event) {
	if progress != nil {
		progress(ev)
	}
}

// Fetch pulls the remote into the local vault copy.
func (e *Engine) Fetch(ctx context.Context, progress func(Event)) error {
	if e == nil {
		return errors.New("sync.Fetch: nil engine")
	}
	if e.remote == nil {
		// A wiring bug, not "nothing to fetch": remote.None{} is the explicit
		// way to say there is no remote.
		return errors.New("sync.Fetch: no remote")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	log := func(line string) { emit(progress, Event{Stage: "fetch", Message: line}) }
	if err := e.remote.Fetch(ctx, log); err != nil {
		emit(progress, Event{Stage: "fetch", Message: "fetch failed", Err: err})
		return fmt.Errorf("sync.Fetch: %w", err)
	}
	return nil
}

// Push uploads the local vault copy to the remote.
func (e *Engine) Push(ctx context.Context, progress func(Event)) error {
	if e == nil {
		return errors.New("sync.Push: nil engine")
	}
	if e.remote == nil {
		return errors.New("sync.Push: no remote")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	var written []string
	if e.vault != nil {
		written = e.vault.Written()
	}
	log := func(line string) { emit(progress, Event{Stage: "push", Message: line}) }
	if err := e.remote.Push(ctx, written, log); err != nil {
		emit(progress, Event{Stage: "push", Message: "push failed", Err: err})
		return fmt.Errorf("sync.Push: %w", err)
	}
	return nil
}

// ApplyStrategy fills missing resolutions for items that need one.
//
// local: ChooseLocal (modify/delete: ChooseKeep; delete-remote: ChooseConfirm);
// remote: ChooseRemote (modify/delete: ChooseDelete; delete-remote: ChooseConfirm);
// abort: ChooseSkip; ask: nothing. Existing resolutions are never replaced.
// res must be a non-nil map.
func ApplyStrategy(p *Plan, s Strategy, res Resolutions) {
	if p == nil || res == nil || s == StrategyAsk {
		return
	}
	for pi := range p.Projects {
		for _, it := range p.Projects[pi].Items {
			if !it.NeedsResolution {
				continue
			}
			if _, ok := res[it.Key]; ok {
				continue
			}
			if r, ok := strategyChoice(&it, s); ok {
				res[it.Key] = r
			}
		}
	}
}

// strategyChoice returns the resolution a strategy implies for an item.
//
// A strategy never guesses: "local" needs a local file to keep (a concurrent
// head with no local copy stays unresolved instead of failing in Apply), and
// "remote" needs a single other side (a concurrent head where several other
// machines disagree stays unresolved; one that the other machine deleted is
// accepted as a deletion).
func strategyChoice(it *Item, s Strategy) (Resolution, bool) {
	switch s {
	case StrategyAbort:
		return Resolution{Kind: ChooseSkip}, true
	case StrategyLocal:
		switch {
		case it.Action == ActionDeleteRemote:
			return Resolution{Kind: ChooseConfirm}, true
		case it.Action != ActionConflict:
			return Resolution{}, false
		case it.Local == nil:
			return Resolution{}, false
		case it.Conflict == ConflictModifyDelete:
			return Resolution{Kind: ChooseKeep}, true
		}
		return Resolution{Kind: ChooseLocal}, true
	case StrategyRemote:
		switch {
		case it.Action == ActionDeleteRemote:
			return Resolution{Kind: ChooseConfirm}, true
		case it.Action != ActionConflict:
			return Resolution{}, false
		case it.Conflict == ConflictModifyDelete:
			return Resolution{Kind: ChooseDelete}, true
		case it.Conflict == ConflictConcurrent && it.Remote == nil:
			return Resolution{}, false
		case it.Conflict == ConflictConcurrent && it.Remote.Kind == vault.KindDeleted:
			return Resolution{Kind: ChooseDelete}, true
		}
		return Resolution{Kind: ChooseRemote}, true
	}
	return Resolution{}, false
}

// Summary counts items by action for badges/status.
type Summary struct {
	InSync, LocalChanges, RemoteChanges, Conflicts, Pending, Rollback, Missing int
}

// Summarize counts a project's items.
func Summarize(pp *ProjectPlan) Summary {
	var s Summary
	if pp == nil {
		return s
	}
	for i := range pp.Items {
		it := &pp.Items[i]
		a := it.Action
		if a == ActionReportOnly {
			a = it.Original
		}
		switch a {
		case ActionInSync:
			s.InSync++
		case ActionUpload, ActionDeleteRemote:
			s.LocalChanges++
		case ActionDownload, ActionTrashLocal, ActionConverge, ActionUntrack:
			s.RemoteChanges++
		case ActionConflict:
			s.Conflicts++
		case ActionPending:
			s.Pending++
		case ActionRollback:
			s.Rollback++
		case ActionMissingLocal:
			s.Missing++
		}
	}
	return s
}

// Items returns every item of the plan in order (all projects).
func (p *Plan) Items() []*Item {
	if p == nil {
		return nil
	}
	var out []*Item
	for pi := range p.Projects {
		for i := range p.Projects[pi].Items {
			out = append(out, &p.Projects[pi].Items[i])
		}
	}
	return out
}

// Find returns the item for a key, or nil.
func (p *Plan) Find(k ItemKey) *Item {
	if p == nil {
		return nil
	}
	for pi := range p.Projects {
		if p.Projects[pi].ID != k.Project {
			continue
		}
		for i := range p.Projects[pi].Items {
			if p.Projects[pi].Items[i].Key == k {
				return &p.Projects[pi].Items[i]
			}
		}
	}
	return nil
}

// Unresolved lists the items that still need a resolution not present in res.
func (p *Plan) Unresolved(res Resolutions) []ItemKey {
	var out []ItemKey
	for _, it := range p.Items() {
		if !it.NeedsResolution {
			continue
		}
		if _, ok := res[it.Key]; ok {
			continue
		}
		out = append(out, it.Key)
	}
	return out
}
