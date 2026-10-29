package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"

	sdnotify "github.com/coreos/go-systemd/v22/daemon"

	"github.com/urfave/cli/v3"

	"github.com/jamesbraid/instigator/internal/bootp"
	"github.com/jamesbraid/instigator/internal/config"
	"github.com/jamesbraid/instigator/internal/logging"
	"github.com/jamesbraid/instigator/internal/qemunet"
	"github.com/jamesbraid/instigator/nfs"
)

// shareOptions is the share command's flags.
type shareOptions struct {
	shares      []shareSpec
	listen      []string
	port        int // NFS and MOUNT
	portmapPort int // 0: no portmap listener
	udp, tcp    bool
	handleTable string
	verbose     bool

	// network, when set, serves the shares on a private network instead of
	// host sockets: a machine connects to the endpoint and sees the server
	// at address, on the fixed ports. listen, port, portmapPort, udp and tcp
	// do not apply there.
	network privateEndpoint
	address netip.Prefix

	// clients are the machines the private network answers BOOTP and DHCP
	// for, as serve does for its configuration's clients. Empty answers
	// neither.
	clients []bootp.Client
}

// runShare serves a share until a signal stops it.
func runShare(o shareOptions, output io.Writer) error {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sig)
	return serveShare(o, output, sig, nil)
}

// serveShare exports o.shares until stop fires. ready, when set, is told
// the bound UDP and TCP addresses once every listener is up.
func serveShare(o shareOptions, output io.Writer, stop <-chan os.Signal, ready func(udp, tcp []net.Addr)) error {
	if o.network.enabled() {
		return servePrivateShare(o, output, stop, nil)
	}
	if !o.udp && !o.tcp {
		return errors.New("share: --udp and --tcp are both off")
	}
	fsys, closer, desc, err := openShares(o.shares, o.handleTable)
	if err != nil {
		return err
	}
	defer closer.Close()

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
	logger.Infof("share: %s on %s, NFS and MOUNT on port %d over %s, %s",
		desc, strings.Join(o.listen, ", "), nfsPort, strings.Join(transports, " and "), portmap)
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

// servePrivateShare serves o.shares on the private network a machine
// connects to at o.network, until stop fires or the machine disconnects.
// connected, when set, is told the network once the machine is attached
// and the server is listening.
func servePrivateShare(o shareOptions, output io.Writer, stop <-chan os.Signal, connected func(*qemunet.Network)) error {
	if err := o.network.check(); err != nil {
		return err
	}
	fsys, closer, desc, err := openShares(o.shares, o.handleTable)
	if err != nil {
		return err
	}
	defer closer.Close()
	level := logging.LevelInfo
	if o.verbose {
		level = logging.LevelDebug
	}
	logger := logging.New(output, level)

	cfg := qemunet.Config{ServerIP: o.address.Addr(), PrefixLen: o.address.Bits(), MAC: qemunet.DefaultServerMAC}
	l, err := o.network.listen(cfg)
	if err != nil {
		return err
	}
	defer l.Close()
	logger.Infof("private network %s: waiting for the machine to connect", l.Addr())
	if _, err := sdnotify.SdNotify(false, sdnotify.SdNotifyReady); err != nil {
		return fmt.Errorf("reporting readiness: %w", err)
	}
	n, err := acceptNetwork(l, stop)
	if err != nil || n == nil {
		if err == nil {
			logger.Infof("shutting down before the machine connected")
		}
		return err
	}
	defer n.Close()

	p, err := serveSharesOn(n, fsys, logger)
	if err != nil {
		return err
	}
	defer p.Close()
	if len(o.clients) > 0 {
		pc, err := n.ListenPacket(67)
		if err != nil {
			return fmt.Errorf("share: bootp: %w", err)
		}
		defer pc.Close()
		bs := &bootp.Server{ServerIP: o.address.Addr(), Netmask: o.address.Masked(), Clients: o.clients, DHCP: true, Logger: logger}
		go func() {
			// Closed with the network at shutdown; any other end is logged,
			// not fatal to the shares.
			if err := bs.Serve(pc); err != nil && !errors.Is(err, net.ErrClosed) {
				logger.Errorf("share: bootp: %v", err)
			}
		}()
		logger.Infof("share: answering BOOTP and DHCP for %d client(s)", len(o.clients))
	}
	logger.Infof("share: %s on %s over the private network, portmap, MOUNT and NFS on ports %d, %d and %d over udp and tcp",
		desc, o.address.Addr(), privatePortmapPort, privateMountPort, privateNFSPort)
	if connected != nil {
		connected(n)
	}
	select {
	case <-stop:
		logger.Infof("shutting down")
		return nil
	case err := <-p.Err():
		return err
	case <-n.Done():
		return fmt.Errorf("private network disconnected: %w", n.Err())
	}
}

// shareOptionsFromCommand reads the share command's arguments and flags.
func shareOptionsFromCommand(cmd *cli.Command) (shareOptions, error) {
	args := cmd.StringArgs("name")
	dirs := cmd.StringArgs("dir")
	if len(args) != len(dirs) {
		return shareOptions{}, errors.New("share: give both <name> and <dir>, or use --share NAME=DIR")
	}
	var flagged []string
	for i := range args {
		flagged = append(flagged, args[i]+"="+dirs[i])
	}
	specs, err := parseShareSpecs(append(flagged, cmd.StringSlice("share")...))
	if err != nil {
		return shareOptions{}, err
	}
	if len(specs) == 0 {
		return shareOptions{}, errors.New("share: nothing to share: give <name> <dir> or --share NAME=DIR")
	}
	o := shareOptions{
		shares: specs,
		listen: cmd.StringSlice("listen"), port: int(cmd.Int("port")), portmapPort: int(cmd.Int("portmap-port")),
		udp: cmd.Bool("udp"), tcp: cmd.Bool("tcp"),
		handleTable: cmd.String("handle-table"), verbose: cmd.Bool("verbose"),
		network: privateEndpoint{socket: cmd.String("network-socket"), tcp: cmd.String("network-tcp")},
	}
	if o.network.enabled() {
		for _, f := range []string{"listen", "port", "portmap-port", "udp", "tcp"} {
			if cmd.IsSet(f) {
				return shareOptions{}, fmt.Errorf("share: --%s does not apply on a private network, which serves UDP and TCP on 111, 635 and 2049", f)
			}
		}
		if o.address, err = netip.ParsePrefix(cmd.String("address")); err != nil || !o.address.Addr().Is4() {
			return shareOptions{}, fmt.Errorf("share: --address %q is not an IPv4 ADDR/PREFIX", cmd.String("address"))
		}
		if o.clients, err = parseClients(cmd.StringSlice("client"), o.address); err != nil {
			return shareOptions{}, err
		}
	} else if cmd.IsSet("address") {
		return shareOptions{}, errors.New("share: --address needs --network-socket or --network-tcp")
	} else if cmd.IsSet("client") {
		return shareOptions{}, errors.New("share: --client needs --network-socket or --network-tcp")
	}
	return o, nil
}

// defaultClientIP is the machine address Origami's configuration uses, and
// the one a lone --client gets when it names none.
var defaultClientIP = netip.MustParseAddr("10.98.0.65")

// parseClients reads --client MAC=IP[,HOSTNAME] options for a private
// network at server. The IP may be left out (MAC, or MAC=,HOSTNAME) when
// there is one client; it is then 10.98.0.65.
func parseClients(args []string, server netip.Prefix) ([]bootp.Client, error) {
	var out []bootp.Client
	for _, a := range args {
		macStr, rest, _ := strings.Cut(a, "=")
		ipStr, host, _ := strings.Cut(rest, ",")
		mac, err := net.ParseMAC(macStr)
		if err != nil || len(mac) != 6 {
			return nil, fmt.Errorf("share: --client %q: want MAC=IP[,HOSTNAME] with an Ethernet MAC", a)
		}
		ip := defaultClientIP
		if ipStr != "" {
			if ip, err = netip.ParseAddr(ipStr); err != nil || !ip.Is4() {
				return nil, fmt.Errorf("share: --client %q: %q is not an IPv4 address", a, ipStr)
			}
		} else if len(args) > 1 {
			return nil, fmt.Errorf("share: --client %q: with several clients each needs an IP", a)
		}
		if !server.Contains(ip) || ip == server.Addr() {
			return nil, fmt.Errorf("share: --client %q: %s is not an address on %s other than the server's", a, ip, server.Masked())
		}
		if err := config.CheckHostname(host); err != nil {
			return nil, fmt.Errorf("share: --client %q: %v", a, err)
		}
		for _, c := range out {
			if bytes.Equal(c.MAC, mac) || c.IP == ip {
				return nil, fmt.Errorf("share: --client %q repeats a MAC or an IP", a)
			}
		}
		out = append(out, bootp.Client{Name: macStr, MAC: mac, IP: ip, Hostname: host})
	}
	return out, nil
}
