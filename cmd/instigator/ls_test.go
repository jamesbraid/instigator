package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jamesbraid/instigator/efs/efstest"
)

func testImage(t *testing.T) string {
	t.Helper()
	img := efstest.New()
	fx := img.AddFile(0o755, []byte("fx dummy"))
	stand := img.AddDir(map[string]uint32{"fx.64": fx})
	img.SetRoot(map[string]uint32{"stand": stand})
	p := filepath.Join(t.TempDir(), "disc1.iso")
	if err := os.WriteFile(p, img.CDImage(64, map[string][]byte{"sash": []byte("raw sash")}), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLsRoot(t *testing.T) {
	var sb strings.Builder
	if err := runLs(&sb, testImage(t), "/"); err != nil {
		t.Fatal(err)
	}
	out := sb.String()
	if !strings.Contains(out, "stand") {
		t.Fatalf("root listing missing stand:\n%s", out)
	}
	if !strings.Contains(out, "sash") {
		t.Fatalf("root listing missing volume-header boot file sash:\n%s", out)
	}
}

func TestLsSubdirAndFile(t *testing.T) {
	var sb strings.Builder
	img := testImage(t)
	if err := runLs(&sb, img, "/stand"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sb.String(), "fx.64") {
		t.Fatalf("listing missing fx.64:\n%s", sb.String())
	}
	sb.Reset()
	if err := runLs(&sb, img, "/stand/fx.64"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sb.String(), "8") { // size of "fx dummy"
		t.Fatalf("file stat missing size:\n%s", sb.String())
	}
	if err := runLs(&sb, img, "/absent"); err == nil {
		t.Fatal("runLs succeeded on missing path")
	}
}
