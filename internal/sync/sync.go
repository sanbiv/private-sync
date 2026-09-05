// Package sync is the three-way synchronisation engine (spec §9). It has no UI,
// reads no flags and no environment: Options and Resolutions are its only inputs.
package sync

import (
	"context"
	"errors"
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

// ConflictKind refines ActionConflict.
type ConflictKind int

const (
	ConflictNone ConflictKind = iota
	ConflictContent
	ConflictNoBase
	ConflictModifyDelete // local modified, vault deleted
	ConflictConcurrent   // several vault heads
)

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
}

// ItemError records a per-item failure.
type ItemError struct {
	Key ItemKey
	Err error
}

// ErrStale is returned when a local file changed between Plan and Apply.
var ErrStale = errors.New("local file changed since planning")

// Engine performs planning and application.
type Engine struct {
	vault   *vault.Vault
	store   *state.Store
	cfg     *config.Config
	remote  remote.Remote
	machine state.Machine
}

// New creates an engine.
func New(v *vault.Vault, st *state.Store, cfg *config.Config, r remote.Remote, m state.Machine) *Engine {
	return &Engine{vault: v, store: st, cfg: cfg, remote: r, machine: m}
}

// Fetch pulls the remote into the local vault copy.
func (e *Engine) Fetch(ctx context.Context, progress func(Event)) error {
	return errors.New("sync.Fetch: not implemented")
}

// Plan computes the items for every selected project without touching anything.
func (e *Engine) Plan(ctx context.Context, opts Options) (*Plan, error) {
	return nil, errors.New("sync.Plan: not implemented")
}

// Apply executes a plan with the given resolutions.
func (e *Engine) Apply(ctx context.Context, p *Plan, res Resolutions, opts Options) (*Report, error) {
	return nil, errors.New("sync.Apply: not implemented")
}

// Push uploads the local vault copy to the remote.
func (e *Engine) Push(ctx context.Context, progress func(Event)) error {
	return errors.New("sync.Push: not implemented")
}

// ApplyStrategy fills missing resolutions for items that need one.
func ApplyStrategy(p *Plan, s Strategy, res Resolutions) {}

// Summary counts items by action for badges/status.
type Summary struct {
	InSync, LocalChanges, RemoteChanges, Conflicts, Pending, Rollback, Missing int
}

// Summarize counts a project's items.
func Summarize(pp *ProjectPlan) Summary { return Summary{} }

// Untrack writes a KindUntracked tombstone for paths (files rm).
func (e *Engine) Untrack(ctx context.Context, projectID string, paths []string) error {
	return errors.New("sync.Untrack: not implemented")
}

// DeleteEverywhere writes a KindDeleted tombstone for paths (files delete).
func (e *Engine) DeleteEverywhere(ctx context.Context, projectID string, paths []string) error {
	return errors.New("sync.DeleteEverywhere: not implemented")
}

// TrackedPaths returns the paths whose vault head is a file (for scan/restore).
func (e *Engine) TrackedPaths(projectID string) (map[string]bool, error) { return nil, nil }
