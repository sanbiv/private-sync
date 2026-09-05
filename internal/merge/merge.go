// Package merge implements three-way merges for dotenv and text files (spec §8).
package merge

import (
	"bytes"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// Kind of merge performed.
type Kind int

const (
	KindText Kind = iota
	KindDotenv
	KindBinary
)

// String returns the kind name.
func (k Kind) String() string {
	switch k {
	case KindText:
		return "text"
	case KindDotenv:
		return "dotenv"
	case KindBinary:
		return "binary"
	}
	return fmt.Sprintf("Kind(%d)", int(k))
}

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

	// segments is the file layout in order: clean byte runs interleaved with
	// references to Hunks. Resolve and RenderMarkers reassemble from it.
	segments []segment
}

// segment is one piece of the merged file layout.
type segment struct {
	text []byte // clean bytes when hunk < 0
	hunk int    // index into Result.Hunks, or -1 for a clean segment
}

// Side selects a hunk resolution.
type Side int

const (
	SideLocal Side = iota
	SideRemote
	SideBase
	SideCustom
)

// String returns the side name.
func (s Side) String() string {
	switch s {
	case SideLocal:
		return "local"
	case SideRemote:
		return "remote"
	case SideBase:
		return "base"
	case SideCustom:
		return "custom"
	}
	return fmt.Sprintf("Side(%d)", int(s))
}

// Choice resolves one hunk.
type Choice struct {
	Side   Side
	Custom []byte // SideCustom only
}

// ThreeWay merges local and remote against base (nil base = no common ancestor).
// local and remote are never nil (tombstones are handled by the sync engine).
func ThreeWay(path string, base, local, remote []byte) *Result {
	if local == nil {
		local = []byte{}
	}
	if remote == nil {
		remote = []byte{}
	}
	binary := isBinary(local) || isBinary(remote) || (base != nil && isBinary(base))
	if base == nil {
		return mergeNoBase(local, remote, binary)
	}
	if binary {
		return mergeBinary(base, local, remote)
	}
	if isDotenvName(path) {
		r, note := mergeDotenv(base, local, remote)
		if r != nil {
			return r
		}
		r = mergeText(base, local, remote)
		if r.Note != "" {
			note += "; " + r.Note
		}
		r.Note = note
		return r
	}
	return mergeText(base, local, remote)
}

// mergeNoBase handles the two-way case: equal contents are clean, anything
// else is one whole-file hunk (KindText so the diff view works).
func mergeNoBase(local, remote []byte, binary bool) *Result {
	kind := KindText
	if binary {
		kind = KindBinary
	}
	if bytes.Equal(local, remote) {
		return &Result{Kind: kind, Clean: true, Merged: clone(local)}
	}
	h := Hunk{Local: cloneOrNil(local), Remote: cloneOrNil(remote)}
	if !binary {
		h.LocalRange = LineRange{0, countLines(local)}
		h.RemoteRange = LineRange{0, countLines(remote)}
	}
	return &Result{
		Kind:     kind,
		Hunks:    []Hunk{h},
		Note:     "no common ancestor",
		segments: []segment{{hunk: 0}},
	}
}

// mergeBinary never merges content: one hunk with the whole contents unless
// both sides are identical.
func mergeBinary(base, local, remote []byte) *Result {
	if bytes.Equal(local, remote) {
		return &Result{Kind: KindBinary, Clean: true, Merged: clone(local)}
	}
	h := Hunk{Base: cloneOrNil(base), Local: cloneOrNil(local), Remote: cloneOrNil(remote)}
	return &Result{
		Kind:     KindBinary,
		Hunks:    []Hunk{h},
		Note:     "binary content: not mergeable",
		segments: []segment{{hunk: 0}},
	}
}

// Resolve reassembles the file from the clean segments plus one choice per hunk.
func Resolve(r *Result, choices []Choice) ([]byte, error) {
	if r == nil {
		return nil, errors.New("merge: nil result")
	}
	if len(choices) != len(r.Hunks) {
		return nil, fmt.Errorf("merge: %d choices for %d hunks", len(choices), len(r.Hunks))
	}
	if len(r.Hunks) == 0 {
		return cloneNonNil(r.Merged), nil
	}
	segs := r.layout()
	if segs == nil {
		return nil, errors.New("merge: result has no layout to resolve")
	}
	out := []byte{}
	for _, s := range segs {
		var part []byte
		if s.hunk < 0 {
			part = s.text
		} else {
			if s.hunk >= len(r.Hunks) {
				return nil, fmt.Errorf("merge: layout references hunk %d of %d", s.hunk, len(r.Hunks))
			}
			p, err := pick(r.Hunks[s.hunk], choices[s.hunk])
			if err != nil {
				return nil, err
			}
			part = p
		}
		if r.Kind == KindBinary {
			out = append(out, part...)
		} else {
			out = appendPart(out, part)
		}
	}
	return out, nil
}

// pick returns the bytes chosen for h, nil meaning "omit".
func pick(h Hunk, c Choice) ([]byte, error) {
	switch c.Side {
	case SideLocal:
		return h.Local, nil
	case SideRemote:
		return h.Remote, nil
	case SideBase:
		return h.Base, nil
	case SideCustom:
		return c.Custom, nil
	}
	return nil, fmt.Errorf("merge: unknown side %d", int(c.Side))
}

// layout returns the segment list, synthesising one for a single-hunk result
// that was built without segments.
func (r *Result) layout() []segment {
	if len(r.segments) > 0 {
		return r.segments
	}
	if len(r.Hunks) == 1 {
		return []segment{{hunk: 0}}
	}
	return nil
}

// RenderMarkers renders the conflicted file with diff3-style markers for $EDITOR.
func RenderMarkers(r *Result, localLabel, remoteLabel string) []byte {
	if r == nil {
		return nil
	}
	if localLabel == "" {
		localLabel = "local"
	}
	if remoteLabel == "" {
		remoteLabel = "remote"
	}
	if len(r.Hunks) == 0 {
		return cloneNonNil(r.Merged)
	}
	segs := r.layout()
	if segs == nil {
		// No layout: render the hunks one after the other.
		segs = make([]segment, len(r.Hunks))
		for i := range r.Hunks {
			segs[i] = segment{hunk: i}
		}
	}
	out := []byte{}
	for _, s := range segs {
		if s.hunk < 0 {
			out = appendPart(out, s.text)
			continue
		}
		if s.hunk >= len(r.Hunks) {
			continue
		}
		h := r.Hunks[s.hunk]
		// Marker lines follow the file's line-ending convention (that of
		// the last terminated line so far, else of the hunk's own lines),
		// as git does, so a CRLF file does not come back mixed.
		eol := eolFor(out, h.Local, h.Base, h.Remote)
		out = ensureNewline(out, eol)
		out = append(out, "<<<<<<< "...)
		out = append(out, localLabel...)
		out = append(out, eol...)
		out = appendBlock(out, h.Local, eol)
		out = append(out, "||||||| base"...)
		out = append(out, eol...)
		out = appendBlock(out, h.Base, eol)
		out = append(out, "======="...)
		out = append(out, eol...)
		out = appendBlock(out, h.Remote, eol)
		out = append(out, ">>>>>>> "...)
		out = append(out, remoteLabel...)
		out = append(out, eol...)
	}
	return out
}

// HasMarkers reports whether b still contains line-anchored conflict markers.
func HasMarkers(b []byte) bool {
	for len(b) > 0 {
		line := b
		if i := bytes.IndexByte(b, '\n'); i >= 0 {
			line = b[:i]
			b = b[i+1:]
		} else {
			b = nil
		}
		if isMarkerLine(line) {
			return true
		}
	}
	return false
}

// isMarkerLine reports whether line (terminator stripped) is one of the four
// diff3 markers: exactly seven marker characters, alone or followed by a
// space and a label. Longer runs, such as a Markdown "========" underline,
// are ordinary content, as in git's own check.
func isMarkerLine(line []byte) bool {
	if n := len(line); n > 0 && line[n-1] == '\r' {
		line = line[:n-1]
	}
	if len(line) < 7 {
		return false
	}
	switch string(line[:7]) {
	case "<<<<<<<", "|||||||", "=======", ">>>>>>>":
		return len(line) == 7 || line[7] == ' '
	}
	return false
}

// DiffOp is one line of a unified diff.
type DiffOp struct {
	Kind byte // ' ', '-', '+'
	Text string
}

// LineDiff computes a line diff between a and b (for the TUI).
func LineDiff(a, b []byte) []DiffOp {
	la, lb := newLineSet(a), newLineSet(b)
	var in interner
	ia, ib := in.intern(la.lines), in.intern(lb.lines)
	ma := myersMatch(ia, ib)
	ops := make([]DiffOp, 0, len(ia)+len(ib))
	i, j := 0, 0
	for i < len(ia) || j < len(ib) {
		switch {
		case i < len(ia) && ma[i] >= 0:
			for j < ma[i] {
				ops = append(ops, DiffOp{'+', lineText(lb.lines[j])})
				j++
			}
			ops = append(ops, DiffOp{' ', lineText(la.lines[i])})
			i++
			j++
		case i < len(ia):
			ops = append(ops, DiffOp{'-', lineText(la.lines[i])})
			i++
		default:
			ops = append(ops, DiffOp{'+', lineText(lb.lines[j])})
			j++
		}
	}
	return ops
}

// lineText strips the line terminator ("\n", "\r\n" or a bare trailing "\r"
// on an unterminated last line) for display, mirroring how lines are compared.
func lineText(l []byte) string {
	n := len(l)
	if n > 0 && l[n-1] == '\n' {
		n--
	}
	if n > 0 && l[n-1] == '\r' {
		n--
	}
	return string(l[:n])
}

// KindFor classifies a file by path and content.
func KindFor(path string, content []byte) Kind {
	if isBinary(content) {
		return KindBinary
	}
	if isDotenvName(path) {
		return KindDotenv
	}
	return KindText
}

// isBinary reports a NUL byte or invalid UTF-8.
func isBinary(b []byte) bool {
	return bytes.IndexByte(b, 0) >= 0 || !utf8.Valid(b)
}

// isDotenvName reports whether the base name is ".env", ".env.*" or "*.env".
func isDotenvName(path string) bool {
	name := filepath.Base(path)
	if i := strings.LastIndexByte(name, '/'); i >= 0 {
		name = name[i+1:]
	}
	return name == ".env" || strings.HasPrefix(name, ".env.") || strings.HasSuffix(name, ".env")
}

// clone copies b, keeping nil as nil.
func clone(b []byte) []byte {
	if b == nil {
		return nil
	}
	return append([]byte{}, b...)
}

// cloneNonNil copies b, mapping nil to an empty slice.
func cloneNonNil(b []byte) []byte {
	return append([]byte{}, b...)
}

// cloneOrNil copies b, mapping an empty slice to nil (a hunk side that
// contributes nothing is reported as absent, as text hunks do).
func cloneOrNil(b []byte) []byte {
	if len(b) == 0 {
		return nil
	}
	return append([]byte{}, b...)
}

var (
	lf   = []byte("\n")
	crlf = []byte("\r\n")
)

// firstEOL returns the terminator of the first terminated line of b, or nil.
func firstEOL(b []byte) []byte {
	i := bytes.IndexByte(b, '\n')
	switch {
	case i < 0:
		return nil
	case i > 0 && b[i-1] == '\r':
		return crlf
	}
	return lf
}

// lastEOL returns the terminator of the last terminated line of b, or nil.
func lastEOL(b []byte) []byte {
	i := bytes.LastIndexByte(b, '\n')
	switch {
	case i < 0:
		return nil
	case i > 0 && b[i-1] == '\r':
		return crlf
	}
	return lf
}

// eolFor picks the terminator to synthesise after dst so that the file keeps
// a single line-ending convention: that of dst's last terminated line, else
// the first one found in the parts that follow, else "\n".
func eolFor(dst []byte, next ...[]byte) []byte {
	if e := lastEOL(dst); e != nil {
		return e
	}
	for _, n := range next {
		if e := firstEOL(n); e != nil {
			return e
		}
	}
	return lf
}

// appendPart appends part to dst, separating it from a preceding part that
// does not end with a newline. Nil/empty parts are omitted.
func appendPart(dst, part []byte) []byte {
	if len(part) == 0 {
		return dst
	}
	dst = ensureNewline(dst, part)
	return append(dst, part...)
}

// ensureNewline terminates dst when it is non-empty and does not end with a
// newline, using the line-ending convention of dst (or, failing that, of the
// parts that follow) rather than a bare '\n'.
func ensureNewline(dst []byte, next ...[]byte) []byte {
	if n := len(dst); n > 0 && dst[n-1] != '\n' {
		dst = append(dst, eolFor(dst, next...)...)
	}
	return dst
}

// appendBlock appends a marker section body, terminated (with the body's own
// convention, else eol) when it is non-empty and unterminated.
func appendBlock(dst, body, eol []byte) []byte {
	if len(body) == 0 {
		return dst
	}
	dst = append(dst, body...)
	return ensureNewline(dst, eol)
}

// countLines counts physical lines (a final line without terminator counts).
func countLines(b []byte) int {
	if len(b) == 0 {
		return 0
	}
	n := bytes.Count(b, []byte{'\n'})
	if b[len(b)-1] != '\n' {
		n++
	}
	return n
}
