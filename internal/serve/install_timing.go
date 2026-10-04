package serve

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jamesbraid/instigator/internal/capture"
	"github.com/jamesbraid/instigator/internal/config"
	"github.com/jamesbraid/instigator/internal/instcmd"
	"github.com/jamesbraid/instigator/internal/logging"
	"github.com/jamesbraid/instigator/internal/vfs"
)

const installReturnPrefix = "__instigator/attempts/"

func installStartPath(script string) string {
	return "/__instigator/" + script + "/start.cmds"
}

func installReturnPath(attempt string) string {
	return "/" + installReturnPrefix + attempt + "/returned.cmds"
}

func installGoCommands(server, attempt string) string {
	return fmt.Sprintf("go\nadmin source %s:%s\n", server, installReturnPath(attempt))
}

func scriptNames(cfg *config.Config) []string {
	names := make([]string, 0, len(cfg.InstallScripts))
	for _, script := range cfg.InstallScripts {
		names = append(names, script.Name)
	}
	return names
}

// installScripts serves token-bearing command files without changing the
// shared media tree. A token becomes live only after its entire go file was
// written to the client; fetching it again yields a separate attempt.
type installScripts struct {
	server   string
	starts   map[string]string
	logger   *logging.Logger
	rec      *capture.Recorder
	mu       sync.Mutex
	attempts map[string]*installAttempt
}

type installAttempt struct {
	id, script, client, address string
	start                       time.Time
	returned                    bool
}

func newInstallScripts(cfg *config.Config, tree *vfs.Tree, logger *logging.Logger, rec *capture.Recorder) *installScripts {
	s := &installScripts{
		server: cfg.ServerIP.String(), logger: logger, rec: rec,
		starts: make(map[string]string), attempts: make(map[string]*installAttempt),
	}
	for _, name := range append([]string{"install"}, scriptNames(cfg)...) {
		path := fsName(installStartPath(name))
		if _, err := tree.Stat(path); err == nil {
			s.starts[path] = name + ".cmds"
		}
	}
	return s
}

func (s *installScripts) open(name, client, address string) (instcmd.File, bool, error) {
	if s == nil {
		return nil, false, nil
	}
	if script, ok := s.starts[name]; ok {
		var token [16]byte
		rand.Read(token[:])
		attempt := &installAttempt{id: hex.EncodeToString(token[:]), script: script, client: client, address: address}
		return newInstallFile([]byte(installGoCommands(s.server, attempt.id)), func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			attempt.start = time.Now()
			s.attempts[attempt.id] = attempt
			s.rec.InstallStart(attempt.id, attempt.client, attempt.script)
			s.logger.Infof("install_start: %s (%s), attempt %s: go dispatched", attempt.script, attempt.client, attempt.id)
		}), true, nil
	}
	if strings.HasPrefix(name, installReturnPrefix) {
		s.mu.Lock()
		defer s.mu.Unlock()
		attempt := s.findReturn(name, address)
		if attempt == nil {
			return nil, true, instcmd.ErrNotFound
		}
		// A newline makes inst read the file's content while executing no
		// commands. An empty file could be skipped based on its stat alone.
		return newInstallFile([]byte("\n"), func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			if attempt.returned {
				return
			}
			attempt.returned = true
			elapsed := time.Since(attempt.start)
			s.rec.InstallReturned(attempt.id, attempt.client, attempt.script, elapsed.Milliseconds())
			s.logger.Infof("install_returned: %s (%s), attempt %s: go returned after %s (success not verified)", attempt.script, attempt.client, attempt.id, elapsed.Round(time.Millisecond))
		}), true, nil
	}
	return nil, false, nil
}

func (s *installScripts) findReturn(name, address string) *installAttempt {
	id := strings.TrimSuffix(strings.TrimPrefix(name, installReturnPrefix), "/returned.cmds")
	attempt := s.attempts[id]
	if attempt == nil || attempt.address != address || fsName(installReturnPath(id)) != name {
		return nil
	}
	return attempt
}

func (s *installScripts) stat(name, address string) (instcmd.FileInfo, bool, error) {
	if s == nil || !strings.HasPrefix(name, installReturnPrefix) {
		return instcmd.FileInfo{}, false, nil
	}
	if _, ok := s.starts[name]; ok {
		return instcmd.FileInfo{}, false, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.findReturn(name, address) == nil {
		return instcmd.FileInfo{}, true, instcmd.ErrNotFound
	}
	return instcmd.FileInfo{Perm: 0o444, Nlink: 1, Size: 1}, true, nil
}

type installFile struct {
	*bytes.Reader
	once sync.Once
	done func()
}

func newInstallFile(body []byte, done func()) *installFile {
	return &installFile{Reader: bytes.NewReader(body), done: done}
}

func (f *installFile) Transferred(offset, length int64) {
	if offset == 0 && length == f.Size() {
		f.once.Do(f.done)
	}
}
