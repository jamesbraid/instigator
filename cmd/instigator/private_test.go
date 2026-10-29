package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/jamesbraid/instigator/efs/efstest"
	"github.com/jamesbraid/instigator/internal/qemunet"
	"github.com/jamesbraid/instigator/internal/sharefs"
)

func TestParseShareSpecs(t *testing.T) {
	got, err := parseShareSpecs([]string{"src=/home/me/src", "docs=rel/dir", "eq=/a=b"})
	if err != nil {
		t.Fatal(err)
	}
	want := []shareSpec{{"src", "/home/me/src"}, {"docs", "rel/dir"}, {"eq", "/a=b"}}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("got %v, want %v", got, want)
	}
	if got, err := parseShareSpecs(nil); err != nil || len(got) != 0 {
		t.Errorf("no options = %v, %v", got, err)
	}
	for _, bad := range []string{"", "src", "=dir", "src=", "a/b=dir", `a\b=dir`} {
		if _, err := parseShareSpecs([]string{bad}); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if _, err := parseShareSpecs([]string{"a=x", "a=y"}); err == nil || !strings.Contains(err.Error(), "already used") {
		t.Errorf("duplicate name: %v", err)
	}
}

// shareRoot runs the root command with args and returns its error. Every
// case here fails before anything is served.
func shareRoot(t *testing.T, args ...string) error {
	t.Helper()
	root := newRoot()
	root.Writer, root.ErrWriter = new(bytes.Buffer), new(bytes.Buffer)
	return root.Run(t.Context(), append([]string{"instigator"}, args...))
}

func TestShareCommandRejectsBadOptions(t *testing.T) {
	d := t.TempDir()
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"nothing", []string{"share"}, "nothing to share"},
		{"name without dir", []string{"share", "src"}, "both <name> and <dir>"},
		{"bad spec", []string{"share", "--share", "src"}, "NAME=DIR"},
		{"duplicate", []string{"share", "src", d, "--share", "src=" + d}, "already used"},
		{"both endpoints", []string{"share", "src", d, "--network-socket", "/x", "--network-tcp", "127.0.0.1:1"}, "cannot be used together"},
		{"host port on private", []string{"share", "src", d, "--network-tcp", "127.0.0.1:1", "--port", "2049"}, "--port does not apply"},
		{"host listen on private", []string{"share", "src", d, "--network-tcp", "127.0.0.1:1", "--listen", "10.0.0.1"}, "--listen does not apply"},
		{"udp off on private", []string{"share", "src", d, "--network-tcp", "127.0.0.1:1", "--udp=false"}, "--udp does not apply"},
		{"address on host", []string{"share", "src", d, "--address", "10.1.0.2/24"}, "--address needs"},
		{"address not ipv4", []string{"share", "src", d, "--network-tcp", "127.0.0.1:1", "--address", "fe80::1/64"}, "IPv4"},
		{"serve share without network", []string{"serve", "--share", "a=" + d, "config.yaml"}, "--share needs --network"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := shareRoot(t, tc.args...)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestServeShareNeedsAPrivateNetwork(t *testing.T) {
	err := runUntilSignal("unused.yaml", false, "", "", "", []string{"a=" + t.TempDir()}, new(bytes.Buffer), nil)
	if err == nil || !strings.Contains(err.Error(), "--share needs --network") {
		t.Fatalf("error = %v", err)
	}
	err = runUntilSignal("unused.yaml", false, "", "", "", []string{"nodir"}, new(bytes.Buffer), nil)
	if err == nil || !strings.Contains(err.Error(), "NAME=DIR") {
		t.Fatalf("error = %v", err)
	}
}

// guest is a stand-in for the QEMU machine: a second stack on the other end
// of the stream.
type guest struct {
	t   *testing.T
	net *qemunet.Network
	pc  net.PacketConn
	xid uint32
}

func attachGuest(t *testing.T, endpoint string) *guest {
	t.Helper()
	conn := dialWhenReady(t, "tcp", endpoint, time.Now().Add(5*time.Second))
	n, err := qemunet.New(conn, qemunet.Config{
		ServerIP:  netip.AddrFrom4([4]byte{10, 98, 0, 65}),
		PrefixLen: 24,
		MAC:       net.HardwareAddr{0x08, 0x00, 0x69, 0x12, 0x34, 0x56},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { n.Close() })
	pc, err := n.ListenPacket(0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	return &guest{t: t, net: n, pc: pc}
}

// udp sends one call to the server and returns the reply body.
func (g *guest) udp(port int, prog, vers, proc uint32, args []byte) []byte {
	g.t.Helper()
	g.xid++
	dst := &net.UDPAddr{IP: net.IPv4(10, 98, 0, 2), Port: port}
	buf := make([]byte, 2048)
	// The first datagram may wait on ARP, which the stack answers by
	// dropping nothing; retry once as a real client does.
	for range 5 {
		if _, err := g.pc.WriteTo(rpc(g.xid, prog, vers, proc, args), dst); err != nil {
			g.t.Fatal(err)
		}
		g.pc.SetReadDeadline(time.Now().Add(time.Second))
		n, from, err := g.pc.ReadFrom(buf)
		if err != nil {
			continue
		}
		if !from.(*net.UDPAddr).IP.Equal(dst.IP) || from.(*net.UDPAddr).Port != port {
			g.t.Fatalf("reply from %v, want %v", from, dst)
		}
		return acceptedBody(g.t, buf[:n], g.xid)
	}
	g.t.Fatalf("no UDP reply from port %d for %d/%d/%d", port, prog, vers, proc)
	return nil
}

func acceptedBody(t *testing.T, reply []byte, xid uint32) []byte {
	t.Helper()
	if len(reply) < 24 || binary.BigEndian.Uint32(reply) != xid || binary.BigEndian.Uint32(reply[20:]) != 0 {
		t.Fatalf("not an accepted reply to %d: %x", xid, reply)
	}
	return reply[24:]
}

func (g *guest) dialTCP(port int) net.Conn {
	g.t.Helper()
	c, err := g.net.DialStderr(net.IPv4(10, 98, 0, 2), port)
	if err != nil {
		g.t.Fatal(err)
	}
	g.t.Cleanup(func() { c.Close() })
	return c
}

func getport(prog, vers, proto uint32) []byte {
	var a []byte
	for _, v := range []uint32{prog, vers, proto, 0} {
		a = binary.BigEndian.AppendUint32(a, v)
	}
	return a
}

// checkPrivateShares sends portmap GETPORT and the NULL procedures of MOUNT
// and NFS over UDP and TCP, with nothing mounted, then mounts every name.
func checkPrivateShares(t *testing.T, g *guest, names []string) {
	t.Helper()
	const (
		pmap, mountd, nfsd = 100000, 100005, 100003
		udpProto, tcpProto = 17, 6
	)
	c111, c635, c2049 := g.dialTCP(111), g.dialTCP(635), g.dialTCP(2049)

	for _, tc := range []struct {
		prog, vers, want uint32
	}{{mountd, 1, 635}, {mountd, 3, 635}, {nfsd, 2, 2049}, {nfsd, 3, 2049}, {pmap, 2, 111}} {
		for _, proto := range []uint32{udpProto, tcpProto} {
			var body []byte
			if proto == udpProto {
				body = g.udp(111, pmap, 2, 3, getport(tc.prog, tc.vers, proto))
			} else {
				body = tcpCall(t, c111, rpc(10, pmap, 2, 3, getport(tc.prog, tc.vers, proto)))
			}
			if got := binary.BigEndian.Uint32(body); got != tc.want {
				t.Errorf("GETPORT %d v%d proto %d = %d, want %d", tc.prog, tc.vers, proto, got, tc.want)
			}
		}
	}
	for _, vers := range []uint32{2, 3} {
		if body := g.udp(2049, nfsd, vers, 0, nil); len(body) != 0 {
			t.Errorf("NFS v%d NULL over UDP body %x", vers, body)
		}
		if body := tcpCall(t, c2049, rpc(11, nfsd, vers, 0, nil)); len(body) != 0 {
			t.Errorf("NFS v%d NULL over TCP body %x", vers, body)
		}
	}
	for _, vers := range []uint32{1, 3} {
		if body := g.udp(635, mountd, vers, 0, nil); len(body) != 0 {
			t.Errorf("MOUNT v%d NULL over UDP body %x", vers, body)
		}
		if body := tcpCall(t, c635, rpc(12, mountd, vers, 0, nil)); len(body) != 0 {
			t.Errorf("MOUNT v%d NULL over TCP body %x", vers, body)
		}
	}

	handles := map[string][]byte{}
	for _, name := range names {
		body := tcpCall(t, c635, rpc(20, mountd, 3, 1, xdrString("/"+name)))
		if binary.BigEndian.Uint32(body) != 0 || len(body) < 40 {
			t.Fatalf("MNT /%s: %x", name, body)
		}
		handles[name] = body[8:40]
	}
	if len(names) > 1 && bytes.Equal(handles[names[0]], handles[names[1]]) {
		t.Errorf("shares %s and %s have the same root handle", names[0], names[1])
	}
	if body := tcpCall(t, c635, rpc(21, mountd, 3, 1, xdrString("/nothere"))); binary.BigEndian.Uint32(body) == 0 {
		t.Error("MNT of a name nobody shares succeeded")
	}
	exp := g.udp(635, mountd, 3, 5, nil)
	for _, name := range names {
		if !bytes.Contains(exp, []byte("/"+name)) {
			t.Errorf("EXPORT omits /%s: %q", name, exp)
		}
	}
}

func TestSharePrivateNetworkServesPortmapMountAndNFS(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	endpoint := loopbackEndpoint(t)
	stop := make(chan os.Signal, 1)
	done := make(chan error, 1)
	var output lockedBuffer
	go func() {
		done <- servePrivateShare(shareOptions{
			shares:  []shareSpec{{"alpha", a}, {"beta", b}},
			network: privateEndpoint{tcp: endpoint},
			address: netip.MustParsePrefix("10.98.0.2/24"),
		}, &output, stop, nil)
	}()
	g := attachGuest(t, endpoint)
	checkPrivateShares(t, g, []string{"alpha", "beta"})

	// A write through one share lands in its directory only.
	c := g.dialTCP(635)
	body := tcpCall(t, c, rpc(30, 100005, 3, 1, xdrString("/beta")))
	root := body[8:40]
	args := binary.BigEndian.AppendUint32(nil, 32)
	args = append(args, root...)
	args = append(args, xdrString("made")...)
	args = binary.BigEndian.AppendUint32(args, 1) // GUARDED
	for range 6 {
		args = binary.BigEndian.AppendUint32(args, 0)
	}
	if st := binary.BigEndian.Uint32(tcpCall(t, c, rpc(31, 100003, 3, 8, args))); st != 0 {
		t.Fatalf("CREATE status %d", st)
	}
	if _, err := os.Stat(filepath.Join(b, "made")); err != nil {
		t.Errorf("file not in beta: %v", err)
	}
	if _, err := os.Stat(filepath.Join(a, "made")); err == nil {
		t.Error("file landed in alpha")
	}

	// The machine going away ends the command.
	g.net.Close()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "private network disconnected") {
			t.Fatalf("share returned %v; log: %s", err, output.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("share did not stop when the machine disconnected")
	}
}

func TestSharePrivateNetworkStopsBeforeTheMachineConnects(t *testing.T) {
	stop := make(chan os.Signal, 1)
	done := make(chan error, 1)
	go func() {
		done <- servePrivateShare(shareOptions{
			shares:  []shareSpec{{"s", t.TempDir()}},
			network: privateEndpoint{tcp: loopbackEndpoint(t)},
			address: netip.MustParsePrefix("10.98.0.2/24"),
		}, new(bytes.Buffer), stop, nil)
	}()
	stop <- os.Interrupt
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("share did not stop")
	}
}

func TestServeSharesOnThePrivateNetwork(t *testing.T) {
	dir := t.TempDir()
	imagePath := filepath.Join(dir, "dist.image")
	image := efstest.New()
	sa := image.AddFile(0o444, []byte("sa"))
	image.SetRoot(map[string]uint32{"dist": image.AddDir(map[string]uint32{"sa": sa})})
	if err := os.WriteFile(imagePath, image.CDImage(64, nil), 0o644); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(dir, "instigator.yaml")
	yaml := fmt.Sprintf(`
server_ip: 10.98.0.2
netmask: 10.98.0.0/24
clients:
  - {name: guest, mac: "08:00:69:12:34:56", ip: 10.98.0.65}
install_sets:
  - name: "6.5.30"
    layers:
      - {name: base, source: %q}
services:
  bootp: true
  tftp: {enabled: false}
  rsh: false
`, imagePath)
	if err := os.WriteFile(configPath, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	src, out := filepath.Join(dir, "src"), filepath.Join(dir, "out")
	for _, d := range []string{src, out} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	endpoint := loopbackEndpoint(t)
	stop := make(chan os.Signal, 1)
	done := make(chan error, 1)
	var output lockedBuffer
	go func() {
		done <- runUntilSignal(configPath, false, "", "", endpoint, []string{"src=" + src, "out=" + out}, &output, stop)
	}()
	g := attachGuest(t, endpoint)
	checkPrivateShares(t, g, []string{"src", "out"})

	// The install path still answers beside the shares.
	req := make([]byte, 300)
	req[0], req[1], req[2] = 1, 1, 6
	copy(req[28:34], []byte{0x08, 0x00, 0x69, 0x12, 0x34, 0x56})
	client, err := g.net.ListenPacket(68)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	reply := make([]byte, 512)
	ok := false
	for i := 0; i < 20 && !ok; i++ {
		client.WriteTo(req, &net.UDPAddr{IP: net.IPv4bcast, Port: 67})
		client.SetReadDeadline(time.Now().Add(250 * time.Millisecond))
		_, _, err := client.ReadFrom(reply)
		ok = err == nil
	}
	if !ok || reply[0] != 2 || net.IP(reply[16:20]).String() != "10.98.0.65" {
		t.Fatalf("no BOOTREPLY beside the shares; log: %s", output.String())
	}

	stop <- os.Interrupt
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serve: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not stop")
	}
}

func TestServeWithoutSharesLeavesNFSOff(t *testing.T) {
	dir := t.TempDir()
	imagePath := filepath.Join(dir, "dist.image")
	image := efstest.New()
	sa := image.AddFile(0o444, []byte("sa"))
	image.SetRoot(map[string]uint32{"dist": image.AddDir(map[string]uint32{"sa": sa})})
	if err := os.WriteFile(imagePath, image.CDImage(64, nil), 0o644); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(dir, "instigator.yaml")
	yaml := fmt.Sprintf("server_ip: 10.98.0.2\nnetmask: 10.98.0.0/24\nclients:\n  - {name: guest, mac: \"08:00:69:12:34:56\", ip: 10.98.0.65}\ninstall_sets:\n  - name: \"6.5.30\"\n    layers:\n      - {name: base, source: %q}\nservices:\n  bootp: true\n  tftp: {enabled: false}\n  rsh: false\n", imagePath)
	if err := os.WriteFile(configPath, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	endpoint := loopbackEndpoint(t)
	stop := make(chan os.Signal, 1)
	done := make(chan error, 1)
	go func() {
		done <- runUntilSignal(configPath, false, "", "", endpoint, nil, new(bytes.Buffer), stop)
	}()
	g := attachGuest(t, endpoint)
	// Wait for serving to start by getting a BOOTREPLY, then probe portmap.
	req := make([]byte, 300)
	req[0], req[1], req[2] = 1, 1, 6
	copy(req[28:34], []byte{0x08, 0x00, 0x69, 0x12, 0x34, 0x56})
	client, err := g.net.ListenPacket(68)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	reply := make([]byte, 512)
	up := false
	for i := 0; i < 20 && !up; i++ {
		client.WriteTo(req, &net.UDPAddr{IP: net.IPv4bcast, Port: 67})
		client.SetReadDeadline(time.Now().Add(250 * time.Millisecond))
		_, _, err := client.ReadFrom(reply)
		up = err == nil
	}
	if !up {
		t.Fatal("serve never answered")
	}
	if c, err := g.net.DialStderr(net.IPv4(10, 98, 0, 2), 111); err == nil {
		c.Close()
		t.Error("portmap answered on TCP 111 without --share")
	}
	stop <- os.Interrupt
	<-done
}

func TestOpenSharesOneOrMany(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	fsys, closer, desc, err := openShares([]shareSpec{{"a", a}}, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := fsys.(*sharefs.FS); !ok {
		t.Errorf("one share is a %T", fsys)
	}
	closer.Close()
	if !strings.Contains(desc, "as /a") {
		t.Errorf("description %q", desc)
	}
	fsys, closer, _, err = openShares([]shareSpec{{"a", a}, {"b", b}}, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := fsys.(*sharefs.Multi); !ok {
		t.Errorf("two shares are a %T", fsys)
	}
	closer.Close()
	if _, _, _, err := openShares([]shareSpec{{"a", a}, {"b", b}}, filepath.Join(a, "t")); err == nil {
		t.Error("one handle table for two shares accepted")
	}
	if _, _, _, err := openShares([]shareSpec{{"a", a}, {"b", filepath.Join(a, "missing")}}, ""); err == nil {
		t.Error("missing directory accepted")
	}
	if _, _, _, err := openShares(nil, ""); err == nil {
		t.Error("no shares accepted")
	}
}

func TestParseClients(t *testing.T) {
	srv := netip.MustParsePrefix("10.98.0.2/24")
	ok, err := parseClients([]string{"08:00:69:ff:da:f7"}, srv)
	if err != nil || len(ok) != 1 || ok[0].IP != netip.MustParseAddr("10.98.0.65") || ok[0].Hostname != "" {
		t.Fatalf("lone MAC: %+v, %v", ok, err)
	}
	ok, err = parseClients([]string{"08:00:69:ff:da:f7=,origin200"}, srv)
	if err != nil || ok[0].IP != netip.MustParseAddr("10.98.0.65") || ok[0].Hostname != "origin200" {
		t.Fatalf("MAC=,HOST: %+v, %v", ok, err)
	}
	ok, err = parseClients([]string{"08:00:69:00:00:01=10.98.0.70,a", "08:00:69:00:00:02=10.98.0.71"}, srv)
	if err != nil || len(ok) != 2 || ok[0].IP.String() != "10.98.0.70" || ok[1].Hostname != "" {
		t.Fatalf("two clients: %+v, %v", ok, err)
	}
	for _, bad := range [][]string{
		{"nonsense"},
		{"08:00:69:00:00:01=notip"},
		{"08:00:69:00:00:01=10.98.0.2"},                       // the server's own
		{"08:00:69:00:00:01=192.0.2.5"},                       // off the segment
		{"08:00:69:00:00:01=10.98.0.70,bad_name"},             // host name
		{"08:00:69:00:00:01", "08:00:69:00:00:02=10.98.0.71"}, // no IP among several
		{"08:00:69:00:00:01=10.98.0.70", "08:00:69:00:00:01=10.98.0.71"},
	} {
		if _, err := parseClients(bad, srv); err == nil {
			t.Errorf("%v accepted", bad)
		}
	}
	if err := shareRoot(t, "share", "src", t.TempDir(), "--client", "08:00:69:00:00:01"); err == nil || !strings.Contains(err.Error(), "--client needs") {
		t.Errorf("--client on a host network: %v", err)
	}
}

func TestSharePrivateNetworkAnswersDHCPForItsClient(t *testing.T) {
	endpoint := loopbackEndpoint(t)
	stop := make(chan os.Signal, 1)
	done := make(chan error, 1)
	var output lockedBuffer
	clients, err := parseClients([]string{"08:00:69:12:34:56=,origin200"}, netip.MustParsePrefix("10.98.0.2/24"))
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		done <- servePrivateShare(shareOptions{
			shares:  []shareSpec{{"s", t.TempDir()}},
			network: privateEndpoint{tcp: endpoint},
			address: netip.MustParsePrefix("10.98.0.2/24"),
			clients: clients,
		}, &output, stop, nil)
	}()
	g := attachGuest(t, endpoint)
	client, err := g.net.ListenPacket(68)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	req := make([]byte, 548)
	req[0], req[1], req[2] = 1, 1, 6
	copy(req[28:], []byte{0x08, 0x00, 0x69, 0x12, 0x34, 0x56})
	copy(req[236:], []byte{99, 130, 83, 99, 53, 1, 1, 255})
	reply := make([]byte, 600)
	var n int
	for range 20 {
		if _, err := client.WriteTo(req, &net.UDPAddr{IP: net.IPv4bcast, Port: 67}); err != nil {
			t.Fatal(err)
		}
		client.SetReadDeadline(time.Now().Add(250 * time.Millisecond))
		if n, _, err = client.ReadFrom(reply); err == nil {
			break
		}
	}
	if err != nil {
		t.Fatalf("no OFFER: %v; log: %s", err, output.String())
	}
	if got := net.IP(reply[16:20]).String(); got != "10.98.0.65" {
		t.Errorf("yiaddr %s", got)
	}
	if !bytes.Contains(reply[:n], append([]byte{12, 9}, "origin200"...)) {
		t.Errorf("OFFER lacks the host name: %x", reply[240:n])
	}
	// End it the way the other private-network tests do, by the machine
	// going away.
	g.net.Close()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "private network disconnected") {
			t.Fatalf("share returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("share did not stop when the machine disconnected")
	}
}

// Stopping the command while a machine is attached must finish promptly,
// with or without a BOOTP/DHCP client configured.
func TestSharePrivateNetworkStopsWhileConnected(t *testing.T) {
	for _, withClient := range []bool{false, true} {
		t.Run(fmt.Sprint("client=", withClient), func(t *testing.T) {
			endpoint := loopbackEndpoint(t)
			stop := make(chan os.Signal, 1)
			done := make(chan error, 1)
			o := shareOptions{
				shares:  []shareSpec{{"s", t.TempDir()}},
				network: privateEndpoint{tcp: endpoint},
				address: netip.MustParsePrefix("10.98.0.2/24"),
			}
			if withClient {
				var err error
				if o.clients, err = parseClients([]string{"08:00:69:12:34:56"}, o.address); err != nil {
					t.Fatal(err)
				}
			}
			go func() { done <- servePrivateShare(o, new(bytes.Buffer), stop, nil) }()
			g := attachGuest(t, endpoint)
			checkPrivateShares(t, g, []string{"s"})
			stop <- os.Interrupt
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(10 * time.Second):
				buf := make([]byte, 1<<20)
				t.Fatalf("share did not stop with a machine attached:\n%s", buf[:runtime.Stack(buf, true)])
			}
		})
	}
}

// Failures beyond the first, from any number of listeners, must not block:
// Close waits for every listener to return.
func TestPrivateSharesReportNeverBlocks(t *testing.T) {
	p := &privateShares{errs: make(chan error, 1)}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 50 {
			p.report(errors.New("listener failed"))
		}
		p.report(nil)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("report blocked")
	}
	if err := <-p.Err(); err == nil {
		t.Fatal("first failure lost")
	}
}
