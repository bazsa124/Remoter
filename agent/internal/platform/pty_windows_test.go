//go:build windows

package platform

import (
	"bytes"
	"os"
	"strings"
	"testing"
	"time"
)

// A bare shell name must still resolve once the console starts in the user's
// home: setting the working directory once made go-pty look for
// C:\Users\<me>\powershell.exe, and every Console open failed.
func TestConsoleStartsInHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}
	con, err := StartConsole("cmd.exe", []string{"/c", "cd"}, 120, 30)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer con.Close()

	var out bytes.Buffer
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 4096)
		for {
			n, err := con.Read(buf)
			out.Write(buf[:n])
			if err != nil || strings.Contains(strings.ToLower(out.String()), strings.ToLower(home)) {
				return
			}
		}
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
	}
	if !strings.Contains(strings.ToLower(out.String()), strings.ToLower(home)) {
		t.Fatalf("console did not start in %s; output: %q", home, out.String())
	}
}
