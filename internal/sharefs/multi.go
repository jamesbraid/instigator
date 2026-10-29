package sharefs

import (
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/jamesbraid/instigator/nfs"
)

// Multi serves several shares from one nfs.Server, one export each. A
// request goes to the share its node or filehandle belongs to, and a
// MOUNT path picks a share by its first element. The shares keep their
// own handle rules, so a handle from a share served alone still works.
type Multi struct {
	shares []*FS
	byID   map[uint32]*FS
	byName map[string]*FS
}

// OpenMulti wraps shares, which must have distinct names. Multi takes
// ownership: Close closes every share.
func OpenMulti(shares ...*FS) (*Multi, error) {
	m := &Multi{shares: shares, byID: map[uint32]*FS{}, byName: map[string]*FS{}}
	for _, s := range shares {
		if m.byName[s.name] != nil {
			return nil, fmt.Errorf("sharefs: share %q given twice", s.name)
		}
		if o := m.byID[s.exportID]; o != nil {
			return nil, fmt.Errorf("sharefs: shares %q and %q have the same id", o.name, s.name)
		}
		m.byName[s.name] = s
		m.byID[s.exportID] = s
	}
	if len(shares) == 0 {
		return nil, errors.New("sharefs: no shares")
	}
	return m, nil
}

// Close closes every share.
func (m *Multi) Close() error {
	var errs []error
	for _, s := range m.shares {
		errs = append(errs, s.Close())
	}
	return errors.Join(errs...)
}

// Exports lists every share's mount path, in the order given.
func (m *Multi) Exports() []string {
	var out []string
	for _, s := range m.shares {
		out = append(out, s.Exports()...)
	}
	return out
}

// Space reports the first share's capacity: the protocol asks without
// naming a file.
func (m *Multi) Space() (nfs.Space, error) { return m.shares[0].Space() }

func (m *Multi) of(n nfs.Node) (*FS, error) {
	nn, err := own(n)
	if err != nil {
		return nil, err
	}
	return nn.fs, nil
}

func (m *Multi) MountRoot(p string) (nfs.Node, error) {
	first, _, _ := cutPath(p)
	s := m.byName[first]
	if s == nil {
		return nil, nfs.ErrNotFound
	}
	return s.MountRoot(p)
}

func cutPath(p string) (first, rest string, ok bool) {
	for len(p) > 0 && p[0] == '/' {
		p = p[1:]
	}
	for i := 0; i < len(p); i++ {
		if p[i] == '/' {
			return p[:i], p[i+1:], true
		}
	}
	return p, "", false
}

// NodeByID is unused: the server resolves handles.
func (m *Multi) NodeByID(uint64) (nfs.Node, error) { return nil, nfs.ErrStale }

func (m *Multi) Handle(n nfs.Node) []byte {
	s, err := m.of(n)
	if err != nil {
		return make([]byte, 32)
	}
	return s.Handle(n)
}

func (m *Multi) NodeByHandle(fh []byte) (nfs.Node, error) {
	if len(fh) != 32 {
		return nil, nfs.ErrBadHandle
	}
	s := m.byID[binary.BigEndian.Uint32(fh[4:])]
	if s == nil {
		// A well-formed handle for a share we don't serve is stale; let
		// the first share judge the format so garbage is BADHANDLE.
		if _, err := m.shares[0].NodeByHandle(fh); errors.Is(err, nfs.ErrBadHandle) {
			return nil, err
		}
		return nil, nfs.ErrStale
	}
	return s.NodeByHandle(fh)
}

func (m *Multi) Lookup(dir nfs.Node, name string) (nfs.Node, error) {
	s, err := m.of(dir)
	if err != nil {
		return nil, err
	}
	return s.Lookup(dir, name)
}

func (m *Multi) ReadDir(dir nfs.Node) ([]nfs.DirEntry, error) {
	s, err := m.of(dir)
	if err != nil {
		return nil, err
	}
	return s.ReadDir(dir)
}

func (m *Multi) ReadAt(n nfs.Node, p []byte, off int64) (int, error) {
	s, err := m.of(n)
	if err != nil {
		return 0, err
	}
	return s.ReadAt(n, p, off)
}

func (m *Multi) Readlink(n nfs.Node) (string, error) {
	s, err := m.of(n)
	if err != nil {
		return "", err
	}
	return s.Readlink(n)
}

func (m *Multi) SetAttr(n nfs.Node, a nfs.SetAttr) error {
	s, err := m.of(n)
	if err != nil {
		return err
	}
	return s.SetAttr(n, a)
}

func (m *Multi) WriteAt(n nfs.Node, p []byte, off int64) error {
	s, err := m.of(n)
	if err != nil {
		return err
	}
	return s.WriteAt(n, p, off)
}

func (m *Multi) Create(dir nfs.Node, name string, how nfs.CreateHow, a nfs.SetAttr, verf [8]byte) (nfs.Node, error) {
	s, err := m.of(dir)
	if err != nil {
		return nil, err
	}
	return s.Create(dir, name, how, a, verf)
}

func (m *Multi) Mkdir(dir nfs.Node, name string, a nfs.SetAttr) (nfs.Node, error) {
	s, err := m.of(dir)
	if err != nil {
		return nil, err
	}
	return s.Mkdir(dir, name, a)
}

func (m *Multi) Symlink(dir nfs.Node, name, target string, a nfs.SetAttr) (nfs.Node, error) {
	s, err := m.of(dir)
	if err != nil {
		return nil, err
	}
	return s.Symlink(dir, name, target, a)
}

func (m *Multi) Remove(dir nfs.Node, name string) error {
	s, err := m.of(dir)
	if err != nil {
		return err
	}
	return s.Remove(dir, name)
}

func (m *Multi) Rmdir(dir nfs.Node, name string) error {
	s, err := m.of(dir)
	if err != nil {
		return err
	}
	return s.Rmdir(dir, name)
}

// Rename refuses to move a file between shares, as between filesystems.
func (m *Multi) Rename(fromDir nfs.Node, fromName string, toDir nfs.Node, toName string) error {
	s, err := m.of(fromDir)
	if err != nil {
		return err
	}
	t, err := m.of(toDir)
	if err != nil {
		return err
	}
	if s != t {
		return nfs.ErrXDev
	}
	return s.Rename(fromDir, fromName, toDir, toName)
}

var (
	_ nfs.WriteFS  = (*Multi)(nil)
	_ nfs.HandleFS = (*Multi)(nil)
	_ nfs.SpaceFS  = (*Multi)(nil)
	_ nfs.Exporter = (*Multi)(nil)
)
