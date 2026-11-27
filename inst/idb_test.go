package inst

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func parseOne(t *testing.T, line string) Entry {
	t.Helper()
	entries, bad, err := ParseIDB(strings.NewReader(line + "\n"))
	if err != nil || len(bad) != 0 || len(entries) != 1 {
		t.Fatalf("ParseIDB(%q) = %v, %v, %v", line, entries, bad, err)
	}
	return entries[0]
}

func TestFields(t *testing.T) {
	for _, typ := range "fdlbcpsX" {
		if e := parseOne(t, string(typ)+" 0755 root sys usr/bin/x src/x p.i.s"); e.Type != byte(typ) {
			t.Errorf("type %c: got %c", typ, e.Type)
		}
	}
	for in, want := range map[string]uint32{"0755": 0o755, "4755": 0o4755, "644": 0o644, "0": 0, "02755": 0o2755} {
		if e := parseOne(t, "f "+in+" root sys a s p.i.s"); e.Mode != want {
			t.Errorf("mode %s = %o, want %o", in, e.Mode, want)
		}
	}
	e := parseOne(t, "f 0644 root sys usr/share/a.z src/a off(1234) size(100) cmpsize(40) prod.img.sub")
	if e.Owner != "root" || e.Group != "sys" || e.Path != "usr/share/a.z" || e.Source != "src/a" || e.Image() != "img" {
		t.Errorf("fields: %+v", e)
	}
	if e.Offset != 1234 || e.Size != 100 || e.CmpSize != 40 || e.Stored() != 40 {
		t.Errorf("off %d size %d stored %d", e.Offset, e.Size, e.Stored())
	}
	if e := parseOne(t, "l 0777 root sys usr/bin/ls src symval(../../bin/ls.real) p.i.s"); e.Symlink != "../../bin/ls.real" {
		t.Errorf("symlink %q", e.Symlink)
	}
	if e := parseOne(t, "d 0755 root sys usr/bin src p"); e.Offset != -1 || e.Size != 0 || e.Stored() != 0 || e.Image() != "sw" {
		t.Errorf("no attributes: %+v", e)
	}
	// A zero cmpsize, however written, means stored as-is.
	for _, cmp := range []string{"0", "00"} {
		if e := parseOne(t, "f 0644 root sys a s size(9) cmpsize("+cmp+") p.i.s"); e.Stored() != 9 {
			t.Errorf("cmpsize(%s): stored %d", cmp, e.Stored())
		}
	}
}

func TestSubsystemPlacement(t *testing.T) {
	cases := []struct {
		name, tail string
		sub        string
		flags      []string
		attrs      map[string]string
	}{
		{"before attrs", "p.i.s off(1) size(2)", "p.i.s", nil, map[string]string{"off": "1", "size": "2"}},
		{"after attrs", "off(1) size(2) p.i.s", "p.i.s", nil, map[string]string{"off": "1", "size": "2"}},
		{"flag after", "off(1) p.i.s needrqs", "p.i.s", []string{"needrqs"}, map[string]string{"off": "1"}},
		{"flag before", "needrqs off(1) p.i.s", "p.i.s", []string{"needrqs"}, map[string]string{"off": "1"}},
		{"undotted fallback", "needrqs off(1)", "needrqs", nil, map[string]string{"off": "1"}},
		{"spaces in value", "config(update suggest) p.i.s", "p.i.s", nil, map[string]string{"config": "update suggest"}},
		{"empty tail", "", "", nil, map[string]string{}},
		{"mach", "mach(CPUBOARD=IP27) p.i.s", "p.i.s", nil, map[string]string{"mach": "CPUBOARD=IP27"}},
		// A dotted word inside a quoted value must not become the
		// subsystem, and a ')' inside one does not end it.
		{"quoted paren", `p.i.s exitop("if test -n $(ls); then a.b.c; fi")`, "p.i.s", nil, map[string]string{"exitop": `"if test -n $(ls); then a.b.c; fi"`}},
		{"quoted then subsystem", `postop("x) y.z.w") p.i.s`, "p.i.s", nil, map[string]string{"postop": `"x) y.z.w"`}},
		// The shell command is echo '(done)'; the ')' in the quotes does not end the attribute.
		{"single-quoted paren", `p.i.s postop(echo '(done)')`, "p.i.s", nil, map[string]string{"postop": `echo '(done)'`}},
		{"single-quoted paren before subsystem", `postop(echo '(done)') p.i.s`, "p.i.s", nil, map[string]string{"postop": `echo '(done)'`}},
		{"single-quoted subshell", `p.i.s exitop('( if test -f x; then a; fi )')`, "p.i.s", nil, map[string]string{"exitop": `'( if test -f x; then a; fi )'`}},
		{"apostrophes in two attributes", `p.i.s config(don't) other(can't)`, "p.i.s", nil, map[string]string{"config": "don't", "other": "can't"}},
		{"apostrophe then quote", `p.i.s postop(echo it's done) needrqs preop(echo 'a')`, "p.i.s", []string{"needrqs"}, map[string]string{"postop": "echo it's done", "preop": "echo 'a'"}},
		{"quote inside double quotes", `postop(tr -d "'" < x > y) p.i.s config(don't)`, "p.i.s", nil, map[string]string{"postop": `tr -d "'" < x > y`, "config": "don't"}},
		{"escaped quote", `postop(echo \' x) p.i.s config(can't)`, "p.i.s", nil, map[string]string{"postop": `echo \' x`, "config": "can't"}},
		{"unmatched quote after space", `postop(echo 'x) p.i.s config(a 'b)`, "p.i.s", nil, map[string]string{"postop": "echo 'x", "config": "a 'b"}},
		{"unbalanced paren in quotes", `postop(echo ')') p.i.s`, "p.i.s", []string{"')"}, map[string]string{"postop": "echo '"}},
		{"backslash paren", `postop(echo \) x) p.i.s`, "p.i.s", []string{"x)"}, map[string]string{"postop": `echo \`}},
		{"unbalanced paren before a later quote", `postop(echo 'x ( ) p.i.s mach('y)`, "p.i.s", nil, map[string]string{"postop": "echo 'x ( ", "mach": "'y"}},
		{"unbalanced paren in a lone quote pair", `p.i.s postop(' ( ) b(')`, "p.i.s", nil, map[string]string{"postop": "' ( ", "b": "'"}},
		{"cosmoworlds style", `p.i.s exitop('(if [ -x a ]; then b; fi ) >/dev/null 2>&1')`, "p.i.s", nil, map[string]string{"exitop": "'(if [ -x a ]; then b; fi ) >/dev/null 2>&1'"}},
		{"apostrophe", `p.i.s config(don't) needrqs`, "p.i.s", []string{"needrqs"}, map[string]string{"config": "don't"}},
		{"quoted dotted after subsystem", `p.i.s exitop("cd $(dirname a.b.c)")`, "p.i.s", nil, map[string]string{"exitop": `"cd $(dirname a.b.c)"`}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := parseOne(t, "f 0644 root sys d s "+c.tail)
			if e.Subsystem != c.sub {
				t.Errorf("subsystem %q, want %q", e.Subsystem, c.sub)
			}
			if !reflect.DeepEqual(e.Flags, c.flags) {
				t.Errorf("flags %v, want %v", e.Flags, c.flags)
			}
			if !reflect.DeepEqual(e.Attrs, c.attrs) {
				t.Errorf("attrs %v, want %v", e.Attrs, c.attrs)
			}
		})
	}
}

func TestSkipAndBad(t *testing.T) {
	in := strings.Join([]string{
		"# comment",
		"",
		"   ",
		"f 0644 root sys a s p.i.s",
		"f 0644 root sys a",
		"x 0644 root sys a s p.i.s",
		"f 09 root sys a s p.i.s",
		"d 0755 root sys b s p.i.s",
		`f 0644 root sys c s p.i.s exitop("unterminated`,
		`f 0644 root sys c s p.i.s exitop("x"y)`,
		"f 0644 root sys c s off(13) size(abc) cmpsize(0) p.i.s",
		"f 0644 root sys c s off(13) size(10) cmpsize(abc) p.i.s",
		"f 0644 root sys c s off(-1) size(10) p.i.s",
		"f 0644 root sys c s off(0x10) size(10) p.i.s",
		"f 0644 root sys c s off(13) size(99999999999999999999) p.i.s",
		"f 0644 root sys c s off() size(1) p.i.s",
	}, "\n")
	entries, bad, err := ParseIDB(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].Path != "a" || entries[1].Path != "b" {
		t.Errorf("entries %+v", entries)
	}
	wantLines := []int{5, 6, 7, 9, 10, 11, 12, 13, 14, 15, 16}
	if len(bad) != len(wantLines) {
		t.Fatalf("bad %+v", bad)
	}
	for i, b := range bad {
		if b.Line != wantLines[i] || b.Text == "" || b.Reason == "" {
			t.Errorf("bad[%d] = %+v", i, b)
		}
	}
}

func TestNoTrailingNewlineAndCRLF(t *testing.T) {
	entries, bad, err := ParseIDB(strings.NewReader("f 0644 root sys a s p.i.s\r\nd 0755 root sys b s p.i.s"))
	if err != nil || len(bad) != 0 || len(entries) != 2 {
		t.Fatalf("%v %v %v", entries, bad, err)
	}
	if entries[0].Subsystem != "p.i.s" {
		t.Errorf("CR leaked: %q", entries[0].Subsystem)
	}
}

func TestBareCRLineEndings(t *testing.T) {
	// A file with no LF ends its lines in bare CRs; line 3 is bad.
	in := "# header\rf 0644 root sys a s p.i.s\rf 0644 bogus\rl 0777 root sys c s p.i.s symval(a)\r"
	for _, tail := range []string{"", "\n", "\r\n"} { // a final LF changes nothing
		entries, bad, err := ParseIDB(strings.NewReader(in + tail))
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 2 || entries[0].Path != "a" || entries[1].Path != "c" {
			t.Fatalf("%q: entries = %+v", tail, entries)
		}
		if entries[0].Subsystem != "p.i.s" || entries[1].Symlink != "a" {
			t.Errorf("CR leaked into a field: %+v", entries)
		}
		if len(bad) != 1 || bad[0].Line != 3 || bad[0].Text != "f 0644 bogus" {
			t.Errorf("%q: bad = %+v", tail, bad)
		}
	}
}

// A stray CR inside the only entry of a file stays in the entry, with or
// without a final newline, as do CRs in a file whose lines do not all look like entries.
func TestStrayCRInSingleLine(t *testing.T) {
	for _, in := range []string{
		"f 0644 root sys a s p.i.s postop(echo a\rb)",
		"f 0644 root sys a s p.i.s postop(echo a\rb)\n",
		"f 0644 root sys a s p.i.s postop(echo a\rb)\r\n",
		"f 0644 root sys a s p.i.s postop(echo a\rb)\rtext\r",
	} {
		entries, bad, err := ParseIDB(strings.NewReader(in))
		if err != nil || len(entries) != 1 || len(bad) != 0 || entries[0].Attrs["postop"] != "echo a\rb" {
			t.Errorf("%q: %+v %+v %v", in, entries, bad, err)
		}
	}
}

// In a file with LFs, a CR inside a line is part of the line, and extra CRs
// before the LF do not add lines.
func TestStrayCRInLFFile(t *testing.T) {
	in := "f 0644 root sys a s p.i.s postop(echo a\rb)\r\r\nbogus\r\nd 0755 root sys c s p.i.s\nbogus2"
	entries, bad, err := ParseIDB(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].Attrs["postop"] != "echo a\rb" || entries[1].Path != "c" {
		t.Fatalf("entries = %+v", entries)
	}
	if len(bad) != 2 || bad[0].Line != 2 || bad[1].Line != 4 || bad[1].Text != "bogus2" {
		t.Errorf("bad = %+v", bad)
	}
}

func TestLongLine(t *testing.T) {
	long := "f 0644 root sys " + strings.Repeat("a", 200000) + " s p.i.s"
	e := parseOne(t, long)
	if len(e.Path) != 200000 {
		t.Errorf("path length %d", len(e.Path))
	}
}

type failReader struct{}

func (failReader) Read([]byte) (int, error) { return 0, errors.New("boom") }

func TestIOError(t *testing.T) {
	if _, _, err := ParseIDB(failReader{}); err == nil {
		t.Fatal("want error")
	}
}
