package capture

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestBuildInfoSelfHashMatchesExecutable(t *testing.T) {
	bi := BuildInfo()
	if bi.GoVersion == "" {
		t.Error("BuildInfo GoVersion empty")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Skipf("os.Executable unavailable: %v", err)
	}
	f, err := os.Open(exe)
	if err != nil {
		t.Skipf("open executable: %v", err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		t.Fatalf("hash executable: %v", err)
	}
	want := hex.EncodeToString(h.Sum(nil))
	if bi.SHA256 != want {
		t.Errorf("BuildInfo SHA256 = %s, want %s (self-hash of %s)", bi.SHA256, want, exe)
	}
}

func TestWriteRunWritesProvenance(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cap")
	r, err := New(dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer r.Close()

	p := Provenance{
		Start:    "2026-08-16T00:00:00Z",
		Binary:   BuildInfo(),
		Services: Services{BOOTP: true, TFTP: true, RSH: true, TFTPPortRange: [2]int{2048, 32767}},
		Config:   ConfigInfo{ServerIP: "192.0.2.1", Netmask: "255.255.255.0"},
		Clients:  []Client{{Alias: "octane", MAC: "08:00:69:00:00:01", IP: "192.0.2.10"}},
		Media:    []Media{{Media: "irix6.5.30", Disc: "foundation1", Image: "foundation1.iso", Size: 654321, Mtime: "2026-08-01T00:00:00Z"}},
	}
	if err := r.WriteRun(p); err != nil {
		t.Fatalf("WriteRun: %v", err)
	}

	fi, err := os.Stat(filepath.Join(dir, "run.json"))
	if err != nil {
		t.Fatalf("stat run.json: %v", err)
	}
	if perm := fi.Mode().Perm(); runtime.GOOS != "windows" && perm != 0o600 {
		t.Errorf("run.json perm = %o, want 600", perm)
	}

	b, err := os.ReadFile(filepath.Join(dir, "run.json"))
	if err != nil {
		t.Fatalf("read run.json: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("run.json not valid JSON: %v", err)
	}
	if m["schema"] != float64(schemaVersion) {
		t.Errorf("schema = %v, want %d", m["schema"], schemaVersion)
	}
	if id, _ := m["run_id"].(string); id == "" {
		t.Errorf("run_id = %v, want the recorder's run id", m["run_id"])
	}

	clients, ok := m["clients"].([]any)
	if !ok || len(clients) != 1 {
		t.Fatalf("clients = %v, want 1 entry", m["clients"])
	}
	c0 := clients[0].(map[string]any)
	if c0["alias"] != "octane" {
		t.Errorf("client alias = %v, want octane", c0["alias"])
	}

	media, ok := m["media"].([]any)
	if !ok || len(media) != 1 {
		t.Fatalf("media = %v, want 1 entry", m["media"])
	}
	md0 := media[0].(map[string]any)
	if md0["size"] != float64(654321) {
		t.Errorf("media size = %v, want 654321", md0["size"])
	}
	if _, ok := md0["mtime"]; !ok {
		t.Error("media entry missing mtime")
	}
	// The roadmap forbids hashing multi-GB ISOs at startup: no digest key.
	for _, k := range []string{"hash", "sha256", "digest"} {
		if _, ok := md0[k]; ok {
			t.Errorf("media entry unexpectedly carries %q (no startup media hashing)", k)
		}
	}

	svc, ok := m["services"].(map[string]any)
	if !ok {
		t.Fatalf("services = %v", m["services"])
	}
	if svc["bootp"] != true || svc["rsh"] != true {
		t.Errorf("services = %v, want bootp and rsh true", svc)
	}
}
