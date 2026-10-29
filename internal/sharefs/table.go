package sharefs

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// pathTable numbers the paths of a share for filehandles on a host
// without stable inode numbers. It lives in a file of lines, each
// "set ID PATH" with PATH quoted as Go quotes strings, or "del ID".
// A "next ID" line, written when the table is compacted, keeps ids above
// ID from being handed out again: an id freed by a removal is never reused,
// so a handle to a removed file cannot reach a later one. Opening the table
// replays the file and rewrites it compacted; changes after that are
// appended, so the numbering survives a restart.
type pathTable struct {
	mu     sync.Mutex
	f      *os.File
	next   uint64
	byID   map[uint64]string
	byPath map[string]uint64
}

func openPathTable(file string) (*pathTable, error) {
	t := &pathTable{next: 1, byID: map[uint64]string{}, byPath: map[string]uint64{}}
	if err := t.load(file); err != nil {
		return nil, err
	}
	// Rewrite the replayed table beside the old one and swap it in.
	tmp := file + ".new"
	out, err := os.Create(tmp)
	if err != nil {
		return nil, err
	}
	ids := make([]uint64, 0, len(t.byID))
	for id := range t.byID {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	w := bufio.NewWriter(out)
	fmt.Fprintf(w, "next %d\n", t.next)
	for _, id := range ids {
		fmt.Fprintf(w, "set %d %s\n", id, strconv.Quote(t.byID[id]))
	}
	err = w.Flush()
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, file)
	}
	if err != nil {
		os.Remove(tmp)
		return nil, err
	}
	if t.f, err = os.OpenFile(file, os.O_WRONLY|os.O_APPEND, 0o600); err != nil {
		return nil, err
	}
	return t, nil
}

func (t *pathTable) load(file string) error {
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		return err
	}
	in, err := os.Open(file)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer in.Close()
	sc := bufio.NewScanner(in)
	sc.Buffer(nil, 1<<20)
	for n := 1; sc.Scan(); n++ {
		f := strings.SplitN(sc.Text(), " ", 3)
		if len(f) < 2 {
			return fmt.Errorf("%s:%d: malformed line", file, n)
		}
		id, err := strconv.ParseUint(f[1], 10, 64)
		if err != nil {
			return fmt.Errorf("%s:%d: %v", file, n, err)
		}
		switch {
		case f[0] == "next" && len(f) == 2:
			t.next = max(t.next, id)
		case f[0] == "set" && len(f) == 3:
			p, err := strconv.Unquote(f[2])
			if err != nil {
				return fmt.Errorf("%s:%d: %v", file, n, err)
			}
			t.set(id, p)
		case f[0] == "del":
			t.del(id)
		default:
			return fmt.Errorf("%s:%d: malformed line", file, n)
		}
	}
	return sc.Err()
}

func (t *pathTable) set(id uint64, p string) {
	if old, ok := t.byID[id]; ok {
		delete(t.byPath, old)
	}
	t.byID[id] = p
	t.byPath[p] = id
	t.next = max(t.next, id+1)
}

func (t *pathTable) del(id uint64) {
	delete(t.byPath, t.byID[id])
	delete(t.byID, id)
}

func (t *pathTable) close() error { return t.f.Close() }

// idFor returns p's id, numbering p if the table hasn't seen it.
func (t *pathTable) idFor(p string) (uint64, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if id, ok := t.byPath[p]; ok {
		return id, nil
	}
	id := t.next
	if _, err := fmt.Fprintf(t.f, "set %d %s\n", id, strconv.Quote(p)); err != nil {
		return 0, err
	}
	t.set(id, p)
	return id, nil
}

func (t *pathTable) path(id uint64) (string, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	p, ok := t.byID[id]
	return p, ok
}

// remove forgets p and everything under it.
func (t *pathTable) remove(p string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for id, q := range t.byID {
		if under(q, p) {
			fmt.Fprintf(t.f, "del %d\n", id)
			t.del(id)
		}
	}
}

// move renames p, and everything under it, to q, keeping their ids.
func (t *pathTable) move(p, q string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for id, old := range t.byID {
		if under(old, p) {
			np := q + old[len(p):]
			fmt.Fprintf(t.f, "set %d %s\n", id, strconv.Quote(np))
			t.set(id, np)
		}
	}
}
