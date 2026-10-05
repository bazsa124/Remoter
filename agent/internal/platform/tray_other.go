//go:build !windows

package platform

import "context"

func runTray(ctx context.Context, updates <-chan TrayState) error { return ErrUnsupported }
