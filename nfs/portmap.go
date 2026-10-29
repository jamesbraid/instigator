package nfs

import "net"

// Portmap transport numbers (RFC 1833 section 3).
const (
	ipprotoTCP = 6
	ipprotoUDP = 17
)

func (s *Server) portmapProcs() map[uint32]handlerFunc {
	return map[uint32]handlerFunc{
		0: null,
		3: s.pmapGetport, // PMAPPROC_GETPORT
		4: s.pmapDump,    // PMAPPROC_DUMP
	}
}

type mapping struct{ prog, vers, prot, port uint32 }

// mappings lists every registration, in the order DUMP reports them.
func (s *Server) mappings() []mapping {
	p := s.ports.Load()
	if p == nil {
		return nil
	}
	var out []mapping
	for i, prot := range []uint32{ipprotoUDP, ipprotoTCP} {
		ports := p[i]
		for _, m := range []mapping{
			{progPortmap, 2, prot, ports.Portmap},
			{progMount, 1, prot, ports.Mount},
			{progMount, 3, prot, ports.Mount},
			{progNFS, 2, prot, ports.NFS},
			{progNFS, 3, prot, ports.NFS},
		} {
			if m.port != 0 {
				out = append(out, m)
			}
		}
	}
	return out
}

// pmapGetport answers the port for a program, version and transport, or
// 0 when nothing is registered for it.
func (s *Server) pmapGetport(_ net.Addr, r *xdrReader) []byte {
	prog := r.uint32()
	vers := r.uint32()
	prot := r.uint32()
	r.uint32() // port, ignored
	w := &xdrWriter{}
	port := uint32(0)
	for _, m := range s.mappings() {
		if m.prog == prog && m.vers == vers && m.prot == prot {
			port = m.port
			break
		}
	}
	w.uint32(port)
	return w.b
}

func (s *Server) pmapDump(net.Addr, *xdrReader) []byte {
	w := &xdrWriter{}
	for _, m := range s.mappings() {
		w.bool(true)
		w.uint32(m.prog)
		w.uint32(m.vers)
		w.uint32(m.prot)
		w.uint32(m.port)
	}
	w.bool(false)
	return w.b
}
