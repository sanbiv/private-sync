// Package scan finds configuration/secret files inside a project directory and
// scores them using git state (spec §6).
package scan

import (
	"context"

	"github.com/sanbiv/private-sync/internal/execx"
)

// Score drives pre-selection in the UI.
type Score int

const (
	ScoreLow    Score = iota // git-tracked: never pre-selected
	ScoreMedium              // untracked / no git info
	ScoreHigh                // git-ignored match or secret-ish
)

// Candidate is a file proposed for tracking.
type Candidate struct {
	Path        string // slash-separated, relative to the scanned dir
	Size        int64
	Mode        uint32
	Score       Score
	Reasons     []string
	GitTracked  bool
	GitIgnored  bool
	SecretName  bool
	Tracked     bool // already tracked in the vault
	Preselected bool
}

// Options configures a scan. Empty lists mean defaults.
type Options struct {
	Include       []string
	ExcludeDirs   []string
	ExcludeFiles  []string
	MaxFileSize   int64
	HardExclude   []string        // absolute paths never listed (key file, vault dir)
	Tracked       map[string]bool // relpaths already in the vault
	MaxFiles      int             // walk cap, default 200000
	MaxCandidates int             // default 5000
	Progress      func(walked, found int)
}

// Result of a scan.
type Result struct {
	Candidates  []Candidate
	NestedRepos []string // directories with their own .git (not descended)
	GitInfo     bool     // git state was available
	Truncated   bool
	Warnings    []string
}

// Scan walks dir and returns scored candidates sorted by score desc, then path.
func Scan(ctx context.Context, dir string, opts Options, r execx.Runner) (*Result, error) {
	return nil, nil
}

// DefaultInclude returns the default include globs.
func DefaultInclude() []string { return nil }

// DefaultExcludeDirs returns the default excluded directory names.
func DefaultExcludeDirs() []string { return nil }

// DefaultExcludeFiles returns the default excluded file globs.
func DefaultExcludeFiles() []string { return nil }

// IsSecretName reports whether a base name looks like a secret (spec §6).
func IsSecretName(name string) bool { return false }

// MatchesAny reports whether name matches any glob (case-insensitive, base name only).
func MatchesAny(name string, globs []string) bool { return false }
