package remote

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/sanbiv/private-sync/internal/execx"
)

// classified wraps a subprocess failure with a sentinel (ErrAuth, ErrNetwork)
// and a short human message, while still unwrapping to the *execx.ExitError.
type classified struct {
	kind  error
	msg   string
	cause error
}

func (e *classified) Error() string { return e.kind.Error() + ": " + e.msg }

// Unwrap exposes both the sentinel and the cause to errors.Is / errors.As.
func (e *classified) Unwrap() []error { return []error{e.kind, e.cause} }

// logger returns log or a no-op when log is nil.
func logger(log func(string)) func(string) {
	if log == nil {
		return func(string) {}
	}
	return log
}

// stderrOf returns the combined stderr+stdout text of a subprocess failure
// ("" when err is not an *execx.ExitError).
func stderrOf(err error) string {
	var ee *execx.ExitError
	if !errors.As(err, &ee) {
		return ""
	}
	return string(ee.Result.Stderr) + "\n" + string(ee.Result.Stdout)
}

// matchLine returns the first line of text containing any of the patterns
// (case-insensitive), trimmed; "" when none matches.
func matchLine(text string, patterns []string) string {
	for _, line := range strings.Split(text, "\n") {
		lower := strings.ToLower(line)
		for _, p := range patterns {
			if strings.Contains(lower, p) {
				return strings.TrimSpace(line)
			}
		}
	}
	return ""
}

// containsAny reports whether the lower-cased text contains any pattern.
func containsAny(text string, patterns []string) bool {
	lower := strings.ToLower(text)
	for _, p := range patterns {
		if strings.Contains(lower, p) {
			return true
		}
	}
	return false
}

// writeFileIfAbsent atomically writes content to name (0600) unless the file
// already exists. It reports whether it wrote the file.
func writeFileIfAbsent(name string, content []byte) (bool, error) {
	if _, err := os.Lstat(name); err == nil {
		return false, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return false, err
	}
	dir := filepath.Dir(name)
	tmp, err := os.CreateTemp(dir, ".psv-tmp-*")
	if err != nil {
		return false, err
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if _, err := tmp.Write(content); err != nil {
		_ = tmp.Close()
		cleanup()
		return false, err
	}
	if err := tmp.Chmod(0o600); err != nil && !errors.Is(err, errors.ErrUnsupported) {
		_ = tmp.Close()
		cleanup()
		return false, err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return false, err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return false, err
	}
	// Re-check right before the rename so a concurrent writer is not clobbered.
	if _, err := os.Lstat(name); err == nil {
		cleanup()
		return false, nil
	}
	if err := os.Rename(tmpName, name); err != nil {
		cleanup()
		return false, err
	}
	return true, nil
}

// vaultID decodes the "id" field of a vault.json document.
func vaultID(data []byte) (string, error) {
	var doc struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return "", fmt.Errorf("vault.json: %w", err)
	}
	if strings.TrimSpace(doc.ID) == "" {
		return "", errors.New("vault.json: missing id")
	}
	return doc.ID, nil
}

// localVaultID reads <dir>/vault.json and returns its id; ok is false when
// the file does not exist. A present but unreadable file yields an error.
func localVaultID(dir string) (id string, ok bool, err error) {
	data, err := os.ReadFile(filepath.Join(dir, vaultFileName))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", false, nil
		}
		return "", false, err
	}
	id, err = vaultID(data)
	if err != nil {
		return "", true, err
	}
	return id, true, nil
}

// vaultRel normalises a written path to a clean, slash-separated path relative
// to vaultDir. Absolute paths inside the vault are made relative; anything that
// escapes the vault, is absolute elsewhere, or is empty yields ok=false.
func vaultRel(vaultDir, p string) (string, bool) {
	p = strings.TrimSpace(p)
	if p == "" {
		return "", false
	}
	if filepath.IsAbs(p) {
		rel, err := filepath.Rel(vaultDir, p)
		if err != nil {
			return "", false
		}
		p = rel
	}
	s := path.Clean(filepath.ToSlash(p))
	s = strings.TrimPrefix(s, "./")
	if s == "." || s == "" || s == ".." || strings.HasPrefix(s, "../") || strings.HasPrefix(s, "/") {
		return "", false
	}
	return s, true
}

// isRegularFile reports whether name exists and is a regular file.
func isRegularFile(name string) bool {
	st, err := os.Stat(name)
	return err == nil && st.Mode().IsRegular()
}
