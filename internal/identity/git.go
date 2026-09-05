package identity

import (
	"bufio"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/sanbiv/private-sync/internal/execx"
)

var errNotDir = errors.New("not a directory")

// gitViaRunner asks git for the remote URLs and the prefix of dir inside its
// repository. ok is false when the runner is nil, git is missing, dir is not
// inside a repository, or the output cannot be parsed; callers then fall back to
// gitViaFilesystem. An existing repository without remotes yields ok=true and
// no URLs.
func gitViaRunner(ctx context.Context, dir string, r execx.Runner) (urls []string, prefix string, ok bool) {
	if r == nil {
		return nil, "", false
	}
	res, err := r.Run(ctx, execx.Cmd{
		Name: "git",
		Args: []string{"-C", dir, "rev-parse", "--show-toplevel", "--show-prefix"},
	})
	if err != nil {
		return nil, "", false
	}
	lines := splitLines(string(res.Stdout))
	if len(lines) == 0 || strings.TrimSpace(lines[0]) == "" {
		return nil, "", false
	}
	if len(lines) > 1 {
		prefix = strings.TrimSuffix(strings.TrimSpace(lines[1]), "/")
	}

	res, err = r.Run(ctx, execx.Cmd{
		Name: "git",
		Args: []string{"-C", dir, "config", "--get-regexp", `^remote\..*\.url$`},
	})
	if err != nil {
		// git config --get-regexp exits 1 when nothing matches: a repository
		// without remotes. Anything else (git broken, ctx cancelled) → fallback.
		if execx.ExitCode(err) == 1 && len(strings.TrimSpace(string(res.Stdout))) == 0 {
			return nil, prefix, true
		}
		return nil, "", false
	}
	for _, l := range splitLines(string(res.Stdout)) {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		// "remote.<name>.url <url>"; the URL may itself contain spaces only in
		// pathological cases, so split on the first whitespace run.
		key, val, found := strings.Cut(l, " ")
		if !found {
			key, val, found = strings.Cut(l, "\t")
		}
		if !found {
			continue
		}
		if !strings.HasPrefix(key, "remote.") || !strings.HasSuffix(key, ".url") {
			continue
		}
		val = strings.TrimSpace(val)
		if val != "" {
			urls = append(urls, val)
		}
	}
	return urls, prefix, true
}

// gitViaFilesystem walks up from dir to the nearest .git directory or ".git"
// file ("gitdir: <path>" pointer) and parses the repository config for remote
// URLs. prefix is the "/"-separated path of dir relative to the repository top.
func gitViaFilesystem(dir string) (urls []string, prefix string) {
	top, gitDir, found := findGitDir(dir)
	if !found {
		return nil, ""
	}
	cfg := filepath.Join(gitDir, "config")
	if _, err := os.Stat(cfg); err != nil {
		// Linked worktree: the shared config lives in the common dir.
		if common := readCommonDir(gitDir); common != "" {
			cfg = filepath.Join(common, "config")
		}
	}
	data, err := os.ReadFile(cfg)
	if err != nil {
		return nil, ""
	}
	urls = parseGitConfigRemotes(string(data))

	rel, err := filepath.Rel(top, dir)
	if err != nil || rel == "." {
		rel = ""
	}
	prefix = strings.TrimSuffix(filepath.ToSlash(rel), "/")
	return urls, prefix
}

// findGitDir walks up from dir looking for ".git". It returns the repository
// top (the directory containing .git) and the resolved git directory.
func findGitDir(dir string) (top, gitDir string, found bool) {
	cur := dir
	for {
		candidate := filepath.Join(cur, ".git")
		st, err := os.Lstat(candidate)
		if err == nil {
			if st.IsDir() {
				return cur, candidate, true
			}
			if st.Mode()&os.ModeSymlink != 0 {
				// ".git" symlinked elsewhere: a link to a directory is the git
				// dir itself; a link to a file is a "gitdir:" pointer.
				target, err := os.Stat(candidate)
				if err == nil && target.IsDir() {
					gd := candidate
					if resolved, err := filepath.EvalSymlinks(candidate); err == nil {
						gd = resolved
					}
					return cur, gd, true
				}
				if err == nil && target.Mode().IsRegular() {
					if gd := readGitDirPointer(candidate, cur); gd != "" {
						return cur, gd, true
					}
				}
			} else if st.Mode().IsRegular() {
				if gd := readGitDirPointer(candidate, cur); gd != "" {
					return cur, gd, true
				}
			}
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return "", "", false
		}
		cur = parent
	}
}

// readGitDirPointer parses a ".git" file of the form "gitdir: <path>" and
// resolves a relative path against base.
func readGitDirPointer(file, base string) string {
	data, err := os.ReadFile(file)
	if err != nil {
		return ""
	}
	for _, l := range splitLines(string(data)) {
		l = strings.TrimSpace(l)
		if rest, ok := strings.CutPrefix(l, "gitdir:"); ok {
			p := strings.TrimSpace(rest)
			if p == "" {
				return ""
			}
			if !filepath.IsAbs(p) {
				p = filepath.Join(base, p)
			}
			return filepath.Clean(p)
		}
	}
	return ""
}

// readCommonDir resolves the "commondir" file of a linked worktree's git dir.
func readCommonDir(gitDir string) string {
	data, err := os.ReadFile(filepath.Join(gitDir, "commondir"))
	if err != nil {
		return ""
	}
	p := strings.TrimSpace(string(data))
	if p == "" {
		return ""
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(gitDir, p)
	}
	return filepath.Clean(p)
}

// parseGitConfigRemotes extracts every "url" value from [remote "<name>"]
// sections of a git config file, in file order.
func parseGitConfigRemotes(content string) []string {
	var urls []string
	inRemote := false
	sc := bufio.NewScanner(strings.NewReader(content))
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || line[0] == '#' || line[0] == ';' {
			continue
		}
		if line[0] == '[' {
			end := strings.Index(line, "]")
			if end < 0 {
				inRemote = false
				continue
			}
			section := strings.TrimSpace(line[1:end])
			name := strings.ToLower(section)
			// [remote "origin"] or the deprecated [remote.origin] form.
			inRemote = strings.HasPrefix(name, "remote ") || strings.HasPrefix(name, "remote\t") || strings.HasPrefix(name, "remote.")
			continue
		}
		if !inRemote {
			continue
		}
		key, val, found := strings.Cut(line, "=")
		if !found {
			continue
		}
		if strings.ToLower(strings.TrimSpace(key)) != "url" {
			continue
		}
		val = cleanConfigValue(val)
		if val != "" {
			urls = append(urls, val)
		}
	}
	return urls
}

// cleanConfigValue trims whitespace, surrounding double quotes and trailing
// comments from a git config value.
func cleanConfigValue(v string) string {
	v = strings.TrimSpace(v)
	if strings.HasPrefix(v, `"`) {
		if end := strings.Index(v[1:], `"`); end >= 0 {
			return v[1 : end+1]
		}
		return strings.TrimPrefix(v, `"`)
	}
	if i := strings.IndexAny(v, "#;"); i >= 0 {
		v = strings.TrimSpace(v[:i])
	}
	return v
}

func splitLines(s string) []string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}
