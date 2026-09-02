package capture

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
)

// Binary identifies the running instigator build, so a capture can be
// tied back to exact source. Fields are best-effort: a build without VCS
// stamping or an unreadable executable leaves them empty rather than
// failing capture.
type Binary struct {
	Version     string `json:"version,omitempty"`
	VCSRevision string `json:"vcs_revision,omitempty"`
	VCSDirty    bool   `json:"vcs_dirty"`
	GoVersion   string `json:"go_version"`
	SHA256      string `json:"sha256,omitempty"`
}

// Services records which listeners were enabled for the run.
type Services struct {
	BOOTP         bool   `json:"bootp"`
	TFTP          bool   `json:"tftp"`
	RSH           bool   `json:"rsh"`
	TFTPPortRange [2]int `json:"tftp_port_range"`
}

// ConfigInfo is the non-secret subset of the effective configuration
// stored in run.json: enough to reproduce the serving layout.
type ConfigInfo struct {
	ServerIP string         `json:"server_ip"`
	Netmask  string         `json:"netmask"`
	Ports    map[string]int `json:"ports,omitempty"`
}

// Client is one configured install client, by alias. The bundle also
// carries its MAC and IP: LAN identifiers, not secrets, but the reason the
// capture directory is private (0700).
type Client struct {
	Alias string `json:"alias"`
	MAC   string `json:"mac"`
	IP    string `json:"ip"`
}

// Media records cheap source identity without hashing the entire image.
type Media struct {
	Media string `json:"media"`
	Disc  string `json:"disc"`
	Image string `json:"image"`
	Size  int64  `json:"size"`
	Mtime string `json:"mtime"`
}

// Provenance is the run.json payload. serve assembles it (it alone knows
// config and the media tree) and hands it here as plain primitives; the
// recorder never imports config or vfs. Schema and RunID are filled by
// WriteRun.
type Provenance struct {
	Schema   int        `json:"schema"`
	RunID    string     `json:"run_id"`
	Start    string     `json:"start"`
	End      string     `json:"end,omitempty"`
	Binary   Binary     `json:"binary"`
	Services Services   `json:"services"`
	Config   ConfigInfo `json:"config"`
	Clients  []Client   `json:"clients"`
	Media    []Media    `json:"media"`
}

// BuildInfo reports the running binary's identity from the Go build stamp
// plus a SHA-256 of the executable file. Everything is best-effort so it
// never blocks capture.
func BuildInfo() Binary {
	b := Binary{GoVersion: runtime.Version()}
	if info, ok := debug.ReadBuildInfo(); ok {
		b.Version = info.Main.Version
		for _, s := range info.Settings {
			switch s.Key {
			case "vcs.revision":
				b.VCSRevision = s.Value
			case "vcs.modified":
				b.VCSDirty = s.Value == "true"
			}
		}
	}
	if exe, err := os.Executable(); err == nil {
		if sum, err := hashFile(exe); err == nil {
			b.SHA256 = sum
		}
	}
	return b
}

// WriteRun marshals the provenance to run.json (0600). It stamps the
// schema version and run id so the caller need not. The stored copy lets a
// clean shutdown rewrite run.json with the end time.
func (r *Recorder) WriteRun(p Provenance) error {
	if r == nil {
		return nil
	}
	p.Schema = schemaVersion
	p.RunID = r.runID
	r.mu.Lock()
	r.prov = p
	r.mu.Unlock()
	return r.writeRun(p)
}

func (r *Recorder) writeRun(p Provenance) error {
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	// Serialized so a startup WriteRun and a shutdown rewrite can never
	// interleave on the file.
	r.mu.Lock()
	defer r.mu.Unlock()
	return os.WriteFile(filepath.Join(r.dir, "run.json"), b, 0o600)
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
