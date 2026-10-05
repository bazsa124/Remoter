package hub

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// auditLog is an append-only record of who did what to which device.
//
// The hub is the one place every session passes through, which is the main
// security payoff of relaying everything: a single log answers "who was on my
// laptop at 3 a.m.", with no per-device logs to collect and correlate.
type auditLog struct {
	path string
	mu   sync.Mutex
}

// AuditEntry is one line of the log.
type AuditEntry struct {
	Time       time.Time `json:"time"`
	Event      string    `json:"event"`
	Controller string    `json:"controller,omitempty"`
	Device     string    `json:"device,omitempty"`
	Tier       string    `json:"tier,omitempty"`
	Detail     string    `json:"detail,omitempty"`
}

const auditRotateBytes = 5 << 20

func newAuditLog(dir string) *auditLog {
	return &auditLog{path: filepath.Join(dir, "audit.log")}
}

func (a *auditLog) write(e AuditEntry) {
	e.Time = time.Now().UTC()
	line, err := json.Marshal(e)
	if err != nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()

	// Rotate once, keeping one previous file: enough history to investigate,
	// bounded so a chatty device cannot fill the disk.
	if info, err := os.Stat(a.path); err == nil && info.Size() > auditRotateBytes {
		_ = os.Rename(a.path, a.path+".1")
	}
	f, err := os.OpenFile(a.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(line, '\n'))
}

// recent returns up to n entries, newest first.
func (a *auditLog) recent(n int) []AuditEntry {
	a.mu.Lock()
	defer a.mu.Unlock()

	f, err := os.Open(a.path)
	if err != nil {
		return nil
	}
	defer f.Close()

	// Only the tail is needed; read the last 512 KB rather than the whole file.
	const window = 512 << 10
	if info, err := f.Stat(); err == nil && info.Size() > window {
		_, _ = f.Seek(info.Size()-window, io.SeekStart)
	}
	data, _ := io.ReadAll(f)

	var all []AuditEntry
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		var e AuditEntry
		if json.Unmarshal(sc.Bytes(), &e) == nil {
			all = append(all, e) // a partial first line after Seek simply fails to parse
		}
	}
	out := make([]AuditEntry, 0, min(n, len(all)))
	for i := len(all) - 1; i >= 0 && len(out) < n; i-- {
		out = append(out, all[i])
	}
	return out
}
