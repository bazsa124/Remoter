package platform

import "context"

// TrayState is what the session indicator shows.
type TrayState struct {
	Active  bool   // show the indicator at all
	Tooltip string // who is connected, on hover
	Banner  string // the always-visible strip at the top of the screen

	// A one-off notification, for the moment a session starts.
	NoticeTitle string
	Notice      string
}

// RunTray shows the session indicator until ctx ends or updates closes. It
// needs a process on the user's desktop; it returns ErrUnsupported where there
// is no notification area to draw in.
func RunTray(ctx context.Context, updates <-chan TrayState) error { return runTray(ctx, updates) }
