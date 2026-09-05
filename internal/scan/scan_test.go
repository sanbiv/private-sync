package scan

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/sanbiv/private-sync/internal/execx"
)

// writeFile creates rel (slash-separated) under root with content.
func writeFile(t *testing.T, root, rel string, content []byte) string {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, content, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func paths(cs []Candidate) []string {
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		out = append(out, c.Path)
	}
	return out
}

func find(cs []Candidate, p string) (Candidate, bool) {
	for _, c := range cs {
		if c.Path == p {
			return c, true
		}
	}
	return Candidate{}, false
}

// noGit is a runner that behaves like a machine without a git binary.
func noGit() execx.Runner {
	return execx.Fake(func(c execx.Cmd) (execx.Result, error) {
		return execx.Result{ExitCode: -1}, errors.New("run git: executable file not found in $PATH")
	})
}

// fakeGit emulates the two git calls made by Scan (ls-files, check-ignore).
// tracked/ignored are relpaths; when inTree is false ls-files exits 128 the
// way real git does outside a work tree.
type fakeGit struct {
	t        *testing.T
	tracked  []string
	ignored  []string
	inTree   bool
	calls    []string
	stdin    []byte
	failWith int // if != 0, check-ignore exits with this code
}

func (f *fakeGit) runner() execx.Runner {
	return execx.Fake(func(c execx.Cmd) (execx.Result, error) {
		if c.Name != "git" {
			f.t.Fatalf("unexpected command %q", c.Name)
		}
		if len(c.Args) < 3 || c.Args[0] != "-C" {
			f.t.Fatalf("expected git -C <dir> <sub>, got %v", c.Args)
		}
		sub := c.Args[2]
		f.calls = append(f.calls, sub)
		switch sub {
		case "ls-files":
			if !contains(c.Args, "-z") {
				f.t.Fatalf("ls-files without -z: %v", c.Args)
			}
			if !f.inTree {
				return execx.Result{ExitCode: 128, Stderr: []byte("fatal: not a git repository")}, nil
			}
			return execx.Result{Stdout: nul(f.tracked)}, nil
		case "check-ignore":
			for _, want := range []string{"-z", "--stdin", "--no-index"} {
				if !contains(c.Args, want) {
					f.t.Fatalf("check-ignore missing %s: %v", want, c.Args)
				}
			}
			f.stdin = c.Stdin
			if f.failWith != 0 {
				return execx.Result{ExitCode: f.failWith, Stderr: []byte("fatal: boom")}, nil
			}
			given := map[string]bool{}
			for _, p := range strings.Split(string(c.Stdin), "\x00") {
				if p != "" {
					given[p] = true
				}
			}
			var out []string
			for _, p := range f.ignored {
				if given[p] {
					out = append(out, p)
				}
			}
			if len(out) == 0 {
				return execx.Result{ExitCode: 1}, nil
			}
			return execx.Result{Stdout: nul(out)}, nil
		}
		f.t.Fatalf("unexpected git subcommand %q", sub)
		return execx.Result{}, nil
	})
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

func nul(ss []string) []byte {
	var b bytes.Buffer
	for _, s := range ss {
		b.WriteString(s)
		b.WriteByte(0)
	}
	return b.Bytes()
}

func TestIsSecretName(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"secrets.go", false},
		{".env.local", true},
		{"id_rsa.pem", true},
		{"useSecretStore.ts", false},
		{"terraform.tfvars", true},
		{".env", true},
		{"prod.env", true},
		{".netrc", true},
		{".npmrc", true},
		{".pypirc", true},
		{".htpasswd", true},
		{"server.key", true},
		{"cert.p12", true},
		{"cert.pfx", true},
		{"keystore.jks", true},
		{"release.keystore", true},
		{"my-secret.yaml", true},
		{"MY-SECRET.YAML", true},
		{"credentials.json", true},
		{"Credentials.java", false},
		{"settings.local.json", true},
		{"config.local.ts", false},
		{"secret_service.py", false},
		{"secret.rs", false},
		{"SecretView.swift", false},
		{"secret.vue", false},
		{"secret.svelte", false},
		{"secrets.txt", true},
		{"secret", true},
		{"config.yaml", false},
		{"main.go", false},
		{"README.md", false},
		{"", false},
		{"sub/dir/.env.production", true},
		{"src/secrets.go", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsSecretName(tc.name); got != tc.want {
				t.Errorf("IsSecretName(%q) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

func TestMatchesAny(t *testing.T) {
	cases := []struct {
		name  string
		globs []string
		want  bool
	}{
		{"config.yaml", []string{"*.yaml"}, true},
		{"CONFIG.YAML", []string{"*.yaml"}, true},
		{"config.yaml", []string{"*.YAML"}, true},
		{"config.yml", []string{"*.yaml"}, false},
		{"my-secret-file.txt", []string{"*secret*"}, true},
		{"package.json", []string{"package.json"}, true},
		{"package.json", []string{"*.lock.json"}, false},
		{"tsconfig.build.json", []string{"tsconfig*.json"}, true},
		{"dir/sub/config.yaml", []string{"*.yaml"}, true},
		{"config.yaml", nil, false},
		{"config.yaml", []string{""}, false},
		{"[bad", []string{"[bad"}, true}, // malformed pattern falls back to literal
		{"other", []string{"[bad"}, false},
		{".env", []string{"*.env"}, true},
		{".env.local", []string{".env.*"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name+"/"+strings.Join(tc.globs, ","), func(t *testing.T) {
			if got := MatchesAny(tc.name, tc.globs); got != tc.want {
				t.Errorf("MatchesAny(%q, %v) = %v, want %v", tc.name, tc.globs, got, tc.want)
			}
		})
	}
}

func TestDefaults(t *testing.T) {
	if n := len(DefaultInclude()); n != 37 {
		t.Errorf("DefaultInclude has %d entries, want 37", n)
	}
	if n := len(DefaultExcludeDirs()); n != 36 {
		t.Errorf("DefaultExcludeDirs has %d entries, want 36", n)
	}
	if n := len(DefaultExcludeFiles()); n != 18 {
		t.Errorf("DefaultExcludeFiles has %d entries, want 18", n)
	}
	for _, want := range []string{".env", "*.yaml", "wp-config.php", "*.env.ts", "*.local.*"} {
		if !contains(DefaultInclude(), want) {
			t.Errorf("DefaultInclude missing %q", want)
		}
	}
	for _, want := range []string{".git", "node_modules", ".github", "testdata"} {
		if !contains(DefaultExcludeDirs(), want) {
			t.Errorf("DefaultExcludeDirs missing %q", want)
		}
	}
	for _, want := range []string{"package.json", "*.lock.json", "go.sum"} {
		if !contains(DefaultExcludeFiles(), want) {
			t.Errorf("DefaultExcludeFiles missing %q", want)
		}
	}
	// Returned slices are fresh copies: mutating one must not affect defaults.
	d := DefaultInclude()
	d[0] = "mutated"
	if DefaultInclude()[0] == "mutated" {
		t.Error("DefaultInclude returns shared slice")
	}
}

func TestScanNoGit(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, ".env", []byte("A=1"))
	writeFile(t, root, "config.yaml", []byte("a: 1"))
	writeFile(t, root, "main.go", []byte("package main"))
	writeFile(t, root, "sub/settings.json", []byte("{}"))
	writeFile(t, root, "sub/deep/creds/credentials.txt", []byte("x"))
	writeFile(t, root, "package.json", []byte("{}"))
	writeFile(t, root, "node_modules/x/config.json", []byte("{}"))
	writeFile(t, root, "README.md", []byte("hi"))

	res, err := Scan(context.Background(), root, Options{}, noGit())
	if err != nil {
		t.Fatal(err)
	}
	if res.GitInfo {
		t.Error("GitInfo should be false without git")
	}
	if len(res.Warnings) == 0 || !strings.Contains(res.Warnings[0], "no git info") {
		t.Errorf("expected a no-git warning, got %v", res.Warnings)
	}
	want := []string{".env", "sub/deep/creds/credentials.txt", "config.yaml", "sub/settings.json"}
	if got := paths(res.Candidates); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("candidates = %v, want %v", got, want)
	}
	env, _ := find(res.Candidates, ".env")
	if env.Score != ScoreHigh || !env.Preselected || !env.SecretName {
		t.Errorf(".env = %+v, want High/preselected/secret", env)
	}
	if !contains(env.Reasons, ReasonSecretName) || !contains(env.Reasons, "matches .env") {
		t.Errorf(".env reasons = %v", env.Reasons)
	}
	yaml, _ := find(res.Candidates, "config.yaml")
	if yaml.Score != ScoreMedium || yaml.Preselected || yaml.SecretName {
		t.Errorf("config.yaml = %+v, want Medium/not preselected", yaml)
	}
	if strings.Join(yaml.Reasons, ",") != "matches *.yaml" {
		t.Errorf("config.yaml reasons = %v", yaml.Reasons)
	}
	if yaml.Size != 4 || yaml.Mode == 0 {
		t.Errorf("config.yaml size/mode = %d/%o", yaml.Size, yaml.Mode)
	}
	if res.Truncated {
		t.Error("unexpected truncation")
	}
}

func TestScanWithFakeGit(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, ".env", []byte("A=1"))
	writeFile(t, root, "config.yaml", []byte("a: 1"))
	writeFile(t, root, "app.toml", []byte(""))
	writeFile(t, root, "local.ini", []byte(""))
	writeFile(t, root, "sub/id_rsa.pem", []byte("pem"))
	writeFile(t, root, "sub/notes.txt", []byte(""))

	fg := &fakeGit{t: t, inTree: true,
		tracked: []string{"config.yaml", "sub/notes.txt", "sub/id_rsa.pem"},
		ignored: []string{".env", "app.toml", "config.yaml"}, // config.yaml: tracked wins
	}
	res, err := Scan(context.Background(), root, Options{Tracked: map[string]bool{"local.ini": true}}, fg.runner())
	if err != nil {
		t.Fatal(err)
	}
	if !res.GitInfo {
		t.Fatalf("GitInfo false; warnings=%v", res.Warnings)
	}
	if strings.Join(fg.calls, ",") != "ls-files,check-ignore" {
		t.Errorf("git calls = %v, want exactly the two-command batch", fg.calls)
	}
	// stdin to check-ignore holds every candidate relpath, NUL separated.
	gotStdin := strings.Split(strings.TrimRight(string(fg.stdin), "\x00"), "\x00")
	if len(gotStdin) != 5 || !contains(gotStdin, "sub/id_rsa.pem") || !contains(gotStdin, ".env") {
		t.Errorf("check-ignore stdin = %q", gotStdin)
	}
	if strings.Contains(string(fg.stdin), "\n") {
		t.Error("check-ignore stdin must be NUL separated, not newline")
	}

	want := []string{".env", "app.toml", "local.ini", "config.yaml", "sub/id_rsa.pem"}
	if got := paths(res.Candidates); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("candidates = %v, want %v", got, want)
	}
	checks := []struct {
		path       string
		score      Score
		tracked    bool
		ignored    bool
		vault      bool
		presel     bool
		reasonHead string
	}{
		{".env", ScoreHigh, false, true, false, true, ReasonIgnored},
		{"app.toml", ScoreHigh, false, true, false, true, ReasonIgnored},
		{"local.ini", ScoreMedium, false, false, true, true, "matches *.ini"},
		{"config.yaml", ScoreLow, true, false, false, false, ReasonTracked},
		{"sub/id_rsa.pem", ScoreLow, true, false, false, false, ReasonTracked},
	}
	for _, c := range checks {
		got, ok := find(res.Candidates, c.path)
		if !ok {
			t.Errorf("%s missing", c.path)
			continue
		}
		if got.Score != c.score || got.GitTracked != c.tracked || got.GitIgnored != c.ignored ||
			got.Tracked != c.vault || got.Preselected != c.presel {
			t.Errorf("%s = %+v, want score=%v tracked=%v ignored=%v vault=%v presel=%v",
				c.path, got, c.score, c.tracked, c.ignored, c.vault, c.presel)
		}
		if len(got.Reasons) == 0 || got.Reasons[0] != c.reasonHead {
			t.Errorf("%s reasons = %v, want first %q", c.path, got.Reasons, c.reasonHead)
		}
		if contains(got.Reasons, ReasonVaultTracked) != c.vault {
			t.Errorf("%s reasons = %v, vault-tracked reason presence should be %v", c.path, got.Reasons, c.vault)
		}
	}
	pem, _ := find(res.Candidates, "sub/id_rsa.pem")
	if !pem.SecretName || !contains(pem.Reasons, ReasonSecretName) {
		t.Errorf("pem should be secret-ish: %+v", pem)
	}
}

func TestScanUntrackedSecretPreselected(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, ".env", []byte("A=1"))
	writeFile(t, root, "config.yaml", []byte(""))
	fg := &fakeGit{t: t, inTree: true}
	res, err := Scan(context.Background(), root, Options{}, fg.runner())
	if err != nil {
		t.Fatal(err)
	}
	if !res.GitInfo {
		t.Fatalf("GitInfo false; warnings=%v", res.Warnings)
	}
	env, _ := find(res.Candidates, ".env")
	if env.Score != ScoreMedium || !env.Preselected {
		t.Errorf(".env untracked secret = %+v, want Medium + preselected", env)
	}
	yaml, _ := find(res.Candidates, "config.yaml")
	if yaml.Score != ScoreMedium || yaml.Preselected {
		t.Errorf("config.yaml untracked = %+v, want Medium, not preselected", yaml)
	}
}

func TestScanGitFailures(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, ".env", []byte("A=1"))
	writeFile(t, root, "config.yaml", []byte(""))

	t.Run("not a work tree", func(t *testing.T) {
		fg := &fakeGit{t: t, inTree: false}
		res, err := Scan(context.Background(), root, Options{}, fg.runner())
		if err != nil {
			t.Fatal(err)
		}
		if res.GitInfo {
			t.Error("GitInfo should be false")
		}
		if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "no git info") {
			t.Errorf("warnings = %v", res.Warnings)
		}
		// ls-files exit 128 is the work-tree probe: check-ignore must not run.
		if strings.Join(fg.calls, ",") != "ls-files" {
			t.Errorf("git calls = %v, want only ls-files", fg.calls)
		}
		if !strings.Contains(res.Warnings[0], "not a git repository") {
			t.Errorf("warning should carry git's stderr: %v", res.Warnings)
		}
		env, _ := find(res.Candidates, ".env")
		if env.Score != ScoreHigh {
			t.Errorf(".env without git = %v, want High", env.Score)
		}
	})
	t.Run("check-ignore exit 128", func(t *testing.T) {
		fg := &fakeGit{t: t, inTree: true, failWith: 128, tracked: []string{"config.yaml"}}
		res, err := Scan(context.Background(), root, Options{}, fg.runner())
		if err != nil {
			t.Fatal(err)
		}
		if res.GitInfo {
			t.Error("GitInfo should be false after check-ignore failure")
		}
		if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "boom") {
			t.Errorf("warnings = %v", res.Warnings)
		}
		yaml, _ := find(res.Candidates, "config.yaml")
		if yaml.GitTracked || yaml.Score != ScoreMedium {
			t.Errorf("config.yaml = %+v, want Medium without git data", yaml)
		}
	})
	t.Run("nil runner", func(t *testing.T) {
		res, err := Scan(context.Background(), root, Options{}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if res.GitInfo || len(res.Warnings) != 1 {
			t.Errorf("nil runner: GitInfo=%v warnings=%v", res.GitInfo, res.Warnings)
		}
	})
	t.Run("ls-files exit 1", func(t *testing.T) {
		// Any non-zero ls-files exit (not only 128) means no git info.
		r := execx.Fake(func(c execx.Cmd) (execx.Result, error) {
			return execx.Result{ExitCode: 1}, nil
		})
		res, err := Scan(context.Background(), root, Options{}, r)
		if err != nil {
			t.Fatal(err)
		}
		if res.GitInfo || !hasWarning(res.Warnings, "no git info") {
			t.Errorf("GitInfo=%v warnings=%v", res.GitInfo, res.Warnings)
		}
	})
	t.Run("check-ignore exit 1 means nothing ignored", func(t *testing.T) {
		fg := &fakeGit{t: t, inTree: true}
		res, err := Scan(context.Background(), root, Options{}, fg.runner())
		if err != nil {
			t.Fatal(err)
		}
		if !res.GitInfo || len(res.Warnings) != 0 {
			t.Errorf("GitInfo=%v warnings=%v, want true and none", res.GitInfo, res.Warnings)
		}
		for _, c := range res.Candidates {
			if c.GitIgnored {
				t.Errorf("%s should not be ignored", c.Path)
			}
		}
	})
}

// Regression: vault-tracked files are always listed, even when the walk would
// have filtered them out (no include match, exclude glob, excluded directory,
// above max size), so the user can see and untrack them.
func TestScanVaultTrackedAlwaysListed(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "notes.md", []byte("hello"))                      // matches no include glob
	writeFile(t, root, "big.json", bytes.Repeat([]byte("x"), 200))       // above MaxFileSize
	writeFile(t, root, "package.json", []byte("{}"))                     // exclude-file glob
	writeFile(t, root, "node_modules/x/config.json", []byte("{}"))       // excluded dir
	writeFile(t, root, "config.yaml", []byte("a: 1"))                    // normal candidate
	writeFile(t, root, "vault/blob.bin", []byte("v"))                    // hard-excluded
	writeFile(t, root, "sub/.env.local", []byte("A=1"))                  // tracked + secret-ish
	writeFile(t, root, "other.yaml", []byte(""))                         // not tracked
	if err := os.Mkdir(filepath.Join(root, "adir"), 0o755); err != nil { // tracked path is a directory
		t.Fatal(err)
	}
	tracked := map[string]bool{
		"notes.md":                   true,
		"./big.json":                 true, // cleaned to big.json
		"package.json":               true,
		"node_modules/x/config.json": true,
		"config.yaml":                true,
		"vault/blob.bin":             true,
		"sub/.env.local":             true,
		"missing.txt":                true,
		"adir":                       true,
		"../escape.txt":              true,
		"":                           true,
		"other.yaml":                 false, // explicitly false: not tracked
	}
	opts := Options{
		MaxFileSize: 100,
		Tracked:     tracked,
		HardExclude: []string{filepath.Join(root, "vault")},
	}

	t.Run("no git", func(t *testing.T) {
		res, err := Scan(context.Background(), root, opts, noGit())
		if err != nil {
			t.Fatal(err)
		}
		want := []string{"sub/.env.local", "big.json", "config.yaml", "node_modules/x/config.json", "notes.md", "other.yaml", "package.json"}
		if got := paths(res.Candidates); strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("candidates = %v, want %v", got, want)
		}
		for _, p := range []string{"notes.md", "big.json", "package.json", "node_modules/x/config.json", "config.yaml", "sub/.env.local"} {
			c, ok := find(res.Candidates, p)
			if !ok {
				t.Errorf("%s missing", p)
				continue
			}
			if !c.Tracked || !c.Preselected {
				t.Errorf("%s = %+v, want Tracked and Preselected", p, c)
			}
			if !contains(c.Reasons, ReasonVaultTracked) {
				t.Errorf("%s reasons = %v, want %q", p, c.Reasons, ReasonVaultTracked)
			}
			if c.Size == 0 || c.Mode == 0 {
				t.Errorf("%s size/mode = %d/%o, want populated", p, c.Size, c.Mode)
			}
		}
		notes, _ := find(res.Candidates, "notes.md")
		if notes.Score != ScoreMedium || notes.SecretName || strings.Join(notes.Reasons, ",") != ReasonVaultTracked {
			t.Errorf("notes.md = %+v, want Medium, only the vault reason", notes)
		}
		big, _ := find(res.Candidates, "big.json")
		if big.Size != 200 || !contains(big.Reasons, "matches *.json") {
			t.Errorf("big.json = %+v, want size 200 and the include reason", big)
		}
		env, _ := find(res.Candidates, "sub/.env.local")
		if env.Score != ScoreHigh || !env.SecretName || !contains(env.Reasons, ReasonSecretName) {
			t.Errorf("sub/.env.local = %+v, want High secret", env)
		}
		other, _ := find(res.Candidates, "other.yaml")
		if other.Tracked || other.Preselected || contains(other.Reasons, ReasonVaultTracked) {
			t.Errorf("other.yaml = %+v, want untracked", other)
		}
		for _, p := range []string{"vault/blob.bin", "missing.txt", "adir", "../escape.txt", ""} {
			if _, ok := find(res.Candidates, p); ok {
				t.Errorf("%q must not be listed", p)
			}
		}
		if !hasWarning(res.Warnings, "missing.txt is missing") {
			t.Errorf("warnings = %v, want one about missing.txt", res.Warnings)
		}
		if !hasWarning(res.Warnings, "adir is not a regular file") {
			t.Errorf("warnings = %v, want one about adir", res.Warnings)
		}
		if !hasWarning(res.Warnings, `"../escape.txt": invalid`) {
			t.Errorf("warnings = %v, want one about ../escape.txt", res.Warnings)
		}
		if hasWarning(res.Warnings, "vault/blob.bin") {
			t.Errorf("hard-excluded tracked file must be silently skipped: %v", res.Warnings)
		}
	})

	t.Run("with git", func(t *testing.T) {
		// Tracked files reach the check-ignore batch and are scored like any
		// other candidate; the vault flag keeps them pre-selected.
		fg := &fakeGit{t: t, inTree: true, tracked: []string{"notes.md"}, ignored: []string{"big.json"}}
		res, err := Scan(context.Background(), root, opts, fg.runner())
		if err != nil {
			t.Fatal(err)
		}
		if !res.GitInfo {
			t.Fatalf("GitInfo false; warnings=%v", res.Warnings)
		}
		if !strings.Contains(string(fg.stdin), "notes.md\x00") || !strings.Contains(string(fg.stdin), "big.json\x00") {
			t.Errorf("check-ignore stdin lacks tracked files: %q", fg.stdin)
		}
		notes, _ := find(res.Candidates, "notes.md")
		if notes.Score != ScoreLow || !notes.GitTracked || !notes.Tracked || !notes.Preselected {
			t.Errorf("notes.md = %+v, want Low/git-tracked/vault-tracked/preselected", notes)
		}
		if notes.Reasons[0] != ReasonTracked || !contains(notes.Reasons, ReasonVaultTracked) {
			t.Errorf("notes.md reasons = %v", notes.Reasons)
		}
		big, _ := find(res.Candidates, "big.json")
		if big.Score != ScoreHigh || !big.GitIgnored || !big.Tracked {
			t.Errorf("big.json = %+v, want High/ignored/vault-tracked", big)
		}
		// Sorting still holds: High first, then Medium, then Low; path within.
		got := paths(res.Candidates)
		if got[0] != "big.json" || got[len(got)-1] != "notes.md" {
			t.Errorf("order = %v", got)
		}
	})

	t.Run("tracked file inside nested repo", func(t *testing.T) {
		nested := t.TempDir()
		writeFile(t, nested, "lib/.git/HEAD", []byte("ref"))
		writeFile(t, nested, "lib/conf.yaml", []byte("a: 1"))
		writeFile(t, nested, "lib/.env", []byte(""))
		res, err := Scan(context.Background(), nested, Options{Tracked: map[string]bool{"lib/conf.yaml": true}}, noGit())
		if err != nil {
			t.Fatal(err)
		}
		if strings.Join(res.NestedRepos, ",") != "lib" {
			t.Errorf("NestedRepos = %v", res.NestedRepos)
		}
		if got := paths(res.Candidates); strings.Join(got, ",") != "lib/conf.yaml" {
			t.Errorf("candidates = %v, want only the vault-tracked file", got)
		}
	})

	t.Run("tracked file is a symlink", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("symlinks")
		}
		dir := t.TempDir()
		target := writeFile(t, dir, "real.txt", []byte("x"))
		if err := os.Symlink(target, filepath.Join(dir, "link.txt")); err != nil {
			t.Skip("cannot create symlink:", err)
		}
		res, err := Scan(context.Background(), dir, Options{Tracked: map[string]bool{"link.txt": true}}, noGit())
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Candidates) != 0 || !hasWarning(res.Warnings, "link.txt is not a regular file") {
			t.Errorf("candidates=%v warnings=%v", paths(res.Candidates), res.Warnings)
		}
	})
}

func TestCleanRel(t *testing.T) {
	cases := []struct {
		in    string
		want  string
		valid bool
	}{
		{"a.txt", "a.txt", true},
		{"./a.txt", "a.txt", true},
		{"sub//b/../c.yaml", "sub/c.yaml", true},
		{"  spaced.txt ", "spaced.txt", true},
		{"", "", false},
		{".", "", false},
		{"..", "", false},
		{"../x", "", false},
		{"a/../../x", "", false},
		{"/abs/x", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, ok := cleanRel(tc.in)
			if got != tc.want || ok != tc.valid {
				t.Errorf("cleanRel(%q) = (%q,%v), want (%q,%v)", tc.in, got, ok, tc.want, tc.valid)
			}
		})
	}
}

// Pins the documented behaviour: exclude_dirs names match case-insensitively,
// consistently with the include/exclude file globs.
func TestScanExcludeDirsCaseInsensitive(t *testing.T) {
	root := t.TempDir()
	for _, p := range []string{
		"Build/a.yaml", "ENV/b.yaml", "Out/c.yaml", "sub/Bin/d.yaml", "pods/e.yaml", "keep/f.yaml",
	} {
		writeFile(t, root, p, []byte(""))
	}
	res, err := Scan(context.Background(), root, Options{}, noGit())
	if err != nil {
		t.Fatal(err)
	}
	if got := paths(res.Candidates); strings.Join(got, ",") != "keep/f.yaml" {
		t.Errorf("candidates = %v, want only keep/f.yaml", got)
	}
	// Custom exclude lists get the same treatment.
	res, err = Scan(context.Background(), root, Options{ExcludeDirs: []string{"KEEP"}}, noGit())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := find(res.Candidates, "keep/f.yaml"); ok {
		t.Errorf("custom exclude KEEP should skip keep/: %v", paths(res.Candidates))
	}
	if _, ok := find(res.Candidates, "Build/a.yaml"); !ok {
		t.Errorf("custom list replaces defaults, Build/ should be scanned: %v", paths(res.Candidates))
	}
}

func TestScanNestedRepos(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "config.yaml", []byte(""))
	writeFile(t, root, ".git/config", []byte("")) // root's own .git: excluded by name, not nested
	writeFile(t, root, "libs/repo-a/.git/HEAD", []byte("ref"))
	writeFile(t, root, "libs/repo-a/config.yaml", []byte(""))
	writeFile(t, root, "libs/repo-a/deep/.env", []byte(""))
	writeFile(t, root, "libs/worktree/.git", []byte("gitdir: /elsewhere")) // .git file
	writeFile(t, root, "libs/worktree/.env", []byte(""))
	writeFile(t, root, "libs/plain/app.json", []byte(""))

	res, err := Scan(context.Background(), root, Options{}, noGit())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(res.NestedRepos, ",") != "libs/repo-a,libs/worktree" {
		t.Errorf("NestedRepos = %v", res.NestedRepos)
	}
	want := []string{"config.yaml", "libs/plain/app.json"}
	if got := paths(res.Candidates); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("candidates = %v, want %v", got, want)
	}
}

func TestScanHardExcludes(t *testing.T) {
	root := t.TempDir()
	key := writeFile(t, root, "master.key", []byte("k"))
	writeFile(t, root, "vault/data/settings.json", []byte("{}"))
	writeFile(t, root, "vault/.env", []byte(""))
	writeFile(t, root, "other.key", []byte("k"))
	writeFile(t, root, "config.json", []byte("{}"))

	// Pass the vault via a relative-ish path with a trailing slash and a
	// symlink-unresolved key path to check normalisation.
	vault := filepath.Join(root, "vault") + string(filepath.Separator)
	res, err := Scan(context.Background(), root, Options{HardExclude: []string{key, vault, ""}}, noGit())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"other.key", "config.json"}
	if got := paths(res.Candidates); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("candidates = %v, want %v", got, want)
	}
}

func TestScanHardExcludeViaSymlinkedRoot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks")
	}
	base := t.TempDir()
	real := filepath.Join(base, "real")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skip("cannot create symlink:", err)
	}
	writeFile(t, real, "key.pem", []byte(""))
	writeFile(t, real, "app.yaml", []byte(""))

	// Scan through the symlink, hard-exclude through the real path.
	res, err := Scan(context.Background(), link, Options{HardExclude: []string{filepath.Join(real, "key.pem")}}, noGit())
	if err != nil {
		t.Fatal(err)
	}
	if got := paths(res.Candidates); strings.Join(got, ",") != "app.yaml" {
		t.Errorf("candidates = %v", got)
	}
	// And the other way round: scan real, exclude through the link.
	res, err = Scan(context.Background(), real, Options{HardExclude: []string{filepath.Join(link, "key.pem")}}, noGit())
	if err != nil {
		t.Fatal(err)
	}
	if got := paths(res.Candidates); strings.Join(got, ",") != "app.yaml" {
		t.Errorf("candidates = %v", got)
	}
}

func TestScanSymlinksNotFollowed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks")
	}
	root := t.TempDir()
	outside := t.TempDir()
	writeFile(t, outside, "secret.txt", []byte("x"))
	writeFile(t, outside, "dir/.env", []byte("x"))
	writeFile(t, root, "real.yaml", []byte(""))
	if err := os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(root, "linked-secret.txt")); err != nil {
		t.Skip("cannot create symlink:", err)
	}
	if err := os.Symlink(filepath.Join(outside, "dir"), filepath.Join(root, "linked-dir")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "real.yaml"), filepath.Join(root, "self.yaml")); err != nil {
		t.Fatal(err)
	}
	res, err := Scan(context.Background(), root, Options{}, noGit())
	if err != nil {
		t.Fatal(err)
	}
	if got := paths(res.Candidates); strings.Join(got, ",") != "real.yaml" {
		t.Errorf("candidates = %v, want only real.yaml", got)
	}
}

func TestScanSizeLimit(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "small.json", bytes.Repeat([]byte("a"), 10))
	writeFile(t, root, "exact.json", bytes.Repeat([]byte("a"), 100))
	writeFile(t, root, "big.json", bytes.Repeat([]byte("a"), 101))

	res, err := Scan(context.Background(), root, Options{MaxFileSize: 100}, noGit())
	if err != nil {
		t.Fatal(err)
	}
	if got := paths(res.Candidates); strings.Join(got, ",") != "exact.json,small.json" {
		t.Errorf("candidates = %v", got)
	}
	// Default of 2 MiB when zero.
	writeFile(t, root, "huge.json", make([]byte, DefaultMaxFileSize+1))
	res, err = Scan(context.Background(), root, Options{}, noGit())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := find(res.Candidates, "huge.json"); ok {
		t.Error("huge.json above default size should be skipped")
	}
	if _, ok := find(res.Candidates, "big.json"); !ok {
		t.Error("big.json should be included under the default size")
	}
}

func TestScanExcludeDirsAndFiles(t *testing.T) {
	root := t.TempDir()
	for _, p := range []string{
		"node_modules/a/config.json", "sub/node_modules/b.yaml", "Vendor/x.toml", "dist/app.json",
		"testdata/fixture.json", "keep/app.yaml", "package.json", "package-lock.json",
		"tsconfig.build.json", "data.schema.json", ".eslintrc.json", "x.lock.json", "go.sum",
		"keep/real.json",
	} {
		writeFile(t, root, p, []byte("{}"))
	}
	res, err := Scan(context.Background(), root, Options{}, noGit())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"keep/app.yaml", "keep/real.json"}
	if got := paths(res.Candidates); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("candidates = %v, want %v", got, want)
	}

	// Custom lists replace the defaults.
	res, err = Scan(context.Background(), root, Options{
		Include:      []string{"*.toml", "*.yaml"},
		ExcludeDirs:  []string{"keep"},
		ExcludeFiles: []string{"b.yaml"},
	}, noGit())
	if err != nil {
		t.Fatal(err)
	}
	if got := paths(res.Candidates); strings.Join(got, ",") != "Vendor/x.toml" {
		t.Errorf("custom candidates = %v", got)
	}
}

func TestScanTruncationFiles(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 30; i++ {
		writeFile(t, root, fmt.Sprintf("f%02d.yaml", i), []byte(""))
	}
	res, err := Scan(context.Background(), root, Options{MaxFiles: 10}, noGit())
	if err != nil {
		t.Fatal(err)
	}
	if !res.Truncated {
		t.Error("expected Truncated")
	}
	if len(res.Candidates) != 10 {
		t.Errorf("got %d candidates, want 10", len(res.Candidates))
	}
	if !hasWarning(res.Warnings, "truncated") {
		t.Errorf("warnings = %v", res.Warnings)
	}
}

func TestScanTruncationCandidates(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 30; i++ {
		writeFile(t, root, fmt.Sprintf("f%02d.yaml", i), []byte(""))
		writeFile(t, root, fmt.Sprintf("n%02d.txt", i), []byte(""))
	}
	res, err := Scan(context.Background(), root, Options{MaxCandidates: 7}, noGit())
	if err != nil {
		t.Fatal(err)
	}
	if !res.Truncated || len(res.Candidates) != 7 {
		t.Errorf("Truncated=%v candidates=%d, want true/7", res.Truncated, len(res.Candidates))
	}
	if !hasWarning(res.Warnings, "candidates") {
		t.Errorf("warnings = %v", res.Warnings)
	}
	// Under the cap: no truncation.
	res, err = Scan(context.Background(), root, Options{MaxCandidates: 30, MaxFiles: 60}, noGit())
	if err != nil {
		t.Fatal(err)
	}
	if res.Truncated || len(res.Candidates) != 30 {
		t.Errorf("Truncated=%v candidates=%d, want false/30", res.Truncated, len(res.Candidates))
	}
}

func hasWarning(ws []string, sub string) bool {
	for _, w := range ws {
		if strings.Contains(w, sub) {
			return true
		}
	}
	return false
}

func TestScanProgress(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 1200; i++ {
		ext := ".txt"
		if i%3 == 0 {
			ext = ".json"
		}
		writeFile(t, root, fmt.Sprintf("d%d/f%04d%s", i%7, i, ext), []byte(""))
	}
	var walkedSeen []int
	var foundSeen []int
	res, err := Scan(context.Background(), root, Options{Progress: func(walked, found int) {
		walkedSeen = append(walkedSeen, walked)
		foundSeen = append(foundSeen, found)
	}}, noGit())
	if err != nil {
		t.Fatal(err)
	}
	if !contains(intsToStrings(walkedSeen), "500") || !contains(intsToStrings(walkedSeen), "1000") {
		t.Errorf("progress walked values = %v, want to include 500 and 1000", walkedSeen)
	}
	last := walkedSeen[len(walkedSeen)-1]
	if last != 1200 || foundSeen[len(foundSeen)-1] != len(res.Candidates) {
		t.Errorf("final progress = (%d,%d), want (1200,%d)", last, foundSeen[len(foundSeen)-1], len(res.Candidates))
	}
	if len(res.Candidates) != 400 {
		t.Errorf("candidates = %d, want 400", len(res.Candidates))
	}
}

func intsToStrings(xs []int) []string {
	out := make([]string, len(xs))
	for i, x := range xs {
		out[i] = fmt.Sprint(x)
	}
	return out
}

func TestScanCancellation(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 700; i++ {
		writeFile(t, root, fmt.Sprintf("f%04d.yaml", i), []byte(""))
	}
	t.Run("already cancelled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		res, err := Scan(ctx, root, Options{}, noGit())
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want context.Canceled", err)
		}
		if res != nil {
			t.Error("result should be nil on cancellation")
		}
	})
	t.Run("cancelled during walk", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		calls := 0
		_, err := Scan(ctx, root, Options{Progress: func(walked, found int) {
			calls++
			cancel()
		}}, noGit())
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want context.Canceled", err)
		}
		if calls != 1 {
			t.Errorf("progress calls = %d, want 1 (walk must stop after cancel)", calls)
		}
	})
	t.Run("cancelled during git", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		r := execx.Fake(func(c execx.Cmd) (execx.Result, error) {
			cancel()
			return execx.Result{Stdout: []byte("true\n")}, nil
		})
		_, err := Scan(ctx, root, Options{}, r)
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want context.Canceled", err)
		}
	})
}

func TestScanSorting(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "z.yaml", []byte(""))
	writeFile(t, root, "a.yaml", []byte(""))
	writeFile(t, root, "m/.env", []byte(""))
	writeFile(t, root, "b/.env", []byte(""))
	writeFile(t, root, "tracked.json", []byte(""))
	fg := &fakeGit{t: t, inTree: true, tracked: []string{"tracked.json"}, ignored: []string{"m/.env", "b/.env"}}
	res, err := Scan(context.Background(), root, Options{}, fg.runner())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"b/.env", "m/.env", "a.yaml", "z.yaml", "tracked.json"}
	if got := paths(res.Candidates); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("order = %v, want %v", got, want)
	}
}

func TestScanBadDir(t *testing.T) {
	if _, err := Scan(context.Background(), filepath.Join(t.TempDir(), "missing"), Options{}, noGit()); err == nil {
		t.Error("expected error for missing dir")
	}
	f := writeFile(t, t.TempDir(), "file.txt", []byte(""))
	if _, err := Scan(context.Background(), f, Options{}, noGit()); err == nil {
		t.Error("expected error for non-directory")
	}
	if _, err := Scan(context.Background(), "", Options{}, noGit()); err == nil {
		t.Error("expected error for empty dir")
	}
	// Empty directory: no candidates, no error.
	res, err := Scan(context.Background(), t.TempDir(), Options{}, noGit())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Candidates) != 0 || res.Truncated {
		t.Errorf("empty dir result = %+v", res)
	}
}

func TestScanUnreadableDirWarns(t *testing.T) {
	if runtime.GOOS == "windows" || os.Getuid() == 0 {
		t.Skip("permission test needs a non-root unix user")
	}
	root := t.TempDir()
	writeFile(t, root, "ok.yaml", []byte(""))
	locked := filepath.Join(root, "locked")
	writeFile(t, root, "locked/x.yaml", []byte(""))
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
	res, err := Scan(context.Background(), root, Options{}, noGit())
	if err != nil {
		t.Fatal(err)
	}
	if got := paths(res.Candidates); strings.Join(got, ",") != "ok.yaml" {
		t.Errorf("candidates = %v", got)
	}
	if !hasWarning(res.Warnings, "locked") {
		t.Errorf("warnings = %v, want one about locked", res.Warnings)
	}
}

// --- real git -------------------------------------------------------------

func realGit(t *testing.T) execx.Runner {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	r := execx.Real()
	if _, err := r.Run(context.Background(), execx.Cmd{Name: "git", Args: []string{"--version"}}); err != nil {
		t.Skip("git not runnable:", err)
	}
	return r
}

func gitRun(t *testing.T, r execx.Runner, dir string, args ...string) {
	t.Helper()
	all := append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)
	if _, err := r.Run(context.Background(), execx.Cmd{Name: "git", Args: all, Env: []string{"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1"}}); err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
}

func TestScanRealGit(t *testing.T) {
	r := realGit(t)
	root := t.TempDir()
	gitRun(t, r, root, "init", "-q")
	writeFile(t, root, ".gitignore", []byte(".env\n*.local.json\nsecrets/\n"))
	writeFile(t, root, "config.yaml", []byte("a: 1"))
	writeFile(t, root, ".env", []byte("A=1"))
	writeFile(t, root, "settings.local.json", []byte("{}"))
	writeFile(t, root, "secrets/api.key", []byte("k"))
	writeFile(t, root, "untracked.toml", []byte(""))
	writeFile(t, root, "sub/app.json", []byte("{}"))
	writeFile(t, root, "sub/.env.production", []byte(""))
	gitRun(t, r, root, "add", ".gitignore", "config.yaml", "sub/app.json")
	gitRun(t, r, root, "commit", "-q", "-m", "init")

	res, err := Scan(context.Background(), root, Options{}, r)
	if err != nil {
		t.Fatal(err)
	}
	if !res.GitInfo {
		t.Fatalf("GitInfo false; warnings=%v", res.Warnings)
	}
	checks := map[string]struct {
		score   Score
		tracked bool
		ignored bool
		presel  bool
	}{
		"config.yaml":         {ScoreLow, true, false, false},
		"sub/app.json":        {ScoreLow, true, false, false},
		".env":                {ScoreHigh, false, true, true},
		"settings.local.json": {ScoreHigh, false, true, true},
		"secrets/api.key":     {ScoreHigh, false, true, true},
		"untracked.toml":      {ScoreMedium, false, false, false},
		"sub/.env.production": {ScoreMedium, false, false, true},
	}
	if len(res.Candidates) != len(checks) {
		t.Errorf("candidates = %v", paths(res.Candidates))
	}
	for p, want := range checks {
		got, ok := find(res.Candidates, p)
		if !ok {
			t.Errorf("%s missing from %v", p, paths(res.Candidates))
			continue
		}
		if got.Score != want.score || got.GitTracked != want.tracked || got.GitIgnored != want.ignored || got.Preselected != want.presel {
			t.Errorf("%s = %+v, want %+v", p, got, want)
		}
	}

	// Scanning a sub-directory of the work tree also works (git -C <dir>).
	sub, err := Scan(context.Background(), filepath.Join(root, "sub"), Options{}, r)
	if err != nil {
		t.Fatal(err)
	}
	if !sub.GitInfo {
		t.Fatalf("sub GitInfo false; warnings=%v", sub.Warnings)
	}
	app, _ := find(sub.Candidates, "app.json")
	if !app.GitTracked || app.Score != ScoreLow {
		t.Errorf("sub app.json = %+v, want tracked/Low", app)
	}
	if strings.Join(paths(sub.Candidates), ",") != ".env.production,app.json" {
		t.Errorf("sub candidates = %v", paths(sub.Candidates))
	}

	// A directory outside any repo yields no git info.
	plain := t.TempDir()
	writeFile(t, plain, ".env", []byte(""))
	out, err := Scan(context.Background(), plain, Options{}, r)
	if err != nil {
		t.Fatal(err)
	}
	if out.GitInfo {
		t.Error("expected no git info outside a repository")
	}
	env, _ := find(out.Candidates, ".env")
	if env.Score != ScoreHigh {
		t.Errorf("no-git .env = %v, want High", env.Score)
	}
}
