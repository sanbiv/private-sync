package merge

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestNormalise(t *testing.T) {
	tests := []struct{ in, want string }{
		{"", ""},
		{"a", "a\n"},
		{"a\n", "a\n"},
		{"a\r\n", "a\n"},
		{"a  \t\n", "a\n"},
		{"\tx\n", " x\n"},
		{"    x\n", " x\n"},
		{" \t x  \r\n", " x\n"},
		{"a\n\n", "a\n\n"},
		{"   \n", "\n"},
		{"a\rb\n", "ab\n"},
	}
	for _, tc := range tests {
		if got := string(normalise([]byte(tc.in))); got != tc.want {
			t.Errorf("normalise(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

type wantTextHunk struct {
	base, local, remote *string
	br, lr, rr          LineRange
}

func TestTextThreeWay(t *testing.T) {
	tests := []struct {
		name                string
		base, local, remote string
		wantClean           bool
		wantMerged          string
		wantHunks           []wantTextHunk
		wantNote            string
	}{
		{
			name: "no changes",
			base: "a\nb\n", local: "a\nb\n", remote: "a\nb\n",
			wantClean: true, wantMerged: "a\nb\n",
		},
		{
			name: "non-overlapping edits both sides",
			base: "a\nb\nc\nd\ne\n", local: "A\nb\nc\nd\ne\n", remote: "a\nb\nc\nd\nE\n",
			wantClean: true, wantMerged: "A\nb\nc\nd\nE\n",
		},
		{
			name: "identical edits",
			base: "a\nb\nc\n", local: "a\nB\nc\n", remote: "a\nB\nc\n",
			wantClean: true, wantMerged: "a\nB\nc\n",
		},
		{
			name: "identical insertions",
			base: "a\nb\n", local: "a\nX\nb\n", remote: "a\nX\nb\n",
			wantClean: true, wantMerged: "a\nX\nb\n",
		},
		{
			name: "adjacent edits conflict",
			base: "a\nb\nc\nd\n", local: "a\nB\nc\nd\n", remote: "a\nb\nC\nd\n",
			wantHunks: []wantTextHunk{{str("b\nc\n"), str("B\nc\n"), str("b\nC\n"), LineRange{1, 3}, LineRange{1, 3}, LineRange{1, 3}}},
		},
		{
			name: "overlapping edit conflict",
			base: "a\nb\nc\n", local: "a\nX\nc\n", remote: "a\nY\nc\n",
			wantHunks: []wantTextHunk{{str("b\n"), str("X\n"), str("Y\n"), LineRange{1, 2}, LineRange{1, 2}, LineRange{1, 2}}},
		},
		{
			name: "insert at same place differently",
			base: "a\nb\n", local: "a\nX\nb\n", remote: "a\nY\nb\n",
			wantHunks: []wantTextHunk{{nil, str("X\n"), str("Y\n"), LineRange{1, 1}, LineRange{1, 2}, LineRange{1, 2}}},
		},
		{
			name: "delete vs modify",
			base: "a\nb\nc\n", local: "a\nc\n", remote: "a\nB\nc\n",
			wantHunks: []wantTextHunk{{str("b\n"), nil, str("B\n"), LineRange{1, 2}, LineRange{1, 1}, LineRange{1, 2}}},
		},
		{
			name: "modify vs delete",
			base: "a\nb\nc\n", local: "a\nB\nc\n", remote: "a\nc\n",
			wantHunks: []wantTextHunk{{str("b\n"), str("B\n"), nil, LineRange{1, 2}, LineRange{1, 2}, LineRange{1, 1}}},
		},
		{
			name: "both delete the same line",
			base: "a\nb\nc\n", local: "a\nc\n", remote: "a\nc\n",
			wantClean: true, wantMerged: "a\nc\n",
		},
		{
			name: "edit at start, append at end",
			base: "a\nb\n", local: "A\nb\n", remote: "a\nb\nc\n",
			wantClean: true, wantMerged: "A\nb\nc\n",
		},
		{
			name: "local unchanged, remote edited",
			base: "a\nb\n", local: "a\nb\n", remote: "a\nB\n",
			wantClean: true, wantMerged: "a\nB\n",
		},
		{
			name: "remote unchanged, local edited",
			base: "a\nb\n", local: "a\nB\n", remote: "a\nb\n",
			wantClean: true, wantMerged: "a\nB\n",
		},
		{
			name: "CRLF conversion on local only is formatting-only",
			base: "a\nb\nc\n", local: "a\r\nb\r\nc\r\n", remote: "a\nB\nc\n",
			wantClean: true, wantMerged: "a\nB\nc\n",
			wantNote: "formatting-only change on local dropped",
		},
		{
			name: "CRLF side with content edit merges line by line",
			base: "a\nb\nc\n", local: "a\r\nb\r\nC\r\n", remote: "A\nb\nc\n",
			wantClean: true, wantMerged: "A\nb\r\nC\r\n",
		},
		{
			name: "reindent on local, content edit on remote",
			base: "f {\n\tx = 1\n\ty = 2\n}\n", local: "f {\n    x = 1\n    y = 2\n}\n", remote: "f {\n\tx = 1\n\ty = 3\n}\n",
			wantClean: true, wantMerged: "f {\n\tx = 1\n\ty = 3\n}\n",
			wantNote: "formatting-only change on local dropped",
		},
		{
			name: "reindent on remote, content edit on local",
			base: "f {\n\tx = 1\n}\n", local: "f {\n\tx = 2\n}\n", remote: "f {\n    x = 1\n}\n",
			wantClean: true, wantMerged: "f {\n\tx = 2\n}\n",
			wantNote: "formatting-only change on remote dropped",
		},
		{
			name: "both whitespace-only keeps local",
			base: "a\n\tb\n", local: "a\n    b\n", remote: "a  \n\tb\n",
			wantClean: true, wantMerged: "a\n    b\n",
			wantNote: "formatting-only changes on both sides: kept local",
		},
		{
			// Spec §8: the short-circuit applies when exactly one side is
			// formatting-only; an untouched local has nothing to conflict
			// with, so remote's reindent is taken, not reverted.
			name: "local untouched, remote whitespace-only",
			base: "a\n\tb\n", local: "a\n\tb\n", remote: "a\n  b\n",
			wantClean: true, wantMerged: "a\n  b\n",
		},
		{
			name: "remote untouched, local whitespace-only",
			base: "a\n\tb\n", local: "a\n  b\n", remote: "a\n\tb\n",
			wantClean: true, wantMerged: "a\n  b\n",
		},
		{
			name: "local untouched, remote converts to CRLF",
			base: "a\nb\n", local: "a\nb\n", remote: "a\r\nb\r\n",
			wantClean: true, wantMerged: "a\r\nb\r\n",
		},
		{
			name: "local untouched, remote edits and converts to CRLF",
			base: "a\nb\nc\n", local: "a\nb\nc\n", remote: "a\r\nB\r\nc\r\n",
			wantClean: true, wantMerged: "a\r\nB\r\nc\r\n",
		},
		{
			name: "remote converts to CRLF and edits, local edits elsewhere",
			base: "a\nb\nc\n", local: "A\nb\nc\n", remote: "a\r\nb\r\nC\r\n",
			wantClean: true, wantMerged: "A\nb\r\nC\r\n",
		},
		{
			name: "trailing newline added on local only is formatting-only",
			base: "a\nb", local: "a\nb\n", remote: "A\nb",
			wantClean: true, wantMerged: "A\nb",
			wantNote: "formatting-only change on local dropped",
		},
		{
			name: "empty base both add differently",
			base: "", local: "x\n", remote: "y\n",
			wantHunks: []wantTextHunk{{nil, str("x\n"), str("y\n"), LineRange{0, 0}, LineRange{0, 1}, LineRange{0, 1}}},
		},
		{
			name: "empty base, remote unchanged",
			base: "", local: "x\n", remote: "",
			wantClean: true, wantMerged: "x\n",
		},
		{
			name: "remote emptied the file, local edited",
			base: "a\nb\n", local: "a\nB\n", remote: "",
			wantHunks: []wantTextHunk{{str("a\nb\n"), str("a\nB\n"), nil, LineRange{0, 2}, LineRange{0, 2}, LineRange{0, 0}}},
		},
		{
			name: "conflict followed by clean local change",
			base: "a\nb\nc\nd\ne\n", local: "a\nX\nc\nd\nE\n", remote: "a\nY\nc\nd\ne\n",
			wantHunks: []wantTextHunk{{str("b\n"), str("X\n"), str("Y\n"), LineRange{1, 2}, LineRange{1, 2}, LineRange{1, 2}}},
		},
		{
			name: "two conflicts",
			base: "a\nb\nc\nd\ne\n", local: "1\nb\nc\nd\n2\n", remote: "3\nb\nc\nd\n4\n",
			wantHunks: []wantTextHunk{
				{str("a\n"), str("1\n"), str("3\n"), LineRange{0, 1}, LineRange{0, 1}, LineRange{0, 1}},
				{str("e\n"), str("2\n"), str("4\n"), LineRange{4, 5}, LineRange{4, 5}, LineRange{4, 5}},
			},
		},
		{
			name: "missing final newline conflict",
			base: "a\nb", local: "a\nB", remote: "a\nb\nc\n",
			wantHunks: []wantTextHunk{{str("b"), str("B"), str("b\nc\n"), LineRange{1, 2}, LineRange{1, 2}, LineRange{1, 3}}},
		},
		{
			name: "moved block on remote, edit elsewhere on local",
			base: "a\nb\nc\nd\ne\nf\n", local: "a\nb\nc\nd\ne\nF\n", remote: "c\nd\na\nb\ne\nf\n",
			wantClean: true, wantMerged: "c\nd\na\nb\ne\nF\n",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := ThreeWay("notes.txt", []byte(tc.base), []byte(tc.local), []byte(tc.remote))
			if r.Kind != KindText {
				t.Fatalf("kind %v, want text", r.Kind)
			}
			if r.Note != tc.wantNote {
				t.Errorf("note %q, want %q", r.Note, tc.wantNote)
			}
			if r.Clean != tc.wantClean {
				t.Fatalf("clean %v, want %v (hunks %+v, merged %q)", r.Clean, tc.wantClean, r.Hunks, r.Merged)
			}
			if tc.wantClean {
				if string(r.Merged) != tc.wantMerged {
					t.Fatalf("merged %q, want %q", r.Merged, tc.wantMerged)
				}
				if len(r.Hunks) != 0 {
					t.Fatalf("clean result has hunks")
				}
				return
			}
			if r.Merged != nil {
				t.Fatalf("conflicted result has Merged %q", r.Merged)
			}
			if len(r.Hunks) != len(tc.wantHunks) {
				t.Fatalf("got %d hunks, want %d: %+v", len(r.Hunks), len(tc.wantHunks), r.Hunks)
			}
			for i, w := range tc.wantHunks {
				h := r.Hunks[i]
				if h.Key != "" {
					t.Errorf("hunk %d has key %q", i, h.Key)
				}
				checkSide := func(side string, g []byte, w *string, gr, wr LineRange) {
					t.Helper()
					switch {
					case w == nil && g != nil:
						t.Errorf("hunk %d %s: got %q, want absent", i, side, g)
					case w != nil && (g == nil || string(g) != *w):
						t.Errorf("hunk %d %s: got %q, want %q", i, side, g, *w)
					}
					if gr != wr {
						t.Errorf("hunk %d %s range: got %v, want %v", i, side, gr, wr)
					}
				}
				checkSide("base", h.Base, w.base, h.BaseRange, w.br)
				checkSide("local", h.Local, w.local, h.LocalRange, w.lr)
				checkSide("remote", h.Remote, w.remote, h.RemoteRange, w.rr)
			}
			// Choosing local everywhere must reproduce local when remote only
			// touched the conflicting regions; at minimum the result must be
			// marker-free and contain every clean segment.
			got, err := Resolve(r, choicesFor(r, SideLocal))
			if err != nil {
				t.Fatal(err)
			}
			if HasMarkers(got) {
				t.Fatalf("resolved output has markers: %q", got)
			}
		})
	}
}

func choicesFor(r *Result, s Side) []Choice {
	c := make([]Choice, len(r.Hunks))
	for i := range c {
		c[i] = Choice{Side: s}
	}
	return c
}

func TestTextResolve(t *testing.T) {
	base := "a\nb\nc\nd\ne\n"
	local := "a\nX\nc\nd\nE\n"
	remote := "a\nY\nc\nd\ne\n"
	r := ThreeWay("f.txt", []byte(base), []byte(local), []byte(remote))
	if r.Clean || len(r.Hunks) != 1 {
		t.Fatalf("unexpected result %+v", r)
	}
	tests := []struct {
		name   string
		choice Choice
		want   string
	}{
		{"local", Choice{Side: SideLocal}, "a\nX\nc\nd\nE\n"},
		{"remote", Choice{Side: SideRemote}, "a\nY\nc\nd\nE\n"},
		{"base", Choice{Side: SideBase}, "a\nb\nc\nd\nE\n"},
		{"custom", Choice{Side: SideCustom, Custom: []byte("Z\n")}, "a\nZ\nc\nd\nE\n"},
		{"custom without newline", Choice{Side: SideCustom, Custom: []byte("Z")}, "a\nZ\nc\nd\nE\n"},
		{"custom nil drops the lines", Choice{Side: SideCustom}, "a\nc\nd\nE\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Resolve(r, []Choice{tc.choice})
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}

	// Delete-vs-modify: picking the deleting side omits the lines.
	r2 := ThreeWay("f.txt", []byte("a\nb\nc\n"), []byte("a\nc\n"), []byte("a\nB\nc\n"))
	got, err := Resolve(r2, []Choice{{Side: SideLocal}})
	if err != nil || string(got) != "a\nc\n" {
		t.Fatalf("delete side: %q %v", got, err)
	}
	got, err = Resolve(r2, []Choice{{Side: SideRemote}})
	if err != nil || string(got) != "a\nB\nc\n" {
		t.Fatalf("modify side: %q %v", got, err)
	}

	// Two conflicts resolved with different sides.
	r3 := ThreeWay("f.txt", []byte("a\nb\nc\nd\ne\n"), []byte("1\nb\nc\nd\n2\n"), []byte("3\nb\nc\nd\n4\n"))
	got, err = Resolve(r3, []Choice{{Side: SideRemote}, {Side: SideLocal}})
	if err != nil || string(got) != "3\nb\nc\nd\n2\n" {
		t.Fatalf("mixed: %q %v", got, err)
	}
}

func TestResolveErrorsAndCleanResults(t *testing.T) {
	if _, err := Resolve(nil, nil); err == nil {
		t.Fatal("nil result: expected error")
	}
	clean := ThreeWay("f.txt", []byte("a\n"), []byte("a\nb\n"), []byte("a\n"))
	got, err := Resolve(clean, nil)
	if err != nil || string(got) != "a\nb\n" {
		t.Fatalf("clean: %q %v", got, err)
	}
	got[0] = 'z'
	if string(clean.Merged) != "a\nb\n" {
		t.Fatal("Resolve returned aliased Merged")
	}
	if _, err := Resolve(clean, []Choice{{Side: SideLocal}}); err == nil {
		t.Fatal("too many choices: expected error")
	}
	conf := ThreeWay("f.txt", []byte("a\n"), []byte("b\n"), []byte("c\n"))
	if _, err := Resolve(conf, nil); err == nil {
		t.Fatal("too few choices: expected error")
	}
	if _, err := Resolve(conf, []Choice{{Side: Side(99)}}); err == nil {
		t.Fatal("unknown side: expected error")
	}
	// A hand-built single-hunk result without layout still resolves.
	manual := &Result{Kind: KindText, Hunks: []Hunk{{Local: []byte("L\n"), Remote: []byte("R\n")}}}
	got, err = Resolve(manual, []Choice{{Side: SideRemote}})
	if err != nil || string(got) != "R\n" {
		t.Fatalf("manual: %q %v", got, err)
	}
	if !HasMarkers(RenderMarkers(manual, "", "")) {
		t.Fatal("manual: markers missing")
	}
	// Two hunks without layout cannot be reassembled.
	manual2 := &Result{Kind: KindText, Hunks: []Hunk{{Local: []byte("1")}, {Local: []byte("2")}}}
	if _, err := Resolve(manual2, []Choice{{Side: SideLocal}, {Side: SideLocal}}); err == nil {
		t.Fatal("two hunks without layout: expected error")
	}
	// Empty clean result.
	empty := ThreeWay("f.txt", []byte(""), []byte(""), []byte(""))
	got, err = Resolve(empty, nil)
	if err != nil || got == nil || len(got) != 0 {
		t.Fatalf("empty: %v %v", got, err)
	}
}

func TestNoBase(t *testing.T) {
	t.Run("equal", func(t *testing.T) {
		r := ThreeWay("a.txt", nil, []byte("same\n"), []byte("same\n"))
		if !r.Clean || r.Kind != KindText || string(r.Merged) != "same\n" || len(r.Hunks) != 0 {
			t.Fatalf("unexpected: %+v", r)
		}
	})
	t.Run("different text", func(t *testing.T) {
		r := ThreeWay("a.txt", nil, []byte("l1\nl2\n"), []byte("r1\n"))
		if r.Clean || r.Kind != KindText || len(r.Hunks) != 1 {
			t.Fatalf("unexpected: %+v", r)
		}
		h := r.Hunks[0]
		if h.Base != nil || string(h.Local) != "l1\nl2\n" || string(h.Remote) != "r1\n" {
			t.Fatalf("hunk sides: %+v", h)
		}
		if h.LocalRange != (LineRange{0, 2}) || h.RemoteRange != (LineRange{0, 1}) || h.BaseRange != (LineRange{}) {
			t.Fatalf("ranges: %+v", h)
		}
		if r.Note == "" {
			t.Fatal("expected a note")
		}
		for _, tc := range []struct {
			side Side
			want string
		}{{SideLocal, "l1\nl2\n"}, {SideRemote, "r1\n"}, {SideBase, ""}} {
			got, err := Resolve(r, []Choice{{Side: tc.side}})
			if err != nil || string(got) != tc.want {
				t.Fatalf("%v: %q %v", tc.side, got, err)
			}
		}
		m := RenderMarkers(r, "here", "there")
		want := "<<<<<<< here\nl1\nl2\n||||||| base\n=======\nr1\n>>>>>>> there\n"
		if string(m) != want {
			t.Fatalf("markers %q, want %q", m, want)
		}
		if ops := LineDiff(h.Local, h.Remote); len(ops) != 3 {
			t.Fatalf("LineDiff ops %+v", ops)
		}
	})
	t.Run("different dotenv path stays text", func(t *testing.T) {
		r := ThreeWay(".env", nil, []byte("A=1\n"), []byte("A=2\n"))
		if r.Kind != KindText || r.Clean || len(r.Hunks) != 1 || r.Hunks[0].Key != "" {
			t.Fatalf("unexpected: %+v", r)
		}
	})
	t.Run("binary", func(t *testing.T) {
		l, rm := []byte("\x00\x01"), []byte("\x00\x02")
		r := ThreeWay("x.bin", nil, l, rm)
		if r.Kind != KindBinary || r.Clean || len(r.Hunks) != 1 {
			t.Fatalf("unexpected: %+v", r)
		}
		got, err := Resolve(r, []Choice{{Side: SideRemote}})
		if err != nil || !bytes.Equal(got, rm) {
			t.Fatalf("resolve: %q %v", got, err)
		}
		if eq := ThreeWay("x.bin", nil, l, l); !eq.Clean || eq.Kind != KindBinary {
			t.Fatalf("equal binary: %+v", eq)
		}
	})
	t.Run("both empty", func(t *testing.T) {
		r := ThreeWay("a.txt", nil, []byte{}, []byte{})
		if !r.Clean || r.Merged == nil || len(r.Merged) != 0 {
			t.Fatalf("unexpected: %+v", r)
		}
	})
}

func TestBinary(t *testing.T) {
	base := []byte("\x00base")
	local := []byte("\x00local\n")
	remote := []byte("\x00remote\n")
	r := ThreeWay("blob.bin", base, local, remote)
	if r.Kind != KindBinary || r.Clean || len(r.Hunks) != 1 || r.Note == "" {
		t.Fatalf("unexpected: %+v", r)
	}
	h := r.Hunks[0]
	if !bytes.Equal(h.Base, base) || !bytes.Equal(h.Local, local) || !bytes.Equal(h.Remote, remote) {
		t.Fatalf("hunk sides: %+v", h)
	}
	for _, tc := range []struct {
		side Side
		want []byte
	}{{SideLocal, local}, {SideRemote, remote}, {SideBase, base}} {
		got, err := Resolve(r, []Choice{{Side: tc.side}})
		if err != nil || !bytes.Equal(got, tc.want) {
			t.Fatalf("%v: %q %v", tc.side, got, err)
		}
	}
	custom := []byte("\x00custom")
	got, err := Resolve(r, []Choice{{Side: SideCustom, Custom: custom}})
	if err != nil || !bytes.Equal(got, custom) {
		t.Fatalf("custom: %q %v", got, err)
	}
	// Identical sides are clean even when the base differs.
	eq := ThreeWay("blob.bin", base, local, local)
	if !eq.Clean || eq.Kind != KindBinary || !bytes.Equal(eq.Merged, local) {
		t.Fatalf("equal: %+v", eq)
	}
	// One-sided binary change is still not merged (never Clean unless equal).
	one := ThreeWay("blob.bin", base, base, remote)
	if one.Clean || len(one.Hunks) != 1 {
		t.Fatalf("one-sided: %+v", one)
	}
	// Invalid UTF-8 on one side only makes the whole merge binary.
	inv := ThreeWay("t.txt", []byte("a\n"), []byte("a\xff\n"), []byte("b\n"))
	if inv.Kind != KindBinary {
		t.Fatalf("invalid utf8: kind %v", inv.Kind)
	}
	// Binary on a dotenv path is binary, not dotenv.
	env := ThreeWay(".env", []byte("A=1\n"), []byte("A=\x00\n"), []byte("A=2\n"))
	if env.Kind != KindBinary {
		t.Fatalf("dotenv with NUL: kind %v", env.Kind)
	}
	if HasMarkers(RenderMarkers(r, "", "")) != true {
		t.Fatal("binary markers missing")
	}
}

func TestRenderMarkersAndHasMarkersRoundTrip(t *testing.T) {
	base := "h\na\nb\nc\nd\ne\nf\n"
	local := "h\nA1\nb\nc\nd\nE1\nf\n"
	remote := "h\nA2\nb\nc\nd\nE2\nf\n"
	r := ThreeWay("f.txt", []byte(base), []byte(local), []byte(remote))
	if r.Clean || len(r.Hunks) != 2 {
		t.Fatalf("unexpected: %+v", r)
	}
	got := RenderMarkers(r, "local", "remote")
	want := "h\n" +
		"<<<<<<< local\nA1\n||||||| base\na\n=======\nA2\n>>>>>>> remote\n" +
		"b\nc\nd\n" +
		"<<<<<<< local\nE1\n||||||| base\ne\n=======\nE2\n>>>>>>> remote\n" +
		"f\n"
	if string(got) != want {
		t.Fatalf("markers:\n%s\nwant:\n%s", got, want)
	}
	if !HasMarkers(got) {
		t.Fatal("HasMarkers false")
	}
	// A user editing the markers away yields a marker-free file; Resolve does too.
	edited := strings.NewReplacer(
		"<<<<<<< local\nA1\n||||||| base\na\n=======\nA2\n>>>>>>> remote\n", "A3\n",
		"<<<<<<< local\nE1\n||||||| base\ne\n=======\nE2\n>>>>>>> remote\n", "E3\n",
	).Replace(string(got))
	if HasMarkers([]byte(edited)) || edited != "h\nA3\nb\nc\nd\nE3\nf\n" {
		t.Fatalf("edited: %q", edited)
	}
	res, err := Resolve(r, []Choice{{Side: SideLocal}, {Side: SideRemote}})
	if err != nil || HasMarkers(res) || string(res) != "h\nA1\nb\nc\nd\nE2\nf\n" {
		t.Fatalf("resolve: %q %v", res, err)
	}
	// Clean results render as the merged bytes; nil result renders nil.
	clean := ThreeWay("f.txt", []byte("a\n"), []byte("a\nb\n"), []byte("a\n"))
	if m := RenderMarkers(clean, "", ""); string(m) != "a\nb\n" {
		t.Fatalf("clean markers %q", m)
	}
	if RenderMarkers(nil, "", "") != nil {
		t.Fatal("nil result should render nil")
	}
	// Sides without a trailing newline still get their own marker lines.
	nb := ThreeWay("f.txt", nil, []byte("L"), []byte("R"))
	if m := RenderMarkers(nb, "", ""); string(m) != "<<<<<<< local\nL\n||||||| base\n=======\nR\n>>>>>>> remote\n" {
		t.Fatalf("no-newline markers %q", m)
	}
}

func TestHasMarkers(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		{"", false},
		{"plain\ntext\n", false},
		{"<<<<<<< local\n", true},
		// Exactly seven marker characters, alone or followed by a space:
		// longer runs (a Markdown setext underline, an ASCII rule) are content.
		{"Title\n========\n", false},
		{"<<<<<<<< x\n", false},
		{">>>>>>>>", false},
		{"=======x", false},
		{"<<<<<<<\tx\n", false},
		{"=======\r\n", true},
		{"<<<<<<< local\r\nx\r\n", true},
		{"a\n<<<<<<<\nb\n", true},
		{"a\n<<<<<<< x\nb\n", true},
		{"a\n||||||| base\n", true},
		{"a\n=======\nb", true},
		{">>>>>>> remote", true},
		{"|||||||", true},
		{" <<<<<<< indented", false},
		{"<<<<<< six", false},
		{"x <<<<<<< y\n", false},
		{"a\r\n=======\r\nb\r\n", true},
		{"======", false},
	}
	for _, tc := range tests {
		if got := HasMarkers([]byte(tc.in)); got != tc.want {
			t.Errorf("HasMarkers(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestLineDiff(t *testing.T) {
	op := func(k byte, s string) DiffOp { return DiffOp{k, s} }
	tests := []struct {
		name string
		a, b string
		want []DiffOp
	}{
		{"both empty", "", "", []DiffOp{}},
		{"identical", "a\nb\n", "a\nb\n", []DiffOp{op(' ', "a"), op(' ', "b")}},
		{"change middle", "a\nb\nc\n", "a\nB\nc\n", []DiffOp{op(' ', "a"), op('-', "b"), op('+', "B"), op(' ', "c")}},
		{"insert", "a\nc\n", "a\nb\nc\n", []DiffOp{op(' ', "a"), op('+', "b"), op(' ', "c")}},
		{"delete", "a\nb\nc\n", "a\nc\n", []DiffOp{op(' ', "a"), op('-', "b"), op(' ', "c")}},
		{"all added", "", "x\ny\n", []DiffOp{op('+', "x"), op('+', "y")}},
		{"all removed", "x\ny", "", []DiffOp{op('-', "x"), op('-', "y")}},
		{"replace all", "x\n", "y\n", []DiffOp{op('-', "x"), op('+', "y")}},
		{"CRLF equals LF", "a\r\nb\r\n", "a\nb\n", []DiffOp{op(' ', "a"), op(' ', "b")}},
		{"append after last line without newline", "a", "a\nb", []DiffOp{op('-', "a"), op('+', "a"), op('+', "b")}},
		{"trailing insert", "a\n", "a\nb\n", []DiffOp{op(' ', "a"), op('+', "b")}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := LineDiff([]byte(tc.a), []byte(tc.b))
			if len(got) != len(tc.want) {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("op %d: got %+v, want %+v (all: %+v)", i, got[i], tc.want[i], got)
				}
			}
			// Reconstruct both sides from the ops.
			var ra, rb []string
			for _, o := range got {
				if o.Kind != '+' {
					ra = append(ra, o.Text)
				}
				if o.Kind != '-' {
					rb = append(rb, o.Text)
				}
			}
			if strings.Join(ra, "|") != strings.Join(lineTexts(tc.a), "|") || strings.Join(rb, "|") != strings.Join(lineTexts(tc.b), "|") {
				t.Fatalf("ops do not reconstruct sides: %+v", got)
			}
		})
	}
}

func lineTexts(s string) []string {
	var out []string
	for _, l := range newLineSet([]byte(s)).lines {
		out = append(out, lineText(l))
	}
	return out
}

// TestCRLFSeparatorsAndMarkers checks that the separators Resolve and
// RenderMarkers synthesise (after an unterminated side or Custom text, and
// for the marker lines themselves) follow the file's line-ending convention
// instead of introducing a bare '\n' into a CRLF file.
func TestCRLFSeparatorsAndMarkers(t *testing.T) {
	r := ThreeWay("f.txt", []byte("a\r\nb\r\nc\r\n"), []byte("a\r\nX\r\nc\r\n"), []byte("a\r\nY\r\nc\r\n"))
	if r.Clean || len(r.Hunks) != 1 {
		t.Fatalf("unexpected: %+v", r)
	}
	got, err := Resolve(r, []Choice{{Side: SideCustom, Custom: []byte("Z")}})
	if err != nil || string(got) != "a\r\nZ\r\nc\r\n" {
		t.Fatalf("custom in CRLF file: %q %v", got, err)
	}
	m := RenderMarkers(r, "", "")
	want := "a\r\n<<<<<<< local\r\nX\r\n||||||| base\r\nb\r\n=======\r\nY\r\n>>>>>>> remote\r\nc\r\n"
	if string(m) != want {
		t.Fatalf("CRLF markers %q, want %q", m, want)
	}
	if !HasMarkers(m) {
		t.Fatal("HasMarkers false on CRLF markers")
	}

	// Conflict at the start of the file: the hunk's own lines set the convention.
	r2 := ThreeWay("f.txt", []byte("a\r\nb\r\n"), []byte("X\r\nb\r\n"), []byte("Y\r\nb\r\n"))
	m2 := RenderMarkers(r2, "L", "R")
	want2 := "<<<<<<< L\r\nX\r\n||||||| base\r\na\r\n=======\r\nY\r\n>>>>>>> R\r\nb\r\n"
	if string(m2) != want2 {
		t.Fatalf("leading CRLF markers %q, want %q", m2, want2)
	}
	got, err = Resolve(r2, []Choice{{Side: SideCustom, Custom: []byte("Z")}})
	if err != nil || string(got) != "Z\r\nb\r\n" {
		t.Fatalf("custom before CRLF segment: %q %v", got, err)
	}

	// Unterminated sides at the end of a CRLF file.
	r3 := ThreeWay("f.txt", []byte("a\r\nb"), []byte("a\r\nB"), []byte("a\r\nb\r\nc\r\n"))
	if r3.Clean || len(r3.Hunks) != 1 || string(r3.Hunks[0].Local) != "B" {
		t.Fatalf("unexpected: %+v", r3)
	}
	got, err = Resolve(r3, []Choice{{Side: SideLocal}})
	if err != nil || string(got) != "a\r\nB" {
		t.Fatalf("unterminated local kept as is: %q %v", got, err)
	}
	m3 := RenderMarkers(r3, "", "")
	want3 := "a\r\n<<<<<<< local\r\nB\r\n||||||| base\r\nb\r\n=======\r\nb\r\nc\r\n>>>>>>> remote\r\n"
	if string(m3) != want3 {
		t.Fatalf("trailing CRLF markers %q, want %q", m3, want3)
	}

	// LF files and files without any terminator are unchanged.
	r4 := ThreeWay("f.txt", []byte("a\nb\nc\n"), []byte("a\nX\nc\n"), []byte("a\nY\nc\n"))
	got, err = Resolve(r4, []Choice{{Side: SideCustom, Custom: []byte("Z")}})
	if err != nil || string(got) != "a\nZ\nc\n" {
		t.Fatalf("custom in LF file: %q %v", got, err)
	}
	r5 := ThreeWay("f.txt", []byte("a"), []byte("b"), []byte("c"))
	got, err = Resolve(r5, []Choice{{Side: SideCustom, Custom: []byte("Z")}})
	if err != nil || string(got) != "Z" {
		t.Fatalf("custom in one-line file: %q %v", got, err)
	}
	if m5 := RenderMarkers(r5, "", ""); string(m5) != "<<<<<<< local\nb\n||||||| base\na\n=======\nc\n>>>>>>> remote\n" {
		t.Fatalf("one-line markers %q", m5)
	}
}

func TestEOLHelpers(t *testing.T) {
	tests := []struct {
		dst, next string
		want      string
	}{
		{"", "", "\n"},
		{"a", "", "\n"},
		{"a\n", "b\r\n", "\n"},
		{"a\r\nb", "c\n", "\r\n"},
		{"a", "b\r\nc\n", "\r\n"},
		{"a", "b\nc\r\n", "\n"},
		{"\r\n", "", "\r\n"},
		{"\n", "", "\n"},
	}
	for _, tc := range tests {
		if got := string(eolFor([]byte(tc.dst), []byte(tc.next))); got != tc.want {
			t.Errorf("eolFor(%q, %q) = %q, want %q", tc.dst, tc.next, got, tc.want)
		}
	}
	if got := string(appendPart([]byte("a\r\nb"), []byte("c\n"))); got != "a\r\nb\r\nc\n" {
		t.Errorf("appendPart CRLF context: %q", got)
	}
	if got := string(appendPart([]byte("a"), []byte("c\r\n"))); got != "a\r\nc\r\n" {
		t.Errorf("appendPart part context: %q", got)
	}
	if got := string(appendPart([]byte("a\n"), nil)); got != "a\n" {
		t.Errorf("appendPart nil part: %q", got)
	}
}

func TestKindFor(t *testing.T) {
	tests := []struct {
		path    string
		content string
		want    Kind
	}{
		{".env", "A=1\n", KindDotenv},
		{"app/.env", "A=1\n", KindDotenv},
		{"config/.env.local", "", KindDotenv},
		{".env.production", "A=1", KindDotenv},
		{"prod.env", "A=1\n", KindDotenv},
		{"deep/dir/staging.env", "A=1\n", KindDotenv},
		{".envrc", "export A=1\n", KindText},
		{"env", "A=1\n", KindText},
		{"x.env.bak", "A=1\n", KindText},
		{"environment.yaml", "a: 1\n", KindText},
		{"notes.txt", "hello\n", KindText},
		{"", "", KindText},
		{"image.png", "\x89PNG\x00", KindBinary},
		{".env", "A=\x00\n", KindBinary},
		{"t.txt", "bad \xff utf8", KindBinary},
		{"t.txt", "héllo ✓\n", KindText},
	}
	for _, tc := range tests {
		if got := KindFor(tc.path, []byte(tc.content)); got != tc.want {
			t.Errorf("KindFor(%q, %q) = %v, want %v", tc.path, tc.content, got, tc.want)
		}
	}
	for _, k := range []Kind{KindText, KindDotenv, KindBinary, Kind(7)} {
		if k.String() == "" {
			t.Errorf("empty String for %d", int(k))
		}
	}
	for _, s := range []Side{SideLocal, SideRemote, SideBase, SideCustom, Side(9)} {
		if s.String() == "" {
			t.Errorf("empty String for side %d", int(s))
		}
	}
}

func TestPerformanceLargeMerge(t *testing.T) {
	const n = 20000
	var bb, lb, rb bytes.Buffer
	inserts := 0
	for i := 0; i < n; i++ {
		line := fmt.Sprintf("line %d: some configuration value = %d\n", i, i*7)
		bb.WriteString(line)
		if i%50 == 0 {
			lb.WriteString(fmt.Sprintf("line %d: locally edited value = %d\n", i, i*11))
		} else {
			lb.WriteString(line)
		}
		rb.WriteString(line)
		if i%70 == 35 {
			rb.WriteString(fmt.Sprintf("inserted by remote after %d\n", i))
			inserts++
		}
	}
	start := time.Now()
	r := ThreeWay("big.conf", bb.Bytes(), lb.Bytes(), rb.Bytes())
	elapsed := time.Since(start)
	if elapsed > 2*time.Second {
		t.Fatalf("ThreeWay took %v", elapsed)
	}
	if !r.Clean {
		t.Fatalf("expected clean merge, got %d hunks", len(r.Hunks))
	}
	if got := countLines(r.Merged); got != n+inserts {
		t.Fatalf("merged has %d lines, want %d", got, n+inserts)
	}
	if !bytes.Contains(r.Merged, []byte("line 50: locally edited")) || !bytes.Contains(r.Merged, []byte("inserted by remote after 35\n")) {
		t.Fatal("merged output misses a side's change")
	}
	start = time.Now()
	ops := LineDiff(bb.Bytes(), lb.Bytes())
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("LineDiff took %v", elapsed)
	}
	changed := 0
	for _, o := range ops {
		if o.Kind == '-' {
			changed++
		}
	}
	if changed != n/50 {
		t.Fatalf("LineDiff found %d removed lines, want %d", changed, n/50)
	}
	// Completely different large files must still finish quickly.
	var xb bytes.Buffer
	for i := 0; i < n; i++ {
		xb.WriteString(fmt.Sprintf("totally different %d\n", i))
	}
	start = time.Now()
	r = ThreeWay("big.conf", bb.Bytes(), lb.Bytes(), xb.Bytes())
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("disjoint ThreeWay took %v", elapsed)
	}
	if r.Clean {
		t.Fatal("disjoint merge should conflict")
	}
}

func TestMergedBytesAreCopies(t *testing.T) {
	local := []byte("a\nb\n")
	r := ThreeWay("f.txt", []byte("a\n"), local, []byte("a\n"))
	local[0] = 'z'
	if string(r.Merged) != "a\nb\n" {
		t.Fatalf("Merged aliases input: %q", r.Merged)
	}
	remote := []byte("a\nc\n")
	r = ThreeWay("f.txt", []byte("a\n"), []byte("a\n"), remote)
	remote[2] = 'z'
	if string(r.Merged) != "a\nc\n" {
		t.Fatalf("Merged aliases remote input: %q", r.Merged)
	}
}
