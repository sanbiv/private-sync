package merge

import (
	"bytes"
	"fmt"
	"regexp"
)

// envEntryRe is the entry grammar of spec §8 applied to one physical line
// (terminator already stripped).
var envEntryRe = regexp.MustCompile(`^\s*(export\s+)?([A-Za-z_][A-Za-z0-9_.-]*)\s*=(.*)$`)

type envItemKind uint8

const (
	envBlank envItemKind = iota
	envComment
	envEntry
)

// envItem is one syntactic unit of a dotenv file: a blank line, a comment
// line or an entry (possibly spanning several physical lines).
type envItem struct {
	kind envItemKind
	key  string
	raw  []byte // every physical line of the item, terminators included
}

// envFile is a strictly parsed dotenv file.
type envFile struct {
	items []envItem
	index map[string]int // key → position in items
}

// rawOf returns the raw entry bytes for key, or nil when absent.
func (f *envFile) rawOf(key string) []byte {
	if i, ok := f.index[key]; ok {
		return f.items[i].raw
	}
	return nil
}

// parseDotenv parses src strictly: every physical line must be blank, a
// comment or an entry; quotes must balance; keys must be unique.
func parseDotenv(src []byte) (*envFile, error) {
	f := &envFile{index: make(map[string]int)}
	pos, lineNo := 0, 0
	for pos < len(src) {
		lineNo++
		end := len(src)
		if i := bytes.IndexByte(src[pos:], '\n'); i >= 0 {
			end = pos + i + 1
		}
		content := stripTerminator(src[pos:end])
		trimmed := bytes.TrimLeft(content, " \t\r\v\f")
		switch {
		case len(trimmed) == 0:
			f.items = append(f.items, envItem{kind: envBlank, raw: src[pos:end]})
			pos = end
			continue
		case trimmed[0] == '#':
			f.items = append(f.items, envItem{kind: envComment, raw: src[pos:end]})
			pos = end
			continue
		}
		m := envEntryRe.FindSubmatchIndex(content)
		if m == nil {
			return nil, fmt.Errorf("line %d: not a dotenv line", lineNo)
		}
		key := string(content[m[4]:m[5]])
		value := content[m[6]:m[7]]
		itemEnd := end
		// A value that starts (right after '=') with a quote runs to the
		// matching closing quote, possibly on a later physical line, and
		// only whitespace or a comment may follow that quote on its line.
		// A value that starts with whitespace is plain text per the
		// grammar, whatever comes after the whitespace.
		if len(value) > 0 && (value[0] == '"' || value[0] == '\'') {
			q := value[0]
			open := pos + m[6] // absolute offset of the opening quote
			closeIdx := findClosingQuote(src, open+1, q)
			if closeIdx < 0 {
				return nil, fmt.Errorf("line %d: unbalanced %c quote in %s", lineNo, q, key)
			}
			closeLine := lineNo
			if closeIdx >= end {
				closeLine += 1 + bytes.Count(src[end:closeIdx], []byte{'\n'})
			}
			// The entry ends with the physical line holding the closing quote.
			closeEnd := len(src)
			if i := bytes.IndexByte(src[closeIdx:], '\n'); i >= 0 {
				closeEnd = closeIdx + i + 1
			}
			rest := bytes.TrimLeft(stripTerminator(src[closeIdx+1:closeEnd]), " \t\r\v\f")
			if len(rest) > 0 && rest[0] != '#' {
				return nil, fmt.Errorf("line %d: text after closing %c quote in %s", closeLine, q, key)
			}
			itemEnd = closeEnd
			lineNo = closeLine
		}
		if _, dup := f.index[key]; dup {
			return nil, fmt.Errorf("line %d: duplicate key %s", lineNo, key)
		}
		f.index[key] = len(f.items)
		f.items = append(f.items, envItem{kind: envEntry, key: key, raw: src[pos:itemEnd]})
		pos = itemEnd
	}
	return f, nil
}

// stripTerminator removes a trailing "\n" or "\r\n".
func stripTerminator(line []byte) []byte {
	n := len(line)
	if n > 0 && line[n-1] == '\n' {
		n--
		if n > 0 && line[n-1] == '\r' {
			n--
		}
	}
	return line[:n]
}

// findClosingQuote scans src from i for the closing quote q. Inside double
// quotes a backslash escapes the next byte; single quotes take every byte
// literally (so 'C:\' is balanced), as in the shell and common dotenv loaders.
func findClosingQuote(src []byte, i int, q byte) int {
	for i < len(src) {
		switch {
		case q == '"' && src[i] == '\\':
			i += 2
			continue
		case src[i] == q:
			return i
		}
		i++
	}
	return -1
}

// eqRaw compares raw entries byte for byte; nil (absent) only equals nil.
// The terminator of the entry's last physical line is ignored: it belongs to
// the file layout, not to the entry, so the final entry of a file without a
// trailing newline still equals its newline-terminated twin (and "\r\n" equals
// "\n" there, as in the text merge). Everything else, including quotes, the
// export prefix and the interior lines of a multi-line value, must match.
func eqRaw(a, b []byte) bool {
	if (a == nil) != (b == nil) {
		return false
	}
	return bytes.Equal(stripTerminator(a), stripTerminator(b))
}

// mergeDotenv merges key by key. It returns (nil, note) when any side fails
// strict parsing; the caller then falls back to the text merge.
func mergeDotenv(base, local, remote []byte) (*Result, string) {
	bf, err := parseDotenv(base)
	if err != nil {
		return nil, fmt.Sprintf("dotenv parse failed (base: %v): used text merge", err)
	}
	lf, err := parseDotenv(local)
	if err != nil {
		return nil, fmt.Sprintf("dotenv parse failed (local: %v): used text merge", err)
	}
	rf, err := parseDotenv(remote)
	if err != nil {
		return nil, fmt.Sprintf("dotenv parse failed (remote: %v): used text merge", err)
	}

	r := &Result{Kind: KindDotenv}
	var pending []byte
	emit := func(raw []byte) { pending = appendPart(pending, raw) }
	flush := func() {
		if len(pending) > 0 {
			r.segments = append(r.segments, segment{text: pending, hunk: -1})
			pending = nil
		}
	}
	conflict := func(key string, b, l, rm []byte) {
		flush()
		r.Hunks = append(r.Hunks, Hunk{Key: key, Base: clone(b), Local: clone(l), Remote: clone(rm)})
		r.segments = append(r.segments, segment{hunk: len(r.Hunks) - 1})
	}

	// Local order, comments and blank lines drive the output.
	for _, it := range lf.items {
		if it.kind != envEntry {
			emit(it.raw)
			continue
		}
		b, l, rm := bf.rawOf(it.key), it.raw, rf.rawOf(it.key)
		switch {
		case eqRaw(l, rm):
			emit(l)
		case eqRaw(l, b):
			emit(rm) // remote changed or removed the key
		case eqRaw(rm, b):
			emit(l) // only local changed
		default:
			conflict(it.key, b, l, rm)
		}
	}
	// Keys present on remote but not locally: new on remote → appended;
	// removed locally while remote kept the base value → dropped;
	// removed locally while remote changed it → conflict.
	for _, it := range rf.items {
		if it.kind != envEntry {
			continue
		}
		if _, ok := lf.index[it.key]; ok {
			continue
		}
		b := bf.rawOf(it.key)
		switch {
		case b == nil:
			emit(it.raw)
		case eqRaw(it.raw, b):
		default:
			conflict(it.key, b, nil, it.raw)
		}
	}
	flush()
	if len(r.Hunks) == 0 {
		r.Clean = true
		r.Merged = []byte{}
		for _, s := range r.segments {
			r.Merged = appendPart(r.Merged, s.text)
		}
	}
	return r, ""
}
