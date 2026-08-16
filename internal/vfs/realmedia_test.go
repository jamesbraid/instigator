package vfs

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"testing"
)

// realMediaImage points at a real IRIX 6.5.30 Installation Tools image.
// The test is skipped unless it exists, so CI (which has no SGI media)
// stays green while a developer with the media gets byte-exact parity
// coverage against the irix-efs-tools Python oracle.
const realMediaImage = "/storage/software/os/irix/Irix 6.5.30_cdimages/Instalation_Tools_and_Overlays1.image"

// oracleChecksums are sha256 of files extracted by irix-efs-tools
// (efs.py read_file) from the image above.
var oracleChecksums = map[string]string{
	"/stand/fx.64":  "05e520e08fa33a7a00232981d85c506e0a0f22045902d3e1e3841c3034829812",
	"/stand/sash64": "7b86941c14cb8679301decc5ebb192b1344510d16f8f7d9f986fea7ec4a17142",
	"/dist/sa":      "cf4318a234aa2e3216799927d197f556d548e07200bb08eb9d486630dd0f48d5",
}

func TestRealMediaParity(t *testing.T) {
	if _, err := os.Stat(realMediaImage); err != nil {
		t.Skip("real IRIX 6.5.30 media not present")
	}
	d, err := OpenImage(realMediaImage)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	for path, want := range oracleChecksums {
		node, err := d.FS().Lookup(path)
		if err != nil {
			t.Errorf("%s: %v", path, err)
			continue
		}
		h := sha256.New()
		buf := make([]byte, 1<<16)
		var off int64
		for {
			n, err := d.FS().ReadAt(node, buf, off)
			h.Write(buf[:n])
			off += int64(n)
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatalf("%s: read at %d: %v", path, off, err)
			}
			if n == 0 {
				break
			}
		}
		if got := hex.EncodeToString(h.Sum(nil)); got != want {
			t.Errorf("%s: sha256 %s, oracle %s", path, got, want)
		} else {
			t.Logf("%s: %d bytes, sha256 matches oracle", path, off)
		}
	}
}
