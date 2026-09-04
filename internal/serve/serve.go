// Package serve wires the configured services over one install-set
// tree: bootp answers the configured MACs, tftp and rsh serve the tree,
// every protocol filters to the configured client IPs. It also generates
// the operator files instigator serves alongside the media and reports
// what each set was assembled from. Callers may send PROM and Inst>
// commands to a separate instructions writer.
package serve

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jamesbraid/instigator/internal/bootp"
	"github.com/jamesbraid/instigator/internal/capture"
	"github.com/jamesbraid/instigator/internal/config"
	"github.com/jamesbraid/instigator/internal/instcmd"
	"github.com/jamesbraid/instigator/internal/instscript"
	"github.com/jamesbraid/instigator/internal/logging"
	"github.com/jamesbraid/instigator/internal/source"
	"github.com/jamesbraid/instigator/internal/tftp"
	"github.com/jamesbraid/instigator/internal/vfs"
	"github.com/jamesbraid/instigator/rcmd"
)

// defaultRSHIdleTimeout bounds how long a silent rsh session is tolerated
// before it is closed and recorded as idle. It is deliberately generous:
// it exists to reclaim a session whose client has crashed or gone away, not
// to cut off an operator pausing to read a conflict list, so it must
// comfortably exceed any real think-time between inst commands.
const defaultRSHIdleTimeout = 30 * time.Minute

// drainTimeout bounds how long Close waits for in-flight requests to finish
// and record their end events before the capture is finalized.
const drainTimeout = 5 * time.Second

// bindErr adds an elevation hint to permission-denied listener errors.
func bindErr(service string, port int, err error) error {
	if isBindPermission(err) {
		hint := "re-run with sudo"
		if runtime.GOOS == "windows" {
			hint = "run as Administrator and allow UDP 67/69 and TCP 514 through the firewall"
		}
		return fmt.Errorf("%s: binding port %d: %w (%s)", service, port, err, hint)
	}
	return fmt.Errorf("%s: %w", service, err)
}

// Servers is a running instigator instance.
type Servers struct {
	tree   *vfs.Tree
	bootp  *bootp.Server
	logger *logging.Logger
	rec    *capture.Recorder // nil when --capture-dir is unset

	// closing distinguishes a listener returning because Close shut it down
	// (expected, silent) from one exiting on its own (unexpected, logged and
	// recorded). started guards the capture finalize: it is set once
	// server_start is emitted, so a failed Start never writes a clean summary.
	closing atomic.Bool
	started atomic.Bool

	tftpSrv    *tftp.Server   // retained so Close can drain in-flight transfers
	rshSrv     *rcmd.Server   // retained so Close can drain active sessions
	listenerWG sync.WaitGroup // the accept/serve-loop goroutines, joined at Close

	bootpConn net.PacketConn
	tftpConn  net.PacketConn
	rshLn     net.Listener
}

// errNotRegular is what a request for a tree directory reports. A
// directory resolves fine, so answering "not found" would send a client
// looking for a path that is right there; it just isn't a byte stream.
var errNotRegular = errors.New("not a regular file")

func fsName(p string) string {
	p = strings.TrimPrefix(p, "/")
	if p == "" {
		return "."
	}
	return p
}

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

// ResolveImage implements tftp.ImageResolver, so a recorded boot transfer
// names the layer and in-layer path a served path came from, resolving
// through the same tree.Resolve every served file does.
func (f treeFS) ResolveImage(path string) (tftp.Resolved, error) {
	origin, err := f.t.Resolve(fsName(path))
	if err != nil {
		return tftp.Resolved{}, err
	}
	switch origin.Kind {
	case vfs.OriginImage, vfs.OriginDirectory:
		return tftp.Resolved{Image: origin.Source, Path: origin.Path}, nil
	default:
		return tftp.Resolved{}, fmt.Errorf("%s: not backed by media", path)
	}
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
		// The tree stores the full twelve permission bits numerically in
		// its Mode (Perm() would drop setuid/setgid/sticky).
		Perm:  uint32(info.Mode() & 0o7777),
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
	captureDir     string
	recorder       *capture.Recorder // injected pre-built recorder, for tests
	rshIdleTimeout time.Duration
}

// withBootpReplyAddr redirects bootp replies away from the broadcast
// address, for tests that cannot receive broadcasts.
func withBootpReplyAddr(a net.Addr) Option {
	return func(o *options) { o.bootpReplyAddr = a }
}

// withRSHHighPorts disables the rsh reserved-source-port check, for
// tests that cannot bind reserved ports.
func withRSHHighPorts() Option {
	return func(o *options) { o.rshHighPorts = true }
}

// withInstructions directs the operator's PROM/inst commands to w instead
// of discarding them. These are console UX for a human at the PROM, never
// part of the leveled server log.
func withInstructions(w io.Writer) Option {
	return func(o *options) { o.instructions = w }
}

// WithCapture turns on the install recorder, writing the run's bundle
// (run.json, events.jsonl, summary.json) under dir. Unset means no
// recording and no overhead.
func WithCapture(dir string) Option {
	return func(o *options) { o.captureDir = dir }
}

// withRecorder injects a pre-built recorder, letting a test supply an
// injected clock and summary sink. It takes precedence over WithCapture.
func withRecorder(r *capture.Recorder) Option {
	return func(o *options) { o.recorder = r }
}

// withRSHIdleTimeout overrides how long a silent rsh session is tolerated
// before it is closed and recorded as idle. Zero keeps defaultRSHIdleTimeout.
func withRSHIdleTimeout(d time.Duration) Option {
	return func(o *options) { o.rshIdleTimeout = d }
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
	res := source.New(source.Options{CacheDir: cacheDir(cfg), Credentials: toSourceCreds(cfg.Credentials)})
	tree, err := vfs.Build(setSpecs(cfg), res)
	if err != nil {
		return nil, err
	}
	prof, err := generate(cfg, tree)
	if err != nil {
		tree.Close()
		return nil, err
	}
	s := &Servers{tree: tree, logger: logger}

	// The recorder, if any, is built before the listeners so its
	// server_start (emitted at the end of Start) marks the true start of
	// serving. A pre-built recorder (tests) wins over --capture-dir.
	switch {
	case o.recorder != nil:
		s.rec = o.recorder
	case o.captureDir != "":
		rec, err := capture.New(o.captureDir)
		if err != nil {
			tree.Close()
			return nil, fmt.Errorf("capture: %w", err)
		}
		s.rec = rec
	}
	if s.rec != nil {
		// A capture that cannot even write its provenance is broken from the
		// start; fail rather than serve a run that will not be recorded.
		if err := s.rec.WriteRun(buildProvenance(cfg)); err != nil {
			s.Close()
			return nil, fmt.Errorf("capture: run.json: %w", err)
		}
	}

	allowed := make(map[netip.Addr]bool, len(cfg.Clients))
	aliasByIP := make(map[netip.Addr]string, len(cfg.Clients))
	for _, c := range cfg.Clients {
		allowed[c.IP] = true
		aliasByIP[c.IP] = c.Name
	}
	allow := func(a netip.Addr) bool { return allowed[a] }

	logStartup(cfg, tree, prof, logger, o.instructions)

	// Listeners are bound now but their serve loops are launched only after
	// server_start is emitted, so no bootp/tftp/rsh event can precede it.
	type listenerFn struct {
		name string
		fn   func() error
	}
	var listeners []listenerFn

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
			Recorder:  s.rec,
		}
		pc, err := bootp.ListenBroadcast(fmt.Sprintf(":%d", cfg.Ports.BOOTP))
		if err != nil {
			s.Close()
			return nil, bindErr("bootp", cfg.Ports.BOOTP, err)
		}
		s.bootpConn = pc
		listeners = append(listeners, listenerFn{"bootp", func() error { return s.bootp.Serve(pc) }})
	}

	if cfg.Services.TFTP.Enabled {
		srv := &tftp.Server{
			FS:         treeFS{tree},
			AllowIP:    allow,
			PortMin:    cfg.Services.TFTP.PortRange[0],
			PortMax:    cfg.Services.TFTP.PortRange[1],
			Logger:     logger,
			Recorder:   s.rec,
			ClientName: func(a netip.Addr) string { return aliasByIP[a] },
		}
		pc, err := net.ListenPacket("udp4", fmt.Sprintf(":%d", cfg.Ports.TFTP))
		if err != nil {
			s.Close()
			return nil, bindErr("tftp", cfg.Ports.TFTP, err)
		}
		s.tftpConn = pc
		s.tftpSrv = srv
		listeners = append(listeners, listenerFn{"tftp", func() error { return srv.Serve(pc) }})
	}

	if cfg.Services.RSH {
		idle := o.rshIdleTimeout
		if idle == 0 {
			idle = defaultRSHIdleTimeout
		}
		srv := &rcmd.Server{
			AllowIP:        allow,
			AllowHighPorts: o.rshHighPorts,
			IdleTimeout:    idle,
			Logger:         logger,
			RejectHook: func(addr netip.Addr, reason string) {
				s.rec.RSHRejected(aliasByIP[addr], addr.String(), reason)
			},
			Handler: func(req *rcmd.Request) error {
				// inst drives the install by opening a shell over rsh
				// (exec /bin/sh) and streaming commands to it - the only
				// rsh vocabulary this server speaks.
				if !isShell(req.Command) {
					logger.Warnf("rsh: refusing non-shell command %q", req.Command)
					return fmt.Errorf("only a shell session is served")
				}
				sess := s.rec.BeginSession(aliasByIP[req.Addr], req.Addr.String(), req.RemoteUser, req.LocalUser)
				err := instcmd.RunShell(cmdFS{tree}, req.Stdin, req.Stdout, req.Stderr, logger, sess)
				sess.End(err)
				return err
			},
		}
		ln, err := net.Listen("tcp4", fmt.Sprintf(":%d", cfg.Ports.RSH))
		if err != nil {
			s.Close()
			return nil, bindErr("rsh", cfg.Ports.RSH, err)
		}
		s.rshLn = ln
		s.rshSrv = srv
		listeners = append(listeners, listenerFn{"rsh", func() error { return srv.Serve(ln) }})
	}

	// Every listener is bound. Emit server_start, then launch the serve
	// loops, so the first recorded event is always the start. Each loop is
	// tracked so Close can join it before finalizing the capture.
	if s.rec != nil {
		s.rec.ServerStart()
		s.started.Store(true)
	}
	for _, l := range listeners {
		s.listenerWG.Add(1)
		go func(l listenerFn) {
			defer s.listenerWG.Done()
			s.runListener(l.name, l.fn)
		}(l)
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

// cacheDir returns where the source resolver caches fetched and extracted
// remote media. cfg.CacheDir wins when set; otherwise it's the user's cache
// directory (~/.cache/instigator on Linux), and when even that is
// unavailable - a scratch container with no HOME or XDG_CACHE_HOME - a fixed
// fallback so a remote source still has somewhere to land.
func cacheDir(cfg *config.Config) string {
	if cfg.CacheDir != "" {
		return cfg.CacheDir
	}
	d, err := os.UserCacheDir()
	if err != nil {
		return "/var/cache/instigator"
	}
	return filepath.Join(d, "instigator")
}

// toSourceCreds maps the configured host credentials onto source.Credentials.
// The two types are shaped identically; the copy exists so config and source
// stay free to diverge without either importing the other's package for a
// shared struct.
func toSourceCreds(creds []config.Credential) source.Credentials {
	out := make(source.Credentials, 0, len(creds))
	for _, c := range creds {
		out = append(out, source.Credential{Host: c.Host, Username: c.Username, Password: c.Password})
	}
	return out
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
				Name:   l.Name,
				Source: l.Source,
				Base:   l.Base,
				Dist:   l.Dist,
				Stand:  l.Stand,
				Boot:   l.Boot,
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

// generatedFile is one file instigator synthesizes: its tree path,
// recorded origin, and bytes.
type generatedFile struct {
	path      string
	generator string
	content   []byte
}

// generate adds the machine-consumed files instigator serves alongside
// the media: the command sequence an operator loads with "admin source"
// (install.cmds at the root) and the .related_dists menu aid that shadows
// whatever stock copy the primary layer's media ships. Human instructions
// live in the repository's static Markdown documentation.
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
	p.remoteDir = "/" + p.primary + "/dist"
	boot := "/" + p.primary + "/stand/fx.64"
	if f, err := tree.Open(fsName(boot)); err == nil {
		f.Close()
		p.bootPath = boot
	}

	cmds := []byte(instscript.Commands(instscript.Params{
		ServerIP: cfg.ServerIP.String(),
		Sets:     p.dists,
	}))
	p.generated = []generatedFile{
		{
			path:      "install.cmds",
			generator: "admin-source",
			content:   cmds,
		},
		{
			path:      p.primary + "/dist/.related_dists",
			generator: "related-dists",
			content:   []byte(instscript.RelatedDists(p.dists)),
		},
	}
	for _, f := range p.generated {
		if err := tree.AddGenerated(f.path, f.generator, f.content); err != nil {
			return profile{}, err
		}
	}
	return p, nil
}

// sanitizeSource redacts URL secrets while leaving local paths unchanged.
func sanitizeSource(src string) string {
	if u, err := url.Parse(src); err == nil && (u.Scheme == "http" || u.Scheme == "https") {
		return source.SafeURL(src)
	}
	return src
}

// logStartup logs the served inventory and writes operator commands separately.
func logStartup(cfg *config.Config, tree *vfs.Tree, p profile, logger *logging.Logger, instructions io.Writer) {
	for _, set := range cfg.InstallSets {
		state := "enabled"
		if !set.Enabled {
			state = "disabled"
		}
		logger.Infof("set %s: %s, %s", set.Name, state, plural(len(set.Layers), "layer"))
		for _, l := range set.Layers {
			role := ""
			if l.Boot {
				role = ", boot"
			}
			logger.Infof("set %s: layer %s  <-  source %q  dist %s%s",
				set.Name, l.Name, sanitizeSource(l.Source), l.Dist, role)
		}
		for _, path := range slices.Sorted(maps.Keys(set.Collisions)) {
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
			// startup drops it in the same case.
			fmt.Fprintf(instructions, "  PROM: Remote Directory: %s\n", p.remoteDir)
		}
		if len(p.dists) == 0 {
			continue
		}
		fmt.Fprintf(instructions, "  Inst>: admin source %s:/install.cmds\n", cfg.ServerIP)
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

// BOOTPAddr returns the bootp socket address.
func (s *Servers) BOOTPAddr() net.Addr { return s.bootpConn.LocalAddr() }

// TFTPAddr returns the tftp socket address.
func (s *Servers) TFTPAddr() net.Addr { return s.tftpConn.LocalAddr() }

// RSHAddr returns the rsh listener address.
func (s *Servers) RSHAddr() net.Addr { return s.rshLn.Addr() }

// Close shuts every listener and releases the media.
func (s *Servers) Close() error {
	s.closing.Store(true)
	if s.bootpConn != nil {
		s.bootpConn.Close()
	}
	if s.tftpConn != nil {
		s.tftpConn.Close()
	}
	if s.rshLn != nil {
		s.rshLn.Close()
	}
	// Drain active work so an interrupted install still records its last
	// command, session, and transfer. Join the serve loops first (this is
	// where bootp does its inline work), then force-close and drain the
	// per-request handlers. drained stays true only if every stage finished
	// within its window; otherwise the capture is incomplete.
	drained := true
	if !waitTimeout(&s.listenerWG, drainTimeout) {
		s.logger.Warnf("serve: serve loops still running after %s", drainTimeout)
		drained = false
	}
	if s.rshSrv != nil && !s.rshSrv.Shutdown(drainTimeout) {
		s.logger.Warnf("serve: rsh sessions still active after %s drain", drainTimeout)
		drained = false
	}
	if s.tftpSrv != nil && !s.tftpSrv.Shutdown(drainTimeout) {
		s.logger.Warnf("serve: tftp transfers still active after %s drain", drainTimeout)
		drained = false
	}

	var recErr error
	if s.rec != nil {
		if s.started.Load() {
			reason := "clean"
			if !drained {
				reason = "incomplete"
			}
			if recErr = s.rec.Finish(reason); recErr != nil {
				s.logger.Errorf("capture: finish: %v", recErr)
			}
		} else {
			// The server never finished starting; close the events file
			// without writing a summary that would imply a clean run.
			recErr = s.rec.Close()
		}
	}

	treeErr := s.tree.Close()
	switch {
	case !drained:
		return fmt.Errorf("shutdown drained incompletely within %s; capture may be missing events", drainTimeout)
	case recErr != nil:
		return recErr
	default:
		return treeErr
	}
}

// runListener runs one protocol listener and reports an unexpected exit -
// Serve returning while the server is not shutting down - to both the log
// and the trace. A return after Close has set closing is the normal
// shutdown path and is silent.
func (s *Servers) runListener(name string, serve func() error) {
	err := serve()
	if s.closing.Load() {
		return
	}
	s.logger.Errorf("serve: %s listener exited unexpectedly: %v", name, err)
	s.rec.ListenerExit(name)
}

// waitTimeout waits for wg up to timeout, returning true if it completed.
func waitTimeout(wg *sync.WaitGroup, timeout time.Duration) bool {
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}

// buildProvenance assembles run.json's provenance from the config. Each
// configured layer's Source becomes a media-manifest entry named by its
// basename - never the operator's absolute path, and never a content hash
// (hashing multi-GB ISOs at startup is out of scope). A URL source is run
// through sanitizeSource first, so a basename computed from it can never
// carry embedded userinfo or a query-string secret into run.json. Size and
// mtime come from an os.Stat of Source, so they are populated for a local
// disc image or directory and simply left zero for a remote URL, which stat
// cannot see.
func buildProvenance(cfg *config.Config) capture.Provenance {
	var media []capture.Media
	for _, set := range cfg.InstallSets {
		for _, layer := range set.Layers {
			src := layer.Source
			m := capture.Media{Media: set.Name, Disc: layer.Name, Image: filepath.Base(sanitizeSource(src))}
			if fi, err := os.Stat(src); err == nil {
				m.Size = fi.Size()
				m.Mtime = fi.ModTime().UTC().Format(time.RFC3339)
			}
			media = append(media, m)
		}
	}

	var clients []capture.Client
	for _, c := range cfg.Clients {
		clients = append(clients, capture.Client{Alias: c.Name, MAC: c.MAC.String(), IP: c.IP.String()})
	}

	return capture.Provenance{
		Start:  time.Now().UTC().Format(time.RFC3339Nano),
		Binary: capture.BuildInfo(),
		Services: capture.Services{
			BOOTP:         cfg.Services.BOOTP,
			TFTP:          cfg.Services.TFTP.Enabled,
			RSH:           cfg.Services.RSH,
			TFTPPortRange: cfg.Services.TFTP.PortRange,
		},
		Config: capture.ConfigInfo{
			ServerIP: cfg.ServerIP.String(),
			Netmask:  cfg.Netmask.String(),
			Ports: map[string]int{
				"bootp": cfg.Ports.BOOTP,
				"tftp":  cfg.Ports.TFTP,
				"rsh":   cfg.Ports.RSH,
			},
		},
		Clients: clients,
		Media:   media,
	}
}
