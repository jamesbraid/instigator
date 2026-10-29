package nfs_test

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
