package nfs_test

// Scenarios that generate testdata/transcripts. Each runs calls in order
// against a fresh server and fixture; a call that needs a filehandle
// takes it from an earlier reply, as a client would.

const (
	progPortmap = 100000
	progNFS     = 100003
	progMount   = 100005

	protoUDP = 17
)

type scenario struct {
	fixture string
	run     func(*recorder)
}

var scenarios = map[string]scenario{
	"portmap": {"tree", portmapScenario},
	"mount1":  {"tree", mount1Scenario},
	"nfs2":    {"tree", nfs2Scenario},
}

func portmapScenario(r *recorder) {
	getport := func(label string, prog, vers uint32) {
		r.call("GETPORT "+label, progPortmap, 2, 3, (&xdr{}).u32(prog).u32(vers).u32(protoUDP).u32(0).b)
	}
	getport("NFS v2 over UDP", progNFS, 2)
	getport("MOUNT v1 over UDP", progMount, 1)
	getport("an unregistered program answers port 0", 100021, 1)
	r.call("CALLIT is not served", progPortmap, 2, 5, nil)
}

func mount1Scenario(r *recorder) {
	r.call("NULL", progMount, 1, 0, nil)
	r.call("MNT the export root", progMount, 1, 1, (&xdr{}).str("/").b)
	r.callWith(authUnix, "MNT with AUTH_UNIX credentials", progMount, 1, 1, (&xdr{}).str("/").b)
	r.call("MNT a subdirectory", progMount, 1, 1, (&xdr{}).str("/sub").b)
	r.call("MNT a missing path is NOENT", progMount, 1, 1, (&xdr{}).str("/nope").b)
	r.call("UMNT", progMount, 1, 3, (&xdr{}).str("/").b)
	r.call("MNT with a truncated path is GARBAGE_ARGS", progMount, 1, 1, (&xdr{}).u32(64).b)
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
	call("CREATE is not served", nfs2Create, (&xdr{}).fixed(root).str("new").b)
}
