// Package execx runs subprocesses (git, rclone, bw, $EDITOR) with a sanitised
// environment and provides a fake runner for tests.
package execx

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
)

// Cmd describes a subprocess invocation.
type Cmd struct {
	Name     string
	Args     []string
	Dir      string
	Env      []string          // extra KEY=VALUE entries added to the sanitised environment
	Stdin    []byte            // nil = no stdin (children never see the terminal)
	OnStderr func(line string) // optional: called for every stderr line as it arrives
}

// Result is the outcome of a subprocess.
type Result struct {
	Stdout   []byte
	Stderr   []byte
	ExitCode int
}

// ExitError is returned (wrapped) when the process exits with a non-zero status.
type ExitError struct {
	Cmd    Cmd
	Result Result
}

func (e *ExitError) Error() string {
	msg := strings.TrimSpace(string(e.Result.Stderr))
	if msg == "" {
		msg = strings.TrimSpace(string(e.Result.Stdout))
	}
	if len(msg) > 400 {
		msg = msg[:400] + "…"
	}
	return fmt.Sprintf("%s %s: exit %d: %s", e.Cmd.Name, strings.Join(e.Cmd.Args, " "), e.Result.ExitCode, msg)
}

// ExitCode extracts the exit code from an error returned by Run (-1 if not an ExitError).
func ExitCode(err error) int {
	var ee *ExitError
	if errors.As(err, &ee) {
		return ee.Result.ExitCode
	}
	return -1
}

// Runner executes commands.
type Runner interface {
	Run(ctx context.Context, c Cmd) (Result, error)
}

// Denylist lists environment variables that are never passed to children.
func Denylist() []string {
	return []string{"PRIVATE_SYNC_PASSPHRASE", "BW_PASSWORD", "BW_SESSION", "BW_CLIENTSECRET", "BW_CLIENTID"}
}

// SanitizedEnv returns os.Environ() minus the denylist, plus extra entries.
func SanitizedEnv(extra []string) []string {
	deny := map[string]bool{}
	for _, k := range Denylist() {
		deny[k] = true
	}
	var env []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if deny[k] {
			continue
		}
		env = append(env, kv)
	}
	return append(env, extra...)
}

type realRunner struct{}

// Real returns a Runner backed by os/exec.
func Real() Runner { return realRunner{} }

func (realRunner) Run(ctx context.Context, c Cmd) (Result, error) {
	cmd := exec.CommandContext(ctx, c.Name, c.Args...)
	cmd.Dir = c.Dir
	cmd.Env = SanitizedEnv(c.Env)
	if c.Stdin != nil {
		cmd.Stdin = bytes.NewReader(c.Stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	var stderrWriter io.Writer = &stderr
	var wg sync.WaitGroup
	if c.OnStderr != nil {
		pr, pw := io.Pipe()
		stderrWriter = io.MultiWriter(&stderr, pw)
		wg.Add(1)
		go func() {
			defer wg.Done()
			sc := bufio.NewScanner(pr)
			sc.Buffer(make([]byte, 64*1024), 1024*1024)
			for sc.Scan() {
				c.OnStderr(sc.Text())
			}
		}()
		defer func() { _ = pw.Close(); wg.Wait() }()
	}
	cmd.Stderr = stderrWriter
	err := cmd.Run()
	res := Result{Stdout: stdout.Bytes(), Stderr: stderr.Bytes()}
	if err != nil {
		var xe *exec.ExitError
		if errors.As(err, &xe) {
			res.ExitCode = xe.ExitCode()
			return res, &ExitError{Cmd: c, Result: res}
		}
		res.ExitCode = -1
		return res, fmt.Errorf("run %s: %w", c.Name, err)
	}
	return res, nil
}

// Fake returns a Runner that delegates to handler (for tests).
func Fake(handler func(c Cmd) (Result, error)) Runner { return fakeRunner{h: handler} }

type fakeRunner struct{ h func(c Cmd) (Result, error) }

func (f fakeRunner) Run(_ context.Context, c Cmd) (Result, error) {
	res, err := f.h(c)
	if err == nil && res.ExitCode != 0 {
		return res, &ExitError{Cmd: c, Result: res}
	}
	if c.OnStderr != nil && len(res.Stderr) > 0 {
		for _, l := range strings.Split(strings.TrimRight(string(res.Stderr), "\n"), "\n") {
			c.OnStderr(l)
		}
	}
	return res, err
}

// LookPath reports whether a binary is available on PATH.
func LookPath(name string) (string, bool) {
	p, err := exec.LookPath(name)
	return p, err == nil
}
