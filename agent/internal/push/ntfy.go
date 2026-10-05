// Package push publishes notifications to ntfy.
//
// Payloads stay opaque on purpose: while the public ntfy.sh server is in use,
// anyone who learns the topic can read them. Detail is fetched over the tailnet.
package push

import (
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Publisher posts to an ntfy topic. A zero Topic disables publishing.
type Publisher struct {
	Server string
	Topic  string
	Client *http.Client
}

// New builds a publisher. Empty topic yields a no-op.
func New(server, topic string) *Publisher {
	if server == "" {
		server = "https://ntfy.sh"
	}
	return &Publisher{
		Server: strings.TrimRight(server, "/"),
		Topic:  topic,
		Client: &http.Client{Timeout: 10 * time.Second},
	}
}

// Enabled reports whether publishing is configured.
func (p *Publisher) Enabled() bool { return p != nil && p.Topic != "" }

// Send posts a message. Title and priority are set via ntfy's header protocol.
func (p *Publisher) Send(title, body string, priority int) error {
	return p.SendLink(title, body, priority, "")
}

// SendLink is Send plus a URL the notification opens when tapped.
func (p *Publisher) SendLink(title, body string, priority int, click string) error {
	if !p.Enabled() {
		return nil
	}
	req, err := http.NewRequest(http.MethodPost, p.Server+"/"+p.Topic, strings.NewReader(body))
	if err != nil {
		return fmt.Errorf("build ntfy request: %w", err)
	}
	req.Header.Set("Title", title)
	if priority > 0 {
		req.Header.Set("Priority", fmt.Sprint(priority))
	}
	if click != "" {
		req.Header.Set("Click", click)
	}

	resp, err := p.Client.Do(req)
	if err != nil {
		return fmt.Errorf("publish to ntfy: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		return fmt.Errorf("ntfy returned %s", resp.Status)
	}
	return nil
}
