package main

import (
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/jamesbraid/instigator/internal/logging"
	"github.com/jamesbraid/instigator/internal/qemunet"
	"github.com/jamesbraid/instigator/internal/sharefs"
	"github.com/jamesbraid/instigator/nfs"
)

// privateEndpoint is where a machine's QEMU stream backend connects:
// a Unix socket or a loopback TCP address, never both.
type privateEndpoint struct{ socket, tcp string }

func (e privateEndpoint) enabled() bool { return e.socket != "" || e.tcp != "" }

func (e privateEndpoint) check() error {
	if e.socket != "" && e.tcp != "" {
		return errors.New("--network-socket and --network-tcp cannot be used together")
	}
	return nil
}

func (e privateEndpoint) listen(cfg qemunet.Config) (*qemunet.Listener, error) {
	if e.tcp != "" {
		return qemunet.ListenTCP(e.tcp, cfg)
	}
	return qemunet.Listen(e.socket, cfg)
}

// shareSpec is one --share NAME=DIR.
type shareSpec struct{ name, dir string }

// parseShareSpecs reads NAME=DIR options. The name is the export's mount
// path, so it must be one path element, and no two may match.
func parseShareSpecs(args []string) ([]shareSpec, error) {
	var specs []shareSpec
	seen := map[string]bool{}
	for _, a := range args {
		name, dir, ok := strings.Cut(a, "=")
		switch {
		case !ok || name == "" || dir == "":
			return nil, fmt.Errorf("--share %q: want NAME=DIR", a)
		case strings.ContainsAny(name, "/\\"):
			return nil, fmt.Errorf("--share %q: the name must be one path element", a)
		case seen[name]:
			return nil, fmt.Errorf("--share %q: the name %q is already used", a, name)
		}
		seen[name] = true
		specs = append(specs, shareSpec{name, dir})
	}
	return specs, nil
}

// openShares exports specs from one filesystem: the share itself for one,
// a Multi for several. handleTable names the path table of a single
// share; on Windows, which needs one, each share defaults to a file in
// the user cache directory.
func openShares(specs []shareSpec, handleTable string) (nfs.FS, io.Closer, string, error) {
	if len(specs) == 0 {
		return nil, nil, "", errors.New("share: nothing to share")
	}
	if handleTable != "" && len(specs) > 1 {
		return nil, nil, "", errors.New("share: --handle-table names one share's table, and there are several")
	}
	var opened []*sharefs.FS
	closeAll := func() {
		for _, f := range opened {
			f.Close()
		}
	}
	var desc []string
	for _, sp := range specs {
		table := handleTable
		if table == "" && runtime.GOOS == "windows" {
			dir, err := os.UserCacheDir()
			if err != nil {
				closeAll()
				return nil, nil, "", fmt.Errorf("share: no --handle-table and no cache directory: %v", err)
			}
			table = filepath.Join(dir, "instigator", "share-"+sp.name+".handles")
		}
		f, err := sharefs.Open(sp.name, sp.dir, sharefs.Options{PathTable: table, TableFallback: fallbackTable(sp)})
		if err != nil {
			closeAll()
			return nil, nil, "", err
		}
		opened = append(opened, f)
		desc = append(desc, fmt.Sprintf("%s as /%s", f.Dir(), sp.name))
	}
	if len(opened) == 1 {
		return opened[0], opened[0], desc[0], nil
	}
	m, err := sharefs.OpenMulti(opened...)
	if err != nil {
		closeAll()
		return nil, nil, "", err
	}
	return m, m, strings.Join(desc, ", "), nil
}

// fallbackTable is where a share keeps its path table when its filesystem
// has no birth time, as an NFS mount has none: a file in the user cache
// directory named for the share and its directory, so two shares of one
// name on different directories don't share numbering. Empty when there
// is no cache directory.
func fallbackTable(sp shareSpec) string {
	dir, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	abs, err := filepath.Abs(sp.dir)
	if err != nil {
		return ""
	}
	h := fnv.New32a()
	h.Write([]byte(abs))
	return filepath.Join(dir, "instigator", fmt.Sprintf("share-%s-%08x.handles", sp.name, h.Sum32()))
}

// shareNetwork is the part of a private network the NFS server needs.
type shareNetwork interface {
	ListenPacket(port int) (net.PacketConn, error)
	Listen(port int) (net.Listener, error)
}

// The ports a private network serves NFS on. The guest's portmap lookups
// go to 111 whatever the mount options say, so there is no choosing them.
const (
	privatePortmapPort = 111
	privateMountPort   = 635
	privateNFSPort     = 2049
)

// privateShares is the NFS server running on a private network.
type privateShares struct {
	closers []io.Closer
	wg      sync.WaitGroup
	errs    chan error
}

// serveSharesOn answers portmap, MOUNT and NFS for fsys on n, over UDP and
// TCP, on 111, 635 and 2049. Every listener answers all three programs,
// as on a host socket.
func serveSharesOn(n shareNetwork, fsys nfs.FS, logger *logging.Logger) (*privateShares, error) {
	srv := &nfs.Server{FS: fsys, Logger: logger}
	ports := nfs.Ports{Portmap: privatePortmapPort, Mount: privateMountPort, NFS: privateNFSPort}
	srv.SetPorts(ports, ports)
	p := &privateShares{errs: make(chan error, 6)}
	for _, port := range []int{privatePortmapPort, privateMountPort, privateNFSPort} {
		pc, err := n.ListenPacket(port)
		if err != nil {
			p.Close()
			return nil, fmt.Errorf("share: udp %d: %w", port, err)
		}
		p.closers = append(p.closers, pc)
		l, err := n.Listen(port)
		if err != nil {
			p.Close()
			return nil, fmt.Errorf("share: tcp %d: %w", port, err)
		}
		p.closers = append(p.closers, l)
		p.wg.Go(func() { p.errs <- srv.ServeUDP(pc) })
		p.wg.Go(func() { p.errs <- srv.ServeTCP(l) })
	}
	return p, nil
}

// Err delivers the first listener to fail. Closing the listeners ends
// them with a nil result.
func (p *privateShares) Err() <-chan error { return p.errs }

// Close stops every listener and waits for them to finish.
func (p *privateShares) Close() {
	for _, c := range p.closers {
		c.Close()
	}
	p.closers = nil
	p.wg.Wait()
}

// acceptNetwork waits for the machine to connect. It returns a nil
// network and a nil error if stop fires first.
func acceptNetwork(l *qemunet.Listener, stop <-chan os.Signal) (*qemunet.Network, error) {
	type accepted struct {
		net *qemunet.Network
		err error
	}
	ch := make(chan accepted, 1)
	go func() {
		n, err := l.Accept()
		ch <- accepted{n, err}
	}()
	select {
	case <-stop:
		return nil, nil
	case a := <-ch:
		if a.err != nil {
			return nil, fmt.Errorf("accepting the machine: %w", a.err)
		}
		return a.net, nil
	}
}
