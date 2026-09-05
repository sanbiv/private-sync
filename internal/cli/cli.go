// Package cli implements the cobra commands (spec §2.1).
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	syncpkg "sync"
	"syscall"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/sanbiv/private-sync/internal/app"
	"github.com/sanbiv/private-sync/internal/config"
	"github.com/sanbiv/private-sync/internal/execx"
	"github.com/sanbiv/private-sync/internal/keysource"
	"github.com/sanbiv/private-sync/internal/paths"
	"github.com/sanbiv/private-sync/internal/sync"
	"github.com/sanbiv/private-sync/internal/ui"
)

// Frontend is the interactive UI injected by main (implemented by package tui).
// Keeping it an interface lets cli and tui build and test independently.
type Frontend interface {
	RunSetup(ctx context.Context, dirs paths.Dirs, existing *config.Config) (*config.Config, error)
	Run(ctx context.Context, s *app.Session) error
	RunAddProject(ctx context.Context, s *app.Session, dir string) error
	ResolveConflicts(ctx context.Context, s *app.Session, p *sync.Plan) (res sync.Resolutions, aborted bool, err error)
}

// Exit codes returned by Main and Run.
const (
	ExitOK        = 0 // success
	ExitError     = 1 // any error
	ExitUsage     = 2 // bad flags or arguments
	ExitConflicts = 3 // unresolved conflicts, or the user aborted
)

// ConfigEnvVar names the environment variable that overrides the config path
// (the --config flag wins over it).
const ConfigEnvVar = "PRIVATE_SYNC_CONFIG"

// Options customise Run: output streams, subprocess runner and interactivity.
// Zero values select the process defaults (os.Stdout, os.Stderr, execx.Real,
// TerminalPrompter, "interactive when stdout is a terminal").
type Options struct {
	Stdout io.Writer
	Stderr io.Writer
	// Interactive overrides the terminal detection used to decide whether the
	// conflict resolver may run (nil = stdout is a terminal). --yes always
	// forces non-interactive behaviour.
	Interactive *bool
	// Prompter replaces the TerminalPrompter (tests).
	Prompter ui.Prompter
	// Runner replaces execx.Real for git/rclone/bw subprocesses (tests).
	Runner execx.Runner

	// remoteFactory replaces remote.New for the opened vault (tests: a
	// remote whose Push fails).
	remoteFactory app.RemoteFactory
}

var (
	// errUnresolved is returned by the sync flows when conflicts remain (exit 3).
	errUnresolved = errors.New("unresolved conflicts")
	// errAborted is returned when the user aborted an interactive step (exit 3).
	errAborted = errors.New("aborted")
	// errNoConfig is returned by commands that need the vault on a first run.
	errNoConfig = errors.New("no configuration found: run `private-sync init` first")
)

// usageError marks bad flags/arguments (exit 2).
type usageError struct{ err error }

func (e *usageError) Error() string { return e.err.Error() }
func (e *usageError) Unwrap() error { return e.err }

// usagef builds a usage error.
func usagef(format string, a ...any) error { return &usageError{err: fmt.Errorf(format, a...)} }

// Main runs the CLI with the given arguments and returns the process exit code.
//
// The first SIGINT/SIGTERM cancels the context (prompts and the engine return
// "interrupted", exit 3); the handler is then removed so that a second one
// terminates the process with the default disposition even while a prompt is
// still waiting for a line.
func Main(args []string, fe Frontend) int {
	keysource.CaptureEnv()
	ctx, stop := interruptContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return Run(ctx, args, fe, Options{})
}

// interruptContext returns a context cancelled by the first of sigs. Unlike
// signal.NotifyContext the handler is removed as soon as that first signal
// arrives, so the next one keeps its default disposition: a process blocked
// in a terminal read (which only returns on Enter) can still be killed with
// a second ctrl+c. stop releases the handler and cancels the context.
func interruptContext(parent context.Context, sigs ...os.Signal) (context.Context, context.CancelFunc) {
	return interruptContextWith(parent, signal.Notify, signal.Stop, sigs...)
}

// interruptContextWith is interruptContext with the signal registration
// injected (tests). stop is called exactly once, before the cancellation.
func interruptContextWith(parent context.Context, notify func(chan<- os.Signal, ...os.Signal), stop func(chan<- os.Signal), sigs ...os.Signal) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)
	ch := make(chan os.Signal, 1)
	notify(ch, sigs...)
	var once syncpkg.Once
	release := func() { once.Do(func() { stop(ch) }) }
	go func() {
		select {
		case <-ch:
			release()
			cancel()
		case <-ctx.Done():
			release()
		}
	}()
	return ctx, func() {
		release()
		cancel()
	}
}

// Run is Main with an explicit context and options (embedding, tests).
func Run(ctx context.Context, args []string, fe Frontend, o Options) int {
	keysource.CaptureEnv()
	if ctx == nil {
		ctx = context.Background()
	}
	return newCLI(fe, o).run(ctx, args)
}

// globals holds the parsed global flags.
type globals struct {
	configPath     string
	yes            bool
	strategy       string
	delete         bool
	noRemote       bool
	acceptRollback bool
	json           bool
}

// strategyValue maps --strategy to the engine strategy: "ask" by default,
// "abort" by default with --yes.
func (g globals) strategyValue() (sync.Strategy, error) {
	switch strings.ToLower(strings.TrimSpace(g.strategy)) {
	case "":
		if g.yes {
			return sync.StrategyAbort, nil
		}
		return sync.StrategyAsk, nil
	case "ask":
		return sync.StrategyAsk, nil
	case "local":
		return sync.StrategyLocal, nil
	case "remote":
		return sync.StrategyRemote, nil
	case "abort":
		return sync.StrategyAbort, nil
	}
	return 0, usagef("invalid --strategy %q (want ask, local, remote or abort)", g.strategy)
}

// cli is one invocation: streams, parsed flags and the injected front end.
type cli struct {
	fe          Frontend
	out         io.Writer
	errOut      io.Writer
	injected    ui.Prompter // Options.Prompter
	prompter    ui.Prompter // set by prepare (after the flags are parsed)
	runner      execx.Runner
	interactive *bool

	g        globals
	strategy sync.Strategy
	color    bool
	// ran is set once a command reached its run phase: an error returned
	// before that (unknown command, bad flag, wrong argument count) is a usage error.
	ran bool
	// remoteFactory is Options.remoteFactory (nil = remote.New).
	remoteFactory app.RemoteFactory
	// linked is the project `restore --path` linked in this run; the JSON
	// report carries it since the text line is not printed with --json.
	linked *linkView
}

func newCLI(fe Frontend, o Options) *cli {
	c := &cli{
		fe:            fe,
		out:           o.Stdout,
		errOut:        o.Stderr,
		injected:      o.Prompter,
		runner:        o.Runner,
		interactive:   o.Interactive,
		remoteFactory: o.remoteFactory,
	}
	if c.out == nil {
		c.out = os.Stdout
	}
	if c.errOut == nil {
		c.errOut = os.Stderr
	}
	if c.runner == nil {
		c.runner = execx.Real()
	}
	return c
}

// run executes the command tree and maps the outcome to an exit code.
func (c *cli) run(ctx context.Context, args []string) int {
	root := c.rootCommand()
	root.SetArgs(args)
	return c.exitCode(root.ExecuteContext(ctx))
}

// exitCode prints err (if any) to stderr and returns the process exit code.
func (c *cli) exitCode(err error) int {
	if err == nil {
		return ExitOK
	}
	code := ExitError
	var ue *usageError
	switch {
	case errors.As(err, &ue), !c.ran:
		code = ExitUsage
	case errors.Is(err, errUnresolved), errors.Is(err, errAborted):
		code = ExitConflicts
	case errors.Is(err, context.Canceled):
		code = ExitConflicts
	}
	msg := err.Error()
	if errors.Is(err, context.Canceled) && !errors.Is(err, errAborted) {
		msg = "interrupted"
	}
	fmt.Fprintf(c.errOut, "error: %s\n", msg)
	if code == ExitUsage {
		fmt.Fprintln(c.errOut, "run 'private-sync --help' for usage")
	}
	return code
}

// rootCommand builds the command tree.
func (c *cli) rootCommand() *cobra.Command {
	root := &cobra.Command{
		Use:   "private-sync",
		Short: "Keep your projects' secret files encrypted and in sync",
		Long: `private-sync finds configuration and secret files (.env, *.yaml, keys, ...)
inside your projects, stores them encrypted in a local vault and keeps that
vault in sync across machines through git, rclone or any folder that a desktop
client already synchronises.

Without a subcommand the interactive dashboard starts (the setup wizard on
the first run).`,
		Args:          noArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			return c.prepare(cmd)
		},
		RunE: c.runRoot,
	}
	root.SetOut(c.out)
	root.SetErr(c.errOut)
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return &usageError{err: err} })
	root.CompletionOptions.HiddenDefaultCmd = true

	pf := root.PersistentFlags()
	pf.StringVar(&c.g.configPath, "config", "", "config file (default $"+ConfigEnvVar+" or ~/.config/private-sync/config.yaml)")
	pf.BoolVar(&c.g.yes, "yes", false, "non-interactive: accept confirmations (never deletes on its own)")
	pf.StringVar(&c.g.strategy, "strategy", "", "conflict strategy: ask|local|remote|abort (default ask, or abort with --yes)")
	pf.BoolVar(&c.g.delete, "delete", false, "propagate local deletions to the vault (rsync convention)")
	pf.BoolVar(&c.g.noRemote, "no-remote", false, "skip fetch and push")
	pf.BoolVar(&c.g.acceptRollback, "accept-rollback", false, "apply vault versions older than the last synced one")
	pf.BoolVar(&c.g.json, "json", false, "machine readable output")

	root.AddCommand(
		c.initCommand(),
		c.addCommand(),
		c.scanCommand(),
		c.statusCommand(),
		c.syncCommand("sync", sync.ModeSync, "Bidirectional sync with merge"),
		c.syncCommand("push", sync.ModePush, "Local → vault; remote-only changes are reported, not applied"),
		c.syncCommand("pull", sync.ModePull, "Vault → local; local-only changes are reported, not applied"),
		c.restoreCommand(),
		c.projectsCommand(),
		c.filesCommand(),
		c.trashCommand(),
		c.passphraseCommand(),
		c.configCommand(),
		c.unlockCommand(),
		c.machineCommand(),
	)
	return root
}

// prepare runs before every command: flags are parsed, so the strategy and
// the prompter can be fixed.
func (c *cli) prepare(_ *cobra.Command) error {
	c.ran = true
	strat, err := c.g.strategyValue()
	if err != nil {
		return err
	}
	c.strategy = strat
	c.prompter = c.injected
	if c.prompter == nil {
		c.prompter = &TerminalPrompter{AssumeYes: c.g.yes}
	}
	c.color = !c.g.json && isTerminal(c.out) && os.Getenv("NO_COLOR") == "" && os.Getenv("TERM") != "dumb"
	return nil
}

// Positional argument validators that report usage errors (exit 2).

func noArgs(cmd *cobra.Command, args []string) error {
	if err := cobra.NoArgs(cmd, args); err != nil {
		return &usageError{err: err}
	}
	return nil
}

func exactArgs(n int) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if err := cobra.ExactArgs(n)(cmd, args); err != nil {
			return &usageError{err: err}
		}
		return nil
	}
}

func minArgs(n int) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if err := cobra.MinimumNArgs(n)(cmd, args); err != nil {
			return &usageError{err: err}
		}
		return nil
	}
}

// groupCommand is a parent without a run function: it prints its help.
func groupCommand(use, short string) *cobra.Command {
	return &cobra.Command{
		Use:   use,
		Short: short,
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
}

// isTerminal reports whether w is a terminal file.
func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	return ok && term.IsTerminal(int(f.Fd()))
}

// isInteractive reports whether the front end may take over the terminal:
// no --yes and stdout is a terminal (or Options.Interactive).
func (c *cli) isInteractive() bool {
	if c.g.yes {
		return false
	}
	if c.interactive != nil {
		return *c.interactive
	}
	return isTerminal(c.out)
}

// warn prints a warning line to stderr.
func (c *cli) warn(msg string) {
	msg = strings.TrimSpace(msg)
	if msg == "" {
		return
	}
	fmt.Fprintf(c.errOut, "warning: %s\n", msg)
}

// confirm asks a yes/no question; --yes answers true without prompting.
func (c *cli) confirm(ctx context.Context, question string, def bool) (bool, error) {
	if c.g.yes {
		return true, nil
	}
	if c.prompter == nil {
		return false, ui.ErrNonInteractive
	}
	return c.prompter.Confirm(ctx, question, def)
}

// dirs resolves the per-user directories.
func (c *cli) dirs() (paths.Dirs, error) {
	d, err := paths.Default()
	if err != nil {
		return paths.Dirs{}, fmt.Errorf("resolve user directories: %w", err)
	}
	return d, nil
}

// configPath returns the config file to use: --config, $PRIVATE_SYNC_CONFIG
// or the default location.
func (c *cli) configPath(dirs paths.Dirs) (string, error) {
	p := strings.TrimSpace(c.g.configPath)
	if p == "" {
		p = strings.TrimSpace(os.Getenv(ConfigEnvVar))
	}
	if p == "" {
		return dirs.ConfigFile(), nil
	}
	exp, err := paths.ExpandHome(p)
	if err != nil {
		return "", fmt.Errorf("config path %s: %w", p, err)
	}
	abs, err := filepath.Abs(exp)
	if err != nil {
		return "", fmt.Errorf("config path %s: %w", p, err)
	}
	return abs, nil
}

// loadApp loads the config and the machine identity (no key material).
func (c *cli) loadApp() (*app.App, error) {
	dirs, err := c.dirs()
	if err != nil {
		return nil, err
	}
	path, err := c.configPath(dirs)
	if err != nil {
		return nil, err
	}
	a, err := app.Load(path, dirs, c.runner)
	if err != nil {
		return nil, err
	}
	a.NewRemote = c.remoteFactory
	for _, w := range a.Config.Warnings() {
		c.warn(w)
	}
	return a, nil
}

// loadConfig loads only the config file (commands that never touch the vault
// or the state dir). A missing file yields the defaults so that `scan` works
// before `init`.
func (c *cli) loadConfig() (cfg *config.Config, path string, err error) {
	dirs, err := c.dirs()
	if err != nil {
		return nil, "", err
	}
	path, err = c.configPath(dirs)
	if err != nil {
		return nil, "", err
	}
	cfg, err = config.Load(path)
	if err != nil {
		if errors.Is(err, config.ErrNotFound) {
			return config.Default(dirs), path, nil
		}
		return nil, path, err
	}
	if err := cfg.Validate(); err != nil {
		return nil, path, fmt.Errorf("config %s: %w", path, err)
	}
	for _, w := range cfg.Warnings() {
		c.warn(w)
	}
	return cfg, path, nil
}

// openSession loads the app and opens the vault (key obtained through the
// prompter). No fetch.
func (c *cli) openSession(ctx context.Context) (*app.Session, error) {
	a, err := c.loadApp()
	if err != nil {
		if errors.Is(err, app.ErrNoConfig) {
			return nil, errNoConfig
		}
		return nil, err
	}
	s, err := a.Open(ctx, c.prompter)
	if err != nil {
		return nil, err
	}
	s.Warn = c.warn
	return s, nil
}

// progress prints remote activity (fetch/push) to stderr; apply events are
// summarised by the report instead.
func (c *cli) progress(ev sync.Event) {
	if ev.Stage != "fetch" && ev.Stage != "push" {
		return
	}
	switch {
	case ev.Err != nil && ev.Message != "":
		fmt.Fprintf(c.errOut, "%s: %s: %v\n", ev.Stage, ev.Message, ev.Err)
	case ev.Err != nil:
		fmt.Fprintf(c.errOut, "%s: %v\n", ev.Stage, ev.Err)
	case ev.Message != "":
		fmt.Fprintf(c.errOut, "%s: %s\n", ev.Stage, ev.Message)
	}
}

// fetch pulls the remote unless --no-remote. A failed fetch is a warning
// (the local vault copy is used); only a cancelled context is an error.
func (c *cli) fetch(ctx context.Context, s *app.Session) error {
	if c.g.noRemote {
		return nil
	}
	if err := s.Engine.Fetch(ctx, c.progress); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		c.warn(fmt.Sprintf("fetch failed: %v (continuing with the local vault copy)", err))
	}
	return nil
}

// push uploads the vault changes unless --no-remote.
func (c *cli) push(ctx context.Context, s *app.Session) error {
	if c.g.noRemote {
		return nil
	}
	if err := s.Engine.Push(ctx, c.progress); err != nil {
		return fmt.Errorf("push: %w", err)
	}
	return nil
}

// resolveProjects maps ids or names of linked projects to ids.
func resolveProjects(s *app.Session, refs []string) ([]string, error) {
	ids := make([]string, 0, len(refs))
	seen := map[string]bool{}
	for _, r := range refs {
		p, err := s.ResolveProject(r)
		if err != nil {
			return nil, err
		}
		if seen[p.ID] {
			continue
		}
		seen[p.ID] = true
		ids = append(ids, p.ID)
	}
	return ids, nil
}

// absDir expands ~, makes p absolute and checks that it is a directory.
func absDir(p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" {
		return "", usagef("empty path")
	}
	exp, err := paths.ExpandHome(p)
	if err != nil {
		return "", fmt.Errorf("%s: %w", p, err)
	}
	abs, err := filepath.Abs(exp)
	if err != nil {
		return "", fmt.Errorf("%s: %w", p, err)
	}
	abs = filepath.Clean(abs)
	st, err := os.Stat(abs)
	if err != nil {
		return "", err
	}
	if !st.IsDir() {
		return "", fmt.Errorf("%s is not a directory", abs)
	}
	return abs, nil
}

// display renders a path for humans (home contracted to ~).
func display(p string) string {
	if p == "" {
		return ""
	}
	return paths.ContractHome(p)
}
