package nfs_test

import (
	"strings"
	"time"
)

// Scenarios that generate testdata/transcripts. Each runs calls in order
// against a fresh server and fixture; a call that needs a filehandle
// takes it from an earlier reply, as a client would.

const (
	progPortmap = 100000
	progNFS     = 100003
	progMount   = 100005

	protoTCP = 6
	protoUDP = 17
)

type scenario struct {
	fixture string
	run     func(*recorder)
}

var scenarios = map[string]scenario{
	"rpc":     {"tree", rpcScenario},
	"portmap": {"tree", portmapScenario},
	"mount1":  {"tree", mount1Scenario},
	"mount3":  {"tree", mount3Scenario},
	"nfs2":    {"tree", nfs2Scenario},
	"nfs3":    {"tree", nfs3Scenario},

	"share-nfs2": {"share", shareNFS2Scenario},
	"share-nfs3": {"share", shareNFS3Scenario},
}

// rpcScenario covers the RPC layer's own refusals (RFC 5531 section 9).
func rpcScenario(r *recorder) {
	r.raw("RPC version 1 is RPC_MISMATCH", (&xdr{}).u32(1).u32(0).u32(1).u32(progNFS).u32(2).u32(0).fixed(authNull).b)
	r.call("an unknown program is PROG_UNAVAIL", 100099, 1, 0, nil)
	r.call("NFS version 4 is PROG_MISMATCH", progNFS, 4, 0, nil)
	r.call("MOUNT version 2 is PROG_MISMATCH", progMount, 2, 0, nil)
	r.call("portmap version 3 is PROG_MISMATCH", progPortmap, 3, 0, nil)
	r.call("an unknown procedure is PROC_UNAVAIL", progNFS, 3, 22, nil)
	r.raw("a truncated credential is GARBAGE_ARGS", (&xdr{}).u32(7).u32(0).u32(2).u32(progNFS).u32(3).u32(0).u32(1).u32(400).b)
}

func portmapScenario(r *recorder) {
	r.call("NULL", progPortmap, 2, 0, nil)
	getport := func(label string, prog, vers, prot uint32) {
		r.call("GETPORT "+label, progPortmap, 2, 3, (&xdr{}).u32(prog).u32(vers).u32(prot).u32(0).b)
	}
	getport("NFS v2 over UDP", progNFS, 2, protoUDP)
	getport("NFS v3 over TCP", progNFS, 3, protoTCP)
	getport("MOUNT v1 over UDP", progMount, 1, protoUDP)
	getport("MOUNT v3 over TCP", progMount, 3, protoTCP)
	getport("portmap itself over TCP", progPortmap, 2, protoTCP)
	getport("an unserved version answers port 0", progNFS, 4, protoTCP)
	getport("an unknown transport answers port 0", progNFS, 3, 132)
	getport("an unregistered program answers port 0", 100021, 1, protoUDP)
	r.call("DUMP", progPortmap, 2, 4, nil)
	r.call("CALLIT is not served", progPortmap, 2, 5, nil)
}

func mount1Scenario(r *recorder) {
	r.call("NULL", progMount, 1, 0, nil)
	r.call("MNT the export root", progMount, 1, 1, (&xdr{}).str("/").b)
	r.callWith(authUnix, "MNT with AUTH_UNIX credentials", progMount, 1, 1, (&xdr{}).str("/").b)
	r.call("MNT a subdirectory", progMount, 1, 1, (&xdr{}).str("/sub").b)
	r.call("MNT a missing path is NOENT", progMount, 1, 1, (&xdr{}).str("/nope").b)
	r.call("MNT a file is NOTDIR", progMount, 1, 1, (&xdr{}).str("/hello.txt").b)
	r.call("UMNT", progMount, 1, 3, (&xdr{}).str("/").b)
	r.call("UMNTALL", progMount, 1, 4, nil)
	r.call("EXPORT", progMount, 1, 5, nil)
	r.call("DUMP is not served", progMount, 1, 2, nil)
	r.call("MNT with a truncated path is GARBAGE_ARGS", progMount, 1, 1, (&xdr{}).u32(64).b)
}

func mount3Scenario(r *recorder) {
	r.call("NULL", progMount, 3, 0, nil)
	r.callWith(authUnix, "MNT the export root", progMount, 3, 1, (&xdr{}).str("/").b)
	r.call("MNT a subdirectory", progMount, 3, 1, (&xdr{}).str("/sub").b)
	r.call("MNT a missing path is NOENT", progMount, 3, 1, (&xdr{}).str("/nope").b)
	r.call("MNT a file is NOTDIR", progMount, 3, 1, (&xdr{}).str("/hello.txt").b)
	r.call("UMNT", progMount, 3, 3, (&xdr{}).str("/").b)
	r.call("UMNTALL", progMount, 3, 4, nil)
	r.call("EXPORT", progMount, 3, 5, nil)
}

// v2 procedure numbers (RFC 1094 section 2.2).
const (
	nfs2Getattr = 1
	nfs2Lookup  = 4
	nfs2Readlnk = 5
	nfs2Read    = 6
	nfs2Write   = 8
	nfs2Create  = 9
	nfs2Readdir = 16
	nfs2Statfs  = 17
)

// staleFH is a well-formed handle for a node the fixture doesn't have;
// badFH carries an unknown format byte.
var (
	staleFH = append([]byte{1, 0, 0, 0, 0, 0, 0, 0xff, 0xff}, make([]byte, 23)...)
	badFH   = append([]byte{0xee}, make([]byte, 31)...)
)

func nfs2Scenario(r *recorder) {
	m := r.call("MNT the export root", progMount, 1, 1, (&xdr{}).str("/").b)
	root := m[4:36]
	call := func(label string, proc uint32, args []byte) []byte {
		return r.call(label, progNFS, 2, proc, args)
	}
	lookup := func(label string, dir []byte, name string) []byte {
		rep := call("LOOKUP "+label, nfs2Lookup, (&xdr{}).fixed(dir).str(name).b)
		if len(rep) < 36 {
			return nil
		}
		return rep[4:36]
	}

	call("NULL", 0, nil)
	call("GETATTR the root", nfs2Getattr, root)
	call("GETATTR a stale handle is STALE", nfs2Getattr, staleFH)
	call("GETATTR a malformed handle is STALE", nfs2Getattr, badFH)
	call("GETATTR with a short handle is GARBAGE_ARGS", nfs2Getattr, root[:16])

	hello := lookup("a file", root, "hello.txt")
	big := lookup("a large file", root, "big.bin")
	link := lookup("a symlink", root, "link")
	sub := lookup("a directory", root, "sub")
	lookup("in a subdirectory", sub, "nested.txt")
	lookup("a missing name is NOENT", root, "nope")
	lookup("under a file is NOTDIR", hello, "x")
	lookup(`".." is NOENT`, sub, "..")
	call("LOOKUP in a stale directory is STALE", nfs2Lookup, (&xdr{}).fixed(staleFH).str("x").b)
	call("GETATTR a symlink", nfs2Getattr, link)

	call("READLINK a symlink", nfs2Readlnk, link)
	call("READLINK a regular file is IO", nfs2Readlnk, hello)

	read := func(label string, fh []byte, off, count uint32) {
		call("READ "+label, nfs2Read, (&xdr{}).fixed(fh).u32(off).u32(count).u32(0).b)
	}
	read("a whole small file", hello, 0, 1024)
	read("the middle of a large file", big, 8192, 8192)
	read("more than 8192 bytes is cut to 8192", big, 0, 10000)
	read("short at end of file", big, 19990, 100)
	read("past end of file returns no data", big, 30000, 100)
	read("a stale handle is STALE", staleFH, 0, 100)

	readdir := func(label string, dir []byte, cookie, count uint32) []byte {
		return call("READDIR "+label, nfs2Readdir, (&xdr{}).fixed(dir).u32(cookie).u32(count).b)
	}
	readdir("the root", root, 0, 4096)
	readdir("an empty directory", lookup("an empty directory", root, "empty"), 0, 4096)
	many := lookup("a large directory", root, "many")
	readdir("a large directory, first page", many, 0, 128)
	readdir("a large directory, from cookie 7", many, 7, 128)
	readdir("a large directory, last page", many, 28, 4096)
	readdir("a file is NOTDIR", hello, 0, 4096)

	call("STATFS", nfs2Statfs, root)
	call("STATFS a stale handle is STALE", nfs2Statfs, staleFH)

	call("WRITE is ROFS", nfs2Write, (&xdr{}).fixed(hello).u32(0).u32(0).u32(0).opaque([]byte("x")).b)
	call("CREATE is ROFS", nfs2Create, (&xdr{}).fixed(root).str("new").b)
	call("ROOT, obsolete, is void", 3, nil)
	for _, p := range []struct {
		name string
		proc uint32
	}{
		{"SETATTR", 2}, {"REMOVE", 10}, {"RENAME", 11}, {"LINK", 12},
		{"SYMLINK", 13}, {"MKDIR", 14}, {"RMDIR", 15},
	} {
		call(p.name+" is ROFS", p.proc, root)
	}
	call("WRITECACHE, obsolete, is void", 7, nil)
}

// v3 procedure numbers (RFC 1813 section 3.3).
const (
	nfs3Getattr     = 1
	nfs3Setattr     = 2
	nfs3Lookup      = 3
	nfs3Access      = 4
	nfs3Readlink    = 5
	nfs3Read        = 6
	nfs3Write       = 7
	nfs3Create      = 8
	nfs3Mkdir       = 9
	nfs3Symlink     = 10
	nfs3Mknod       = 11
	nfs3Remove      = 12
	nfs3Rmdir       = 13
	nfs3Rename      = 14
	nfs3Link        = 15
	nfs3Readdir     = 16
	nfs3Readdirplus = 17
	nfs3Fsstat      = 18
	nfs3Fsinfo      = 19
	nfs3Pathconf    = 20
	nfs3Commit      = 21
)

func nfs3Scenario(r *recorder) {
	m := r.call("MNT the export root", progMount, 3, 1, (&xdr{}).str("/").b)
	root := m[8 : 8+32]
	fh := func(h []byte) []byte { return (&xdr{}).opaque(h).b }
	call := func(label string, proc uint32, args []byte) []byte {
		return r.callWith(authUnix, label, progNFS, 3, proc, args)
	}
	lookup := func(label string, dir []byte, name string) []byte {
		rep := call("LOOKUP "+label, nfs3Lookup, (&xdr{}).opaque(dir).str(name).b)
		if len(rep) < 40 || rep[3] != 0 {
			return nil
		}
		return rep[8:40]
	}

	call("NULL", 0, nil)
	call("GETATTR the root", nfs3Getattr, fh(root))
	call("GETATTR a stale handle is STALE", nfs3Getattr, fh(staleFH))
	call("GETATTR a malformed handle is BADHANDLE", nfs3Getattr, fh(badFH))
	call("GETATTR a short handle is BADHANDLE", nfs3Getattr, fh(root[:16]))
	call("GETATTR a handle over 64 bytes is GARBAGE_ARGS", nfs3Getattr, fh(make([]byte, 68)))

	hello := lookup("a file", root, "hello.txt")
	big := lookup("a large file", root, "big.bin")
	link := lookup("a symlink", root, "link")
	run := lookup("an executable", root, "run.sh")
	sub := lookup("a directory", root, "sub")
	lookup("in a subdirectory", sub, "nested.txt")
	lookup("a missing name is NOENT", root, "nope")
	lookup("under a file is NOTDIR", hello, "x")
	lookup(`".." is NOENT`, sub, "..")
	call("LOOKUP in a stale directory is STALE", nfs3Lookup, (&xdr{}).opaque(staleFH).str("x").b)

	access := func(label string, h []byte) {
		call("ACCESS "+label, nfs3Access, (&xdr{}).opaque(h).u32(0x3f).b)
	}
	access("a directory grants read and lookup", root)
	access("a file grants read", hello)
	access("an executable grants read and execute", run)
	access("a stale handle is STALE", staleFH)

	call("READLINK a symlink", nfs3Readlink, fh(link))
	call("READLINK a regular file is INVAL", nfs3Readlink, fh(hello))

	read := func(label string, h []byte, off uint64, count uint32) {
		call("READ "+label, nfs3Read, (&xdr{}).opaque(h).u64(off).u32(count).b)
	}
	read("a whole small file, at eof", hello, 0, 1024)
	read("the middle of a large file", big, 8192, 4096)
	read("more than 32768 bytes is cut to 32768", lookup("a file over 32768 bytes", root, "xl.bin"), 0, 40000)
	read("a count past end of file stops there", big, 16384, 8192)
	read("short at end of file", big, 19990, 100)
	read("past end of file returns no data", big, 30000, 100)
	read("a directory is ISDIR", root, 0, 100)
	read("a symlink is INVAL", link, 0, 100)
	read("a stale handle is STALE", staleFH, 0, 100)

	verf := make([]byte, 8)
	readdir := func(label string, h []byte, cookie uint64, count uint32) {
		call("READDIR "+label, nfs3Readdir, (&xdr{}).opaque(h).u64(cookie).fixed(verf).u32(count).b)
	}
	readdir("the root", root, 0, 4096)
	readdir("an empty directory", lookup("an empty directory", root, "empty"), 0, 4096)
	many := lookup("a large directory", root, "many")
	readdir("a large directory, first page", many, 0, 256)
	readdir("a large directory, from cookie 7", many, 7, 256)
	readdir("a large directory, last page", many, 28, 4096)
	readdir("too small for one entry is TOOSMALL", many, 0, 120)
	readdir("a file is NOTDIR", hello, 0, 4096)
	readdir("a stale handle is STALE", staleFH, 0, 4096)

	readdirplus := func(label string, h []byte, cookie uint64, dircount, maxcount uint32) {
		call("READDIRPLUS "+label, nfs3Readdirplus, (&xdr{}).opaque(h).u64(cookie).fixed(verf).u32(dircount).u32(maxcount).b)
	}
	readdirplus("the root", root, 0, 4096, 8192)
	readdirplus("a large directory, dircount bounds the page", many, 0, 100, 8192)
	readdirplus("a large directory, maxcount bounds the page", many, 4, 4096, 600)
	readdirplus("too small for one entry is TOOSMALL", many, 0, 4096, 200)
	readdirplus("a file is NOTDIR", hello, 0, 4096, 8192)

	call("FSSTAT", nfs3Fsstat, fh(root))
	call("FSSTAT a stale handle is STALE", nfs3Fsstat, fh(staleFH))
	call("FSINFO", nfs3Fsinfo, fh(root))
	call("FSINFO a stale handle is STALE", nfs3Fsinfo, fh(staleFH))
	call("PATHCONF", nfs3Pathconf, fh(root))
	call("PATHCONF a stale handle is STALE", nfs3Pathconf, fh(staleFH))

	for _, p := range []struct {
		name string
		proc uint32
	}{
		{"SETATTR", nfs3Setattr}, {"WRITE", nfs3Write}, {"CREATE", nfs3Create},
		{"MKDIR", nfs3Mkdir}, {"SYMLINK", nfs3Symlink}, {"MKNOD", nfs3Mknod},
		{"REMOVE", nfs3Remove}, {"RMDIR", nfs3Rmdir}, {"RENAME", nfs3Rename},
		{"LINK", nfs3Link}, {"COMMIT", nfs3Commit},
	} {
		call(p.name+" is ROFS", p.proc, fh(hello))
	}
}

// sattr3 builds an NFSv3 sattr3. A nil field is left unset; a time of
// hostTime asks for the server's clock.
type sattr3 struct {
	mode, uid, gid *uint32
	size           *uint64
	atime, mtime   *time.Time
}

func ptr[T any](v T) *T { return &v }

func (a sattr3) xdr(x *xdr) *xdr {
	opt32 := func(v *uint32) {
		if v == nil {
			x.u32(0)
		} else {
			x.u32(1).u32(*v)
		}
	}
	opt32(a.mode)
	opt32(a.uid)
	opt32(a.gid)
	if a.size == nil {
		x.u32(0)
	} else {
		x.u32(1).u64(*a.size)
	}
	for _, t := range []*time.Time{a.atime, a.mtime} {
		switch {
		case t == nil:
			x.u32(0) // DONT_CHANGE
		case t.Equal(hostTime):
			x.u32(1) // SET_TO_SERVER_TIME
		default:
			x.u32(2).u32(uint32(t.Unix())).u32(uint32(t.Nanosecond()))
		}
	}
	return x
}

// clientTime is a time a client sets, before any the host clock gives.
var clientTime = time.Unix(1000000000, 500)

func shareNFS3Scenario(r *recorder) {
	m := r.callWith(authUnix, "MNT the share", progMount, 3, 1, (&xdr{}).str("/share").b)
	root := m[8 : 8+32]
	call := func(label string, proc uint32, args []byte) []byte {
		return r.callWith(authUnix, label, progNFS, 3, proc, args)
	}
	fh := func(h []byte) []byte { return (&xdr{}).opaque(h).b }
	where := func(dir []byte, name string) *xdr { return (&xdr{}).opaque(dir).str(name) }
	lookup := func(label string, dir []byte, name string) []byte {
		rep := call("LOOKUP "+label, nfs3Lookup, where(dir, name).b)
		if len(rep) < 40 || rep[3] != 0 {
			return nil
		}
		return rep[8:40]
	}
	// made takes the handle from a CREATE, MKDIR or SYMLINK reply.
	made := func(rep []byte) []byte {
		if len(rep) < 44 || rep[3] != 0 {
			return nil
		}
		return rep[12:44]
	}
	create := func(label string, dir []byte, name string, how uint32, tail *xdr) []byte {
		args := where(dir, name).u32(how).fixed(tail.b)
		return made(call("CREATE "+label, nfs3Create, args.b))
	}
	unchecked, guarded, exclusive := uint32(0), uint32(1), uint32(2)

	call("GETATTR the share root", nfs3Getattr, fh(root))
	call("FSINFO of a writable share", nfs3Fsinfo, fh(root))
	call("ACCESS a writable directory", nfs3Access, (&xdr{}).opaque(root).u32(0x3f).b)
	lookup(`"." is the directory itself`, root, ".")
	lookup(`".." of the share root is the root`, root, "..")
	hello := lookup("a fixture file", root, "hello.txt")
	call("ACCESS a writable file", nfs3Access, (&xdr{}).opaque(hello).u32(0x3f).b)

	nf := create("UNCHECKED makes a file", root, "new.txt", unchecked, sattr3{mode: ptr(uint32(0o644))}.xdr(&xdr{}))
	write := func(label string, h []byte, off uint64, data string) {
		call("WRITE "+label, nfs3Write, (&xdr{}).opaque(h).u64(off).u32(uint32(len(data))).u32(0).opaque([]byte(data)).b)
	}
	write("UNSTABLE is answered FILE_SYNC", nf, 0, "hello, share\n")
	write("past the end leaves a hole", nf, 100, "tail")
	call("READ it back", nfs3Read, (&xdr{}).opaque(nf).u64(0).u32(200).b)
	call("COMMIT returns the verifier", nfs3Commit, (&xdr{}).opaque(nf).u64(0).u32(0).b)

	setattr := func(label string, h []byte, a sattr3, guard *time.Time) {
		x := a.xdr((&xdr{}).opaque(h))
		if guard == nil {
			x.u32(0)
		} else {
			x.u32(1).u32(uint32(guard.Unix())).u32(uint32(guard.Nanosecond()))
		}
		call("SETATTR "+label, nfs3Setattr, x.b)
	}
	setattr("truncates", nf, sattr3{size: ptr(uint64(5))}, nil)
	setattr("drops setuid from the mode", nf, sattr3{mode: ptr(uint32(0o4755))}, nil)
	setattr("chown succeeds and changes nothing", nf, sattr3{uid: ptr(uint32(1000)), gid: ptr(uint32(1000))}, nil)
	setattr("sets client times", nf, sattr3{atime: &clientTime, mtime: &clientTime}, nil)
	setattr("sets server times", nf, sattr3{atime: &hostTime, mtime: &hostTime}, nil)
	setattr("with a matching guard", nf, sattr3{mode: ptr(uint32(0o600))}, &hostTime)
	setattr("with a stale guard is NOT_SYNC", nf, sattr3{mode: ptr(uint32(0o644))}, &clientTime)
	setattr("of a directory's size is ISDIR", root, sattr3{size: ptr(uint64(0))}, nil)

	create("GUARDED of an existing name is EXIST", root, "new.txt", guarded, sattr3{}.xdr(&xdr{}))
	g := create("GUARDED makes a file", root, "g.txt", guarded, sattr3{mode: ptr(uint32(0o600))}.xdr(&xdr{}))
	create("UNCHECKED of an existing file applies its size", root, "hello.txt", unchecked, sattr3{size: ptr(uint64(0))}.xdr(&xdr{}))
	verf := (&xdr{}).u32(0x12345678).u32(0x00abcdef)
	x := create("EXCLUSIVE makes a file", root, "x.txt", exclusive, verf)
	create("EXCLUSIVE retransmitted finds the same file", root, "x.txt", exclusive, verf)
	create("EXCLUSIVE with another verifier is EXIST", root, "x.txt", exclusive, (&xdr{}).u32(1).u32(2))
	setattr("after EXCLUSIVE sets the real attributes", x, sattr3{mode: ptr(uint32(0o644)), mtime: &clientTime}, nil)
	create("in a file is NOTDIR", hello, "y", guarded, sattr3{}.xdr(&xdr{}))
	create(`named ".." is INVAL`, root, "..", guarded, sattr3{}.xdr(&xdr{}))
	create("named with a slash is INVAL", root, "a/b", guarded, sattr3{}.xdr(&xdr{}))
	create("with a 256-byte name is NAMETOOLONG", root, strings.Repeat("n", 256), guarded, sattr3{}.xdr(&xdr{}))
	// The root's handle with a table id the share never issued.
	stale := append([]byte(nil), root...)
	stale[16] = 0x7f
	call("CREATE in a stale directory is STALE", nfs3Create, where(stale, "z").u32(guarded).fixed(sattr3{}.xdr(&xdr{}).b).b)
	call("CREATE in a handle of another format is BADHANDLE", nfs3Create, where(staleFH, "z").u32(guarded).fixed(sattr3{}.xdr(&xdr{}).b).b)

	mkdir := func(label string, dir []byte, name string) []byte {
		return made(call("MKDIR "+label, nfs3Mkdir, sattr3{mode: ptr(uint32(0o755))}.xdr(where(dir, name)).b))
	}
	d := mkdir("makes a directory", root, "d")
	mkdir("of an existing name is EXIST", root, "d")
	sym := func(label string, dir []byte, name, target string) []byte {
		return made(call("SYMLINK "+label, nfs3Symlink, sattr3{}.xdr(where(dir, name)).str(target).b))
	}
	l := sym("stores a target out of the share as text", root, "l", "../../etc/passwd")
	call("READLINK the new link", nfs3Readlink, fh(l))
	lookup("through a link is NOTDIR", l, "passwd")
	write("to a symlink is INVAL", l, 0, "x")
	write("to a directory is ISDIR", d, 0, "x")
	call("LINK is NOTSUPP", nfs3Link, (&xdr{}).opaque(g).fixed(where(root, "hard").b).b)
	call("MKNOD is NOTSUPP", nfs3Mknod, where(root, "fifo").u32(7).b)

	rename := func(label string, from []byte, fromName string, to []byte, toName string) {
		call("RENAME "+label, nfs3Rename, where(from, fromName).fixed(where(to, toName).b).b)
	}
	rename("moves a file into a directory", root, "new.txt", d, "moved.txt")
	call("GETATTR by the old handle still finds it", nfs3Getattr, fh(nf))
	rename("over an existing file replaces it", root, "g.txt", root, "x.txt")
	call("GETATTR the replaced file's handle is STALE", nfs3Getattr, fh(x))
	rename("of a missing name is NOENT", root, "nope", root, "nope2")
	rename("of a directory into itself is INVAL", root, "d", d, "sub")
	rename(`to ".." is INVAL`, root, "x.txt", root, "..")

	remove := func(label string, proc uint32, dir []byte, name string) {
		call(label, proc, where(dir, name).b)
	}
	remove("RMDIR of a non-empty directory is NOTEMPTY", nfs3Rmdir, root, "d")
	remove("REMOVE of a directory is ISDIR", nfs3Remove, root, "d")
	remove("RMDIR of a file is NOTDIR", nfs3Rmdir, root, "x.txt")
	remove("REMOVE a file", nfs3Remove, d, "moved.txt")
	remove("REMOVE of a missing name is NOENT", nfs3Remove, d, "moved.txt")
	remove("RMDIR an empty directory", nfs3Rmdir, root, "d")
	call("GETATTR a removed file's handle is STALE", nfs3Getattr, fh(nf))
	remove("REMOVE a symlink", nfs3Remove, root, "l")

	verf0 := make([]byte, 8)
	call("READDIR the share root, in cookie order", nfs3Readdir, (&xdr{}).opaque(root).u64(0).fixed(verf0).u32(4096).b)
	call("READDIRPLUS the share root", nfs3Readdirplus, (&xdr{}).opaque(root).u64(0).fixed(verf0).u32(4096).u32(16384).b)
	many := lookup("a large directory", root, "many")
	page := call("READDIR a large directory, first page", nfs3Readdir, (&xdr{}).opaque(many).u64(0).fixed(verf0).u32(300).b)
	// Remove the entries the first page listed, as rm -r does, and
	// carry on from the last cookie: nothing is skipped.
	last, names := lastCookie3(page)
	for _, name := range names {
		remove("REMOVE "+name+" from the first page", nfs3Remove, many, name)
	}
	call("READDIR a large directory, after removing the first page", nfs3Readdir, (&xdr{}).opaque(many).u64(last).fixed(verf0).u32(4096).b)
}

// lastCookie3 walks a READDIR3 reply body for its names and last cookie.
func lastCookie3(rep []byte) (uint64, []string) {
	if len(rep) < 4 || rep[3] != 0 {
		return 0, nil
	}
	off := 4 + 4 + 84 + 8 // status, dir_attributes, cookieverf
	var last uint64
	var names []string
	for u32be(rep[off:]) == 1 {
		off += 4 + 8 // value follows, fileid
		n := int(u32be(rep[off:]))
		names = append(names, string(rep[off+4:off+4+n]))
		off += 4 + (n+3)&^3
		last = uint64(u32be(rep[off:]))<<32 | uint64(u32be(rep[off+4:]))
		off += 8
	}
	return last, names
}

func u32be(b []byte) uint32 {
	return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
}

// sattr2 builds an NFSv2 sattr, all ones for each field left unset.
func sattr2(mode, size uint32, atime, mtime [2]uint32) *xdr {
	return (&xdr{}).u32(mode).u32(^uint32(0)).u32(^uint32(0)).u32(size).
		u32(atime[0]).u32(atime[1]).u32(mtime[0]).u32(mtime[1])
}

var (
	unset2      = [2]uint32{^uint32(0), ^uint32(0)}
	serverTime2 = [2]uint32{0, 1000000}
	clientTime2 = [2]uint32{1000000000, 7}
)

func shareNFS2Scenario(r *recorder) {
	m := r.callWith(authUnix, "MNT the share", progMount, 1, 1, (&xdr{}).str("/share").b)
	root := m[4:36]
	call := func(label string, proc uint32, args []byte) []byte {
		return r.callWith(authUnix, label, progNFS, 2, proc, args)
	}
	where := func(dir []byte, name string) *xdr { return (&xdr{}).fixed(dir).str(name) }
	// handle takes the handle from a CREATE, MKDIR or LOOKUP reply.
	handle := func(rep []byte) []byte {
		if len(rep) < 36 || rep[3] != 0 {
			return nil
		}
		return rep[4:36]
	}
	none := ^uint32(0)

	f := handle(call("CREATE makes a file", 9, where(root, "new.txt").fixed(sattr2(0o644, none, unset2, unset2).b).b))
	call("WRITE", 8, (&xdr{}).fixed(f).u32(0).u32(0).u32(0).opaque([]byte("hello, v2\n")).b)
	call("READ it back", 6, (&xdr{}).fixed(f).u32(0).u32(100).u32(0).b)
	call("SETATTR truncates", 2, (&xdr{}).fixed(f).fixed(sattr2(none, 5, unset2, unset2).b).b)
	call("SETATTR drops setuid from the mode", 2, (&xdr{}).fixed(f).fixed(sattr2(0o4755, none, unset2, unset2).b).b)
	call("SETATTR sets client times", 2, (&xdr{}).fixed(f).fixed(sattr2(none, none, clientTime2, clientTime2).b).b)
	call("SETATTR with a microsecond of one million takes server time", 2, (&xdr{}).fixed(f).fixed(sattr2(none, none, serverTime2, serverTime2).b).b)
	call("CREATE of an existing file truncates it", 9, where(root, "hello.txt").fixed(sattr2(none, 0, unset2, unset2).b).b)
	call(`CREATE named ".." is INVAL`, 9, where(root, "..").fixed(sattr2(0o644, none, unset2, unset2).b).b)

	d := handle(call("MKDIR makes a directory", 14, where(root, "d").fixed(sattr2(0o755, none, unset2, unset2).b).b))
	call("MKDIR of an existing name is EXIST", 14, where(root, "d").fixed(sattr2(0o755, none, unset2, unset2).b).b)
	call("SYMLINK stores the target", 13, where(root, "l").str("/etc/motd").fixed(sattr2(0o777, none, unset2, unset2).b).b)
	l := handle(call("LOOKUP the new link", 4, where(root, "l").b))
	call("READLINK it", 5, l)
	call("LINK is PERM", 12, (&xdr{}).fixed(f).fixed(where(root, "hard").b).b)
	call("WRITE to a directory is ISDIR", 8, (&xdr{}).fixed(d).u32(0).u32(0).u32(0).opaque([]byte("x")).b)

	call("RENAME moves a file into a directory", 11, where(root, "new.txt").fixed(where(d, "moved.txt").b).b)
	call("GETATTR by the old handle still finds it", 1, f)
	call("RMDIR of a non-empty directory is NOTEMPTY", 15, where(root, "d").b)
	call("REMOVE of a directory is ISDIR", 10, where(root, "d").b)
	call("REMOVE a file", 10, where(d, "moved.txt").b)
	call("REMOVE of a missing name is NOENT", 10, where(d, "moved.txt").b)
	call("RMDIR an empty directory", 15, where(root, "d").b)
	call("GETATTR a removed file's handle is STALE", 1, f)
	call("READDIR the share root, in cookie order", 16, (&xdr{}).fixed(root).u32(0).u32(4096).b)
}
