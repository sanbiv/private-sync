// Package identity computes project fingerprints and matches them across machines.
package identity

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/sanbiv/private-sync/internal/execx"
)

// Level ranks fingerprint strength.
type Level int

const (
	LevelStrong  Level = 1 // git remote
	LevelPackage Level = 2 // language manifests
	LevelDir     Level = 3 // directory basename
)

// Fingerprint identifies a project.
type Fingerprint struct {
	Kind  string `json:"kind"`  // git, go, npm, cargo, py, composer, maven, gem, swift, dart, dir
	Value string `json:"value"` // normalised value
	Level Level  `json:"level"`
}

// String renders "kind:value".
func (f Fingerprint) String() string { return f.Kind + ":" + f.Value }

// Detect computes every fingerprint of dir (spec §7). Never returns an empty list:
// "dir:<basename>" is always present.
//
// Order: git remotes (LevelStrong, one per distinct normalised remote), language
// manifests found directly in dir (LevelPackage), then "dir:<basename>" (LevelDir).
// Git remotes are read through the runner; when git is unavailable or fails the
// nearest .git directory / gitdir pointer file is parsed instead.
func Detect(ctx context.Context, dir string, r execx.Runner) ([]Fingerprint, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	abs = filepath.Clean(abs)
	st, err := os.Stat(abs)
	if err != nil {
		return nil, err
	}
	if !st.IsDir() {
		return nil, &os.PathError{Op: "detect", Path: abs, Err: errNotDir}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	var fps []Fingerprint

	// 1. git remotes.
	remotes, prefix, ok := gitViaRunner(ctx, abs, r)
	if !ok {
		remotes, prefix = gitViaFilesystem(abs)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	for _, u := range remotes {
		hp, ok := NormalizeGitURL(u)
		if !ok {
			continue
		}
		if prefix != "" {
			hp += "#" + prefix
		}
		fps = append(fps, Fingerprint{Kind: "git", Value: hp, Level: LevelStrong})
	}

	// 2. language manifests in dir only.
	fps = append(fps, detectManifests(abs)...)

	// 3. directory basename, always last.
	fps = append(fps, Fingerprint{Kind: "dir", Value: filepath.Base(abs), Level: LevelDir})

	return Union(fps), nil
}

// Strength of a match.
type Strength int

const (
	StrengthNone   Strength = iota
	StrengthWeak            // only dir: shared
	StrengthStrong          // a level 1-2 fingerprint shared
)

// Match is a candidate vault project for a local directory.
type Match struct {
	ProjectID string
	Strength  Strength
	Shared    []Fingerprint
}

// MatchProjects compares local fingerprints with the vault projects' fingerprints;
// strong matches first, then weak. Projects sharing nothing are omitted.
// Within a strength class matches are ordered by project id; Shared keeps the
// order of the local fingerprints.
func MatchProjects(local []Fingerprint, vaultProjects map[string][]Fingerprint) []Match {
	if len(local) == 0 || len(vaultProjects) == 0 {
		return nil
	}
	ids := make([]string, 0, len(vaultProjects))
	for id := range vaultProjects {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	var strong, weak []Match
	for _, id := range ids {
		have := map[string]bool{}
		for _, f := range vaultProjects[id] {
			have[f.String()] = true
		}
		var shared []Fingerprint
		seen := map[string]bool{}
		strength := StrengthNone
		for _, f := range local {
			key := f.String()
			if !have[key] || seen[key] {
				continue
			}
			seen[key] = true
			shared = append(shared, f)
			switch matchLevel(f) {
			case LevelStrong, LevelPackage:
				strength = StrengthStrong
			case LevelDir:
				if strength == StrengthNone {
					strength = StrengthWeak
				}
			}
		}
		m := Match{ProjectID: id, Strength: strength, Shared: shared}
		switch strength {
		case StrengthStrong:
			strong = append(strong, m)
		case StrengthWeak:
			weak = append(weak, m)
		}
	}
	if len(strong)+len(weak) == 0 {
		return nil
	}
	return append(strong, weak...)
}

// packageKinds lists every LevelPackage fingerprint kind produced by Detect.
var packageKinds = map[string]bool{
	"go": true, "npm": true, "cargo": true, "py": true, "composer": true,
	"maven": true, "gem": true, "swift": true, "dart": true,
}

// matchLevel is the level used by MatchProjects to classify a shared
// fingerprint. A fingerprint carrying one of the three known levels is taken
// as is; a hand-built or older-format fingerprint with an unset (zero) level
// is classified by its kind; anything else is unknown (0) and contributes
// nothing to the match strength.
func matchLevel(f Fingerprint) Level {
	switch f.Level {
	case LevelStrong, LevelPackage, LevelDir:
		return f.Level
	}
	if f.Level != 0 {
		return 0
	}
	switch {
	case f.Kind == "git":
		return LevelStrong
	case f.Kind == "dir":
		return LevelDir
	case packageKinds[f.Kind]:
		return LevelPackage
	}
	return 0
}

// Strongest returns the first level-1 fingerprint, else the first level-2 one.
func Strongest(fps []Fingerprint) (Fingerprint, bool) {
	for _, f := range fps {
		if f.Level == LevelStrong {
			return f, true
		}
	}
	for _, f := range fps {
		if f.Level == LevelPackage {
			return f, true
		}
	}
	return Fingerprint{}, false
}

// NormalizeGitURL turns any git remote URL into "host/path" (spec §7 rules):
// lowercase; strip scheme, user@ and :port; scp form host:path → host/path;
// strip leading "/", trailing "/" and ".git"; "/_git/" → "/"; "ssh." and "www."
// host prefixes removed; on dev.azure.com the ssh-only leading "v3/" path
// segment is dropped so ssh and https clones of one repository match.
// Returns ok=false for empty strings and local paths (file:// URLs,
// absolute/relative paths, Windows drive letters).
func NormalizeGitURL(raw string) (string, bool) {
	s := strings.ToLower(strings.TrimSpace(raw))
	if s == "" {
		return "", false
	}

	var host, path string
	if i := strings.Index(s, "://"); i >= 0 {
		scheme := s[:i]
		rest := s[i+3:]
		if scheme == "file" || scheme == "" {
			return "", false
		}
		authority := rest
		if j := strings.Index(rest, "/"); j >= 0 {
			authority, path = rest[:j], rest[j:]
		}
		host = stripUserInfo(authority)
		host = stripPort(host)
	} else {
		if strings.HasPrefix(s, "/") || strings.HasPrefix(s, ".") || strings.HasPrefix(s, "~") || strings.HasPrefix(s, "\\") {
			return "", false
		}
		slash := strings.Index(s, "/")
		// User info ("user[:password]@") may itself contain a colon, so locate
		// its terminator first: the last '@' before the first '/'.
		at := -1
		if slash >= 0 {
			at = strings.LastIndex(s[:slash], "@")
		} else {
			at = strings.LastIndex(s, "@")
		}
		colon := -1
		if i := strings.Index(s[at+1:], ":"); i >= 0 {
			colon = at + 1 + i
		}
		if colon < 0 || (slash >= 0 && slash < colon) {
			// no host:path separator → a local path.
			return "", false
		}
		authority, p := s[:colon], s[colon+1:]
		host = stripUserInfo(authority)
		if len(host) == 1 && host[0] >= 'a' && host[0] <= 'z' {
			// Windows drive letter (c:\repo, c:/repo).
			return "", false
		}
		// scp form never carries a port; "host:22/path" would be ambiguous and is rare.
		if strings.HasPrefix(p, "//") || strings.ContainsAny(host, "\\") {
			return "", false
		}
		path = p
	}

	host = strings.TrimSpace(host)
	host = strings.TrimPrefix(host, "ssh.")
	host = strings.TrimPrefix(host, "www.")
	if host == "" || strings.ContainsAny(host, " \t\\") {
		return "", false
	}

	path = strings.ReplaceAll(path, "\\", "/")
	path = strings.ReplaceAll(path, "/_git/", "/")
	path = strings.Trim(path, "/")
	path = strings.TrimSuffix(path, ".git")
	path = strings.Trim(path, "/")
	if strings.HasPrefix(path, "_git/") {
		path = strings.TrimPrefix(path, "_git/")
	}
	// Collapse repeated slashes.
	for strings.Contains(path, "//") {
		path = strings.ReplaceAll(path, "//", "/")
	}
	// Azure DevOps: the ssh form "git@ssh.dev.azure.com:v3/org/project/repo"
	// names the same repository as "https://dev.azure.com/org/project/_git/repo";
	// drop the ssh-only "v3/" API segment so both forms match.
	if host == "dev.azure.com" {
		if rest, ok := strings.CutPrefix(path, "v3/"); ok && rest != "" {
			path = rest
		}
	}
	if path == "" {
		return "", false
	}
	return host + "/" + path, true
}

// Union merges fingerprint lists without duplicates, keeping order of first appearance.
func Union(lists ...[]Fingerprint) []Fingerprint {
	seen := map[string]bool{}
	var out []Fingerprint
	for _, l := range lists {
		for _, f := range l {
			key := f.String()
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, f)
		}
	}
	return out
}

// stripUserInfo removes "user[:password]@" from an authority component.
func stripUserInfo(authority string) string {
	if i := strings.LastIndex(authority, "@"); i >= 0 {
		return authority[i+1:]
	}
	return authority
}

// stripPort removes a trailing ":port" from a host (IPv6 literals in brackets are kept).
func stripPort(host string) string {
	if strings.HasPrefix(host, "[") {
		if j := strings.Index(host, "]"); j >= 0 {
			return host[:j+1]
		}
		return host
	}
	if i := strings.LastIndex(host, ":"); i >= 0 {
		port := host[i+1:]
		if port == "" || isDigits(port) {
			return host[:i]
		}
	}
	return host
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}
