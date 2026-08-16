package serve

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"net"
	"os"
	"testing"

	"github.com/jamesbraid/instigator/internal/config"
)

// realMediaDir holds the real IRIX 6.5.30 CD images. Tests here are
// skipped when it is absent, so CI stays green with no SGI media while a
// developer with the media gets end-to-end coverage over real bytes.
const realMediaDir = "/storage/software/os/irix/Irix 6.5.30_cdimages"

// dist/sa on the Installation Tools disc, per the irix-efs-tools oracle.
const (
	realSAPath = "dist/sa"
	realSADisc = "6.5.30/instalation-tools-and-overlays1"
	realSASHA  = "cf4318a234aa2e3216799927d197f556d548e07200bb08eb9d486630dd0f48d5"
	realSASize = 20067840
)

func realMediaConfig(t *testing.T) *config.Config {
	t.Helper()
	if _, err := os.Stat(realMediaDir); err != nil {
		t.Skip("real IRIX 6.5.30 media not present")
	}
	yaml := `
server_ip: 127.0.0.1
netmask: 127.0.0.0/8
clients: [{name: octane, mac: "08:00:69:0e:af:12", ip: 127.0.0.1}]
media: [{name: "6.5.30", discs: "` + realMediaDir + `"}]
services: {bootp: false, tftp: {port_range: [0,0]}, rsh: true, nfs: true}
`
	c, err := config.Parse([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	c.Ports = config.Ports{} // all kernel-assigned for the unprivileged run
	return c
}

// TestNFSRealMediaRead reads the 20MB dist/sa over the full NFS wire
// (portmap, mount, lookup, paged reads) from a real image and checks it
// against the oracle checksum, proving the NFS stack on real EFS layouts.
func TestNFSRealMediaRead(t *testing.T) {
	cfg := realMediaConfig(t)
	s, err := Start(cfg, testLogger(t), WithRSHHighPorts())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	pm := s.PortmapAddr().(*net.UDPAddr)
	mountPort := getport(t, pm, 100005, 1)
	fh := mountRoot(t, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: mountPort}, "/"+realSADisc)
	nfsPort := getport(t, pm, 100003, 2)
	na := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: nfsPort}

	dist := nfsLookup(t, na, fh, "dist")
	sa := nfsLookup(t, na, dist, "sa")

	h := sha256.New()
	var off uint32
	for {
		data := nfsReadRaw(t, na, sa, off, 8192)
		if len(data) == 0 {
			break
		}
		h.Write(data)
		off += uint32(len(data))
	}
	if int(off) != realSASize {
		t.Fatalf("read %d bytes, want %d", off, realSASize)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != realSASHA {
		t.Fatalf("sha256 %s, oracle %s", got, realSASHA)
	}
	t.Logf("dist/sa: %d bytes over NFS, sha256 matches oracle", off)
}

// nfsReadRaw issues one READ and returns just the data, or nil at EOF.
func nfsReadRaw(t *testing.T, na *net.UDPAddr, fh []byte, off, count uint32) []byte {
	t.Helper()
	data := nfsRead(t, na, fh, off, count)
	return data
}

var _ = binary.BigEndian
