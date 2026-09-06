// Package scan finds configuration/secret files inside a project directory and
// scores them using git state (spec §6).
package scan

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/sanbiv/private-sync/internal/execx"
	"github.com/sanbiv/private-sync/internal/fsutil"
)

// gitDirName is the VCS metadata directory, always skipped by the walk.
const gitDirName = ".git"

// mergeSuffix names the scratch file the conflict resolver writes next to a
// conflicted file ("<file>.psv-merge"). It holds the plaintext of both sides
// and its cleanup is best-effort, so a leftover must never be offered for
// tracking. fsutil.TempPrefix covers the atomic-write temps the same way.
const mergeSuffix = ".psv-merge"

// isScratchName reports whether name is one of private-sync's own scratch
// files (an atomic-write temp or a conflict merge file).
func isScratchName(name string) bool {
	base := filepath.Base(name)
	return fsutil.IsTemp(base) || strings.HasSuffix(base, mergeSuffix)
}

// Score drives pre-selection in the UI.
type Score int

const (
	ScoreLow    Score = iota // git-tracked: never pre-selected
	ScoreMedium              // untracked / no git info
	ScoreHigh                // git-ignored match or secret-ish
)

// String returns a human readable label for the score.
func (s Score) String() string {
	switch s {
	case ScoreLow:
		return "low"
	case ScoreMedium:
		return "medium"
	case ScoreHigh:
		return "high"
	}
	return fmt.Sprintf("Score(%d)", int(s))
}

// Candidate is a file proposed for tracking.
type Candidate struct {
	Path        string // slash-separated, relative to the scanned dir
	Size        int64
	Mode        uint32
	Score       Score
	Reasons     []string
	GitTracked  bool
	GitIgnored  bool
	SecretName  bool
	Tracked     bool // already tracked in the vault
	Preselected bool
}

// Options configures a scan. Empty lists mean defaults.
type Options struct {
	Include      []string
	ExcludeDirs  []string // directory names, matched case-insensitively anywhere in the tree
	ExcludeFiles []string
	MaxFileSize  int64
	HardExclude  []string // absolute paths never listed (key file, vault dir)
	// Tracked lists slash-separated relpaths already in the vault. Every
	// tracked file that exists on disk (and is not hard-excluded) is always
	// listed as a candidate with Tracked=true, even when it matches no include
	// glob, matches an exclude glob, sits in an excluded directory or exceeds
	// MaxFileSize, so the user can see it and untrack it.
	Tracked       map[string]bool
	MaxFiles      int // walk cap, default 200000
	MaxCandidates int // default 5000
	Progress      func(walked, found int)
}

// Result of a scan.
type Result struct {
	Candidates  []Candidate
	NestedRepos []string // directories with their own .git (not descended)
	GitInfo     bool     // git state was available
	Truncated   bool
	Warnings    []string
}

// Default limits (spec §6).
const (
	DefaultMaxFileSize   int64 = 2 * 1024 * 1024
	DefaultMaxFiles            = 200000
	DefaultMaxCandidates       = 5000
	ProgressInterval           = 500
)

// Reason strings attached to candidates.
const (
	ReasonTracked      = "committed to git"
	ReasonIgnored      = "ignored by git"
	ReasonSecretName   = "secret-like name"
	ReasonVaultTracked = "tracked in vault"
)

// Scan walks dir and returns scored candidates sorted by score desc, then path.
func Scan(ctx context.Context, dir string, opts Options, r execx.Runner) (*Result, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	root, err := resolveDir(dir)
	if err != nil {
		return nil, err
	}

	include := opts.Include
	if len(include) == 0 {
		include = DefaultInclude()
	}
	excludeDirs := opts.ExcludeDirs
	if len(excludeDirs) == 0 {
		excludeDirs = DefaultExcludeDirs()
	}
	excludeFiles := opts.ExcludeFiles
	if len(excludeFiles) == 0 {
		excludeFiles = DefaultExcludeFiles()
	}
	maxSize := opts.MaxFileSize
	if maxSize <= 0 {
		maxSize = DefaultMaxFileSize
	}
	maxFiles := opts.MaxFiles
	if maxFiles <= 0 {
		maxFiles = DefaultMaxFiles
	}
	maxCand := opts.MaxCandidates
	if maxCand <= 0 {
		maxCand = DefaultMaxCandidates
	}
	// Directory names are compared case-insensitively, consistently with the
	// include/exclude globs, so `Build/` is skipped like `build/`.
	excludeDirSet := make(map[string]bool, len(excludeDirs))
	for _, d := range excludeDirs {
		excludeDirSet[strings.ToLower(d)] = true
	}
	hard := normalizeHardExcludes(opts.HardExclude)
	vaultTracked, badTracked := normalizeTracked(opts.Tracked)

	res := &Result{}
	walked, found := 0, 0
	progress := func() {
		if opts.Progress != nil {
			opts.Progress(walked, found)
		}
	}

	walkErr := filepath.WalkDir(root, func(p string, d fs.DirEntry, werr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if werr != nil {
			if p == root {
				return werr
			}
			rel := relPath(root, p)
			res.Warnings = append(res.Warnings, fmt.Sprintf("skipped %s: %v", rel, werr))
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if p == root {
			return nil
		}
		name := d.Name()
		if d.IsDir() {
			// VCS metadata is never a candidate source. A non-empty
			// scan.exclude_dirs replaces the defaults (spec §3), so relying on
			// DefaultExcludeDirs() to carry ".git" would let a custom list walk
			// the repo's own object store (spec §14).
			if name == gitDirName {
				return fs.SkipDir
			}
			if excludeDirSet[strings.ToLower(name)] {
				return fs.SkipDir
			}
			if isHardExcluded(p, hard) {
				return fs.SkipDir
			}
			if hasGitEntry(p) {
				res.NestedRepos = append(res.NestedRepos, relPath(root, p))
				return fs.SkipDir
			}
			return nil
		}
		// Non-directory entry: symlinks (to anything) and special files are skipped.
		if !d.Type().IsRegular() {
			return nil
		}
		walked++
		if walked > maxFiles {
			walked = maxFiles
			res.Truncated = true
			res.Warnings = append(res.Warnings, fmt.Sprintf("scan truncated: more than %d files walked", maxFiles))
			return fs.SkipAll
		}
		if walked%ProgressInterval == 0 {
			progress()
		}
		if isHardExcluded(p, hard) {
			return nil
		}
		if MatchesAny(name, excludeFiles) {
			return nil
		}
		// private-sync's own scratch files hold plaintext copies of tracked
		// secrets (the conflict resolver's ".psv-merge" and the atomic-write
		// temps). They match include globs such as ".env.*" and score as
		// secrets, so they are skipped unconditionally: a user-supplied
		// scan.exclude_files replaces the defaults and could otherwise drop
		// them.
		if isScratchName(name) {
			return nil
		}
		matched, pattern := matchInclude(name, include)
		secret := IsSecretName(name)
		if !matched && !secret {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			res.Warnings = append(res.Warnings, fmt.Sprintf("skipped %s: %v", relPath(root, p), err))
			return nil
		}
		if !info.Mode().IsRegular() || info.Size() > maxSize {
			return nil
		}
		if found >= maxCand {
			res.Truncated = true
			res.Warnings = append(res.Warnings, fmt.Sprintf("scan truncated: more than %d candidates found", maxCand))
			return fs.SkipAll
		}
		c := Candidate{
			Path:       relPath(root, p),
			Size:       info.Size(),
			Mode:       uint32(info.Mode().Perm()),
			SecretName: secret,
		}
		if secret {
			c.Reasons = append(c.Reasons, ReasonSecretName)
		}
		if matched {
			c.Reasons = append(c.Reasons, "matches "+pattern)
		}
		res.Candidates = append(res.Candidates, c)
		found++
		return nil
	})
	if walkErr != nil {
		if errors.Is(walkErr, context.Canceled) || errors.Is(walkErr, context.DeadlineExceeded) {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("scan %s: %w", dir, walkErr)
	}
	progress()

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// Vault-tracked files are always listed (spec §6: "shown as tracked and
	// checked"), even when the walk filtered them out or never reached them.
	for _, rel := range badTracked {
		res.Warnings = append(res.Warnings, fmt.Sprintf("tracked file %q: invalid relative path", rel))
	}
	res.Candidates = appendVaultTracked(root, res.Candidates, vaultTracked, hard, include, &res.Warnings)

	// Git state: one batch per scan.
	tracked, ignored, gitInfo, gitWarn := gitState(ctx, root, res.Candidates, r)
	res.GitInfo = gitInfo
	if gitWarn != "" {
		res.Warnings = append(res.Warnings, gitWarn)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	for i := range res.Candidates {
		c := &res.Candidates[i]
		scoreCandidate(c, gitInfo, tracked[c.Path], ignored[c.Path], vaultTracked[c.Path])
	}
	sort.SliceStable(res.Candidates, func(i, j int) bool {
		a, b := res.Candidates[i], res.Candidates[j]
		if a.Score != b.Score {
			return a.Score > b.Score
		}
		return a.Path < b.Path
	})
	sort.Strings(res.NestedRepos)
	return res, nil
}

// scoreCandidate applies the spec §6 scoring table to c.
func scoreCandidate(c *Candidate, gitInfo, gitTracked, gitIgnored, vaultTracked bool) {
	var gitReason string
	switch {
	case gitInfo && gitTracked:
		c.GitTracked = true
		c.Score = ScoreLow
		gitReason = ReasonTracked
	case gitInfo && gitIgnored:
		c.GitIgnored = true
		c.Score = ScoreHigh
		gitReason = ReasonIgnored
	case gitInfo:
		c.Score = ScoreMedium
	case c.SecretName:
		c.Score = ScoreHigh
	default:
		c.Score = ScoreMedium
	}
	if gitReason != "" {
		c.Reasons = append([]string{gitReason}, c.Reasons...)
	}
	c.Tracked = vaultTracked
	if c.Tracked {
		c.Reasons = append(c.Reasons, ReasonVaultTracked)
	}
	c.Preselected = c.Score == ScoreHigh || c.Tracked || (c.Score == ScoreMedium && c.SecretName)
}

// normalizeTracked cleans the vault-tracked relpaths into a slash-separated
// set and reports the entries that cannot name a file inside the project.
func normalizeTracked(in map[string]bool) (set map[string]bool, bad []string) {
	set = map[string]bool{}
	for rel, ok := range in {
		if !ok {
			continue
		}
		clean, valid := cleanRel(rel)
		if !valid {
			bad = append(bad, rel)
			continue
		}
		set[clean] = true
	}
	sort.Strings(bad)
	return set, bad
}

// cleanRel normalises a relative path to slash form and reports whether it
// stays inside the project directory.
func cleanRel(rel string) (string, bool) {
	rel = strings.TrimSpace(rel)
	if rel == "" {
		return "", false
	}
	if filepath.IsAbs(rel) || filepath.IsAbs(filepath.FromSlash(rel)) || filepath.VolumeName(rel) != "" {
		return "", false
	}
	clean := path.Clean(filepath.ToSlash(rel))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || strings.HasPrefix(clean, "/") {
		return "", false
	}
	return clean, true
}

// appendVaultTracked adds every vault-tracked file that the walk did not list
// (excluded by glob, directory, size, or never reached) as a candidate, as long
// as it is a regular file on disk and not hard-excluded. Missing files are
// reported through warnings.
func appendVaultTracked(root string, cands []Candidate, tracked map[string]bool, hard []string, include []string, warnings *[]string) []Candidate {
	if len(tracked) == 0 {
		return cands
	}
	have := make(map[string]bool, len(cands))
	for _, c := range cands {
		have[c.Path] = true
	}
	rels := make([]string, 0, len(tracked))
	for rel := range tracked {
		if !have[rel] {
			rels = append(rels, rel)
		}
	}
	sort.Strings(rels)
	for _, rel := range rels {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if isHardExcluded(p, hard) {
			continue
		}
		info, err := os.Lstat(p)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				*warnings = append(*warnings, fmt.Sprintf("tracked file %s is missing on disk", rel))
			} else {
				*warnings = append(*warnings, fmt.Sprintf("tracked file %s: %v", rel, err))
			}
			continue
		}
		if !info.Mode().IsRegular() {
			*warnings = append(*warnings, fmt.Sprintf("tracked file %s is not a regular file", rel))
			continue
		}
		// Lstat only guards the final component: an intermediate symlinked
		// directory is resolved by the OS and would expose a file outside the
		// project, which the walk itself never follows. root is already
		// symlink-resolved by resolveDir.
		real, err := filepath.EvalSymlinks(p)
		if err != nil {
			*warnings = append(*warnings, fmt.Sprintf("tracked file %s: %v", rel, err))
			continue
		}
		if isHardExcluded(real, hard) {
			continue
		}
		if !isWithinDir(root, real) {
			*warnings = append(*warnings, fmt.Sprintf("tracked file %s resolves outside the project", rel))
			continue
		}
		c := Candidate{
			Path:       rel,
			Size:       info.Size(),
			Mode:       uint32(info.Mode().Perm()),
			SecretName: IsSecretName(rel),
		}
		if c.SecretName {
			c.Reasons = append(c.Reasons, ReasonSecretName)
		}
		if ok, pattern := matchInclude(rel, include); ok {
			c.Reasons = append(c.Reasons, "matches "+pattern)
		}
		cands = append(cands, c)
	}
	return cands
}

// resolveDir turns dir into an absolute, symlink-resolved directory path.
func resolveDir(dir string) (string, error) {
	if strings.TrimSpace(dir) == "" {
		return "", errors.New("scan: empty directory")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("scan %s: %w", dir, err)
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("scan %s: %w", dir, err)
	}
	info, err := os.Stat(real)
	if err != nil {
		return "", fmt.Errorf("scan %s: %w", dir, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("scan %s: not a directory", dir)
	}
	return real, nil
}

// caseInsensitivePaths mirrors config.caseInsensitivePaths: on macOS and
// Windows the default filesystems ignore case, so path containment checks must
// fold case or the key file could be listed under a differently-cased project
// root. A case-sensitive volume on those platforms gets a false positive at
// worst, which is the safer failure for a check guarding the passphrase file.
var caseInsensitivePaths = runtime.GOOS == "darwin" || runtime.GOOS == "windows"

// foldPath lowercases p on case-insensitive platforms.
func foldPath(p string) string {
	if caseInsensitivePaths {
		return strings.ToLower(p)
	}
	return p
}

// normalizeHardExcludes returns cleaned absolute (symlink-resolved when
// possible) versions of the given paths, dropping empty entries. Entries are
// case-folded on case-insensitive platforms so they still match a root spelled
// with different casing.
func normalizeHardExcludes(in []string) []string {
	var out []string
	for _, h := range in {
		if strings.TrimSpace(h) == "" {
			continue
		}
		abs, err := filepath.Abs(h)
		if err != nil {
			continue
		}
		abs = filepath.Clean(abs)
		out = append(out, foldPath(abs))
		if real, err := filepath.EvalSymlinks(abs); err == nil && real != abs {
			out = append(out, foldPath(real))
		}
	}
	return out
}

// isHardExcluded reports whether p equals or lies below any hard exclude. The
// hard excludes are already folded by normalizeHardExcludes.
func isHardExcluded(p string, hard []string) bool {
	p = foldPath(p)
	for _, h := range hard {
		if p == h {
			return true
		}
		if strings.HasPrefix(p, h+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// isWithinDir reports whether p equals dir or lies below it (both absolute and
// cleaned), folding case on case-insensitive platforms.
func isWithinDir(dir, p string) bool {
	dir, p = foldPath(filepath.Clean(dir)), foldPath(filepath.Clean(p))
	return p == dir || strings.HasPrefix(p, dir+string(filepath.Separator))
}

// hasGitEntry reports whether dir contains a ".git" entry (dir or file).
func hasGitEntry(dir string) bool {
	_, err := os.Lstat(filepath.Join(dir, ".git"))
	return err == nil
}

func relPath(root, p string) string {
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return filepath.ToSlash(p)
	}
	return filepath.ToSlash(rel)
}

// matchInclude returns the first include glob that matches name.
func matchInclude(name string, globs []string) (bool, string) {
	base := strings.ToLower(filepath.Base(name))
	for _, g := range globs {
		if matchGlob(strings.ToLower(g), base) {
			return true, g
		}
	}
	return false, ""
}

// matchGlob matches a lowercased base name against a lowercased glob; a
// malformed pattern falls back to a literal comparison.
func matchGlob(pattern, base string) bool {
	if pattern == "" {
		return false
	}
	ok, err := path.Match(pattern, base)
	if err != nil {
		return pattern == base
	}
	return ok
}

// gitState runs the git batch for root (spec §6): `git ls-files -z` for the
// tracked set and `git check-ignore -z --stdin --no-index` for the ignored
// set. ls-files exits 128 outside a work tree, which (like a missing git
// binary or any other failure) yields "no git info". It returns the tracked
// and ignored relpath sets, whether git information is available, and a
// warning.
func gitState(ctx context.Context, root string, cands []Candidate, r execx.Runner) (tracked, ignored map[string]bool, ok bool, warn string) {
	if r == nil {
		return nil, nil, false, "no git info: no command runner"
	}
	res, err := r.Run(ctx, execx.Cmd{Name: "git", Args: []string{"-C", root, "ls-files", "-z"}})
	if err != nil {
		return nil, nil, false, "no git info: " + gitErr(err)
	}
	tracked = splitNUL(res.Stdout)

	ignored = map[string]bool{}
	if len(cands) > 0 {
		var stdin []byte
		for _, c := range cands {
			stdin = append(stdin, c.Path...)
			stdin = append(stdin, 0)
		}
		res, err = r.Run(ctx, execx.Cmd{
			Name:  "git",
			Args:  []string{"-C", root, "check-ignore", "-z", "--stdin", "--no-index"},
			Stdin: stdin,
		})
		if err != nil {
			if execx.ExitCode(err) != 1 {
				return nil, nil, false, "no git info: " + gitErr(err)
			}
			// exit 1: no path is ignored.
		} else {
			ignored = splitNUL(res.Stdout)
		}
	}
	return tracked, ignored, true, ""
}

func gitErr(err error) string {
	var ee *execx.ExitError
	if errors.As(err, &ee) {
		msg := strings.TrimSpace(string(ee.Result.Stderr))
		if msg == "" {
			msg = fmt.Sprintf("git exited with status %d", ee.Result.ExitCode)
		}
		return msg
	}
	return err.Error()
}

// splitNUL parses NUL-separated paths into a set of slash-separated relpaths.
func splitNUL(b []byte) map[string]bool {
	out := map[string]bool{}
	for _, p := range strings.Split(string(b), "\x00") {
		p = strings.TrimRight(p, "\r\n")
		if p == "" {
			continue
		}
		out[filepath.ToSlash(p)] = true
	}
	return out
}

// DefaultInclude returns the default include globs.
func DefaultInclude() []string {
	return []string{
		".env", ".env.*", "*.env", ".envrc", "*.yaml", "*.yml", "*.json", "*.toml",
		"*.ini", "*.cfg", "*.conf", "*.properties", ".npmrc", ".yarnrc", ".yarnrc.yml", ".pypirc",
		".netrc", "*.pem", "*.key", "*.crt", "*.p12", "*.pfx", "*.jks", "*.keystore", "*.tfvars",
		"*secret*", "*credential*", "service-account*.json", "appsettings.*.json",
		"application-*.yml", "application-*.yaml", "application-*.properties", "*.local.*",
		"wp-config.php", ".htpasswd", "*.env.js", "*.env.ts",
	}
}

// DefaultExcludeDirs returns the default excluded directory names. Names are
// matched case-insensitively against every directory in the tree.
func DefaultExcludeDirs() []string {
	return []string{
		".git", "node_modules", "vendor", "dist", "build", "out", "target",
		".next", ".nuxt", ".svelte-kit", ".venv", "venv", "env", "__pycache__", ".idea", ".cache",
		"coverage", ".terraform", "bin", "obj", "Pods", "DerivedData", ".gradle", ".dart_tool",
		".turbo", ".parcel-cache", ".pytest_cache", ".mypy_cache", "tmp", "logs", "testdata",
		"fixtures", "__fixtures__", "locales", "i18n", ".github",
	}
}

// DefaultExcludeFiles returns the default excluded file globs.
func DefaultExcludeFiles() []string {
	return []string{
		"package-lock.json", "yarn.lock", "pnpm-lock.yaml", "composer.lock",
		"Cargo.lock", "go.sum", "*.min.json", "tsconfig*.json", "*.schema.json", ".eslintrc*",
		".prettierrc*", "renovate.json", "lerna.json", "jsconfig.json", "package.json",
		"composer.json", "manifest.json", "*.lock.json",
		"*" + mergeSuffix, "*" + fsutil.TempPrefix + "*",
	}
}

// secretAlways are secret-ish name patterns regardless of extension.
var secretAlways = []string{
	".env", ".env.*", "*.env", ".netrc", ".npmrc", ".pypirc", ".htpasswd",
	"*.tfvars", "*.pem", "*.key", "*.p12", "*.pfx", "*.jks", "*.keystore",
}

// secretNonSource are secret-ish only when the extension is not source code.
var secretNonSource = []string{"*secret*", "*credential*", "*.local.*"}

// sourceExts are extensions that disqualify the secretNonSource patterns.
var sourceExts = map[string]bool{
	".go": true, ".ts": true, ".tsx": true, ".js": true, ".jsx": true, ".mjs": true, ".cjs": true,
	".java": true, ".kt": true, ".py": true, ".rs": true, ".rb": true, ".php": true, ".cs": true,
	".swift": true, ".c": true, ".h": true, ".cpp": true, ".m": true, ".scala": true,
	".vue": true, ".svelte": true,
}

// IsSecretName reports whether a base name looks like a secret (spec §6).
func IsSecretName(name string) bool {
	base := strings.ToLower(filepath.Base(name))
	if base == "" || base == "." || base == string(filepath.Separator) {
		return false
	}
	if MatchesAny(base, secretAlways) {
		return true
	}
	if sourceExts[path.Ext(base)] {
		return false
	}
	return MatchesAny(base, secretNonSource)
}

// MatchesAny reports whether name matches any glob (case-insensitive, base name only).
func MatchesAny(name string, globs []string) bool {
	ok, _ := matchInclude(name, globs)
	return ok
}
