package identity

import (
	"bufio"
	"encoding/json"
	"encoding/xml"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// maxManifestSize bounds how much of a manifest is read (manifests are tiny;
// anything larger is not one we want to parse).
const maxManifestSize = 4 << 20

// manifestDetector reads one manifest kind from dir and returns the
// fingerprint value (empty when absent or unparsable).
type manifestDetector struct {
	kind   string
	detect func(dir string) string
}

// manifestDetectors lists every LevelPackage manifest in output order.
var manifestDetectors = []manifestDetector{
	{"go", detectGoMod},
	{"npm", detectPackageJSON},
	{"cargo", detectCargoToml},
	{"py", detectPyproject},
	{"composer", detectComposerJSON},
	{"maven", detectPomXML},
	{"gem", detectGemspec},
	{"swift", detectPackageSwift},
	{"dart", detectPubspec},
}

// detectManifests returns the LevelPackage fingerprints of manifests located
// directly in dir (parent directories are never consulted).
func detectManifests(dir string) []Fingerprint {
	var fps []Fingerprint
	for _, d := range manifestDetectors {
		if v := d.detect(dir); v != "" {
			fps = append(fps, Fingerprint{Kind: d.kind, Value: v, Level: LevelPackage})
		}
	}
	return fps
}

// readManifest reads a regular file in dir, returning "" when missing/unreadable.
func readManifest(dir, name string) string {
	p := filepath.Join(dir, name)
	st, err := os.Stat(p)
	if err != nil || !st.Mode().IsRegular() || st.Size() > maxManifestSize {
		return ""
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	return string(data)
}

// ---- go.mod -----------------------------------------------------------------

func detectGoMod(dir string) string {
	content := readManifest(dir, "go.mod")
	if content == "" {
		return ""
	}
	sc := bufio.NewScanner(strings.NewReader(content))
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		rest, ok := strings.CutPrefix(line, "module")
		if !ok || (rest != "" && rest[0] != ' ' && rest[0] != '\t') {
			continue
		}
		rest = strings.TrimSpace(rest)
		if i := strings.Index(rest, "//"); i >= 0 {
			rest = strings.TrimSpace(rest[:i])
		}
		rest = strings.Trim(rest, `"`)
		return strings.TrimSpace(rest)
	}
	return ""
}

// ---- package.json / composer.json --------------------------------------------

func detectPackageJSON(dir string) string {
	return strings.ToLower(jsonName(readManifest(dir, "package.json")))
}

func detectComposerJSON(dir string) string {
	return strings.ToLower(jsonName(readManifest(dir, "composer.json")))
}

func jsonName(content string) string {
	if strings.TrimSpace(content) == "" {
		return ""
	}
	var m struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal([]byte(content), &m); err != nil {
		return ""
	}
	return strings.TrimSpace(m.Name)
}

// ---- Cargo.toml / pyproject.toml (minimal TOML) -------------------------------

func detectCargoToml(dir string) string {
	t := parseTOMLSections(readManifest(dir, "Cargo.toml"))
	return t.get("package", "name")
}

func detectPyproject(dir string) string {
	t := parseTOMLSections(readManifest(dir, "pyproject.toml"))
	name := t.get("project", "name")
	if name == "" {
		name = t.get("tool.poetry", "name")
	}
	return strings.ToLower(name)
}

// tomlSections is a minimal section → key → value map. Only top-level
// "key = value" lines inside [section] headers are recorded; arrays of tables,
// inline tables and multi-line strings are ignored, which suffices for names.
type tomlSections map[string]map[string]string

func (t tomlSections) get(section, key string) string {
	if t == nil {
		return ""
	}
	return t[section][key]
}

func parseTOMLSections(content string) tomlSections {
	if content == "" {
		return nil
	}
	out := tomlSections{}
	section := ""
	sc := bufio.NewScanner(strings.NewReader(content))
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || line[0] == '#' {
			continue
		}
		if line[0] == '[' {
			if strings.HasPrefix(line, "[[") {
				// array of tables: no name lookups inside.
				section = "\x00array"
				continue
			}
			end := strings.Index(line, "]")
			if end < 0 {
				section = "\x00bad"
				continue
			}
			section = normalizeTOMLKey(line[1:end])
			continue
		}
		key, val, found := strings.Cut(line, "=")
		if !found {
			continue
		}
		key = normalizeTOMLKey(key)
		if key == "" {
			continue
		}
		if out[section] == nil {
			out[section] = map[string]string{}
		}
		if _, dup := out[section][key]; !dup {
			out[section][key] = tomlScalar(val)
		}
	}
	return out
}

// normalizeTOMLKey trims whitespace and quotes around each dotted component.
func normalizeTOMLKey(k string) string {
	parts := strings.Split(k, ".")
	for i, p := range parts {
		p = strings.TrimSpace(p)
		p = strings.Trim(p, `"'`)
		parts[i] = strings.TrimSpace(p)
	}
	return strings.Join(parts, ".")
}

// tomlScalar extracts a string scalar: quoted (basic or literal) or bare up to
// a comment. Non-string values (tables, arrays) yield "".
func tomlScalar(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	switch v[0] {
	case '"':
		if strings.HasPrefix(v, `"""`) {
			rest := v[3:]
			if end := strings.Index(rest, `"""`); end >= 0 {
				return strings.TrimSpace(rest[:end])
			}
			return ""
		}
		var b strings.Builder
		esc := false
		for _, c := range v[1:] {
			if esc {
				switch c {
				case 'n':
					b.WriteByte('\n')
				case 't':
					b.WriteByte('\t')
				default:
					b.WriteRune(c)
				}
				esc = false
				continue
			}
			if c == '\\' {
				esc = true
				continue
			}
			if c == '"' {
				return b.String()
			}
			b.WriteRune(c)
		}
		return ""
	case '\'':
		if strings.HasPrefix(v, `'''`) {
			rest := v[3:]
			if end := strings.Index(rest, `'''`); end >= 0 {
				return strings.TrimSpace(rest[:end])
			}
			return ""
		}
		if end := strings.Index(v[1:], "'"); end >= 0 {
			return v[1 : end+1]
		}
		return ""
	case '{', '[':
		return ""
	}
	if i := strings.Index(v, "#"); i >= 0 {
		v = v[:i]
	}
	return strings.TrimSpace(v)
}

// ---- pom.xml -----------------------------------------------------------------

func detectPomXML(dir string) string {
	content := readManifest(dir, "pom.xml")
	if strings.TrimSpace(content) == "" {
		return ""
	}
	var p struct {
		GroupID    string `xml:"groupId"`
		ArtifactID string `xml:"artifactId"`
		Parent     struct {
			GroupID string `xml:"groupId"`
		} `xml:"parent"`
	}
	if err := xml.Unmarshal([]byte(content), &p); err != nil {
		return ""
	}
	g := strings.TrimSpace(p.GroupID)
	if g == "" {
		// Maven inherits groupId from the parent when omitted.
		g = strings.TrimSpace(p.Parent.GroupID)
	}
	a := strings.TrimSpace(p.ArtifactID)
	if g == "" || a == "" {
		return ""
	}
	return g + ":" + a
}

// ---- *.gemspec ---------------------------------------------------------------

var gemspecNameRe = regexp.MustCompile(`\.name\s*=\s*(?:"([^"]*)"|'([^']*)')`)

func detectGemspec(dir string) string {
	// os.ReadDir rather than filepath.Glob: dir is user-controlled and may
	// contain glob metacharacters ("[", "]", "*", "?"), which would silently
	// break or misdirect a Glob pattern.
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".gemspec") {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)
	for _, name := range names {
		// readManifest requires a regular file (after following symlinks).
		content := readManifest(dir, name)
		if content == "" {
			continue
		}
		if sm := gemspecNameRe.FindStringSubmatch(content); sm != nil {
			name := sm[1]
			if name == "" {
				name = sm[2]
			}
			if name = strings.TrimSpace(name); name != "" {
				return name
			}
		}
	}
	return ""
}

// ---- Package.swift -----------------------------------------------------------

var swiftNameRe = regexp.MustCompile(`\bname\s*:\s*"([^"]*)"`)

func detectPackageSwift(dir string) string {
	content := readManifest(dir, "Package.swift")
	if content == "" {
		return ""
	}
	if sm := swiftNameRe.FindStringSubmatch(content); sm != nil {
		return strings.TrimSpace(sm[1])
	}
	return ""
}

// ---- pubspec.yaml ------------------------------------------------------------

func detectPubspec(dir string) string {
	content := readManifest(dir, "pubspec.yaml")
	if strings.TrimSpace(content) == "" {
		return ""
	}
	var m struct {
		Name string `yaml:"name"`
	}
	if err := yaml.Unmarshal([]byte(content), &m); err != nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(m.Name))
}
