// Package identity computes project fingerprints and matches them across machines.
package identity

import (
	"context"

	"github.com/sanbiv/private-sync/internal/execx"
)

// Level ranks fingerprint strength.
type Level int

const (
	LevelStrong  Level = 1 // git remote
	LevelPackage Level = 2 // language manifests
	LevelDir     Level = 3 // directory basename
)

// Fingerprint identifies a project.
type Fingerprint struct {
	Kind  string `json:"kind"`  // git, go, npm, cargo, py, composer, maven, gem, swift, dart, dir
	Value string `json:"value"` // normalised value
	Level Level  `json:"level"`
}

// String renders "kind:value".
func (f Fingerprint) String() string { return f.Kind + ":" + f.Value }

// Detect computes every fingerprint of dir (spec §7). Never returns an empty list:
// "dir:<basename>" is always present.
func Detect(ctx context.Context, dir string, r execx.Runner) ([]Fingerprint, error) {
	return nil, nil
}

// Strength of a match.
type Strength int

const (
	StrengthNone   Strength = iota
	StrengthWeak            // only dir: shared
	StrengthStrong          // a level 1-2 fingerprint shared
)

// Match is a candidate vault project for a local directory.
type Match struct {
	ProjectID string
	Strength  Strength
	Shared    []Fingerprint
}

// MatchProjects compares local fingerprints with the vault projects' fingerprints;
// strong matches first, then weak. Projects sharing nothing are omitted.
func MatchProjects(local []Fingerprint, vaultProjects map[string][]Fingerprint) []Match {
	return nil
}

// Strongest returns the first level-1 fingerprint, else the first level-2 one.
func Strongest(fps []Fingerprint) (Fingerprint, bool) { return Fingerprint{}, false }

// NormalizeGitURL turns any git remote URL into "host/path" (spec §7 rules).
func NormalizeGitURL(raw string) (string, bool) { return "", false }

// Union merges fingerprint lists without duplicates, keeping order of first appearance.
func Union(lists ...[]Fingerprint) []Fingerprint { return nil }
