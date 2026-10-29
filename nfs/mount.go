package nfs

import "net"

func (s *Server) mountProcs(vers uint32) map[uint32]handlerFunc {
	return map[uint32]handlerFunc{
		0: null,
		1: func(caller net.Addr, r *xdrReader) []byte { return s.mountMnt(caller, r, vers) },
		3: s.mountUmnt,
		4: null, // UMNTALL: the server keeps no mount list to clear
		5: s.mountExport,
	}
}

// mountMnt answers a mount path with its root handle. Version 1 replies
// with NFSv2 status values (RFC 1094 appendix A), version 3 with
// mountstat3 (RFC 1813 section 5.1.5); the two agree on every status
// sent here.
func (s *Server) mountMnt(caller net.Addr, r *xdrReader, vers uint32) []byte {
	path := r.str()
	w := &xdrWriter{}
	if r.err {
		return nil
	}
	root, err := s.FS.MountRoot(path)
	if err == nil && ftype(root.Mode()) != nfDIR {
		err = ErrNotDir
	}
	if err != nil {
		s.Logger.Warnf("nfs: mount %q from %s: %v", path, caller, err)
		w.uint32(statusFor(err))
		return w.b
	}
	s.Logger.Infof("nfs: mount %q v%d from %s", path, vers, caller)
	w.uint32(nfsOK)
	if vers == 1 {
		w.fixed(s.handle(root))
		return w.b
	}
	w.opaque(s.handle(root))
	w.uint32(1) // one auth flavor:
	w.uint32(1) // AUTH_UNIX
	return w.b
}

// mountUmnt acknowledges an unmount; the server keeps no mount list.
func (s *Server) mountUmnt(_ net.Addr, r *xdrReader) []byte {
	r.str()
	return nil
}

// mountExport lists the exports, each open to every client: an empty
// group list.
func (s *Server) mountExport(net.Addr, *xdrReader) []byte {
	exports := []string{"/"}
	if e, ok := s.FS.(Exporter); ok {
		exports = e.Exports()
	}
	w := &xdrWriter{}
	for _, p := range exports {
		w.bool(true)
		w.opaque([]byte(p))
		w.bool(false) // no groups
	}
	w.bool(false)
	return w.b
}
