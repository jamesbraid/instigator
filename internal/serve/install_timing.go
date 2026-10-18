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
	"github.com/jamesbraid/instigator/internal/instcmd"
	"github.com/jamesbraid/instigator/internal/logging"
)

const installReturnPrefix = "__instigator/attempts/"

func installStartPath(script string) string {
	return "/__instigator/" + script + "/start.cmds"
}

// installThenPath holds a script's second pass, sourced after its first go
// returns. installThenStartPath times that pass's go.
func installThenPath(script string) string {
	return "/__instigator/" + script + "/then.cmds"
}

func installThenStartPath(script string) string {
	return "/__instigator/" + script + "/then/start.cmds"
}

// installPostPath holds a script's post-install commands, sourced after its
// last go returns.
func installPostPath(script string) string {
	return "/__instigator/" + script + "/post-install.cmds"
}

func installReturnPath(attempt string) string {
	return "/" + installReturnPrefix + attempt + "/returned.cmds"
}

func installGoCommands(server, attempt, next string) string {
	commands := fmt.Sprintf("go\nadmin source %s:%s\n", server, installReturnPath(attempt))
	if next != "" {
		commands += fmt.Sprintf("admin source %s:%s\n", server, next)
	}
	return commands
}

// installScripts serves token-bearing command files without changing the
// shared media tree. A token becomes live only after its entire go file was
// written to the client; fetching it again yields a separate attempt.
type installScripts struct {
	server   string
	starts   map[string]installEntry
	logger   *logging.Logger
	rec      *capture.Recorder
	mu       sync.Mutex
	attempts map[string]*installAttempt
}

// installEntry is one timed go: the script pass it belongs to and the
// command file sourced after it returns.
type installEntry struct {
	script, next string
}

type installAttempt struct {
	id, script, client, address string
	start                       time.Time
	returned                    bool
}

func newInstallScripts(server string, generated []generatedFile, logger *logging.Logger, rec *capture.Recorder) *installScripts {
	s := &installScripts{
		server: server, logger: logger, rec: rec,
		starts: make(map[string]installEntry), attempts: make(map[string]*installAttempt),
	}
	for _, file := range generated {
		if file.generator == "install-timing" {
			s.starts[file.path] = installEntry{script: file.script, next: file.next}
		}
	}
	return s
}

func (s *installScripts) open(name, client, address string) (instcmd.File, bool, error) {
	if s == nil {
		return nil, false, nil
	}
	if entry, ok := s.starts[name]; ok {
		var token [16]byte
		rand.Read(token[:])
		attempt := &installAttempt{id: hex.EncodeToString(token[:]), script: entry.script, client: client, address: address}
		return newInstallFile([]byte(installGoCommands(s.server, attempt.id, entry.next)), func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			attempt.start = time.Now()
			s.attempts[fsName(installReturnPath(attempt.id))] = attempt
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
	attempt := s.attempts[name]
	if attempt == nil || attempt.address != address {
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
