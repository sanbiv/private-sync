package merge

import (
	"bytes"
	"strings"
	"testing"
)

const pemValue = "-----BEGIN PRIVATE KEY-----\nMIIEvQIBADANBg\nkqhkiG9w0BAQEF\n-----END PRIVATE KEY-----"

type wantItem struct {
	kind envItemKind
	key  string
	raw  string
}

func TestParseDotenv(t *testing.T) {
	tests := []struct {
		name    string
		src     string
		want    []wantItem
		wantErr string
	}{
		{name: "empty", src: "", want: nil},
		{
			name: "simple",
			src:  "A=1\nB=two words\n",
			want: []wantItem{{envEntry, "A", "A=1\n"}, {envEntry, "B", "B=two words\n"}},
		},
		{
			name: "no trailing newline",
			src:  "A=1\nB=2",
			want: []wantItem{{envEntry, "A", "A=1\n"}, {envEntry, "B", "B=2"}},
		},
		{
			name: "export and spaces around equals",
			src:  "export FOO = bar\n  export\tBAZ=qux\n",
			want: []wantItem{{envEntry, "FOO", "export FOO = bar\n"}, {envEntry, "BAZ", "  export\tBAZ=qux\n"}},
		},
		{
			name: "comments and blanks preserved",
			src:  "# top\n\nA=1\n   \n  # indented comment\nB=2\n",
			want: []wantItem{
				{envComment, "", "# top\n"}, {envBlank, "", "\n"}, {envEntry, "A", "A=1\n"},
				{envBlank, "", "   \n"}, {envComment, "", "  # indented comment\n"}, {envEntry, "B", "B=2\n"},
			},
		},
		{
			name: "quoted values with hash and equals",
			src:  "A=\"x # not a comment\"\nB='y=z' # comment\nC=plain=eq\n",
			want: []wantItem{
				{envEntry, "A", "A=\"x # not a comment\"\n"},
				{envEntry, "B", "B='y=z' # comment\n"},
				{envEntry, "C", "C=plain=eq\n"},
			},
		},
		{
			name: "escaped quote inside value",
			src:  "A=\"say \\\"hi\\\"\"\nB=2\n",
			want: []wantItem{{envEntry, "A", "A=\"say \\\"hi\\\"\"\n"}, {envEntry, "B", "B=2\n"}},
		},
		{
			name: "empty quoted value",
			src:  "A=\"\"\nB=''\n",
			want: []wantItem{{envEntry, "A", "A=\"\"\n"}, {envEntry, "B", "B=''\n"}},
		},
		{
			name: "multi-line PEM",
			src:  "KEY=\"" + pemValue + "\"\nNEXT=1\n",
			want: []wantItem{{envEntry, "KEY", "KEY=\"" + pemValue + "\"\n"}, {envEntry, "NEXT", "NEXT=1\n"}},
		},
		{
			name: "multi-line single quoted at EOF without newline",
			src:  "A=1\nB='line1\nline2'",
			want: []wantItem{{envEntry, "A", "A=1\n"}, {envEntry, "B", "B='line1\nline2'"}},
		},
		{
			name: "CRLF",
			src:  "A=1\r\n# c\r\n\r\nB=\"x\r\ny\"\r\n",
			want: []wantItem{{envEntry, "A", "A=1\r\n"}, {envComment, "", "# c\r\n"}, {envBlank, "", "\r\n"}, {envEntry, "B", "B=\"x\r\ny\"\r\n"}},
		},
		{
			name: "key charset",
			src:  "_a.b-c9=1\n",
			want: []wantItem{{envEntry, "_a.b-c9", "_a.b-c9=1\n"}},
		},
		{
			name: "empty value",
			src:  "A=\n",
			want: []wantItem{{envEntry, "A", "A=\n"}},
		},
		{
			// Per the grammar the value is everything after '=': one that
			// starts with whitespace is plain text, so no quote handling.
			name: "space before quote is a plain value",
			src:  "A= \"x\"\nB= 'y\n",
			want: []wantItem{{envEntry, "A", "A= \"x\"\n"}, {envEntry, "B", "B= 'y\n"}},
		},
		{name: "space before quote does not open a multi-line value", src: "A= \"x\ny\"\n", wantErr: "line 2: not a dotenv line"},
		{
			name: "backslash is literal in single quotes",
			src:  "A='C:\\'\nB='a\\\nb'\n",
			want: []wantItem{{envEntry, "A", "A='C:\\'\n"}, {envEntry, "B", "B='a\\\nb'\n"}},
		},
		{name: "backslash-quote in single quotes ends the value", src: "A='it\\'s'\n", wantErr: "line 1: text after closing ' quote in A"},
		{name: "text after closing double quote", src: "A=\"x\"y\n", wantErr: "line 1: text after closing \" quote in A"},
		{name: "second quoted word after closing quote", src: "A=\"x\" \"y\"\n", wantErr: "line 1: text after closing \" quote in A"},
		{
			name: "comment after closing quote",
			src:  "A=\"x\" # c\nB='y'#c\nC=\"m\nn\"  # c\nD=\"\"\t\n",
			want: []wantItem{
				{envEntry, "A", "A=\"x\" # c\n"}, {envEntry, "B", "B='y'#c\n"},
				{envEntry, "C", "C=\"m\nn\"  # c\n"}, {envEntry, "D", "D=\"\"\t\n"},
			},
		},
		{name: "unbalanced quote does not swallow the next entry", src: "A=\"open\nB=\"x\"\n", wantErr: "line 2: text after closing \" quote in A"},
		{name: "text after multi-line closing quote", src: "K=\"a\nb\" c\nN=1\n", wantErr: "line 2: text after closing \" quote in K"},
		{name: "text after closing quote at EOF", src: "K=\"a\nb\nc\"x", wantErr: "line 3: text after closing \" quote in K"},
		{
			name: "closing quote at the start of a CRLF line",
			src:  "A=\"x\r\n\"\r\nB=1\r\n",
			want: []wantItem{{envEntry, "A", "A=\"x\r\n\"\r\n"}, {envEntry, "B", "B=1\r\n"}},
		},
		{name: "duplicate after multi-line with trailing comment", src: "K=\"a\nb\" # c\nK=2\n", wantErr: "line 3: duplicate key K"},
		{name: "duplicate key", src: "A=1\nB=2\nA=3\n", wantErr: "line 3: duplicate key A"},
		{name: "invalid line", src: "A=1\nthis is not an entry\n", wantErr: "line 2: not a dotenv line"},
		{name: "key starting with digit", src: "1A=1\n", wantErr: "line 1: not a dotenv line"},
		{name: "unbalanced double quote", src: "A=\"open\nB=2\n", wantErr: "line 1: unbalanced \" quote in A"},
		{name: "unbalanced single quote at EOF", src: "A='x", wantErr: "unbalanced ' quote in A"},
		{name: "escaped closing quote only", src: "A=\"x\\\"\n", wantErr: "unbalanced"},
		{name: "duplicate after multi-line", src: "K=\"a\nb\"\nK=2\n", wantErr: "line 3: duplicate key K"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f, err := parseDotenv([]byte(tc.src))
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got items %+v", tc.wantErr, f.items)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error %q does not contain %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(f.items) != len(tc.want) {
				t.Fatalf("got %d items, want %d: %+v", len(f.items), len(tc.want), f.items)
			}
			for i, w := range tc.want {
				it := f.items[i]
				if it.kind != w.kind || it.key != w.key || string(it.raw) != w.raw {
					t.Errorf("item %d: got {%d %q %q}, want {%d %q %q}", i, it.kind, it.key, it.raw, w.kind, w.key, w.raw)
				}
			}
			// Raw items must round-trip to the source.
			var joined []byte
			for _, it := range f.items {
				joined = append(joined, it.raw...)
			}
			if string(joined) != tc.src {
				t.Fatalf("items do not round-trip: %q", joined)
			}
			for k, i := range f.index {
				if f.items[i].key != k {
					t.Fatalf("index %q → item %d with key %q", k, i, f.items[i].key)
				}
			}
		})
	}
}

type wantHunk struct {
	key                 string
	base, local, remote *string // nil = absent
}

func str(s string) *string { return &s }

func checkHunks(t *testing.T, got []Hunk, want []wantHunk) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d hunks, want %d: %+v", len(got), len(want), got)
	}
	cmp := func(i int, side string, g []byte, w *string) {
		t.Helper()
		if w == nil {
			if g != nil {
				t.Errorf("hunk %d %s: got %q, want absent", i, side, g)
			}
			return
		}
		if g == nil || string(g) != *w {
			t.Errorf("hunk %d %s: got %q, want %q", i, side, g, *w)
		}
	}
	for i, w := range want {
		if got[i].Key != w.key {
			t.Errorf("hunk %d key: got %q, want %q", i, got[i].Key, w.key)
		}
		cmp(i, "base", got[i].Base, w.base)
		cmp(i, "local", got[i].Local, w.local)
		cmp(i, "remote", got[i].Remote, w.remote)
	}
}

func TestDotenvThreeWay(t *testing.T) {
	tests := []struct {
		name                string
		base, local, remote string
		wantKind            Kind
		wantClean           bool
		wantMerged          string
		wantHunks           []wantHunk
		wantNote            string
	}{
		{
			name: "no changes",
			base: "A=1\nB=2\n", local: "A=1\nB=2\n", remote: "A=1\nB=2\n",
			wantKind: KindDotenv, wantClean: true, wantMerged: "A=1\nB=2\n",
		},
		{
			name: "local changes A, remote changes B",
			base: "A=1\nB=2\n", local: "A=10\nB=2\n", remote: "A=1\nB=20\n",
			wantKind: KindDotenv, wantClean: true, wantMerged: "A=10\nB=20\n",
		},
		{
			name: "remote removes key",
			base: "A=1\nB=2\nC=3\n", local: "A=1\nB=2\nC=3\n", remote: "A=1\nC=3\n",
			wantKind: KindDotenv, wantClean: true, wantMerged: "A=1\nC=3\n",
		},
		{
			name: "local removes key, remote unchanged",
			base: "A=1\nB=2\nC=3\n", local: "A=1\nC=3\n", remote: "A=1\nB=2\nC=3\n",
			wantKind: KindDotenv, wantClean: true, wantMerged: "A=1\nC=3\n",
		},
		{
			name: "both remove same key",
			base: "A=1\nB=2\n", local: "A=1\n", remote: "A=1\n",
			wantKind: KindDotenv, wantClean: true, wantMerged: "A=1\n",
		},
		{
			name: "both change same key same value",
			base: "A=1\n", local: "A=2\n", remote: "A=2\n",
			wantKind: KindDotenv, wantClean: true, wantMerged: "A=2\n",
		},
		{
			name: "both change same key differently",
			base: "A=1\nB=2\n", local: "A=2\nB=2\n", remote: "A=3\nB=2\n",
			wantKind:  KindDotenv,
			wantHunks: []wantHunk{{"A", str("A=1\n"), str("A=2\n"), str("A=3\n")}},
		},
		{
			name: "local removed, remote edited: conflict appended at end",
			base: "A=1\nB=2\n", local: "B=2\n", remote: "A=9\nB=2\n",
			wantKind:  KindDotenv,
			wantHunks: []wantHunk{{"A", str("A=1\n"), nil, str("A=9\n")}},
		},
		{
			name: "local edited, remote removed: conflict in place",
			base: "A=1\nB=2\n", local: "A=9\nB=2\n", remote: "B=2\n",
			wantKind:  KindDotenv,
			wantHunks: []wantHunk{{"A", str("A=1\n"), str("A=9\n"), nil}},
		},
		{
			name: "both add same key same value",
			base: "A=1\n", local: "A=1\nN=x\n", remote: "A=1\nN=x\n",
			wantKind: KindDotenv, wantClean: true, wantMerged: "A=1\nN=x\n",
		},
		{
			name: "both add same key different value",
			base: "A=1\n", local: "A=1\nN=x\n", remote: "A=1\nN=y\n",
			wantKind:  KindDotenv,
			wantHunks: []wantHunk{{"N", nil, str("N=x\n"), str("N=y\n")}},
		},
		{
			name: "remote adds keys: appended at end in remote order",
			base: "A=1\n", local: "A=1\n", remote: "Z=26\nA=1\nY=25\n",
			wantKind: KindDotenv, wantClean: true, wantMerged: "A=1\nZ=26\nY=25\n",
		},
		{
			name: "remote adds key, local lacks trailing newline",
			base: "A=1", local: "A=2", remote: "A=1\nB=2\n",
			wantKind: KindDotenv, wantClean: true, wantMerged: "A=2\nB=2\n",
		},
		{
			name: "local order and comments kept, remote value replaced in place",
			base: "A=1\nB=2\n", local: "# header\nB=2\n\n# a\nA=1\n", remote: "A=100\nB=2\n",
			wantKind: KindDotenv, wantClean: true, wantMerged: "# header\nB=2\n\n# a\nA=100\n",
		},
		{
			name: "quoting change is a change",
			base: "A=1\n", local: "A=\"1\"\n", remote: "A=1\n",
			wantKind: KindDotenv, wantClean: true, wantMerged: "A=\"1\"\n",
		},
		{
			name: "quoting change on both sides differently",
			base: "A=1\n", local: "A=\"1\"\n", remote: "A='1'\n",
			wantKind:  KindDotenv,
			wantHunks: []wantHunk{{"A", str("A=1\n"), str("A=\"1\"\n"), str("A='1'\n")}},
		},
		{
			name: "export prefix change is a change",
			base: "A=1\n", local: "A=1\n", remote: "export A=1\n",
			wantKind: KindDotenv, wantClean: true, wantMerged: "export A=1\n",
		},
		{
			name: "multi-line value changed on remote",
			base: "K=\"" + pemValue + "\"\nA=1\n", local: "K=\"" + pemValue + "\"\nA=2\n", remote: "K=\"new\nkey\"\nA=1\n",
			wantKind: KindDotenv, wantClean: true, wantMerged: "K=\"new\nkey\"\nA=2\n",
		},
		{
			name: "multiple conflicts in local order",
			base: "A=1\nB=2\nC=3\n", local: "C=30\nB=2\nA=10\n", remote: "A=11\nB=2\nC=31\n",
			wantKind: KindDotenv,
			wantHunks: []wantHunk{
				{"C", str("C=3\n"), str("C=30\n"), str("C=31\n")},
				{"A", str("A=1\n"), str("A=10\n"), str("A=11\n")},
			},
		},
		{
			name: "duplicate key on remote falls back to text merge",
			base: "A=1\nB=2\n", local: "A=10\nB=2\n", remote: "A=1\nB=2\nB=3\n",
			wantKind: KindText, wantClean: true, wantMerged: "A=10\nB=2\nB=3\n",
			wantNote: "dotenv parse failed (remote: line 3: duplicate key B): used text merge",
		},
		{
			name: "invalid line on base falls back to text merge",
			base: "A=1\ngarbage\n", local: "A=2\ngarbage\n", remote: "A=1\ngarbage\n",
			wantKind: KindText, wantClean: true, wantMerged: "A=2\ngarbage\n",
			wantNote: "dotenv parse failed (base: line 2: not a dotenv line): used text merge",
		},
		{
			name: "unbalanced quote on local falls back to text merge with conflict",
			base: "A=1\n", local: "A=\"1\n", remote: "A=2\n",
			wantKind:  KindText,
			wantHunks: []wantHunk{{"", str("A=1\n"), str("A=\"1\n"), str("A=2\n")}},
			wantNote:  "dotenv parse failed (local: line 1: unbalanced \" quote in A): used text merge",
		},
		{
			name: "empty base, both add different keys",
			base: "", local: "A=1\n", remote: "B=2\n",
			wantKind: KindDotenv, wantClean: true, wantMerged: "A=1\nB=2\n",
		},
		{
			name: "single-quoted windows path merges as dotenv",
			base: "P='C:\\'\nA=1\n", local: "P='C:\\'\nA=2\n", remote: "P='D:\\'\nA=1\n",
			wantKind: KindDotenv, wantClean: true, wantMerged: "P='D:\\'\nA=2\n",
		},
		{
			name: "text after closing quote falls back to text merge",
			base: "A=\"1\"x\nB=2\n", local: "A=\"1\"x\nB=3\n", remote: "A=\"1\"x\nB=2\n",
			wantKind: KindText, wantClean: true, wantMerged: "A=\"1\"x\nB=3\n",
			wantNote: "dotenv parse failed (base: line 1: text after closing \" quote in A): used text merge",
		},
		{
			name: "unbalanced quote before another quoted entry falls back to text merge",
			base: "A=\"open\nB=\"x\"\n", local: "A=\"open\nB=\"y\"\n", remote: "A=\"open\nB=\"x\"\n",
			wantKind: KindText, wantClean: true, wantMerged: "A=\"open\nB=\"y\"\n",
			wantNote: "dotenv parse failed (base: line 2: text after closing \" quote in A): used text merge",
		},
		{
			name: "space before quote is a plain single-line value",
			base: "A= \"1\"\nB=1\n", local: "A= \"2\"\nB=1\n", remote: "A= \"1\"\nB=2\n",
			wantKind: KindDotenv, wantClean: true, wantMerged: "A= \"2\"\nB=2\n",
		},
		{
			name: "everything removed on remote",
			base: "A=1\nB=2\n", local: "A=1\nB=2\n", remote: "",
			wantKind: KindDotenv, wantClean: true, wantMerged: "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := ThreeWay("app/.env", []byte(tc.base), []byte(tc.local), []byte(tc.remote))
			if r.Kind != tc.wantKind {
				t.Fatalf("kind %v, want %v", r.Kind, tc.wantKind)
			}
			if r.Clean != tc.wantClean {
				t.Fatalf("clean %v, want %v (hunks %+v, merged %q)", r.Clean, tc.wantClean, r.Hunks, r.Merged)
			}
			if tc.wantClean {
				if string(r.Merged) != tc.wantMerged {
					t.Fatalf("merged %q, want %q", r.Merged, tc.wantMerged)
				}
				if len(r.Hunks) != 0 {
					t.Fatalf("clean result has hunks: %+v", r.Hunks)
				}
			} else {
				if r.Merged != nil {
					t.Fatalf("conflicted result has Merged %q", r.Merged)
				}
				checkHunks(t, r.Hunks, tc.wantHunks)
			}
			if r.Note != tc.wantNote {
				t.Fatalf("note %q, want %q", r.Note, tc.wantNote)
			}
		})
	}
}

func TestDotenvResolve(t *testing.T) {
	base := "# cfg\nA=1\nB=2\nC=3\n"
	local := "# cfg\nA=10\nB=2\nC=3\n"
	remote := "# cfg\nA=11\nB=2\nD=4\n"
	r := ThreeWay(".env", []byte(base), []byte(local), []byte(remote))
	if r.Clean || len(r.Hunks) != 1 || r.Hunks[0].Key != "A" {
		t.Fatalf("unexpected result: %+v", r)
	}
	tests := []struct {
		name   string
		choice Choice
		want   string
	}{
		{"local", Choice{Side: SideLocal}, "# cfg\nA=10\nB=2\nD=4\n"},
		{"remote", Choice{Side: SideRemote}, "# cfg\nA=11\nB=2\nD=4\n"},
		{"base", Choice{Side: SideBase}, "# cfg\nA=1\nB=2\nD=4\n"},
		{"custom with newline", Choice{Side: SideCustom, Custom: []byte("A=42\n")}, "# cfg\nA=42\nB=2\nD=4\n"},
		{"custom without newline", Choice{Side: SideCustom, Custom: []byte("A=42")}, "# cfg\nA=42\nB=2\nD=4\n"},
		{"custom nil omits the key", Choice{Side: SideCustom}, "# cfg\nB=2\nD=4\n"},
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
			if HasMarkers(got) {
				t.Fatalf("resolved output has markers: %q", got)
			}
		})
	}

	// Removal-vs-edit conflict: choosing local (absent) drops the key.
	r2 := ThreeWay(".env", []byte("A=1\nB=2\n"), []byte("B=2\n"), []byte("A=9\nB=2\n"))
	got, err := Resolve(r2, []Choice{{Side: SideLocal}})
	if err != nil || string(got) != "B=2\n" {
		t.Fatalf("local (absent): got %q, %v", got, err)
	}
	got, err = Resolve(r2, []Choice{{Side: SideRemote}})
	if err != nil || string(got) != "B=2\nA=9\n" {
		t.Fatalf("remote: got %q, %v", got, err)
	}
	if _, err := Resolve(r2, nil); err == nil {
		t.Fatal("expected error for missing choices")
	}
}

func TestDotenvRenderMarkers(t *testing.T) {
	base := "A=1\nB=2\nC=3\n"
	local := "A=10\nB=2\nC=30\n"
	remote := "A=11\nB=2\nC=31\n"
	r := ThreeWay("prod.env", []byte(base), []byte(local), []byte(remote))
	if r.Clean || len(r.Hunks) != 2 {
		t.Fatalf("unexpected result: %+v", r)
	}
	got := RenderMarkers(r, "mine", "theirs")
	want := "<<<<<<< mine\nA=10\n||||||| base\nA=1\n=======\nA=11\n>>>>>>> theirs\n" +
		"B=2\n" +
		"<<<<<<< mine\nC=30\n||||||| base\nC=3\n=======\nC=31\n>>>>>>> theirs\n"
	if string(got) != want {
		t.Fatalf("markers:\n%s\nwant:\n%s", got, want)
	}
	if !HasMarkers(got) {
		t.Fatal("HasMarkers false on rendered markers")
	}
	// Default labels.
	def := RenderMarkers(r, "", "")
	if !bytes.HasPrefix(def, []byte("<<<<<<< local\n")) || !bytes.Contains(def, []byte(">>>>>>> remote\n")) {
		t.Fatalf("default labels missing: %q", def)
	}
	res, err := Resolve(r, []Choice{{Side: SideLocal}, {Side: SideRemote}})
	if err != nil {
		t.Fatal(err)
	}
	if string(res) != "A=10\nB=2\nC=31\n" || HasMarkers(res) {
		t.Fatalf("resolve: %q", res)
	}
}

func TestDotenvHunkBytesAreCopies(t *testing.T) {
	local := []byte("A=2\n")
	r := ThreeWay(".env", []byte("A=1\n"), local, []byte("A=3\n"))
	local[2] = '9'
	if string(r.Hunks[0].Local) != "A=2\n" {
		t.Fatalf("hunk aliases caller input: %q", r.Hunks[0].Local)
	}
}
