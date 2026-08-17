// Package serve wires the configured services over one install-set
// tree: bootp answers the configured MACs, tftp and rsh serve the tree,
// every protocol filters to the configured client IPs. It also generates
// the operator files instigator serves alongside the media, reports what
// each set was assembled from, and prints the exact PROM and Inst>
// commands to type.
package serve

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
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
	"github.com/jamesbraid/instigator/rcmd"
)

// Servers is a running instigator instance.
type Servers struct {
	tree  *vfs.Tree
	bootp *bootp.Server

	bootpConn net.PacketConn
	tftpConn  net.PacketConn
	rshLn     net.Listener
}

// errNotRegular is what a request for a tree directory reports. A
// directory resolves fine, so answering "not found" would send a client
// looking for a path that is right there; it just isn't a byte stream.
var errNotRegular = errors.New("not a regular file")

// fsName converts a protocol path to an io/fs name: no leading slash,
// and "." for the tree root, which is what "/" becomes once the slash
// comes off.
func fsName(p string) string {
	p = strings.TrimPrefix(p, "/")
	if p == "" {
		return "."
	}
	return p
}

// notFound translates a tree error into the sentinel a protocol server
// tests for. Anything else - a directory, an unreadable image - passes
// through as itself, so a real failure is never reported as a missing
// file.
func notFound(path string, err, sentinel error) error {
	if errors.Is(err, vfs.ErrNotFound) || errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%s: %w", path, sentinel)
	}
	return err
}

// openRegular opens a tree path a protocol is about to stream. Both
// protocols want random access, which the tree gives regular files and
// only regular files.
func openRegular(t *vfs.Tree, path string) (vfs.File, error) {
	name := fsName(path)
	f, err := t.Open(name)
	if err != nil {
		return nil, err
	}
	file, ok := f.(vfs.File)
	if !ok {
		f.Close()
		return nil, &fs.PathError{Op: "open", Path: name, Err: errNotRegular}
	}
	return file, nil
}

// treeFS adapts the install-set tree to tftp's filesystem interface.
type treeFS struct{ t *vfs.Tree }

func (f treeFS) Open(path string) (tftp.File, error) {
	file, err := openRegular(f.t, path)
	if err != nil {
		return nil, notFound(path, err, tftp.ErrNotFound)
	}
	return file, nil
}

// cmdFS adapts the install-set tree to the rsh command interpreter's
// filesystem interface.
type cmdFS struct{ t *vfs.Tree }

func (f cmdFS) Open(path string) (instcmd.File, error) {
	file, err := openRegular(f.t, path)
	if err != nil {
		return nil, notFound(path, err, instcmd.ErrNotFound)
	}
	return file, nil
}

func (f cmdFS) ReadDir(path string) ([]string, error) {
	ents, err := f.t.ReadDir(fsName(path))
	if err != nil {
		return nil, notFound(path, err, instcmd.ErrNotFound)
	}
	names := make([]string, 0, len(ents))
	for _, e := range ents {
		names = append(names, e.Name())
	}
	return names, nil
}

func (f cmdFS) Stat(path string) (instcmd.FileInfo, error) {
	info, err := f.t.Stat(fsName(path))
	if err != nil {
		return instcmd.FileInfo{}, notFound(path, err, instcmd.ErrNotFound)
	}
	// The tree carries what ls -l/-i needs past fs.FileInfo - the stable
	// inode, the owner, the link count - in Sys.
	md, ok := info.Sys().(*vfs.Metadata)
	if !ok {
		md = &vfs.Metadata{}
	}
	return instcmd.FileInfo{
		Ino:   md.Ino,
		IsDir: info.IsDir(),
		Perm:  uint32(info.Mode().Perm()),
		Nlink: md.Nlink,
		UID:   md.UID,
		GID:   md.GID,
		Size:  info.Size(),
		Mtime: info.ModTime(),
	}, nil
}

// ResolveImage implements instcmd.ImageResolver, so a leaf command
// (dd, cat) can log which layer and in-layer path a served path
// actually came from, not just the tree path it was given.
func (f cmdFS) ResolveImage(path string) (instcmd.Resolved, error) {
	origin, err := f.t.Resolve(fsName(path))
	if err != nil {
		return instcmd.Resolved{}, notFound(path, err, instcmd.ErrNotFound)
	}
	switch origin.Kind {
	case vfs.OriginImage, vfs.OriginDirectory:
		return instcmd.Resolved{Image: origin.Source, Path: origin.Path}, nil
	default:
		// A generated file, or a structural directory above the layers,
		// has no media behind it to name - the manifest line then reports
		// the served path alone.
		return instcmd.Resolved{}, fmt.Errorf("%s: not backed by media", path)
	}
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

// Start assembles the configured install sets, generates the operator
// files, binds the enabled services on the configured ports, and serves
// until Close. logger receives leveled server output; nil is silent,
// exactly like an unset Logf used to be. -v maps to a DEBUG-level logger
// at the call site (cmd/instigator), not an option here: verbosity is a
// property of the logger a caller builds, not a separate flag threaded
// through every service.
func Start(cfg *config.Config, logger *logging.Logger, opts ...Option) (*Servers, error) {
	o := options{instructions: io.Discard}
	for _, opt := range opts {
		opt(&o)
	}
	tree, err := vfs.Build(setSpecs(cfg))
	if err != nil {
		return nil, err
	}
	prof, err := generate(cfg, tree)
	if err != nil {
		tree.Close()
		return nil, err
	}
	s := &Servers{tree: tree}

	allowed := make(map[netip.Addr]bool, len(cfg.Clients))
	for _, c := range cfg.Clients {
		allowed[c.IP] = true
	}
	allow := func(a netip.Addr) bool { return allowed[a] }

	logStartup(cfg, tree, prof, logger, o.instructions)

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

// setSpecs maps the configured install sets onto the tree builder's own
// shape. Every configured set is built and served, enabled or not: a set
// left out of the install stays browsable for inspection, and Enabled
// decides only what the generated files and the operator instructions
// offer.
func setSpecs(cfg *config.Config) []vfs.SetSpec {
	sets := make([]vfs.SetSpec, 0, len(cfg.InstallSets))
	for _, set := range cfg.InstallSets {
		layers := make([]vfs.LayerSpec, 0, len(set.Layers))
		for _, l := range set.Layers {
			layers = append(layers, vfs.LayerSpec{
				Name:      l.Name,
				Image:     l.Image,
				Dir:       l.Dir,
				SourceDir: l.SourceDir,
				TargetDir: l.TargetDir,
			})
		}
		sets = append(sets, vfs.SetSpec{Name: set.Name, Layers: layers, Collisions: set.Collisions})
	}
	return sets
}

// profile is what the enabled sets add up to: the dist path of every set
// the operator opens, in config order, the two PROM values that only
// exist once a first set does, and the files generated around them. The
// zero profile is the nothing-enabled case - no set to open, no boot
// artifact, no directory to fetch from, nothing generated.
type profile struct {
	primary   string
	dists     []string
	bootPath  string
	remoteDir string
	generated []generatedFile
}

// generatedFile is one file instigator synthesized: the tree path it is
// served at, the generator that produced it (its recorded origin), and
// its bytes.
type generatedFile struct {
	path      string
	generator string
	content   []byte
}

// generate adds the operator files instigator serves alongside the
// media: the command sequence inst picks up by itself (inst.init beside
// the primary set's dist) and the byte-identical copy an operator loads
// by hand with "admin source" (install.cmds at the root), the
// .related_dists menu aid that shadows whatever stock copy the primary
// layer's media ships, and the human runbook at /install.
//
// With no set enabled there is no first set to build any of them around,
// so nothing is generated at all - every configured set is still served,
// just not offered.
func generate(cfg *config.Config, tree *vfs.Tree) (profile, error) {
	var p profile
	for _, set := range cfg.InstallSets {
		if !set.Enabled {
			continue
		}
		if p.primary == "" {
			p.primary = set.Name
		}
		p.dists = append(p.dists, "/"+set.Name+"/dist")
	}
	if p.primary == "" {
		return p, nil
	}

	// The PROM boots only what this server can actually answer for, so a
	// primary set whose layers carry no miniroot partitioner leaves the
	// boot path empty rather than name a file that would 404.
	p.remoteDir = "/" + p.primary + "/dist/"
	boot := "/" + p.primary + "/stand/fx.64"
	if f, err := tree.Open(fsName(boot)); err == nil {
		f.Close()
		p.bootPath = boot
	}

	cmds := []byte(instscript.Commands(instscript.Params{
		ServerIP: cfg.ServerIP.String(),
		Sets:     p.dists,
	}))
	runbook := []byte(instscript.Generate(instscript.Params{
		ServerIP:  cfg.ServerIP.String(),
		Sets:      p.dists,
		BootPath:  p.bootPath,
		RemoteDir: p.remoteDir,
	}))
	p.generated = []generatedFile{
		{p.primary + "/dist/inst.init", "inst.init", cmds},
		{"install.cmds", "admin-source", cmds},
		{p.primary + "/dist/.related_dists", "related-dists", []byte(instscript.RelatedDists(p.dists))},
		{"install", "runbook", runbook},
	}
	for _, f := range p.generated {
		if err := tree.AddGenerated(f.path, f.generator, f.content); err != nil {
			return profile{}, err
		}
	}
	return p, nil
}

// logStartup logs the install-set inventory at INFO - what each set was
// assembled from, in layer order, which collisions configuration
// settled, and where a representative file's bytes actually came from -
// and separately writes the operator's commands (what to type at the
// PROM and at the Inst> prompt) to instructions. The two are different
// audiences: the inventory is what this server is doing, the PROM/Inst>
// lines are what a human does next, and the latter were never server log
// content - they went to the logger only because nothing else was wired
// up to carry them.
func logStartup(cfg *config.Config, tree *vfs.Tree, p profile, logger *logging.Logger, instructions io.Writer) {
	for _, set := range cfg.InstallSets {
		state := "enabled"
		if !set.Enabled {
			state = "disabled"
		}
		logger.Infof("set %s: %s, %s", set.Name, state, plural(len(set.Layers), "layer"))
		for _, l := range set.Layers {
			kind, source := "image", l.Image
			if l.Dir != "" {
				kind, source = "directory", l.Dir
			}
			logger.Infof("set %s: layer %s  <-  %s %q  %s -> %s",
				set.Name, l.Name, kind, source, l.SourceDir, l.TargetDir)
		}
		for _, path := range sortedKeys(set.Collisions) {
			logger.Infof("set %s: collision %s  <-  layer %s", set.Name, path, set.Collisions[path])
		}
	}
	// A merge is only as clear as the file it produced: name the layer
	// behind the artifact the PROM boots and the one inst reads first, so
	// a surprise there is visible at startup rather than mid-install.
	if p.bootPath != "" {
		logOrigin(tree, logger, p.primary, p.bootPath)
	}
	for _, dist := range p.dists {
		set := strings.TrimSuffix(strings.TrimPrefix(dist, "/"), "/dist")
		logOrigin(tree, logger, set, dist+"/sa")
	}
	for _, f := range p.generated {
		logger.Infof("generated: /%s  <-  %s", f.path, f.generator)
	}
	if p.primary == "" {
		logger.Warnf("no install set is enabled: serving the tree with no generated commands")
	}

	for _, c := range cfg.Clients {
		logger.Infof("client %s: mac=%s ip=%s", c.Name, c.MAC, c.IP)
		fmt.Fprintf(instructions, "  PROM: setenv netaddr %s\n", c.IP)
		if p.bootPath != "" {
			fmt.Fprintf(instructions, "  PROM: boot -f bootp():%s\n", p.bootPath)
			// The PROM asks for the Remote Directory as part of that
			// netboot, so it belongs to the boot line and goes with it -
			// the served runbook drops it in the same case.
			fmt.Fprintf(instructions, "  PROM: Remote Directory: %s\n", p.remoteDir)
		}
		if len(p.dists) == 0 {
			continue
		}
		// inst cannot discover that these sets belong together, so every
		// one of them is opened explicitly: "from" for the first, "open"
		// for each after it.
		fmt.Fprintf(instructions, "  Inst>: from %s:%s\n", cfg.ServerIP, p.dists[0])
		for _, dist := range p.dists[1:] {
			fmt.Fprintf(instructions, "  Inst>: open %s:%s\n", cfg.ServerIP, dist)
		}
	}
}

// logOrigin reports which layer one representative served file came
// from. A path that isn't there is simply not reported: these are
// illustrative files, and a set that doesn't carry one is not an error.
func logOrigin(tree *vfs.Tree, logger *logging.Logger, set, path string) {
	origin, err := tree.Resolve(fsName(path))
	if err != nil {
		return
	}
	logger.Infof("set %s: %s  <-  %s %s:/%s", set, path, origin.Kind, origin.Source, origin.Path)
}

func plural(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
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
	return s.tree.Close()
}
