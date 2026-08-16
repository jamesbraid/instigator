// Package serve wires the configured services over one vfs tree: bootp
// answers the configured MACs, tftp and rsh serve the tree, every
// protocol filters to the configured client IPs. It also prints the
// startup map - disc slugs and the exact PROM commands to type.
package serve

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sort"
	"strings"

	"github.com/jamesbraid/instigator/internal/bootp"
	"github.com/jamesbraid/instigator/internal/config"
	"github.com/jamesbraid/instigator/internal/instcmd"
	"github.com/jamesbraid/instigator/internal/instscript"
	"github.com/jamesbraid/instigator/internal/tftp"
	"github.com/jamesbraid/instigator/internal/vfs"
	"github.com/jamesbraid/instigator/nfs"
	"github.com/jamesbraid/instigator/rcmd"
)

// Servers is a running instigator instance.
type Servers struct {
	tree  *vfs.Tree
	bootp *bootp.Server

	bootpConn net.PacketConn
	tftpConn  net.PacketConn
	rshLn     net.Listener

	pmapConn  net.PacketConn
	mountConn net.PacketConn
	nfsConn   net.PacketConn
}

// treeFS adapts the vfs tree to the protocol servers' filesystem
// interfaces, translating the not-found error each expects.
type treeFS struct{ t *vfs.Tree }

func (f treeFS) Open(path string) (tftp.File, error) {
	file, err := f.t.Open(path)
	if errors.Is(err, vfs.ErrNotFound) {
		return nil, fmt.Errorf("%s: %w", path, tftp.ErrNotFound)
	}
	return file, err
}

type cmdFS struct{ t *vfs.Tree }

func (f cmdFS) Open(path string) (instcmd.File, error) {
	file, err := f.t.Open(path)
	if errors.Is(err, vfs.ErrNotFound) {
		return nil, fmt.Errorf("%s: %w", path, instcmd.ErrNotFound)
	}
	return file, err
}

func (f cmdFS) ReadDir(path string) ([]string, error) {
	names, err := f.t.ReadDir(path)
	if errors.Is(err, vfs.ErrNotFound) {
		return nil, fmt.Errorf("%s: %w", path, instcmd.ErrNotFound)
	}
	return names, err
}

func (f cmdFS) Stat(path string) (instcmd.FileInfo, error) {
	info, err := f.t.Stat(path)
	if errors.Is(err, vfs.ErrNotFound) {
		return instcmd.FileInfo{}, fmt.Errorf("%s: %w", path, instcmd.ErrNotFound)
	}
	if err != nil {
		return instcmd.FileInfo{}, err
	}
	return instcmd.FileInfo{
		Ino:   info.Ino,
		IsDir: info.IsDir,
		Perm:  info.Perm,
		Nlink: info.Nlink,
		UID:   info.UID,
		GID:   info.GID,
		Size:  info.Size,
		Mtime: info.Mtime,
	}, nil
}

// Option adjusts Start for tests.
type Option func(*options)

type options struct {
	bootpReplyAddr net.Addr
	rshHighPorts   bool
	verbose        bool
}

// WithBootpReplyAddr redirects bootp replies away from the broadcast
// address, for tests that cannot receive broadcasts.
func WithBootpReplyAddr(a net.Addr) Option {
	return func(o *options) { o.bootpReplyAddr = a }
}

// WithVerbose logs every datagram received and sent, decoded.
func WithVerbose() Option {
	return func(o *options) { o.verbose = true }
}

// WithRSHHighPorts disables the rsh reserved-source-port check, for
// tests that cannot bind reserved ports.
func WithRSHHighPorts() Option {
	return func(o *options) { o.rshHighPorts = true }
}

// Start opens the media, binds the enabled services on the configured
// ports, and serves until Close.
func Start(cfg *config.Config, logf func(format string, args ...any), opts ...Option) (*Servers, error) {
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	var sets []vfs.MediaSet
	for _, m := range cfg.Media {
		sets = append(sets, vfs.MediaSet{Name: m.Name, Dir: m.Dir, DiscNames: m.DiscNames})
	}
	tree, err := vfs.BuildTree(sets)
	if err != nil {
		return nil, err
	}
	var firstPrimaryDist string
	for i, cb := range cfg.Combined {
		primaryDist, err := tree.AddCombined(cb.Name, cb.Layers, cb.DiscNames)
		if err != nil {
			tree.Close()
			return nil, err
		}
		if i == 0 {
			firstPrimaryDist = primaryDist
		}
		if logf != nil {
			logf("combined: /%s  <-  %d discs, open %s:%s", cb.Name, len(cb.Layers), cfg.ServerIP, primaryDist)
		}
	}

	// Serve a generated install runbook at /install, filled in with this
	// server's address and the first combined set's primary dist path -
	// the single distribution inst opens, from which its synthesized
	// .related_dists auto-opens the rest.
	if len(cfg.Combined) > 0 {
		script := instscript.Generate(instscript.Params{
			ServerIP: cfg.ServerIP.String(),
			DistPath: firstPrimaryDist,
			Release:  cfg.Combined[0].Name,
			Stream:   "feature",
		})
		tree.AddSynthetic("install", []byte(script))
		if logf != nil {
			logf("runbook: /install  (from %s:%s)", cfg.ServerIP, firstPrimaryDist)
		}
	}
	s := &Servers{tree: tree}

	allowed := make(map[netip.Addr]bool, len(cfg.Clients))
	for _, c := range cfg.Clients {
		allowed[c.IP] = true
	}
	allow := func(a netip.Addr) bool { return allowed[a] }

	logStartup(cfg, tree, logf)

	if cfg.Services.BOOTP {
		var clients []bootp.Client
		for _, c := range cfg.Clients {
			clients = append(clients, bootp.Client{Name: c.Name, MAC: c.MAC, IP: c.IP})
		}
		s.bootp = &bootp.Server{
			ServerIP:  cfg.ServerIP,
			Netmask:   cfg.Netmask,
			Clients:   clients,
			ReplyAddr: o.bootpReplyAddr,
			Logf:      logf,
			Verbose:   o.verbose,
		}
		pc, err := bootp.ListenBroadcast(fmt.Sprintf(":%d", cfg.Ports.BOOTP))
		if err != nil {
			s.Close()
			return nil, fmt.Errorf("bootp: %w", err)
		}
		s.bootpConn = pc
		go s.bootp.Serve(pc)
	}

	if cfg.Services.TFTP.Enabled {
		srv := &tftp.Server{
			FS:      treeFS{tree},
			AllowIP: allow,
			PortMin: cfg.Services.TFTP.PortRange[0],
			PortMax: cfg.Services.TFTP.PortRange[1],
			Logf:    logf,
			Verbose: o.verbose,
		}
		pc, err := net.ListenPacket("udp4", fmt.Sprintf(":%d", cfg.Ports.TFTP))
		if err != nil {
			s.Close()
			return nil, fmt.Errorf("tftp: %w", err)
		}
		s.tftpConn = pc
		go srv.Serve(pc)
	}

	if cfg.Services.RSH {
		srv := &rcmd.Server{
			AllowIP:        allow,
			AllowHighPorts: o.rshHighPorts,
			Logf:           logf,
			Verbose:        o.verbose,
			Handler: func(req *rcmd.Request) error {
				// inst drives the install by opening a shell over rsh
				// (exec /bin/sh) and streaming commands to it, rather than
				// issuing each as its own rsh command.
				if isShell(req.Command) {
					var log func(string)
					if logf != nil {
						log = func(line string) { logf("rsh-sh: %q", line) }
					}
					return instcmd.RunShell(cmdFS{tree}, req.Stdin, req.Stdout, req.Stderr, log)
				}
				return instcmd.Run(cmdFS{tree}, req.Command, req.Stdout, req.Stderr)
			},
		}
		ln, err := net.Listen("tcp4", fmt.Sprintf(":%d", cfg.Ports.RSH))
		if err != nil {
			s.Close()
			return nil, fmt.Errorf("rsh: %w", err)
		}
		s.rshLn = ln
		go srv.Serve(ln)
	}

	if cfg.Services.NFS {
		if err := s.startNFS(cfg, allow, logf, o.verbose); err != nil {
			s.Close()
			return nil, err
		}
	}

	return s, nil
}

// isShell reports whether a command is inst asking for an interactive
// shell rather than a single command.
func isShell(command string) bool {
	f := strings.Fields(command)
	if len(f) == 0 {
		return false
	}
	last := f[len(f)-1] // "exec /bin/sh" -> "/bin/sh"
	switch last {
	case "sh", "/bin/sh", "/sbin/sh", "/usr/bin/sh":
		return true
	}
	return false
}

// startNFS binds portmap, mountd, and nfsd and registers the assigned
// ports with portmap.
func (s *Servers) startNFS(cfg *config.Config, allow func(netip.Addr) bool, logf func(string, ...any), verbose bool) error {
	srv := &nfs.Server{FS: s.tree.NFSExport(), AllowIP: allow, Logf: logf, Verbose: verbose}

	pmap, err := net.ListenPacket("udp4", fmt.Sprintf(":%d", cfg.Ports.Portmap))
	if err != nil {
		return fmt.Errorf("portmap: %w", err)
	}
	s.pmapConn = pmap
	mnt, err := net.ListenPacket("udp4", fmt.Sprintf(":%d", cfg.Ports.Mount))
	if err != nil {
		return fmt.Errorf("mountd: %w", err)
	}
	s.mountConn = mnt
	nfsc, err := net.ListenPacket("udp4", fmt.Sprintf(":%d", cfg.Ports.NFS))
	if err != nil {
		return fmt.Errorf("nfsd: %w", err)
	}
	s.nfsConn = nfsc

	srv.SetPorts(
		uint32(mnt.LocalAddr().(*net.UDPAddr).Port),
		uint32(nfsc.LocalAddr().(*net.UDPAddr).Port),
	)
	go srv.ServePortmap(pmap)
	go srv.ServeMount(mnt)
	go srv.ServeNFS(nfsc)
	return nil
}

func logStartup(cfg *config.Config, tree *vfs.Tree, logf func(string, ...any)) {
	if logf == nil {
		return
	}
	dm := tree.DiscMap()
	medias := make([]string, 0, len(dm))
	for m := range dm {
		medias = append(medias, m)
	}
	sort.Strings(medias)
	for _, m := range medias {
		slugs := make([]string, 0, len(dm[m]))
		for s := range dm[m] {
			slugs = append(slugs, s)
		}
		sort.Strings(slugs)
		for _, s := range slugs {
			logf("media: /%s/%s  <-  %q", m, s, dm[m][s])
		}
	}
	for _, c := range cfg.Clients {
		logf("client %s: mac=%s ip=%s", c.Name, c.MAC, c.IP)
		logf("  PROM: setenv netaddr %s", c.IP)
		for _, m := range medias {
			for s := range dm[m] {
				if _, err := tree.Open(fmt.Sprintf("%s/%s/stand/fx.64", m, s)); err == nil {
					logf("  PROM: boot -f bootp():/%s/%s/stand/fx.64", m, s)
				}
			}
		}
	}
}

// BOOTPAddr returns the bootp socket address.
func (s *Servers) BOOTPAddr() net.Addr { return s.bootpConn.LocalAddr() }

// TFTPAddr returns the tftp socket address.
func (s *Servers) TFTPAddr() net.Addr { return s.tftpConn.LocalAddr() }

// RSHAddr returns the rsh listener address.
func (s *Servers) RSHAddr() net.Addr { return s.rshLn.Addr() }

// Close shuts every listener and releases the media.
func (s *Servers) Close() error {
	if s.bootpConn != nil {
		s.bootpConn.Close()
	}
	if s.tftpConn != nil {
		s.tftpConn.Close()
	}
	if s.rshLn != nil {
		s.rshLn.Close()
	}
	for _, c := range []net.PacketConn{s.pmapConn, s.mountConn, s.nfsConn} {
		if c != nil {
			c.Close()
		}
	}
	return s.tree.Close()
}

// PortmapAddr returns the portmap socket address.
func (s *Servers) PortmapAddr() net.Addr { return s.pmapConn.LocalAddr() }
