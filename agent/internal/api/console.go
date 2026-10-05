package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/coder/websocket"

	"remoter/agent/internal/platform"
)

// Tier 2. One WebSocket carries a shell in both directions: JSON control frames
// from the client, raw bytes back.
//
// This endpoint is a full interactive shell - strictly more powerful than the
// action whitelist, which exists precisely so that Tier 1 is NOT a shell. It is
// gated by the same bearer token and can be switched off in config.

type consoleMsg struct {
	Type string `json:"type"`           // "data" | "resize"
	Data string `json:"data,omitempty"` // keystrokes
	Cols int    `json:"cols,omitempty"`
	Rows int    `json:"rows,omitempty"`
}

const (
	minDim = 1
	maxDim = 500
)

func (s *Server) console(w http.ResponseWriter, r *http.Request) {
	if !s.ConsoleEnabled {
		writeError(w, http.StatusForbidden, "console is disabled in agent config")
		return
	}

	cols := clampDim(intOr(r.URL.Query().Get("cols"), 80))
	rows := clampDim(intOr(r.URL.Query().Get("rows"), 24))

	name, args := s.ConsoleShell, s.ConsoleArgs
	if name == "" {
		name, args = platform.DefaultShell()
	}

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: []string{"*"}})
	if err != nil {
		s.Log.Warn("console: websocket accept failed", "err", err)
		return
	}
	defer conn.CloseNow()

	con, err := platform.StartConsole(name, args, cols, rows)
	if err != nil {
		s.Log.Warn("console: start failed", "shell", name, "err", err)
		_ = conn.Close(websocket.StatusInternalError, truncateReason(err.Error()))
		return
	}

	s.Log.Info("console opened", "shell", name, "cols", cols, "rows", rows)
	release := platform.HoldAwake(false)
	defer release()
	defer func() {
		_ = con.Close()
		s.Log.Info("console closed", "shell", name)
	}()

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	go keepAlive(ctx, conn, cancel)

	// pty -> client
	go func() {
		defer cancel()
		buf := make([]byte, 4096)
		for {
			n, err := con.Read(buf)
			if n > 0 {
				// Raw bytes: the terminal emulator on the other end owns decoding,
				// and splitting a UTF-8 sequence across frames is fine because
				// xterm.js reassembles its own stream.
				if werr := conn.Write(ctx, websocket.MessageBinary, buf[:n]); werr != nil {
					return
				}
			}
			if err != nil {
				if !errors.Is(err, io.EOF) {
					s.Log.Debug("console: read ended", "err", err)
				}
				return
			}
		}
	}()

	// client -> pty
	for {
		typ, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		if typ != websocket.MessageText {
			continue
		}

		var msg consoleMsg
		if err := json.Unmarshal(data, &msg); err != nil {
			continue // a malformed frame is not worth tearing the shell down for
		}

		switch msg.Type {
		case "data":
			if _, err := con.Write([]byte(msg.Data)); err != nil {
				return
			}
		case "resize":
			if err := con.Resize(clampDim(msg.Cols), clampDim(msg.Rows)); err != nil {
				s.Log.Debug("console: resize failed", "err", err)
			}
		}
	}
}

func clampDim(v int) int {
	if v < minDim {
		return minDim
	}
	if v > maxDim {
		return maxDim
	}
	return v
}

func intOr(raw string, fallback int) int {
	v, err := intParam(raw, fallback)
	if err != nil {
		return fallback
	}
	return v
}

// truncateReason keeps a close reason inside the protocol's 123-byte limit.
func truncateReason(s string) string {
	const limit = 120
	if len(s) <= limit {
		return s
	}
	return s[:limit]
}
