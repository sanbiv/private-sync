// Package merge implements three-way merges for dotenv and text files (spec §8).
package merge

// Kind of merge performed.
type Kind int

const (
	KindText Kind = iota
	KindDotenv
	KindBinary
)

// LineRange is 0-based, half-open, in the lines of one side.
type LineRange struct{ Start, End int }

// Hunk is one conflict.
type Hunk struct {
	Key                 string // dotenv variable name; "" for text
	Base, Local, Remote []byte // nil = absent on that side
	BaseRange           LineRange
	LocalRange          LineRange
	RemoteRange         LineRange
}

// Result of ThreeWay.
type Result struct {
	Kind   Kind
	Clean  bool
	Merged []byte // valid only when Clean
	Hunks  []Hunk // conflicts, in file order; empty when Clean
	Note   string // e.g. formatting-only change dropped, dotenv parse fallback
}

// Side selects a hunk resolution.
type Side int

const (
	SideLocal Side = iota
	SideRemote
	SideBase
	SideCustom
)

// Choice resolves one hunk.
type Choice struct {
	Side   Side
	Custom []byte // SideCustom only
}

// ThreeWay merges local and remote against base (nil base = no common ancestor).
// local and remote are never nil (tombstones are handled by the sync engine).
func ThreeWay(path string, base, local, remote []byte) *Result { return nil }

// Resolve reassembles the file from the clean segments plus one choice per hunk.
func Resolve(r *Result, choices []Choice) ([]byte, error) { return nil, nil }

// RenderMarkers renders the conflicted file with diff3-style markers for $EDITOR.
func RenderMarkers(r *Result, localLabel, remoteLabel string) []byte { return nil }

// HasMarkers reports whether b still contains line-anchored conflict markers.
func HasMarkers(b []byte) bool { return false }

// DiffOp is one line of a unified diff.
type DiffOp struct {
	Kind byte // ' ', '-', '+'
	Text string
}

// LineDiff computes a line diff between a and b (for the TUI).
func LineDiff(a, b []byte) []DiffOp { return nil }

// KindFor classifies a file by path and content.
func KindFor(path string, content []byte) Kind { return KindText }
