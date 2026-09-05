package merge

import "bytes"

// lineSet is a byte buffer split into physical lines that keep their
// terminators; offs allows slicing any line range out of the source bytes.
type lineSet struct {
	src   []byte
	lines [][]byte
	offs  []int // offs[i] = byte offset of line i; offs[len(lines)] = len(src)
}

func newLineSet(src []byte) lineSet {
	ls := lineSet{src: src}
	n := countLines(src)
	ls.lines = make([][]byte, 0, n)
	ls.offs = make([]int, 0, n+1)
	pos := 0
	for pos < len(src) {
		end := len(src)
		if i := bytes.IndexByte(src[pos:], '\n'); i >= 0 {
			end = pos + i + 1
		}
		ls.lines = append(ls.lines, src[pos:end])
		ls.offs = append(ls.offs, pos)
		pos = end
	}
	ls.offs = append(ls.offs, len(src))
	return ls
}

// raw returns the source bytes of lines [s, e) without copying (nil when empty).
func (ls *lineSet) raw(s, e int) []byte {
	if s >= e {
		return nil
	}
	return ls.src[ls.offs[s]:ls.offs[e]]
}

// slice returns a copy of the bytes of lines [s, e), or nil when empty.
func (ls *lineSet) slice(s, e int) []byte {
	return clone(ls.raw(s, e))
}

// interner maps line contents (with a trailing '\r' stripped) to small ints.
type interner struct {
	ids map[string]int
	buf []byte
}

func (in *interner) intern(lines [][]byte) []int {
	if in.ids == nil {
		in.ids = make(map[string]int, len(lines))
	}
	out := make([]int, len(lines))
	for i, l := range lines {
		key := l
		n := len(l)
		switch {
		case n >= 2 && l[n-1] == '\n' && l[n-2] == '\r':
			in.buf = append(in.buf[:0], l[:n-2]...)
			in.buf = append(in.buf, '\n')
			key = in.buf
		case n >= 1 && l[n-1] == '\r':
			key = l[:n-1]
		}
		id, ok := in.ids[string(key)]
		if !ok {
			id = len(in.ids)
			in.ids[string(key)] = id
		}
		out[i] = id
	}
	return out
}

func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\r' || c == '\v' || c == '\f'
}

// normalise drops '\r', strips trailing whitespace per line, collapses runs
// of leading whitespace to one space and ignores the final newline.
func normalise(b []byte) []byte {
	out := make([]byte, 0, len(b))
	ls := newLineSet(b)
	for _, l := range ls.lines {
		if n := len(l); n > 0 && l[n-1] == '\n' {
			l = l[:n-1]
		}
		end := len(l)
		for end > 0 && isSpace(l[end-1]) {
			end--
		}
		l = l[:end]
		i := 0
		for i < len(l) && isSpace(l[i]) {
			i++
		}
		if i > 0 {
			out = append(out, ' ')
		}
		for _, c := range l[i:] {
			if c != '\r' {
				out = append(out, c)
			}
		}
		out = append(out, '\n')
	}
	return out
}

// mergeText performs the whitespace-only short-circuit and then a line diff3.
func mergeText(base, local, remote []byte) *Result {
	nb := normalise(base)
	lEq := bytes.Equal(normalise(local), nb)
	rEq := bytes.Equal(normalise(remote), nb)
	switch {
	case lEq && rEq:
		// Both sides normalise to base: the side that actually changed
		// wins (there is nothing to conflict with); only when both changed
		// differently is local kept, and the note says so.
		r := &Result{Kind: KindText, Clean: true}
		switch {
		case bytes.Equal(local, base):
			r.Merged = clone(remote)
		case bytes.Equal(remote, base), bytes.Equal(local, remote):
			r.Merged = clone(local)
		default:
			r.Merged = clone(local)
			r.Note = "formatting-only changes on both sides: kept local"
		}
		return r
	case lEq:
		r := &Result{Kind: KindText, Clean: true, Merged: clone(remote)}
		if !bytes.Equal(local, base) {
			r.Note = "formatting-only change on local dropped"
		}
		return r
	case rEq:
		r := &Result{Kind: KindText, Clean: true, Merged: clone(local)}
		if !bytes.Equal(remote, base) {
			r.Note = "formatting-only change on remote dropped"
		}
		return r
	}
	return diff3(base, local, remote)
}

func eqInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// diff3 is the classic three-way line merge: stable chunks (all sides agree)
// alternate with unstable chunks that are either one-sided, identical on both
// sides, or conflicts.
func diff3(base, local, remote []byte) *Result {
	bs, ls, rs := newLineSet(base), newLineSet(local), newLineSet(remote)
	var in interner
	bi, li, ri := in.intern(bs.lines), in.intern(ls.lines), in.intern(rs.lines)
	ma := myersMatch(bi, li)
	mb := myersMatch(bi, ri)
	nB, nL, nR := len(bi), len(li), len(ri)

	r := &Result{Kind: KindText}
	var pending []byte
	emit := func(set *lineSet, s, e int) {
		pending = appendPart(pending, set.raw(s, e))
	}
	flush := func() {
		if len(pending) > 0 {
			r.segments = append(r.segments, segment{text: pending, hunk: -1})
			pending = nil
		}
	}
	// emitKept emits n lines that both sides kept (equal modulo line
	// terminators): local's bytes, unless local kept base's bytes exactly,
	// in which case remote's are used so that a one-sided line-ending
	// change survives the merge instead of being silently reverted.
	emitKept := func(lo, la, lb, n int) {
		if bytes.Equal(ls.raw(la, la+n), bs.raw(lo, lo+n)) {
			emit(&rs, lb, lb+n)
		} else {
			emit(&ls, la, la+n)
		}
	}

	lo, la, lb := 0, 0, 0
	for {
		// Stable chunk: base line matched on both sides at the current offsets.
		i := 0
		for lo+i < nB && ma[lo+i] == la+i && mb[lo+i] == lb+i {
			i++
		}
		if i > 0 {
			emitKept(lo, la, lb, i)
			lo, la, lb = lo+i, la+i, lb+i
		}
		if lo >= nB && la >= nL && lb >= nR {
			break
		}
		// Unstable chunk: up to the next base line matched on both sides.
		o := lo
		for o < nB && (ma[o] < 0 || mb[o] < 0) {
			o++
		}
		eL, eR := nL, nR
		if o < nB {
			eL, eR = ma[o], mb[o]
		}
		lSame, rSame := eqInts(bi[lo:o], li[la:eL]), eqInts(bi[lo:o], ri[lb:eR])
		switch {
		case lSame && rSame:
			emitKept(lo, la, lb, o-lo) // unchanged modulo terminators
		case lSame:
			emit(&rs, lb, eR) // only remote changed
		case rSame:
			emit(&ls, la, eL) // only local changed
		case eqInts(li[la:eL], ri[lb:eR]):
			emit(&ls, la, eL) // identical change on both sides
		default:
			flush()
			r.Hunks = append(r.Hunks, Hunk{
				Base:        bs.slice(lo, o),
				Local:       ls.slice(la, eL),
				Remote:      rs.slice(lb, eR),
				BaseRange:   LineRange{lo, o},
				LocalRange:  LineRange{la, eL},
				RemoteRange: LineRange{lb, eR},
			})
			r.segments = append(r.segments, segment{hunk: len(r.Hunks) - 1})
		}
		lo, la, lb = o, eL, eR
	}
	flush()
	if len(r.Hunks) == 0 {
		r.Clean = true
		r.Merged = []byte{}
		for _, s := range r.segments {
			r.Merged = appendPart(r.Merged, s.text)
		}
	}
	return r
}
