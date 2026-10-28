package proxy

import (
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
)

func normalizeAccountProxy(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	fail := func() (string, error) {
		return "", invalid("代理地址必须为有效的 HTTP、HTTPS 或 SOCKS5 URL，可包含用户名和密码，不能包含路径、查询参数或片段")
	}
	if len(raw) > 4096 {
		return fail()
	}
	u, err := url.Parse(raw)
	if err != nil || u.Opaque != "" || u.Hostname() == "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawFragment != "" || strings.Contains(raw, "#") || u.Path != "" && u.Path != "/" {
		return fail()
	}
	port := ""
	switch u.Scheme {
	case "http":
		port = "80"
	case "https":
		port = "443"
	case "socks5", "socks5h":
		port = "1080"
	default:
		return fail()
	}
	if strings.HasSuffix(u.Host, ":") || strings.Contains(u.Hostname(), ":") && net.ParseIP(u.Hostname()) == nil {
		return fail()
	}
	if u.Port() != "" {
		port = u.Port()
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return fail()
	}
	if u.User != nil {
		password, _ := u.User.Password()
		if u.User.Username() == "" || strings.ContainsAny(u.User.Username()+password, "\x00\r\n") {
			return fail()
		}
		if (u.Scheme == "socks5" || u.Scheme == "socks5h") && (len(u.User.Username()) > 255 || len(password) > 255) {
			return fail()
		}
	}
	u.Host = net.JoinHostPort(u.Hostname(), port)
	u.Path, u.RawPath = "", ""
	return u.String(), nil
}

func (a account) proxyHint() string {
	u, err := url.Parse(a.ProxyURL)
	if err != nil || a.ProxyURL == "" {
		return ""
	}
	u.User = nil
	return u.String()
}

func (a account) secrets() []string {
	secrets := []string{a.APIToken}
	if a.ProxyURL == "" {
		return secrets
	}
	secrets = append(secrets, a.ProxyURL)
	if u, err := url.Parse(a.ProxyURL); err == nil && u.User != nil {
		password, _ := u.User.Password()
		secrets = append(secrets, u.User.String(), u.User.Username(), password,
			base64.StdEncoding.EncodeToString([]byte(u.User.Username()+":"+password)))
	}
	return secrets
}

func accountProxyURL(req *http.Request, a account) (string, error) {
	if a.ProxyURL != "" {
		return a.ProxyURL, nil
	}
	proxy, err := http.ProxyFromEnvironment(req)
	if err != nil {
		return "", fmt.Errorf("invalid upstream proxy")
	}
	if proxy != nil {
		return proxy.String(), nil
	}
	return "", nil
}

type proxyClientEntry struct {
	client *http.Client
	used   uint64
}

type accountProxyPool struct {
	mu      sync.Mutex
	clients map[string]proxyClientEntry
	clock   uint64
}

func (p *accountProxyPool) get(a account, base *http.Client) (*http.Client, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.clients == nil {
		p.clients = make(map[string]proxyClientEntry)
	}
	key := a.ID + "\x00" + a.ProxyURL
	p.clock++
	if entry, ok := p.clients[key]; ok {
		entry.used = p.clock
		p.clients[key] = entry
		return entry.client, nil
	}
	proxyURL, err := url.Parse(a.ProxyURL)
	if err != nil {
		return nil, fmt.Errorf("invalid account proxy")
	}
	transport, ok := base.Transport.(*http.Transport)
	if !ok {
		return nil, fmt.Errorf("account proxy requires an HTTP transport")
	}
	proxied := transport.Clone()
	proxied.Proxy = http.ProxyURL(proxyURL)
	client := *base
	client.Transport = proxied
	// Keep connections isolated when an account or its proxy credentials change.
	if len(p.clients) >= 100 {
		oldestKey := ""
		var oldest uint64
		for key, entry := range p.clients {
			if oldestKey == "" || entry.used < oldest {
				oldestKey, oldest = key, entry.used
			}
		}
		p.clients[oldestKey].client.CloseIdleConnections()
		delete(p.clients, oldestKey)
	}
	p.clients[key] = proxyClientEntry{client: &client, used: p.clock}
	return &client, nil
}

func (p *accountProxyPool) do(req *http.Request, a account, base *http.Client) (*http.Response, error) {
	if a.ProxyURL == "" {
		return base.Do(req)
	}
	client, err := p.get(a, base)
	if err != nil {
		return nil, err
	}
	return client.Do(req)
}
