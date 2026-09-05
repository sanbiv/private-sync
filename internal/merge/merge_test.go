package merge

import (
	"bytes"
	"math/rand"
	"strings"
	"testing"
)

func TestEqRaw(t *testing.T) {
	tests := []struct {
		name string
		a, b []byte
		want bool
	}{
		{"both absent", nil, nil, true},
		{"absent vs empty", nil, []byte{}, false},
		{"absent vs present", nil, []byte("A=1\n"), false},
		{"equal", []byte("A=1\n"), []byte("A=1\n"), true},
		{"final newline ignored", []byte("A=1"), []byte("A=1\n"), true},
		{"final CRLF equals LF", []byte("A=1\r\n"), []byte("A=1\n"), true},
		{"final CRLF equals none", []byte("A=1\r\n"), []byte("A=1"), true},
		{"value differs", []byte("A=1\n"), []byte("A=2\n"), false},
		{"quotes differ", []byte("A=1\n"), []byte("A=\"1\"\n"), false},
		{"export differs", []byte("A=1\n"), []byte("export A=1\n"), false},
		{"leading space differs", []byte("A=1\n"), []byte(" A=1\n"), false},
		{"interior CRLF of multi-line value differs", []byte("K=\"a\r\nb\"\n"), []byte("K=\"a\nb\"\n"), false},
		{"multi-line equal modulo final terminator", []byte("K=\"a\nb\""), []byte("K=\"a\nb\"\r\n"), true},
		{"only a newline vs empty", []byte("\n"), []byte(""), true},
	}
	for _, tc := range tests {
		if got := eqRaw(tc.a, tc.b); got != tc.want {
			t.Errorf("%s: eqRaw(%q, %q) = %v, want %v", tc.name, tc.a, tc.b, got, tc.want)
		}
		if got := eqRaw(tc.b, tc.a); got != tc.want {
			t.Errorf("%s: eqRaw(%q, %q) = %v, want %v (symmetry)", tc.name, tc.b, tc.a, got, tc.want)
		}
	}
}

func TestDotenvTerminatorsAndCRLF(t *testing.T) {
	tests := []struct {
		name                string
		base, local, remote string
		wantClean           bool
		wantMerged          string
		wantHunks           []wantHunk
	}{
		{
			name: "CRLF file merges key by key",
			base: "A=1\r\nB=2\r\n", local: "A=10\r\nB=2\r\n", remote: "A=1\r\nB=20\r\n",
			wantClean: true, wantMerged: "A=10\r\nB=20\r\n",
		},
		{
			name: "local adds final newline, remote edits last key",
			base: "A=1\nB=2", local: "A=1\nB=2\n", remote: "A=1\nB=3",
			wantClean: true, wantMerged: "A=1\nB=3",
		},
		{
			name: "last entry CRLF on local, value edit on remote",
			base: "A=1\n", local: "A=1\r\n", remote: "A=2\n",
			wantClean: true, wantMerged: "A=2\n",
		},
		{
			name: "local drops final newline while remote appends a key",
			base: "A=1\nB=2\n", local: "A=1\nB=2", remote: "A=1\nB=2\nC=3\n",
			wantClean: true, wantMerged: "A=1\nB=2\nC=3\n",
		},
		{
			name: "local drops final CRLF while remote appends a key",
			base: "A=1\r\nB=2\r\n", local: "A=1\r\nB=2", remote: "A=1\r\nB=2\r\nC=3\r\n",
			wantClean: true, wantMerged: "A=1\r\nB=2\r\nC=3\r\n",
		},
		{
			name: "single unterminated local line takes the separator from remote",
			base: "A=1", local: "A=1", remote: "A=1\r\nB=2\r\n",
			wantClean: true, wantMerged: "A=1\r\nB=2\r\n",
		},
		{
			name: "CRLF conflict renders CRLF markers and custom text",
			base: "A=1\r\nB=2\r\n", local: "A=2\r\nB=2\r\n", remote: "A=3\r\nB=2\r\n",
			wantHunks: []wantHunk{{"A", str("A=1\r\n"), str("A=2\r\n"), str("A=3\r\n")}},
		},
		{
			name: "interior CRLF in a multi-line value is a change",
			base: "K=\"a\nb\"\n", local: "K=\"a\r\nb\"\n", remote: "K=\"a\nb\"\n",
			wantClean: true, wantMerged: "K=\"a\r\nb\"\n",
		},
		{
			name: "both edit last key without newline differently",
			base: "A=1", local: "A=2", remote: "A=3\n",
			wantHunks: []wantHunk{{"A", str("A=1"), str("A=2"), str("A=3\n")}},
		},
		{
			name: "remote removes the only key of a file without newline",
			base: "A=1", local: "A=1", remote: "",
			wantClean: true, wantMerged: "",
		},
		{
			name: "comment-only local keeps comments and takes remote keys",
			base: "", local: "# only a comment\n", remote: "A=1\n",
			wantClean: true, wantMerged: "# only a comment\nA=1\n",
		},
		{
			name: "blank lines around a removed key are kept",
			base: "A=1\n\nB=2\n\nC=3\n", local: "A=1\n\nB=2\n\nC=3\n", remote: "A=1\n\nC=3\n",
			wantClean: true, wantMerged: "A=1\n\n\nC=3\n",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := ThreeWay("svc/.env.local", []byte(tc.base), []byte(tc.local), []byte(tc.remote))
			if r.Kind != KindDotenv {
				t.Fatalf("kind %v, want dotenv (note %q)", r.Kind, r.Note)
			}
			if r.Clean != tc.wantClean {
				t.Fatalf("clean %v, want %v (hunks %+v, merged %q)", r.Clean, tc.wantClean, r.Hunks, r.Merged)
			}
			if tc.wantClean {
				if string(r.Merged) != tc.wantMerged {
					t.Fatalf("merged %q, want %q", r.Merged, tc.wantMerged)
				}
				return
			}
			checkHunks(t, r.Hunks, tc.wantHunks)
			if strings.Contains(tc.base, "\r\n") {
				m := RenderMarkers(r, "", "")
				if !bytes.Contains(m, []byte("<<<<<<< local\r\n")) || !bytes.Contains(m, []byte("=======\r\n")) || bytes.Contains(m, []byte("=\n")) {
					t.Fatalf("CRLF dotenv markers %q", m)
				}
				got, err := Resolve(r, []Choice{{Side: SideCustom, Custom: []byte("A=9")}})
				if err != nil || string(got) != "A=9\r\nB=2\r\n" {
					t.Fatalf("CRLF dotenv custom: %q %v", got, err)
				}
			}
		})
	}
}

func TestDotenvRemovalConflictMarkersAndResolve(t *testing.T) {
	base := "# cfg\nA=1\nB=2\n"
	local := "# cfg\nB=2\n"
	remote := "# cfg\nA=9\nB=2\n"
	r := ThreeWay(".env", []byte(base), []byte(local), []byte(remote))
	if r.Clean || len(r.Hunks) != 1 || r.Hunks[0].Local != nil {
		t.Fatalf("unexpected: %+v", r)
	}
	got := RenderMarkers(r, "", "")
	want := "# cfg\nB=2\n<<<<<<< local\n||||||| base\nA=1\n=======\nA=9\n>>>>>>> remote\n"
	if string(got) != want {
		t.Fatalf("markers %q, want %q", got, want)
	}
	if !HasMarkers(got) {
		t.Fatal("HasMarkers false")
	}
	for _, tc := range []struct {
		side Side
		want string
	}{
		{SideLocal, "# cfg\nB=2\n"},
		{SideRemote, "# cfg\nB=2\nA=9\n"},
		{SideBase, "# cfg\nB=2\nA=1\n"},
	} {
		res, err := Resolve(r, []Choice{{Side: tc.side}})
		if err != nil || string(res) != tc.want {
			t.Fatalf("%v: %q %v, want %q", tc.side, res, err, tc.want)
		}
	}
}

func TestThreeWayNilAndEmptyInputs(t *testing.T) {
	// nil local/remote are treated as empty and never panic.
	r := ThreeWay("f.txt", []byte("a\n"), nil, nil)
	if !r.Clean || r.Merged == nil || len(r.Merged) != 0 || len(r.Hunks) != 0 {
		t.Fatalf("both nil: %+v", r)
	}
	r = ThreeWay(".env", []byte("A=1\n"), nil, []byte("A=1\n"))
	if !r.Clean || string(r.Merged) != "" || r.Kind != KindDotenv {
		t.Fatalf("nil local dotenv: %+v", r)
	}
	r = ThreeWay("f.txt", nil, nil, nil)
	if !r.Clean || r.Merged == nil || len(r.Merged) != 0 {
		t.Fatalf("all nil: %+v", r)
	}
	r = ThreeWay("f.txt", nil, nil, []byte("x"))
	if r.Clean || len(r.Hunks) != 1 || r.Hunks[0].Local != nil || string(r.Hunks[0].Remote) != "x" {
		t.Fatalf("nil local no base: %+v", r)
	}
	// Empty base with an empty side.
	r = ThreeWay("f.txt", []byte{}, []byte{}, []byte("x\n"))
	if !r.Clean || string(r.Merged) != "x\n" {
		t.Fatalf("empty base: %+v", r)
	}
	// Whitespace-only files.
	r = ThreeWay("f.txt", []byte("\n"), []byte("\n\n"), []byte("  \n"))
	if !r.Clean {
		t.Fatalf("whitespace files: %+v", r)
	}
}

func TestResolveBaseOnInsertConflictOmits(t *testing.T) {
	r := ThreeWay("f.txt", []byte("a\nb\n"), []byte("a\nX\nb\n"), []byte("a\nY\nb\n"))
	if r.Clean || len(r.Hunks) != 1 || r.Hunks[0].Base != nil {
		t.Fatalf("unexpected: %+v", r)
	}
	got, err := Resolve(r, []Choice{{Side: SideBase}})
	if err != nil || string(got) != "a\nb\n" {
		t.Fatalf("base on insert conflict: %q %v", got, err)
	}
	m := RenderMarkers(r, "L", "R")
	want := "a\n<<<<<<< L\nX\n||||||| base\n=======\nY\n>>>>>>> R\nb\n"
	if string(m) != want {
		t.Fatalf("markers %q, want %q", m, want)
	}
}

// checkTextHunkLayout verifies that every text hunk's bytes are exactly the
// lines named by its ranges and that hunks appear in increasing file order.
func checkTextHunkLayout(t *testing.T, r *Result, base, local, remote []byte) {
	t.Helper()
	bs, ls, rs := newLineSet(base), newLineSet(local), newLineSet(remote)
	prev := Hunk{}
	for i, h := range r.Hunks {
		if h.Key != "" {
			t.Fatalf("hunk %d: text hunk with key %q", i, h.Key)
		}
		check := func(side string, got []byte, set *lineSet, lr LineRange) {
			t.Helper()
			if lr.Start > lr.End || lr.Start < 0 || lr.End > len(set.lines) {
				t.Fatalf("hunk %d %s: range %v out of bounds (%d lines)", i, side, lr, len(set.lines))
			}
			want := set.raw(lr.Start, lr.End)
			if !bytes.Equal(got, want) || (got == nil) != (want == nil) {
				t.Fatalf("hunk %d %s: bytes %q do not match range %v (%q)", i, side, got, lr, want)
			}
		}
		check("base", h.Base, &bs, h.BaseRange)
		check("local", h.Local, &ls, h.LocalRange)
		check("remote", h.Remote, &rs, h.RemoteRange)
		if i > 0 {
			if h.BaseRange.Start < prev.BaseRange.End || h.LocalRange.Start < prev.LocalRange.End || h.RemoteRange.Start < prev.RemoteRange.End {
				t.Fatalf("hunk %d overlaps or precedes hunk %d: %+v vs %+v", i, i-1, h, prev)
			}
		}
		prev = h
	}
}

func TestTextHunkLayoutTable(t *testing.T) {
	cases := []struct{ base, local, remote string }{
		{"a\nb\nc\nd\n", "a\nB\nc\nd\n", "a\nb\nC\nd\n"},
		{"a\nb\nc\ne\n", "1\nb\nc\n2\n", "3\nb\nc\n4\n"},
		{"a\nb", "a\nB", "a\nb\nc\n"},
		{"", "x\n", "y\n"},
		{"a\nb\nc\n", "a\nc\n", "a\nB\nc\n"},
		{"x\n", "y", "z"},
	}
	for _, c := range cases {
		r := ThreeWay("f.txt", []byte(c.base), []byte(c.local), []byte(c.remote))
		if r.Clean {
			t.Fatalf("%q/%q/%q: expected conflict", c.base, c.local, c.remote)
		}
		checkTextHunkLayout(t, r, []byte(c.base), []byte(c.local), []byte(c.remote))
	}
}

// randomText builds a small file from a tiny line alphabet so that repeated
// lines, indentation and CRLF variants all show up.
func randomText(rng *rand.Rand, n int) []byte {
	words := []string{"alpha", "beta", "gamma", "delta", "alpha", "", "  indented", "\tx = 1"}
	var b strings.Builder
	for i := 0; i < n; i++ {
		b.WriteString(words[rng.Intn(len(words))])
		if rng.Intn(8) == 0 {
			b.WriteString("\r\n")
		} else {
			b.WriteString("\n")
		}
	}
	s := b.String()
	if rng.Intn(5) == 0 {
		s = strings.TrimSuffix(strings.TrimSuffix(s, "\n"), "\r")
	}
	return []byte(s)
}

// mutate applies a few random line edits to src.
func mutate(rng *rand.Rand, src []byte) []byte {
	lines := newLineSet(src).lines
	out := make([][]byte, 0, len(lines)+4)
	out = append(out, lines...)
	edits := rng.Intn(4)
	for e := 0; e < edits; e++ {
		pos := 0
		if len(out) > 0 {
			pos = rng.Intn(len(out) + 1)
		}
		switch rng.Intn(4) {
		case 0: // insert
			out = append(out[:pos], append([][]byte{randomText(rng, 1)}, out[pos:]...)...)
		case 1: // delete
			if pos < len(out) {
				out = append(out[:pos], out[pos+1:]...)
			}
		case 2: // replace
			if pos < len(out) {
				out[pos] = randomText(rng, 1)
			}
		case 3: // reindent
			if pos < len(out) {
				out[pos] = append([]byte("    "), bytes.TrimLeft(out[pos], " \t")...)
			}
		}
	}
	var b []byte
	for _, l := range out {
		b = append(b, l...)
		if n := len(b); n > 0 && b[n-1] != '\n' {
			b = append(b, '\n')
		}
	}
	if rng.Intn(6) == 0 {
		b = bytes.TrimSuffix(b, []byte("\n"))
	}
	if b == nil {
		b = []byte{}
	}
	return b
}

func TestTextThreeWayRandomInvariants(t *testing.T) {
	rng := rand.New(rand.NewSource(2026))
	for iter := 0; iter < 4000; iter++ {
		base := randomText(rng, rng.Intn(9))
		local := mutate(rng, base)
		remote := mutate(rng, base)
		switch rng.Intn(10) {
		case 0:
			local = append([]byte{}, base...)
		case 1:
			remote = append([]byte{}, base...)
		case 2:
			remote = append([]byte{}, local...)
		}
		r := ThreeWay("f.txt", base, local, remote)
		if r.Kind != KindText {
			t.Fatalf("iter %d: kind %v", iter, r.Kind)
		}
		desc := func() string {
			return "base=" + string(base) + " local=" + string(local) + " remote=" + string(remote)
		}
		if r.Clean {
			if len(r.Hunks) != 0 || r.Merged == nil {
				t.Fatalf("iter %d: clean result malformed: %+v (%s)", iter, r, desc())
			}
			if !bytes.Equal(RenderMarkers(r, "", ""), r.Merged) {
				t.Fatalf("iter %d: markers of clean result differ from Merged (%s)", iter, desc())
			}
			got, err := Resolve(r, nil)
			if err != nil || !bytes.Equal(got, r.Merged) {
				t.Fatalf("iter %d: resolve clean: %q %v (%s)", iter, got, err, desc())
			}
			if HasMarkers(r.Merged) {
				t.Fatalf("iter %d: clean merge contains markers (%s)", iter, desc())
			}
		} else {
			if r.Merged != nil || len(r.Hunks) == 0 {
				t.Fatalf("iter %d: conflicted result malformed: %+v (%s)", iter, r, desc())
			}
			checkTextHunkLayout(t, r, base, local, remote)
			m := RenderMarkers(r, "L", "R")
			if !HasMarkers(m) {
				t.Fatalf("iter %d: rendered conflict has no markers (%s)", iter, desc())
			}
			for _, s := range []Side{SideLocal, SideRemote, SideBase} {
				got, err := Resolve(r, choicesFor(r, s))
				if err != nil {
					t.Fatalf("iter %d: resolve %v: %v (%s)", iter, s, err, desc())
				}
				if HasMarkers(got) {
					t.Fatalf("iter %d: resolved %v has markers (%s)", iter, s, desc())
				}
			}
		}
		switch {
		case bytes.Equal(local, remote):
			if !r.Clean || !bytes.Equal(r.Merged, local) {
				t.Fatalf("iter %d: local==remote must yield local: %+v (%s)", iter, r, desc())
			}
		case bytes.Equal(remote, base):
			if !r.Clean || !bytes.Equal(r.Merged, local) || r.Note != "" {
				t.Fatalf("iter %d: remote==base must yield local without a note: %+v (%s)", iter, r, desc())
			}
		case bytes.Equal(local, base):
			// An untouched local takes remote exactly, whitespace-only or
			// CRLF changes included: there is nothing to conflict with.
			if !r.Clean || !bytes.Equal(r.Merged, remote) || r.Note != "" {
				t.Fatalf("iter %d: local==base must yield remote without a note: %+v (%s)", iter, r, desc())
			}
		}
	}
}

func TestLineDiffRandomIsMinimalAndReconstructs(t *testing.T) {
	rng := rand.New(rand.NewSource(99))
	for iter := 0; iter < 2000; iter++ {
		a := randomText(rng, rng.Intn(12))
		b := mutate(rng, a)
		ops := LineDiff(a, b)
		var ra, rb []string
		same := 0
		for _, o := range ops {
			switch o.Kind {
			case ' ':
				same++
				ra = append(ra, o.Text)
				rb = append(rb, o.Text)
			case '-':
				ra = append(ra, o.Text)
			case '+':
				rb = append(rb, o.Text)
			default:
				t.Fatalf("iter %d: bad op kind %q", iter, o.Kind)
			}
		}
		if strings.Join(ra, "|") != strings.Join(lineTexts(string(a)), "|") || strings.Join(rb, "|") != strings.Join(lineTexts(string(b)), "|") {
			t.Fatalf("iter %d: ops do not reconstruct a=%q b=%q: %+v", iter, a, b, ops)
		}
		var in interner
		ia, ib := in.intern(newLineSet(a).lines), in.intern(newLineSet(b).lines)
		if want := lcsLen(ia, ib); same != want {
			t.Fatalf("iter %d: %d common lines, LCS is %d (a=%q b=%q)", iter, same, want, a, b)
		}
	}
}

func TestDotenvRandomInvariants(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	keys := []string{"A", "B", "C", "D"}
	gen := func(present map[string]bool) []byte {
		var b strings.Builder
		for _, k := range keys {
			if !present[k] {
				continue
			}
			if rng.Intn(4) == 0 {
				b.WriteString("# about " + k + "\n")
			}
			b.WriteString(k + "=" + []string{"1", "2", "\"q\"", "x y"}[rng.Intn(4)] + "\n")
		}
		s := b.String()
		if rng.Intn(5) == 0 {
			s = strings.TrimSuffix(s, "\n")
		}
		return []byte(s)
	}
	randomSet := func() map[string]bool {
		m := map[string]bool{}
		for _, k := range keys {
			if rng.Intn(3) != 0 {
				m[k] = true
			}
		}
		return m
	}
	for iter := 0; iter < 3000; iter++ {
		base := gen(randomSet())
		local := gen(randomSet())
		remote := gen(randomSet())
		switch rng.Intn(6) {
		case 0:
			remote = append([]byte{}, base...)
		case 1:
			remote = append([]byte{}, local...)
		}
		r := ThreeWay(".env", base, local, remote)
		if r.Kind != KindDotenv {
			t.Fatalf("iter %d: kind %v note %q", iter, r.Kind, r.Note)
		}
		if r.Clean {
			if len(r.Hunks) != 0 || r.Merged == nil {
				t.Fatalf("iter %d: malformed clean result %+v", iter, r)
			}
			if _, err := parseDotenv(r.Merged); err != nil {
				t.Fatalf("iter %d: merged output does not parse: %v (%q)", iter, err, r.Merged)
			}
		} else {
			if r.Merged != nil {
				t.Fatalf("iter %d: conflicted result has Merged", iter)
			}
			seen := map[string]bool{}
			for _, h := range r.Hunks {
				if h.Key == "" || seen[h.Key] {
					t.Fatalf("iter %d: bad or duplicate hunk key %q", iter, h.Key)
				}
				seen[h.Key] = true
			}
			for _, s := range []Side{SideLocal, SideRemote, SideBase} {
				got, err := Resolve(r, choicesFor(r, s))
				if err != nil {
					t.Fatalf("iter %d: resolve %v: %v", iter, s, err)
				}
				if _, err := parseDotenv(got); err != nil {
					t.Fatalf("iter %d: resolved %v does not parse: %v (%q)", iter, s, err, got)
				}
			}
			if !HasMarkers(RenderMarkers(r, "", "")) {
				t.Fatalf("iter %d: no markers rendered", iter)
			}
		}
		switch {
		case bytes.Equal(local, remote):
			if !r.Clean || !bytes.Equal(r.Merged, local) {
				t.Fatalf("iter %d: local==remote must yield local: %+v (local %q)", iter, r, local)
			}
		case bytes.Equal(remote, base):
			if !r.Clean || !bytes.Equal(r.Merged, local) {
				t.Fatalf("iter %d: remote==base must yield local: %+v (local %q)", iter, r, local)
			}
		}
	}
}

func TestSideAndKindStrings(t *testing.T) {
	if KindDotenv.String() != "dotenv" || KindText.String() != "text" || KindBinary.String() != "binary" {
		t.Fatal("kind names")
	}
	if SideLocal.String() != "local" || SideRemote.String() != "remote" || SideBase.String() != "base" || SideCustom.String() != "custom" {
		t.Fatal("side names")
	}
}
