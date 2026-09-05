package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/sanbiv/private-sync/internal/app"
	"github.com/sanbiv/private-sync/internal/config"
	"github.com/sanbiv/private-sync/internal/execx"
	"github.com/sanbiv/private-sync/internal/keysource"
	"github.com/sanbiv/private-sync/internal/paths"
	"github.com/sanbiv/private-sync/internal/sync"
	"github.com/sanbiv/private-sync/internal/ui"
)

// fakeFrontend records every call and answers from its fields.
type fakeFrontend struct {
	setupCfg   *config.Config
	setupErr   error
	setupCalls []*config.Config // the "existing" argument of every RunSetup call
	setupDirs  paths.Dirs

	runCalls int
	runErr   error
	sessions []*app.Session

	addDirs []string
	addErr  error

	resolve      func(p *sync.Plan) (sync.Resolutions, bool, error)
	resolveCalls int
	resolvePlans []*sync.Plan
}

func (f *fakeFrontend) RunSetup(_ context.Context, dirs paths.Dirs, existing *config.Config) (*config.Config, error) {
	f.setupCalls = append(f.setupCalls, existing)
	f.setupDirs = dirs
	if f.setupErr != nil {
		return nil, f.setupErr
	}
	return f.setupCfg, nil
}

func (f *fakeFrontend) Run(_ context.Context, s *app.Session) error {
	f.runCalls++
	f.sessions = append(f.sessions, s)
	return f.runErr
}

func (f *fakeFrontend) RunAddProject(_ context.Context, _ *app.Session, dir string) error {
	f.addDirs = append(f.addDirs, dir)
	return f.addErr
}

func (f *fakeFrontend) ResolveConflicts(_ context.Context, _ *app.Session, p *sync.Plan) (sync.Resolutions, bool, error) {
	f.resolveCalls++
	f.resolvePlans = append(f.resolvePlans, p)
	if f.resolve == nil {
		return nil, true, nil
	}
	return f.resolve(p)
}

// noGit is a runner where every subprocess fails (no git, no bw, no rclone).
func noGit() execx.Runner {
	return execx.Fake(func(c execx.Cmd) (execx.Result, error) {
		return execx.Result{ExitCode: 127}, errors.New("fake runner: " + c.Name + " not available")
	})
}

// result is the outcome of one CLI invocation.
type result struct {
	code     int
	out, err string
}

// runOpts tunes runCLI.
type runOpts struct {
	interactive   bool
	prompter      ui.Prompter
	fe            Frontend
	remoteFactory app.RemoteFactory
}

// runCLI runs the CLI with captured streams (non-interactive by default).
func runCLI(t *testing.T, o runOpts, args ...string) result {
	t.Helper()
	var out, errb bytes.Buffer
	var fe Frontend = &fakeFrontend{}
	if o.fe != nil {
		fe = o.fe
	}
	var prompter ui.Prompter = ui.Silent{}
	if o.prompter != nil {
		prompter = o.prompter
	}
	interactive := o.interactive
	code := Run(context.Background(), args, fe, Options{
		Stdout:        &out,
		Stderr:        &errb,
		Interactive:   &interactive,
		Prompter:      prompter,
		Runner:        noGit(),
		remoteFactory: o.remoteFactory,
	})
	t.Logf("$ private-sync %s → %d\n%s%s", strings.Join(args, " "), code, out.String(), errb.String())
	return result{code: code, out: out.String(), err: errb.String()}
}

// isolate points every XDG directory into a fresh temp root and returns the
// resolved dirs. Secrets from the real environment never leak in.
func isolate(t *testing.T) (root string, dirs paths.Dirs) {
	t.Helper()
	root = t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "xdg-config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "xdg-state"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "xdg-data"))
	for _, k := range []string{ConfigEnvVar, keysource.EnvVar, "NO_COLOR", "VISUAL", "EDITOR"} {
		if v, ok := os.LookupEnv(k); ok {
			t.Setenv(k, v) // restored after the test
			_ = os.Unsetenv(k)
		}
	}
	dirs, err := paths.Default()
	if err != nil {
		t.Fatal(err)
	}
	return root, dirs
}

// writeConfig writes a remote-none / key-file config with a fresh key file
// and returns it.
func writeConfig(t *testing.T, dirs paths.Dirs, root string, name string) *config.Config {
	t.Helper()
	cfg := config.Default(dirs)
	cfg.Machine.Name = name
	cfg.Vault.Path = filepath.Join(root, "vault")
	cfg.Vault.Remote = config.RemoteConfig{Type: config.RemoteNone}
	keyPath := filepath.Join(root, "key")
	if _, err := os.Lstat(keyPath); err != nil {
		if err := keysource.WriteKeyFile(keyPath); err != nil {
			t.Fatalf("write key file: %v", err)
		}
	}
	cfg.Key = config.KeyConfig{Source: config.KeyFile, File: config.FileKey{Path: keyPath}}
	if err := cfg.Save(dirs.ConfigFile()); err != nil {
		t.Fatalf("save config: %v", err)
	}
	return cfg
}

// commandPaths walks the tree and returns "a b c" paths of every command.
func commandPaths(cmd *cobra.Command, prefix string, into map[string]*cobra.Command) {
	for _, sub := range cmd.Commands() {
		p := strings.TrimSpace(prefix + " " + sub.Name())
		into[p] = sub
		commandPaths(sub, p, into)
	}
}

func TestCommandTree(t *testing.T) {
	c := newCLI(&fakeFrontend{}, Options{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}})
	root := c.rootCommand()
	got := map[string]*cobra.Command{}
	commandPaths(root, "", got)

	want := []string{
		"init", "add", "scan", "status", "sync", "push", "pull", "restore",
		"projects", "projects list", "projects link", "projects unlink",
		"files", "files add", "files rm", "files delete",
		"trash", "trash list", "trash restore", "trash purge",
		"passphrase", "passphrase change",
		"config", "config edit", "config show", "config path",
		"unlock", "machine",
	}
	for _, w := range want {
		if _, ok := got[w]; !ok {
			t.Errorf("command %q not registered", w)
		}
	}
	for _, flag := range []string{"config", "yes", "strategy", "delete", "no-remote", "accept-rollback", "json"} {
		if root.PersistentFlags().Lookup(flag) == nil {
			t.Errorf("global flag --%s missing", flag)
		}
	}
	local := map[string]string{
		"status":       "fetch",
		"restore":      "path",
		"trash purge":  "older-than",
		"files delete": "local",
		"scan":         "all",
	}
	for path, flag := range local {
		cmd := got[path]
		if cmd == nil {
			continue
		}
		if cmd.Flags().Lookup(flag) == nil {
			t.Errorf("%s: flag --%s missing", path, flag)
		}
	}
	if root.RunE == nil {
		t.Error("root has no run function (dashboard)")
	}
	for path, cmd := range got {
		if cmd.Name() == "help" || cmd.Name() == "completion" {
			continue
		}
		if cmd.RunE == nil && cmd.Run == nil {
			t.Errorf("%s has no run function", path)
		}
		if cmd.Short == "" {
			t.Errorf("%s has no short description", path)
		}
	}
}

func TestGlobalFlagsParsed(t *testing.T) {
	_, dirs := isolate(t)
	var seen globals
	var strat sync.Strategy
	c := newCLI(&fakeFrontend{}, Options{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}})
	root := c.rootCommand()
	// Replace machine's run so we only observe the parsed flags.
	got := map[string]*cobra.Command{}
	commandPaths(root, "", got)
	got["machine"].RunE = func(*cobra.Command, []string) error {
		seen = c.g
		strat = c.strategy
		return nil
	}
	root.SetArgs([]string{"--config", filepath.Join(dirs.Config, "x.yaml"), "--yes", "--strategy", "remote", "--delete", "--no-remote", "--accept-rollback", "--json", "machine"})
	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !seen.yes || !seen.delete || !seen.noRemote || !seen.acceptRollback || !seen.json || seen.strategy != "remote" {
		t.Fatalf("flags = %+v", seen)
	}
	if !strings.HasSuffix(seen.configPath, "x.yaml") {
		t.Fatalf("configPath = %q", seen.configPath)
	}
	if strat != sync.StrategyRemote {
		t.Fatalf("strategy = %v", strat)
	}
	if !c.ran {
		t.Fatal("ran not set by prepare")
	}
}

func TestStrategyDefault(t *testing.T) {
	tests := []struct {
		name    string
		flag    string
		yes     bool
		want    sync.Strategy
		wantErr bool
	}{
		{"default interactive", "", false, sync.StrategyAsk, false},
		{"default with --yes", "", true, sync.StrategyAbort, false},
		{"explicit ask with --yes", "ask", true, sync.StrategyAsk, false},
		{"local", "local", false, sync.StrategyLocal, false},
		{"remote uppercase", "REMOTE", false, sync.StrategyRemote, false},
		{"abort", " abort ", false, sync.StrategyAbort, false},
		{"invalid", "merge", false, 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := globals{strategy: tt.flag, yes: tt.yes}.strategyValue()
			if tt.wantErr {
				var ue *usageError
				if err == nil || !errors.As(err, &ue) {
					t.Fatalf("want usage error, got %v", err)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("got %v, %v; want %v", got, err, tt.want)
			}
		})
	}
}

func TestExitCodeMapping(t *testing.T) {
	tests := []struct {
		name string
		err  error
		ran  bool
		want int
		msg  string
	}{
		{"nil", nil, true, ExitOK, ""},
		{"usage error", usagef("bad %s", "flag"), true, ExitUsage, "error: bad flag"},
		{"error before run is usage", errors.New("unknown command"), false, ExitUsage, "error: unknown command"},
		{"unresolved", errUnresolved, true, ExitConflicts, "error: unresolved conflicts"},
		{"wrapped unresolved", errors.Join(errors.New("x"), errUnresolved), true, ExitConflicts, ""},
		{"aborted", errAborted, true, ExitConflicts, "error: aborted"},
		{"interrupted", context.Canceled, true, ExitConflicts, "error: interrupted"},
		{"other", errors.New("boom"), true, ExitError, "error: boom"},
		{"non-interactive", ui.ErrNonInteractive, true, ExitError, "error: a prompt is required"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var errb bytes.Buffer
			c := newCLI(nil, Options{Stdout: &bytes.Buffer{}, Stderr: &errb})
			c.ran = tt.ran
			if got := c.exitCode(tt.err); got != tt.want {
				t.Fatalf("exit = %d, want %d (stderr %q)", got, tt.want, errb.String())
			}
			if tt.msg != "" && !strings.Contains(errb.String(), tt.msg) {
				t.Fatalf("stderr = %q, want %q", errb.String(), tt.msg)
			}
			if tt.err != nil && !strings.HasPrefix(errb.String(), "error: ") {
				t.Fatalf("stderr = %q, want the error: prefix", errb.String())
			}
		})
	}
}

func TestUsageErrorsExitTwo(t *testing.T) {
	isolate(t)
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"unknown command", []string{"frobnicate"}, "unknown command"},
		{"unknown flag", []string{"--bogus"}, "unknown flag"},
		{"missing arg", []string{"add"}, "accepts 1 arg"},
		{"too many args", []string{"unlock", "x"}, "unknown command"},
		{"restore without project", []string{"restore"}, "accepts 1 arg"},
		{"files add one arg", []string{"files", "add", "proj"}, "requires at least 2"},
		{"projects link one arg", []string{"projects", "link", "x"}, "accepts 2 arg"},
		{"invalid strategy", []string{"--strategy", "maybe", "status"}, "invalid --strategy"},
		{"bad older-than", []string{"trash", "purge", "--older-than", "soon"}, "--older-than"},
		{"empty path", []string{"scan", " "}, "empty path"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := runCLI(t, runOpts{}, tt.args...)
			if r.code != ExitUsage {
				t.Fatalf("exit = %d, want %d", r.code, ExitUsage)
			}
			if !strings.Contains(r.err, tt.want) || !strings.Contains(r.err, "--help") {
				t.Fatalf("stderr = %q, want %q and a --help hint", r.err, tt.want)
			}
		})
	}
}

func TestHelpExitsZero(t *testing.T) {
	isolate(t)
	for _, args := range [][]string{{"--help"}, {"help"}, {"sync", "--help"}, {"projects"}, {"files", "-h"}} {
		r := runCLI(t, runOpts{}, args...)
		if r.code != ExitOK {
			t.Fatalf("%v: exit = %d, stderr %q", args, r.code, r.err)
		}
		if !strings.Contains(r.out, "Usage") && !strings.Contains(r.out, "Available Commands") {
			t.Fatalf("%v: no help printed: %q", args, r.out)
		}
	}
}

func TestNoConfigNeedsInit(t *testing.T) {
	isolate(t)
	for _, args := range [][]string{{"unlock"}, {"sync", "--no-remote"}, {"status"}, {"projects", "list"}, {"files", "rm", "p", "x"}, {"projects", "unlink", "p"}} {
		r := runCLI(t, runOpts{}, args...)
		if r.code != ExitError {
			t.Fatalf("%v: exit = %d, want 1", args, r.code)
		}
		if !strings.Contains(r.err, "run `private-sync init` first") {
			t.Fatalf("%v: stderr = %q", args, r.err)
		}
	}
}

func TestMachineAndConfigCommands(t *testing.T) {
	root, dirs := isolate(t)

	r := runCLI(t, runOpts{}, "config", "path")
	if r.code != 0 || strings.TrimSpace(r.out) != dirs.ConfigFile() {
		t.Fatalf("config path: %d %q", r.code, r.out)
	}
	r = runCLI(t, runOpts{}, "config", "show")
	if r.code != 0 || !strings.Contains(r.out, "version: 1") || !strings.Contains(r.err, "does not exist") {
		t.Fatalf("config show (missing): %d %q %q", r.code, r.out, r.err)
	}
	r = runCLI(t, runOpts{}, "machine")
	if r.code != 0 || !strings.Contains(r.out, "id") {
		t.Fatalf("machine: %d %q", r.code, r.out)
	}
	r = runCLI(t, runOpts{}, "--json", "machine")
	if r.code != 0 || !strings.Contains(r.out, `"id":`) {
		t.Fatalf("machine --json: %d %q", r.code, r.out)
	}

	writeConfig(t, dirs, root, "unit-box")
	r = runCLI(t, runOpts{}, "config", "show")
	if r.code != 0 || !strings.Contains(r.out, "name: unit-box") || r.err != "" {
		t.Fatalf("config show: %d %q %q", r.code, r.out, r.err)
	}
	r = runCLI(t, runOpts{}, "machine")
	if r.code != 0 || !strings.Contains(r.out, "unit-box") {
		t.Fatalf("machine with config: %d %q", r.code, r.out)
	}

	// --config and $PRIVATE_SYNC_CONFIG select another file; the flag wins.
	alt := filepath.Join(root, "alt.yaml")
	t.Setenv(ConfigEnvVar, alt)
	r = runCLI(t, runOpts{}, "config", "path")
	if strings.TrimSpace(r.out) != alt {
		t.Fatalf("env config path = %q", r.out)
	}
	flagged := filepath.Join(root, "flag.yaml")
	r = runCLI(t, runOpts{}, "--config", flagged, "config", "path")
	if strings.TrimSpace(r.out) != flagged {
		t.Fatalf("flag config path = %q", r.out)
	}
}

func TestConfigEdit(t *testing.T) {
	_, dirs := isolate(t)
	if _, ok := execx.LookPath("true"); !ok {
		t.Skip("no `true` binary")
	}
	t.Setenv("EDITOR", "true")
	r := runCLI(t, runOpts{}, "config", "edit")
	if r.code != 0 {
		t.Fatalf("config edit: %d %q", r.code, r.err)
	}
	if _, err := os.Stat(dirs.ConfigFile()); err != nil {
		t.Fatalf("default config not created: %v", err)
	}
	if !strings.Contains(r.out, "is valid") {
		t.Fatalf("stdout = %q", r.out)
	}
	// $VISUAL wins over $EDITOR; a failing editor is an error.
	if _, ok := execx.LookPath("false"); ok {
		t.Setenv("VISUAL", "false")
		r = runCLI(t, runOpts{}, "config", "edit")
		if r.code != ExitError || !strings.Contains(r.err, "editor") {
			t.Fatalf("failing editor: %d %q", r.code, r.err)
		}
	}
	t.Setenv("VISUAL", "")
	t.Setenv("EDITOR", "definitely-not-an-editor-xyz")
	r = runCLI(t, runOpts{}, "config", "edit")
	if r.code != ExitError || !strings.Contains(r.err, "not found") {
		t.Fatalf("missing editor: %d %q", r.code, r.err)
	}
}

func TestEditorCommand(t *testing.T) {
	tests := []struct {
		visual, editor string
		want           []string
	}{
		{"", "", []string{"vi"}},
		{"", "nano", []string{"nano"}},
		{"code --wait", "nano", []string{"code", "--wait"}},
		{"  ", "vim -u NONE", []string{"vim", "-u", "NONE"}},
	}
	for _, tt := range tests {
		got := editorCommand(tt.visual, tt.editor)
		if strings.Join(got, " ") != strings.Join(tt.want, " ") {
			t.Errorf("editorCommand(%q, %q) = %v, want %v", tt.visual, tt.editor, got, tt.want)
		}
	}
}

func TestParseAge(t *testing.T) {
	tests := []struct {
		in      string
		want    time.Duration
		wantErr bool
	}{
		{"30d", 30 * 24 * time.Hour, false},
		{"1w", 7 * 24 * time.Hour, false},
		{"12h", 12 * time.Hour, false},
		{"1d12h", 36 * time.Hour, false},
		{"1w2d", 9 * 24 * time.Hour, false},
		{"2d1w", 9 * 24 * time.Hour, false},
		{"1w2d12h", 9*24*time.Hour + 12*time.Hour, false},
		{"1d1d", 48 * time.Hour, false},
		{"0d", 0, false},
		{"0s", 0, false},
		{" 2D ", 48 * time.Hour, false},
		{"", 0, true},
		{"soon", 0, true},
		{"d", 0, true},
		{"1x", 0, true},
		{"1d1x", 0, true},
		{"-1h", 0, true},
		{"1d-1h", 0, true},
		{"99999999999999999d", 0, true},
		{"999999999999999w", 0, true},
		{"1000000000d1000000000d", 0, true},
	}
	for _, tt := range tests {
		got, err := parseAge(tt.in)
		if (err != nil) != tt.wantErr || got != tt.want {
			t.Errorf("parseAge(%q) = %v, %v; want %v, err=%v", tt.in, got, err, tt.want, tt.wantErr)
		}
	}
}

func TestRelPaths(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sub", "a.env"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name      string
		in        []string
		mustExist bool
		want      []string
		wantErr   string
	}{
		{"relative", []string{"sub/a.env", "b.json"}, false, []string{"sub/a.env", "b.json"}, ""},
		{"dedup and clean", []string{"./sub/a.env", "sub//a.env"}, false, []string{"sub/a.env"}, ""},
		{"absolute inside", []string{filepath.Join(dir, "sub", "a.env")}, true, []string{"sub/a.env"}, ""},
		{"absolute outside", []string{filepath.Join(filepath.Dir(dir), "other")}, false, nil, "outside the project"},
		{"parent escape", []string{"../x"}, false, nil, "invalid project-relative path"},
		{"missing with mustExist", []string{"nope.env"}, true, nil, "no such file"},
		{"directory with mustExist", []string{"sub"}, true, nil, "not a regular file"},
		{"empty", []string{""}, false, nil, "empty path"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := relPaths(dir, tt.in, tt.mustExist)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil || strings.Join(got, ",") != strings.Join(tt.want, ",") {
				t.Fatalf("got %v, %v; want %v", got, err, tt.want)
			}
		})
	}
}

func TestBadgeAndDescribe(t *testing.T) {
	pp := func(items ...sync.Item) *sync.ProjectPlan { return &sync.ProjectPlan{ID: "p", Items: items} }
	item := func(a sync.Action) sync.Item { return sync.Item{Key: sync.ItemKey{Project: "p", Path: "f"}, Action: a} }
	tests := []struct {
		name string
		pp   *sync.ProjectPlan
		want string
	}{
		{"missing", &sync.ProjectPlan{Missing: true, Items: []sync.Item{item(sync.ActionConflict)}}, "path missing"},
		{"duplicate", &sync.ProjectPlan{DuplicateOf: "q"}, "duplicate of q"},
		{"unreadable", &sync.ProjectPlan{Unreadable: true}, "unreadable"},
		{"synced", pp(item(sync.ActionInSync)), "synced"},
		{"empty", pp(), "synced"},
		{"local", pp(item(sync.ActionUpload)), "local changes"},
		{"remote", pp(item(sync.ActionDownload)), "remote changes"},
		{"both", pp(item(sync.ActionUpload), item(sync.ActionDownload)), "local and remote changes"},
		{"conflict wins", pp(item(sync.ActionUpload), item(sync.ActionConflict)), "conflicts"},
		{"pending", pp(item(sync.ActionPending), item(sync.ActionUpload)), "pending"},
		{"rollback", pp(item(sync.ActionRollback)), "rollback"},
		{"missing locally", pp(item(sync.ActionMissingLocal)), "missing locally"},
	}
	for _, tt := range tests {
		if got := badge(tt.pp); got != tt.want {
			t.Errorf("%s: badge = %q, want %q", tt.name, got, tt.want)
		}
	}

	key := sync.ItemKey{Project: "p", Path: "f"}
	glyphTests := []struct {
		name    string
		it      sync.Item
		res     sync.Resolutions
		applied bool
		want    string
	}{
		{"upload", item(sync.ActionUpload), nil, true, glyphUp},
		{"download", item(sync.ActionDownload), nil, true, glyphDown},
		{"in sync", item(sync.ActionInSync), nil, false, glyphSame},
		{"converge", item(sync.ActionConverge), nil, true, glyphSame},
		{"conflict unresolved", sync.Item{Key: key, Action: sync.ActionConflict, NeedsResolution: true}, sync.Resolutions{}, true, glyphConflict},
		{"conflict skipped", sync.Item{Key: key, Action: sync.ActionConflict, NeedsResolution: true}, sync.Resolutions{key: {Kind: sync.ChooseSkip}}, true, glyphConflict},
		{"conflict local", sync.Item{Key: key, Action: sync.ActionConflict, NeedsResolution: true}, sync.Resolutions{key: {Kind: sync.ChooseLocal}}, true, glyphUp},
		{"conflict remote", sync.Item{Key: key, Action: sync.ActionConflict, NeedsResolution: true}, sync.Resolutions{key: {Kind: sync.ChooseRemote}}, true, glyphDown},
		{"pending", item(sync.ActionPending), nil, true, glyphPending},
		{"rollback", item(sync.ActionRollback), nil, true, glyphRollback},
		{"report only", item(sync.ActionReportOnly), nil, true, glyphInfo},
		{"missing", item(sync.ActionMissingLocal), nil, true, glyphMissing},
		{"delete remote confirmed", sync.Item{Key: key, Action: sync.ActionDeleteRemote, NeedsResolution: true}, sync.Resolutions{key: {Kind: sync.ChooseConfirm}}, true, glyphUp},
		{"delete remote unconfirmed", sync.Item{Key: key, Action: sync.ActionDeleteRemote, NeedsResolution: true}, sync.Resolutions{}, true, glyphConflict},
	}
	for _, tt := range glyphTests {
		glyph, _, _ := describe(&tt.it, sync.ModeSync, tt.res, tt.applied)
		if glyph != tt.want {
			t.Errorf("%s: glyph = %q, want %q", tt.name, glyph, tt.want)
		}
	}
}

func TestUnresolvedKeys(t *testing.T) {
	k1 := sync.ItemKey{Project: "p", Path: "a"}
	k2 := sync.ItemKey{Project: "p", Path: "b"}
	k3 := sync.ItemKey{Project: "p", Path: "c"}
	plan := &sync.Plan{Projects: []sync.ProjectPlan{{ID: "p", Items: []sync.Item{
		{Key: k1, Action: sync.ActionConflict, NeedsResolution: true},
		{Key: k2, Action: sync.ActionConflict, NeedsResolution: true},
		{Key: k3, Action: sync.ActionConflict, NeedsResolution: true},
		{Key: sync.ItemKey{Project: "p", Path: "d"}, Action: sync.ActionUpload},
	}}}}
	res := sync.Resolutions{k2: {Kind: sync.ChooseSkip}, k3: {Kind: sync.ChooseLocal}}
	rep := &sync.Report{Unresolved: []sync.ItemKey{k1}}
	got := unresolvedKeys(plan, res, rep)
	if len(got) != 2 || got[0] != k1 || got[1] != k2 {
		t.Fatalf("unresolved = %v", got)
	}
	if got := unresolvedKeys(plan, sync.Resolutions{k1: {Kind: sync.ChooseLocal}, k2: {Kind: sync.ChooseRemote}, k3: {Kind: sync.ChooseMerged}}, &sync.Report{}); len(got) != 0 {
		t.Fatalf("all resolved, got %v", got)
	}
}

func TestFormatSize(t *testing.T) {
	tests := map[int64]string{0: "0B", 512: "512B", 1024: "1.0KiB", 1536: "1.5KiB", 2 << 20: "2.0MiB", 3 << 30: "3.0GiB"}
	for in, want := range tests {
		if got := formatSize(in); got != want {
			t.Errorf("formatSize(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestIsInteractive(t *testing.T) {
	yes, no := true, false
	tests := []struct {
		name string
		g    globals
		ov   *bool
		want bool
	}{
		{"override true", globals{}, &yes, true},
		{"override false", globals{}, &no, false},
		{"--yes beats override", globals{yes: true}, &yes, false},
		{"buffer is not a terminal", globals{}, nil, false},
	}
	for _, tt := range tests {
		c := newCLI(nil, Options{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}, Interactive: tt.ov})
		c.g = tt.g
		if got := c.isInteractive(); got != tt.want {
			t.Errorf("%s: interactive = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestConfirmWithYes(t *testing.T) {
	c := newCLI(nil, Options{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}})
	c.g.yes = true
	c.prompter = ui.Silent{}
	ok, err := c.confirm(context.Background(), "really?", false)
	if err != nil || !ok {
		t.Fatalf("--yes confirm = %v, %v", ok, err)
	}
	c.g.yes = false
	ok, err = c.confirm(context.Background(), "really?", false)
	if err != nil || ok {
		t.Fatalf("silent confirm = %v, %v (want the default)", ok, err)
	}
	c.prompter = nil
	if _, err := c.confirm(context.Background(), "really?", true); !errors.Is(err, ui.ErrNonInteractive) {
		t.Fatalf("nil prompter err = %v", err)
	}
}
