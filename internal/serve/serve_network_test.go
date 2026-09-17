package serve

import (
	"net"
	"slices"
	"strconv"
	"testing"

	"github.com/jamesbraid/instigator/internal/config"
)

// fakeNetwork records which ports Start binds through the injected network and
// returns real loopback sockets so the services start normally.
type fakeNetwork struct {
	packetPorts []int
	tcpPorts    []int
}

func (f *fakeNetwork) ListenPacket(port int) (net.PacketConn, error) {
	f.packetPorts = append(f.packetPorts, port)
	return net.ListenPacket("udp", "127.0.0.1:0")
}

func (f *fakeNetwork) Listen(port int) (net.Listener, error) {
	f.tcpPorts = append(f.tcpPorts, port)
	return net.Listen("tcp", "127.0.0.1:0")
}

func (f *fakeNetwork) DialStderr(ip net.IP, port int) (net.Conn, error) {
	return net.Dial("tcp", net.JoinHostPort(ip.String(), strconv.Itoa(port)))
}

// TestStartBindsServicesThroughInjectedNetwork proves WithNetwork routes
// bootp, tftp, and rsh onto the injected network instead of the host stack.
func TestStartBindsServicesThroughInjectedNetwork(t *testing.T) {
	cfg := testConfig(t)
	cfg.Ports = config.Ports{BOOTP: 67, TFTP: 69, RSH: 514}
	f := &fakeNetwork{}

	s, err := Start(cfg, testLogger(t), WithNetwork(f), withRSHHighPorts())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	if !slices.Contains(f.packetPorts, 67) || !slices.Contains(f.packetPorts, 69) {
		t.Errorf("packet ports bound through network = %v, want 67 and 69", f.packetPorts)
	}
	if !slices.Contains(f.tcpPorts, 514) {
		t.Errorf("tcp ports bound through network = %v, want 514", f.tcpPorts)
	}
}
