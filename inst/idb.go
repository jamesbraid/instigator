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
	Path      string // install path, as written
	Source    string // build-tree source path
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

// ParseIDB reads a manifest. Blank lines and lines starting with '#' are
// ignored. Every other line that does not parse, including one whose off(),
// size() or cmpsize() is not a non-negative decimal number, is returned as
// a BadLine instead of being skipped, so a format variant shows up. The
// error is non-nil only for a read failure.
func ParseIDB(r io.Reader) ([]Entry, []BadLine, error) {
	var entries []Entry
	var bad []BadLine
	br := bufio.NewReader(r)
	for n := 1; ; n++ {
		line, err := br.ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return entries, bad, err
		}
		if line != "" {
			text := strings.TrimRight(line, "\r\n")
			if e, reason := parseLine(text); reason != "" {
				bad = append(bad, BadLine{Line: n, Text: text, Reason: reason})
			} else if e != nil {
				entries = append(entries, *e)
			}
		}
		if err != nil {
			return entries, bad, nil
		}
	}
}

// parseLine returns (nil, "") for lines to skip.
func parseLine(text string) (*Entry, string) {
	trim := strings.TrimSpace(text)
	if trim == "" || trim[0] == '#' {
		return nil, ""
	}
	f := strings.Fields(trim)
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
	// Re-find the tail in the original text: attribute values may contain
	// spaces, so it cannot be rebuilt from Fields.
	tail := trim
	for i := 0; i < 6; i++ {
		tail = strings.TrimLeft(tail, " \t")
		j := strings.IndexAny(tail, " \t")
		if j < 0 {
			tail = ""
			break
		}
		tail = tail[j:]
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

// scanTail splits the tail into key(value) attributes and bare words. A
// value runs to the first ')' and may contain spaces. A value that starts
// with '"' runs to the next '"', which must be followed by ')'; the quotes
// are kept. A repeated key keeps its last value. It returns a reason when
// the tail cannot be split.
func scanTail(tail string, attrs map[string]string, words *[]string) string {
	for i := 0; i < len(tail); {
		if c := tail[i]; c == ' ' || c == '\t' {
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
			if end := strings.IndexByte(tail[j:], ')'); end >= 0 {
				attrs[tail[i:j]] = tail[j+1 : j+end]
				i = j + end + 1
				continue
			}
		}
		k := i
		for k < len(tail) && tail[k] != ' ' && tail[k] != '\t' {
			k++
		}
		*words = append(*words, tail[i:k])
		i = k
	}
	return ""
}

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
