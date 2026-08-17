// Package serve wires the configured services over one vfs tree: bootp
// answers the configured MACs, tftp and rsh serve the tree, every
// protocol filters to the configured client IPs. It also prints the
// startup map - disc slugs and the exact PROM commands to type.
package serve

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"sort"
	"strings"

	"github.com/jamesbraid/instigator/internal/bootp"
	"github.com/jamesbraid/instigator/internal/config"
	"github.com/jamesbraid/instigator/internal/instcmd"
	"github.com/jamesbraid/instigator/internal/instscript"
	"github.com/jamesbraid/instigator/internal/logging"
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

// ResolveImage implements instcmd.ImageResolver, so a leaf command
// (dd, cat) can log which image and in-image path a served path
// actually came from, not just the tree path it was given.
func (f cmdFS) ResolveImage(path string) (instcmd.Resolved, error) {
	r, err := f.t.ResolveImage(path)
	if errors.Is(err, vfs.ErrNotFound) {
		return instcmd.Resolved{}, fmt.Errorf("%s: %w", path, instcmd.ErrNotFound)
	}
	if err != nil {
		return instcmd.Resolved{}, err
	}
	return instcmd.Resolved{Image: r.Image, Path: r.Path}, nil
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
	instructions   io.Writer
}

// WithBootpReplyAddr redirects bootp replies away from the broadcast
// address, for tests that cannot receive broadcasts.
func WithBootpReplyAddr(a net.Addr) Option {
	return func(o *options) { o.bootpReplyAddr = a }
}

// WithRSHHighPorts disables the rsh reserved-source-port check, for
// tests that cannot bind reserved ports.
func WithRSHHighPorts() Option {
	return func(o *options) { o.rshHighPorts = true }
}

// WithInstructions directs the operator's PROM/inst commands - "type
// this to boot" - to w instead of discarding them. These are console
// UX for a human at the PROM, never part of the leveled server log: no
// timestamp, no level, and never written unless a caller asks for them.
func WithInstructions(w io.Writer) Option {
	return func(o *options) { o.instructions = w }
}

// Start opens the media, binds the enabled services on the configured
// ports, and serves until Close. logger receives leveled server output;
// nil is silent, exactly like an unset Logf used to be. -v maps to a
// DEBUG-level logger at the call site (cmd/instigator), not an option
// here: verbosity is a property of the logger a caller builds, not a
// separate flag threaded through every service.
func Start(cfg *config.Config, logger *logging.Logger, opts ...Option) (*Servers, error) {
	o := options{instructions: io.Discard}
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
	var primaryDists []string
	for _, cb := range cfg.Combined {
		primaryDist, err := tree.AddCombined(cb.Name, cb.Layers, cb.DiscNames)
		if err != nil {
			tree.Close()
			return nil, err
		}
		primaryDists = append(primaryDists, primaryDist)
		logger.Infof("combined: /%s  <-  %d discs, open %s:%s", cb.Name, len(cb.Layers), cfg.ServerIP, primaryDist)
	}

	// Serve a generated install runbook at /install, filled in with this
	// server's address and the first combined set's primary dist path -
	// the single distribution inst opens, from which its synthesized
	// .related_dists auto-opens the rest.
	if len(primaryDists) > 0 {
		script := instscript.Generate(instscript.Params{
			ServerIP: cfg.ServerIP.String(),
			DistPath: primaryDists[0],
			Release:  cfg.Combined[0].Name,
			Stream:   "feature",
		})
		tree.AddSynthetic("install", []byte(script))
		logger.Infof("runbook: /install  (from %s:%s)", cfg.ServerIP, primaryDists[0])
	}
	s := &Servers{tree: tree}

	allowed := make(map[netip.Addr]bool, len(cfg.Clients))
	for _, c := range cfg.Clients {
		allowed[c.IP] = true
	}
	allow := func(a netip.Addr) bool { return allowed[a] }

	logStartup(cfg, tree, primaryDists, logger, o.instructions)

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
			Logger:    logger,
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
			Logger:  logger,
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
			Logger:         logger,
			Handler: func(req *rcmd.Request) error {
				// inst drives the install by opening a shell over rsh
				// (exec /bin/sh) and streaming commands to it, rather than
				// issuing each as its own rsh command.
				if isShell(req.Command) {
					return instcmd.RunShell(cmdFS{tree}, req.Stdin, req.Stdout, req.Stderr, logger)
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
		if err := s.startNFS(cfg, allow, logger); err != nil {
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
func (s *Servers) startNFS(cfg *config.Config, allow func(netip.Addr) bool, logger *logging.Logger) error {
	srv := &nfs.Server{FS: s.tree.NFSExport(), AllowIP: allow, Logger: logger}

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

// combinedFxPath returns the tree path of the miniroot partitioner that
// sits beside a combined set's primary dist. AddCombined returns
// "/<name>/<primary>/dist"; stand/ is that disc's other top-level
// directory, so the "dist" leaf comes off and "stand/fx.64" goes on.
func combinedFxPath(primaryDist string) string {
	base := strings.TrimSuffix(strings.Trim(primaryDist, "/"), "/dist")
	return base + "/stand/fx.64"
}

// logStartup logs the serve map - media and combined sets available -
// at INFO, and separately writes the operator's commands (what to type
// at the PROM and at the Inst> prompt) to instructions. The two are
// different audiences: the serve map is what this server is doing, the
// PROM/Inst> lines are what a human does next, and the latter were
// never server log content - they went to the logger only because
// nothing else was wired up to carry them. primaryDists is one primary
// dist path per combined set, in config order: a combined set is
// opened with a single "from", so its whole recipe fits on two lines
// and belongs next to the client it is for.
func logStartup(cfg *config.Config, tree *vfs.Tree, primaryDists []string, logger *logging.Logger, instructions io.Writer) {
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
			logger.Infof("media: /%s/%s  <-  %q", m, s, dm[m][s])
		}
	}
	for _, c := range cfg.Clients {
		logger.Infof("client %s: mac=%s ip=%s", c.Name, c.MAC, c.IP)
		fmt.Fprintf(instructions, "  PROM: setenv netaddr %s\n", c.IP)
		for _, m := range medias {
			for s := range dm[m] {
				if _, err := tree.Open(fmt.Sprintf("%s/%s/stand/fx.64", m, s)); err == nil {
					fmt.Fprintf(instructions, "  PROM: boot -f bootp():/%s/%s/stand/fx.64\n", m, s)
				}
			}
		}
		for _, p := range primaryDists {
			fx := combinedFxPath(p)
			if _, err := tree.Open(fx); err == nil {
				fmt.Fprintf(instructions, "  PROM: boot -f bootp():/%s\n", fx)
			}
			fmt.Fprintf(instructions, "  Inst>: from %s:%s\n", cfg.ServerIP, p)
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
