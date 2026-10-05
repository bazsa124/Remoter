//go:build windows

package platform

import (
	"context"
	"os"
	"testing"
	"time"
)

// Shows the indicator for a few seconds so it can be looked at. Manual only:
// REMOTER_MANUAL=1 go test -run TestIndicatorManual ./internal/platform
func TestIndicatorManual(t *testing.T) {
	if os.Getenv("REMOTER_MANUAL") == "" {
		t.Skip("set REMOTER_MANUAL=1 to show the indicator")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	ch := make(chan TrayState, 1)
	ch <- TrayState{
		Active:  true,
		Tooltip: "Remoter - connected: phone (Live)",
		Banner:  "Remote session · phone (Live)",
	}
	if err := RunTray(ctx, ch); err != nil {
		t.Fatal(err)
	}
	if !theBanner.visible {
		t.Fatal("banner was never shown")
	}
}
