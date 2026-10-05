package api

import (
	"context"
	"time"

	"github.com/coder/websocket"
)

const (
	pingEvery   = 20 * time.Second
	pingTimeout = 15 * time.Second // generous: a phone on a congested cell
)

// keepAlive pings the far end of a socket and cancels the session when it stops
// answering.
//
// Without it a phone that vanishes - app killed, radio dropped, WebView
// discarded - leaves its socket half-open for as long as TCP cares to wait, and
// an idle Live view sends nothing that would expose it: an unchanged screen
// costs no frames, so there are no failed writes either. The pong comes from
// the client itself (browsers answer pings on their own), so the check covers
// the whole path through the hub, not just the next hop.
func keepAlive(ctx context.Context, conn *websocket.Conn, cancel context.CancelFunc) {
	t := time.NewTicker(pingEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			pctx, pcancel := context.WithTimeout(ctx, pingTimeout)
			err := conn.Ping(pctx)
			pcancel()
			if err != nil {
				cancel()
				return
			}
		}
	}
}
