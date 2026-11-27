package inst

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// Entry is one .idb manifest line:
//
//	type mode owner group dest source [tail...]
//
// The tail mixes key(value) attributes (off, size, cmpsize, symval, mach,
// config, ...), bare flag words such as needrqs or delhist, and the
// subsystem name (product.image.name). A value in double quotes, such as a
// shell command in exitop("..."), runs to the closing quote and may contain
// ')'.
type Entry struct {
	Type      byte   // f d l b c p s, or X (no data; meaning unconfirmed)
	Mode      uint32 // parsed from octal
	Owner     string
	Group     string
	Path      string // install path, without the quotes a path with spaces is written in
	Source    string // build-tree source path, likewise
	Subsystem string // product.image.subsystem

	// Offset is where the entry's archive record starts: off(), or for a
	// file without one the offset OpenDist finds by walking the image.
	// It is -1 when there is no record.
	Offset  int64
	Size    int64  // size(), the installed length
	CmpSize int64  // cmpsize(): non-zero when the archive compress(1)ed the record, its stored length
	Symlink string // symval(), a link's target
	Mach    string // mach(), the machines the entry is for

	Attrs map[string]string // every key(value) attribute, the above included
	Flags []string          // bare words other than the subsystem
}

// BadLine is a non-blank, non-comment line that could not be parsed.
type BadLine struct {
	Line   int    // 1-based line number
	Text   string // the line, without its line terminator
	Reason string
}

// Image returns the image holding the entry's record, the middle word of
// its subsystem ("eoe.man.base" is stored in eoe.man), or "sw" for a
// subsystem naming none.
func (e Entry) Image() string {
	if parts := strings.Split(e.Subsystem, "."); len(parts) > 2 {
		return parts[1]
	}
	return "sw"
}

// Stored returns the length of the entry's record data: cmpsize() when
// non-zero, else size().
func (e Entry) Stored() int64 {
	if e.CmpSize != 0 {
		return e.CmpSize
	}
	return e.Size
}

// ParseIDB reads a manifest whose lines end in LF or CRLF. A single-line file
// (no LF, or only a final one) ends its lines at bare CRs when each looks like
// an entry or comment. Blank and '#' lines are ignored; any other line that
// does not parse, including a non-decimal off(), size() or cmpsize(), is a
// BadLine. The error is non-nil only for a read failure.
func ParseIDB(r io.Reader) ([]Entry, []BadLine, error) {
	var entries []Entry
	var bad []BadLine
	br := bufio.NewReader(r)
	for n, first := 1, true; ; first = false {
		line, err := br.ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return entries, bad, err
		}
		lines := []string{strings.TrimRight(line, "\r\n")}
		if _, perr := br.Peek(1); first && errors.Is(perr, io.EOF) {
			lines = crLines(lines[0])
		}
		for _, text := range lines {
			if e, reason := parseLine(text); reason != "" {
				bad = append(bad, BadLine{Line: n, Text: text, Reason: reason})
			} else if e != nil {
				entries = append(entries, *e)
			}
			n++
		}
		if err != nil {
			return entries, bad, nil
		}
	}
}

// crLines splits text at bare CRs, but only if every line looks like an entry or
// comment; a stray CR inside one line leaves it whole.
func crLines(text string) []string {
	lines := strings.Split(text, "\r")
	for _, l := range lines {
		l = strings.TrimLeft(l, " \t")
		if l != "" && l[0] != '#' && (len(l) < 2 || !strings.Contains("fdlbcpsX", l[:1]) || l[1] != ' ' && l[1] != '\t') {
			return []string{text}
		}
	}
	return lines
}

// parseLine returns (nil, "") for lines to skip.
func parseLine(text string) (*Entry, string) {
	trim := strings.TrimSpace(text)
	if trim == "" || trim[0] == '#' {
		return nil, ""
	}
	f, tail := splitHead(trim)
	if len(f) < 6 {
		return nil, fmt.Sprintf("want at least 6 fields, got %d", len(f))
	}
	if len(f[0]) != 1 || !strings.Contains("fdlbcpsX", f[0]) {
		return nil, fmt.Sprintf("unknown entry type %q", f[0])
	}
	mode, err := strconv.ParseUint(f[1], 8, 32)
	if err != nil {
		return nil, fmt.Sprintf("bad octal mode %q", f[1])
	}
	e := &Entry{
		Type: f[0][0], Mode: uint32(mode),
		Owner: f[2], Group: f[3], Path: f[4], Source: f[5],
		Offset: -1, Attrs: map[string]string{},
	}
	var words []string
	if reason := scanTail(tail, e.Attrs, &words); reason != "" {
		return nil, reason
	}
	for _, n := range []struct {
		key string
		dst *int64
	}{{"off", &e.Offset}, {"size", &e.Size}, {"cmpsize", &e.CmpSize}} {
		v, ok := e.Attrs[n.key]
		if !ok {
			continue
		}
		var err error
		if *n.dst, err = strconv.ParseInt(v, 10, 64); err != nil || strings.Trim(v, "0123456789") != "" {
			return nil, fmt.Sprintf("%s(%s) is not a decimal number", n.key, v)
		}
	}
	e.Symlink, e.Mach = e.Attrs["symval"], e.Attrs["mach"]
	e.Subsystem, e.Flags = splitSubsystem(words)
	return e, ""
}

// splitHead returns the first six fields of a line and the text after them.
// A double-quoted field loses its quotes; an unclosed quote or "" does not.
func splitHead(line string) ([]string, string) {
	var f []string
	for len(f) < 6 {
		line = strings.TrimLeft(line, space)
		if line == "" {
			break
		}
		n := strings.IndexAny(line, space)
		if n < 0 {
			n = len(line)
		}
		if line[0] == '"' {
			if q := strings.IndexByte(line[1:], '"'); q > 0 {
				if e := q + 2; e == len(line) || strings.IndexByte(space, line[e]) >= 0 {
					f, line = append(f, line[1:e-1]), line[e:]
					continue
				}
			}
		}
		f, line = append(f, line[:n]), line[n:]
	}
	return f, line
}

// closingParen returns the index of the ')' ending an attribute value, or -1.
// A ')' in single quotes, as in nekoware's exitop('( ... )'), does not end it.
func closingParen(s string) int {
	dq := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\\' && i+1 < len(s) && s[i+1] == '\'':
			i++
		case c == '"':
			dq = !dq
		case c == '\'' && !dq && (i == 0 || !isWord(s[i-1])):
			if k := quoteEnd(s[i+1:]); k >= 0 {
				i += k + 1
			}
		case c == ')':
			return i
		}
	}
	return -1
}

// quoteEnd returns the index of the quote closing s, or -1 unless the text
// before it has balanced parentheses.
func quoteEnd(s string) int {
	depth := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\'':
			if depth == 0 {
				return i
			}
			return -1
		case '(':
			depth++
		case ')':
			if depth--; depth < 0 {
				return -1
			}
		}
	}
	return -1
}

// scanTail splits the tail into key(value) attributes and bare words. A
// value runs to the first ')' outside single quotes and may contain spaces.
// A value that starts with '"' runs to the next '"', which must be followed
// by ')'; the quotes are kept. A repeated key keeps its last value. It
// returns a reason when the tail cannot be split.
func scanTail(tail string, attrs map[string]string, words *[]string) string {
	for i := 0; i < len(tail); {
		if isSpace(tail[i]) {
			i++
			continue
		}
		j := i
		for j < len(tail) && isWord(tail[j]) {
			j++
		}
		if j > i && j+1 < len(tail) && tail[j] == '(' && tail[j+1] == '"' {
			q := strings.IndexByte(tail[j+2:], '"')
			if q < 0 {
				return fmt.Sprintf("%s( has an unterminated quoted value", tail[i:j])
			}
			end := j + 2 + q + 1 // just past the closing quote
			if end >= len(tail) || tail[end] != ')' {
				return fmt.Sprintf("%s( quoted value is not followed by ')'", tail[i:j])
			}
			attrs[tail[i:j]] = tail[j+1 : end]
			i = end + 1
			continue
		}
		if j > i && j < len(tail) && tail[j] == '(' {
			if end := closingParen(tail[j+1:]); end >= 0 {
				attrs[tail[i:j]] = tail[j+1 : j+1+end]
				i = j + end + 2
				continue
			}
		}
		k := i
		for k < len(tail) && !isSpace(tail[k]) {
			k++
		}
		*words = append(*words, tail[i:k])
		i = k
	}
	return ""
}

// space is what separates the words of a manifest line.
const space = " \t\v\f\r"

func isSpace(c byte) bool { return strings.IndexByte(space, c) >= 0 }

func isWord(c byte) bool {
	return c == '_' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

// splitSubsystem picks the subsystem out of the non-attribute tail words and
// returns the rest as flags.
//
// Vendor build tooling disagrees on where the subsystem goes: some products
// put it right after the source path, before the attributes; others put the
// attributes first and the subsystem last, sometimes after a bare flag word
// like needrqs. A fixed position, or the first non-attribute word, misreads
// one of those orderings. A subsystem name is always dotted
// (product.image.name) and no flag or attribute is, so the last dotted word
// is the subsystem. A plain product name with no dot falls back to the first
// word, which is then not reported as a flag.
func splitSubsystem(words []string) (string, []string) {
	idx := -1
	for i := len(words) - 1; i >= 0; i-- {
		if strings.Contains(words[i], ".") {
			idx = i
			break
		}
	}
	if idx < 0 {
		if len(words) == 0 {
			return "", nil
		}
		idx = 0
	}
	var flags []string
	for i, w := range words {
		if i != idx {
			flags = append(flags, w)
		}
	}
	return words[idx], flags
}
