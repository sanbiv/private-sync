package remote

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sanbiv/private-sync/internal/config"
	"github.com/sanbiv/private-sync/internal/execx"
)

// rawGitErr runs git and returns its error (nil on success); used to assert
// that a ref does not exist.
func rawGitErr(dir string, args ...string) error {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
	return cmd.Run()
}

func TestNewDispatch(t *testing.T) {
	runner := execx.Fake(func(execx.Cmd) (execx.Result, error) { return execx.Result{}, nil })
	opts := Options{MachineID: mid, Runner: runner}
	cases := []struct {
		name     string
		cfg      config.RemoteConfig
		wantName string
		wantErr  error
	}{
		{"none", config.RemoteConfig{Type: config.RemoteNone}, "none", nil},
		{"empty type", config.RemoteConfig{}, "none", nil},
		{"git", config.RemoteConfig{Type: config.RemoteGit, Git: config.GitRemote{URL: "git@h:r.git"}}, "git", nil},
		{"rclone", config.RemoteConfig{Type: config.RemoteRclone, Rclone: config.RcloneRemote{Remote: "gdrive", Path: "v"}}, "rclone", nil},
		{"unknown", config.RemoteConfig{Type: "ftp"}, "", ErrUnsupported},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, err := New(tc.cfg, filepath.Join(t.TempDir(), "v"), opts)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if r.Name() != tc.wantName {
				t.Errorf("Name = %q, want %q", r.Name(), tc.wantName)
			}
		})
	}
	if _, err := New(config.RemoteConfig{Type: config.RemoteGit}, "v", opts); err == nil {
		t.Error("git without url accepted")
	}
}

func TestNoneIsNoop(t *testing.T) {
	var n Remote = None{}
	ctx := context.Background()
	if n.Name() != "none" {
		t.Errorf("Name = %q", n.Name())
	}
	if err := n.Prepare(ctx, nil); err != nil {
		t.Error(err)
	}
	if err := n.Fetch(ctx, nil); err != nil {
		t.Error(err)
	}
	if err := n.Push(ctx, []string{"x"}, nil); err != nil {
		t.Error(err)
	}
}

func TestOwnFiles(t *testing.T) {
	got := OwnFiles(mid)
	want := []string{"machines/" + mid + ".json.enc", "projects/*/meta/" + mid + ".json.enc", "projects/*/state/" + mid + ".json.enc"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("OwnFiles = %v", got)
	}
}

func TestVaultConflictError(t *testing.T) {
	var err error = &VaultConflict{Dir: "/v", LocalID: "L", RemoteID: "R"}
	if !errors.Is(err, ErrVaultConflict) {
		t.Error("not ErrVaultConflict")
	}
	for _, s := range []string{"/v", `"L"`, `"R"`, ErrVaultConflict.Error()} {
		if !strings.Contains(err.Error(), s) {
			t.Errorf("message lacks %q: %v", s, err)
		}
	}
	wrapped := errors.Join(errors.New("ctx"), err)
	var vc *VaultConflict
	if !errors.As(wrapped, &vc) || vc.LocalID != "L" {
		t.Error("errors.As through wrapping failed")
	}
}

func TestRebaseConflictError(t *testing.T) {
	cause := errors.New("boom")
	err := &RebaseConflictError{Files: []string{"a", "b"}, Cause: cause}
	if !strings.Contains(err.Error(), "a, b") || !errors.Is(err, cause) {
		t.Errorf("err = %v", err)
	}
}

func TestClassifiedUnwrap(t *testing.T) {
	cause := &execx.ExitError{Cmd: execx.Cmd{Name: "git"}, Result: execx.Result{ExitCode: 128}}
	err := &classified{kind: ErrAuth, msg: "denied", cause: cause}
	if !errors.Is(err, ErrAuth) {
		t.Error("kind not unwrapped")
	}
	var ee *execx.ExitError
	if !errors.As(err, &ee) || ee.Result.ExitCode != 128 {
		t.Error("cause not unwrapped")
	}
	if err.Error() != ErrAuth.Error()+": denied" {
		t.Errorf("message = %q", err.Error())
	}
}

func TestVaultRel(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "vault")
	cases := []struct {
		in     string
		want   string
		wantOK bool
	}{
		{"blobs/aa/x.enc", "blobs/aa/x.enc", true},
		{"./blobs/aa/x.enc", "blobs/aa/x.enc", true},
		{"blobs//aa/../aa/x.enc", "blobs/aa/x.enc", true},
		{filepath.Join(dir, "blobs", "aa", "x.enc"), "blobs/aa/x.enc", true},
		{"  vault.json ", "vault.json", true},
		{"", "", false},
		{".", "", false},
		{"..", "", false},
		{"../x", "", false},
		{"blobs/../../x", "", false},
		{filepath.Join(filepath.Dir(dir), "elsewhere", "x"), "", false},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, ok := vaultRel(dir, tc.in)
			if ok != tc.wantOK || got != tc.want {
				t.Errorf("vaultRel(%q) = %q, %v; want %q, %v", tc.in, got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

func TestVaultID(t *testing.T) {
	cases := []struct {
		name   string
		in     string
		want   string
		wantOK bool
	}{
		{"ok", `{"version":1,"id":"abc","kdf":{}}`, "abc", true},
		{"missing", `{"version":1}`, "", false},
		{"blank", `{"id":"  "}`, "", false},
		{"garbage", `not json`, "", false},
		{"empty", ``, "", false},
		{"wrong type", `{"id":5}`, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := vaultID([]byte(tc.in))
			if (err == nil) != tc.wantOK || got != tc.want {
				t.Errorf("vaultID = %q, %v", got, err)
			}
		})
	}
	dir := t.TempDir()
	if _, present, err := localVaultID(dir); present || err != nil {
		t.Errorf("absent vault.json: present=%v err=%v", present, err)
	}
	mustWrite(t, dir, "vault.json", "nope")
	if _, present, err := localVaultID(dir); !present || err == nil {
		t.Errorf("garbage vault.json: present=%v err=%v", present, err)
	}
	mustWrite(t, dir, "vault.json", `{"id":"z"}`)
	if id, present, err := localVaultID(dir); !present || err != nil || id != "z" {
		t.Errorf("vault.json: id=%q present=%v err=%v", id, present, err)
	}
}

func TestWriteFileIfAbsent(t *testing.T) {
	dir := t.TempDir()
	name := filepath.Join(dir, "f")
	wrote, err := writeFileIfAbsent(name, []byte("one"))
	if err != nil || !wrote {
		t.Fatalf("first write: wrote=%v err=%v", wrote, err)
	}
	st, err := os.Stat(name)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("mode = %o", st.Mode().Perm())
	}
	wrote, err = writeFileIfAbsent(name, []byte("two"))
	if err != nil || wrote {
		t.Fatalf("second write: wrote=%v err=%v", wrote, err)
	}
	if got := mustRead(t, dir, "f"); got != "one" {
		t.Errorf("content = %q", got)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".psv-tmp-") {
			t.Errorf("temp file left behind: %s", e.Name())
		}
	}
	if _, err := writeFileIfAbsent(filepath.Join(dir, "missing", "f"), []byte("x")); err == nil {
		t.Error("write into a missing directory succeeded")
	}
}
