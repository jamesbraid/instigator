package main

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/jamesbraid/instigator/internal/logging"
	"github.com/jamesbraid/instigator/internal/sharefs"
	"github.com/jamesbraid/instigator/nfs"
)

// shareOptions is the share command's flags.
type shareOptions struct {
	name, dir   string
	listen      []string
	port        int // NFS and MOUNT
	portmapPort int // 0: no portmap listener
	udp, tcp    bool
	handleTable string
	verbose     bool
}

// runShare serves a share until a signal stops it.
func runShare(o shareOptions, output io.Writer) error {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sig)
	return serveShare(o, output, sig, nil)
}

// serveShare exports o.dir until stop fires. ready, when set, is told
// the bound UDP and TCP addresses once every listener is up.
func serveShare(o shareOptions, output io.Writer, stop <-chan os.Signal, ready func(udp, tcp []net.Addr)) error {
	if !o.udp && !o.tcp {
		return errors.New("share: --udp and --tcp are both off")
	}
	if o.handleTable == "" && runtime.GOOS == "windows" {
		dir, err := os.UserCacheDir()
		if err != nil {
			return fmt.Errorf("share: no --handle-table and no cache directory: %v", err)
		}
		o.handleTable = filepath.Join(dir, "instigator", "share-"+o.name+".handles")
	}
	fsys, err := sharefs.Open(o.name, o.dir, sharefs.Options{PathTable: o.handleTable})
	if err != nil {
		return err
	}
	defer fsys.Close()

	level := logging.LevelInfo
	if o.verbose {
		level = logging.LevelDebug
	}
	logger := logging.New(output, level)
	srv := &nfs.Server{FS: fsys, Logger: logger}

	var (
		packets   []net.PacketConn
		listeners []net.Listener
		closers   []io.Closer
	)
	defer func() {
		for _, c := range closers {
			c.Close()
		}
	}()
	// Port 0 picks a free port once; every other listener of that service
	// takes the same number, so one port serves both transports.
	bind := func(port *int) error {
		for _, addr := range o.listen {
			hp := net.JoinHostPort(addr, strconv.Itoa(*port))
			if o.udp {
				pc, err := net.ListenPacket("udp", hp)
				if err != nil {
					return err
				}
				closers = append(closers, pc)
				packets = append(packets, pc)
				*port = pc.LocalAddr().(*net.UDPAddr).Port
				hp = net.JoinHostPort(addr, strconv.Itoa(*port))
			}
			if o.tcp {
				l, err := net.Listen("tcp", hp)
				if err != nil {
					return err
				}
				closers = append(closers, l)
				listeners = append(listeners, l)
				*port = l.Addr().(*net.TCPAddr).Port
			}
		}
		return nil
	}
	nfsPort := o.port
	if err := bind(&nfsPort); err != nil {
		return fmt.Errorf("share: %v", err)
	}
	pmapPort := o.portmapPort
	if pmapPort != 0 {
		if err := bind(&pmapPort); err != nil {
			return fmt.Errorf("share: portmap: %v (--portmap-port 0 serves without one)", err)
		}
	}
	ports := nfs.Ports{Portmap: uint32(pmapPort), Mount: uint32(nfsPort), NFS: uint32(nfsPort)}
	var udpPorts, tcpPorts nfs.Ports
	if o.udp {
		udpPorts = ports
	}
	if o.tcp {
		tcpPorts = ports
	}
	srv.SetPorts(udpPorts, tcpPorts)

	var wg sync.WaitGroup
	errs := make(chan error, len(packets)+len(listeners))
	for _, pc := range packets {
		wg.Go(func() { errs <- srv.ServeUDP(pc) })
	}
	for _, l := range listeners {
		wg.Go(func() { errs <- srv.ServeTCP(l) })
	}

	var transports []string
	if o.udp {
		transports = append(transports, "udp")
	}
	if o.tcp {
		transports = append(transports, "tcp")
	}
	portmap := "no portmap"
	if pmapPort != 0 {
		portmap = fmt.Sprintf("portmap on port %d", pmapPort)
	}
	logger.Infof("share: %s exported as /%s on %s, NFS and MOUNT on port %d over %s, %s",
		fsys.Dir(), o.name, strings.Join(o.listen, ", "), nfsPort, strings.Join(transports, " and "), portmap)
	if ready != nil {
		var udp, tcp []net.Addr
		for _, pc := range packets {
			udp = append(udp, pc.LocalAddr())
		}
		for _, l := range listeners {
			tcp = append(tcp, l.Addr())
		}
		ready(udp, tcp)
	}

	var serveErr error
	select {
	case <-stop:
	case serveErr = <-errs:
	}
	for _, c := range closers {
		c.Close()
	}
	closers = nil
	wg.Wait()
	return serveErr
}
