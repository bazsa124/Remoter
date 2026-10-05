package hub

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// tsapi talks to tailscaled's LocalAPI over its unix socket.
//
// Spoken directly rather than through the tailscale.com client package: three
// GET endpoints are all the hub needs, and importing tailscale.com would pull
// in a dependency tree larger than the rest of the agent combined.
type tsapi struct {
	client *http.Client

	mu    sync.Mutex
	cache map[string]cachedWho
}

type cachedWho struct {
	who     *Peer
	expires time.Time
}

// Peer is a tailnet device as tailscaled identifies it.
type Peer struct {
	ID   string // stable node ID; survives renames and re-auth
	Name string // short host name, the first label of the MagicDNS name
	FQDN string
	OS   string
	IP   string // first tailnet IPv4
}

const whoisTTL = 30 * time.Second

func newTSAPI(socket string) *tsapi {
	dialer := &net.Dialer{Timeout: 3 * time.Second}
	return &tsapi{
		// No client-wide timeout: every call carries its own deadline, and the
		// first certificate issuance goes to Let's Encrypt and can take a minute.
		client: &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					return dialer.DialContext(ctx, "unix", socket)
				},
			},
		},
		cache: make(map[string]cachedWho),
	}
}

func (t *tsapi) get(ctx context.Context, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://local-tailscaled.sock"+path, nil)
	if err != nil {
		return nil, err
	}
	res, err := t.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("tailscaled: %w", err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("tailscaled %s: %s: %s", path, res.Status, strings.TrimSpace(string(body)))
	}
	return body, nil
}

// SelfInfo is what the hub needs to know about the machine it runs on.
type SelfInfo struct {
	DNSName string // without the trailing dot
	IPv4    string
}

func (t *tsapi) self(ctx context.Context) (SelfInfo, error) {
	body, err := t.get(ctx, "/localapi/v0/status?peers=false")
	if err != nil {
		return SelfInfo{}, err
	}
	var st struct {
		Self struct {
			DNSName      string
			TailscaleIPs []string
		}
	}
	if err := json.Unmarshal(body, &st); err != nil {
		return SelfInfo{}, fmt.Errorf("parse status: %w", err)
	}
	info := SelfInfo{DNSName: strings.TrimSuffix(st.Self.DNSName, ".")}
	for _, ip := range st.Self.TailscaleIPs {
		if strings.Count(ip, ".") == 3 {
			info.IPv4 = ip
			break
		}
	}
	if info.DNSName == "" || info.IPv4 == "" {
		return info, fmt.Errorf("tailscale is not up yet (no DNS name or IPv4)")
	}
	return info, nil
}

// whois identifies the tailnet device behind a remote address.
//
// This is the hub's entire notion of "who is calling": tailscaled has already
// authenticated the WireGuard peer, so the address IS the device. All devices
// here belong to one Tailscale user, which is why the node - not the user - is
// the identity that matters.
func (t *tsapi) whois(ctx context.Context, remoteAddr string) (*Peer, error) {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}

	t.mu.Lock()
	if c, ok := t.cache[host]; ok && time.Now().Before(c.expires) {
		t.mu.Unlock()
		return c.who, nil
	}
	t.mu.Unlock()

	wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	body, err := t.get(wctx, "/localapi/v0/whois?addr="+url.QueryEscape(net.JoinHostPort(host, "1")))
	if err != nil {
		return nil, err
	}
	var resp struct {
		Node struct {
			StableID  string
			Name      string
			Addresses []string
			Hostinfo  struct {
				OS       string
				Hostname string
			}
		}
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("parse whois: %w", err)
	}
	if resp.Node.StableID == "" {
		return nil, fmt.Errorf("whois %s: no node", host)
	}

	fqdn := strings.TrimSuffix(resp.Node.Name, ".")
	name, _, _ := strings.Cut(fqdn, ".")
	if name == "" {
		name = resp.Node.Hostinfo.Hostname
	}
	p := &Peer{ID: resp.Node.StableID, Name: name, FQDN: fqdn, OS: resp.Node.Hostinfo.OS, IP: host}
	for _, a := range resp.Node.Addresses {
		ip, _, _ := strings.Cut(a, "/")
		if strings.Count(ip, ".") == 3 {
			p.IP = ip
			break
		}
	}

	t.mu.Lock()
	t.cache[host] = cachedWho{who: p, expires: time.Now().Add(whoisTTL)}
	t.mu.Unlock()
	return p, nil
}

// certificate fetches the Let's Encrypt pair tailscaled maintains for domain.
//
// tailscaled caches and renews it; asking again returns the cached pair until
// renewal is due. The hub's user needs TS_PERMIT_CERT_UID in tailscaled's
// environment, since fetching certs is otherwise root-only.
func (t *tsapi) certificate(ctx context.Context, domain string) (*tls.Certificate, error) {
	body, err := t.get(ctx, "/localapi/v0/cert/"+url.PathEscape(domain)+"?type=pair")
	if err != nil {
		return nil, err
	}
	// The pair is key and certificate PEM concatenated; X509KeyPair picks the
	// blocks it needs out of each argument, so the same bytes serve both.
	cert, err := tls.X509KeyPair(body, body)
	if err != nil {
		return nil, fmt.Errorf("parse certificate for %s: %w", domain, err)
	}
	return &cert, nil
}
