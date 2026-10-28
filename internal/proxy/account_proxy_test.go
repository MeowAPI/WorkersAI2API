package proxy

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
	tlsclient "github.com/bogdanfinn/tls-client"
)

func TestNormalizeAccountProxy(t *testing.T) {
	for raw, want := range map[string]string{
		"":                                     "",
		"  ":                                   "",
		" http://localhost/ ":                  "http://localhost:80",
		"https://proxy.example":                "https://proxy.example:443",
		"socks5://proxy.example":               "socks5://proxy.example:1080",
		"socks5h://proxy.example:9050":         "socks5h://proxy.example:9050",
		"http://[::1]:7890":                    "http://[::1]:7890",
		"https://[::1]":                        "https://[::1]:443",
		"http://user:p%40ss%3Aword@host:8080/": "http://user:p%40ss%3Aword@host:8080",
	} {
		got, err := normalizeAccountProxy(raw)
		if err != nil || got != want {
			t.Errorf("normalize %q = %q, %v; want %q", raw, got, err, want)
		}
	}
	for _, raw := range []string{
		"localhost:8080", "//localhost:8080", "ftp://host:21", "socks4://host:1080",
		"http://", "http://:8080", "http://host:0", "http://host:65536", "http://host:abc",
		"http://host:", "http://[::1]:", "http://::1", "http://host/path", "http://host/?",
		"http://host?token=secret", "http://host#", "http://host/#secret", "http://bad host:8080",
		"http://user:secret@host/\r\npath", "http://user:%00@host:80", "http://:secret@host:80",
		"socks5://" + strings.Repeat("u", 256) + ":secret@host:1080", strings.Repeat("x", 4097),
	} {
		if _, err := normalizeAccountProxy(raw); err == nil {
			t.Errorf("invalid proxy accepted: %q", raw)
		}
	}
}

func TestAccountProxyAdminPersistenceAndValidation(t *testing.T) {
	cfg := Config{AuthToken: "client-secret", AccountsFile: filepath.Join(t.TempDir(), "accounts.json")}
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	cookie, csrf := loginAdmin(t, s)
	proxyURL := "http://proxy-user:proxy%3Asecret@127.0.0.1:7890"
	data, _ := json.Marshal(map[string]any{
		"name": "proxied", "account_id": strings.Repeat("a", 32), "api_token": "secret-a", "proxy_url": proxyURL,
	})
	w := adminRequest(s, "POST", "/admin/api/accounts", string(data), cookie, csrf)
	if w.Code != http.StatusCreated {
		t.Fatal(w.Code, w.Body)
	}
	id := object(t, w.Body.Bytes())["id"].(string)
	checkView := func(data []byte) {
		t.Helper()
		if strings.Contains(string(data), "proxy-user") || strings.Contains(string(data), "proxy%3Asecret") || strings.Contains(string(data), "proxy_url") {
			t.Fatal("proxy credentials leaked in management response", string(data))
		}
	}
	checkView(w.Body.Bytes())
	if object(t, w.Body.Bytes())["proxy_hint"] != "http://127.0.0.1:7890" {
		t.Fatal("missing safe proxy hint", w.Body)
	}
	w = adminRequest(s, "GET", "/admin/api/state", "", cookie, "")
	checkView(w.Body.Bytes())
	for _, patch := range []string{`{"name":"renamed"}`, `{"enabled":false}`, `{"api_token":""}`} {
		w = adminRequest(s, "PATCH", "/admin/api/accounts/"+id, patch, cookie, csrf)
		if w.Code != 200 || s.accounts.accounts[0].ProxyURL != proxyURL {
			t.Fatal("unrelated edit changed proxy", w.Code, w.Body)
		}
	}
	for _, value := range []any{"ftp://proxy-user:secret@host:21", "http://host:99999", 123, true} {
		data, _ = json.Marshal(map[string]any{"proxy_url": value})
		w = adminRequest(s, "PATCH", "/admin/api/accounts/"+id, string(data), cookie, csrf)
		if w.Code != 400 || s.accounts.accounts[0].ProxyURL != proxyURL {
			t.Fatal("invalid proxy changed account", w.Code, w.Body)
		}
		checkView(w.Body.Bytes())
	}
	restarted, err := New(cfg)
	if err != nil || restarted.accounts.accounts[0].ProxyURL != proxyURL {
		t.Fatal("proxy lost after restart", err)
	}
	selected := restarted.accounts.accounts[0]
	restarted.accounts.record(selected, &http.Response{StatusCode: 500}, nil)
	if restarted.accounts.views()[0].CooldownUntil == nil {
		t.Fatal("expected cooldown")
	}
	if _, err := restarted.accounts.update(id, accountUpdate{ProxyURL: ptr("socks5://127.0.0.1")}, false); err != nil {
		t.Fatal(err)
	}
	if restarted.accounts.accounts[0].ProxyURL != "socks5://127.0.0.1:1080" || restarted.accounts.views()[0].CooldownUntil != nil {
		t.Fatal("proxy replacement was not normalized or did not clear cooldown")
	}
	restarted.accounts.record(selected, nil, errors.New("old proxy failed"))
	if restarted.accounts.views()[0].CooldownUntil != nil {
		t.Fatal("old in-flight proxy failure cooled down new proxy configuration")
	}
	parent := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(parent, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	restarted.accounts.file = filepath.Join(parent, "accounts.json")
	if _, err := restarted.accounts.update(id, accountUpdate{ProxyURL: ptr("")}, false); err == nil || restarted.accounts.accounts[0].ProxyURL == "" {
		t.Fatal("failed write changed live proxy")
	}
	w = adminRequest(s, "PATCH", "/admin/api/accounts/"+id, `{"proxy_url":""}`, cookie, csrf)
	if w.Code != 200 || object(t, w.Body.Bytes())["proxy_hint"] != "" {
		t.Fatal("proxy unbind failed", w.Code, w.Body)
	}
	restarted, err = New(cfg)
	if err != nil || restarted.accounts.accounts[0].ProxyURL != "" {
		t.Fatal("cleared proxy reappeared", err)
	}
}

// The local CONNECT proxy routes tunnels to a test server while preserving the requested authority.
func accountTestCONNECTProxy(t *testing.T, target, credentials string, secure bool) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	calls := &atomic.Int32{}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			t.Error("expected CONNECT", r.Method)
			w.WriteHeader(405)
			return
		}
		if credentials != "" && r.Header.Get("Proxy-Authorization") != "Basic "+base64.StdEncoding.EncodeToString([]byte(credentials)) {
			t.Error("proxy credentials missing or incorrect")
			w.WriteHeader(407)
			return
		}
		calls.Add(1)
		upstream, err := net.DialTimeout("tcp", target, time.Second)
		if err != nil {
			t.Error(err)
			w.WriteHeader(502)
			return
		}
		defer upstream.Close()
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		rw.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
		rw.Flush()
		go io.Copy(upstream, rw)
		io.Copy(conn, upstream)
	})
	var server *httptest.Server
	if secure {
		server = httptest.NewTLSServer(handler)
	} else {
		server = httptest.NewServer(handler)
	}
	t.Cleanup(server.Close)
	return server, calls
}

func accountTestSOCKSProxy(t *testing.T, target, user, credential string) (string, *atomic.Int32) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	calls := &atomic.Int32{}
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				conn.SetDeadline(time.Now().Add(10 * time.Second))
				var header [4]byte
				if _, err := io.ReadFull(conn, header[:2]); err != nil {
					return
				}
				methods := make([]byte, int(header[1]))
				if _, err := io.ReadFull(conn, methods); err != nil || header[0] != 5 {
					return
				}
				if user == "" {
					conn.Write([]byte{5, 0})
				} else {
					conn.Write([]byte{5, 2})
					if _, err := io.ReadFull(conn, header[:2]); err != nil || header[0] != 1 {
						return
					}
					username := make([]byte, int(header[1]))
					if _, err := io.ReadFull(conn, username); err != nil {
						return
					}
					if _, err := io.ReadFull(conn, header[:1]); err != nil {
						return
					}
					secret := make([]byte, int(header[0]))
					if _, err := io.ReadFull(conn, secret); err != nil {
						return
					}
					if string(username) != user || string(secret) != credential {
						t.Error("SOCKS credentials mismatch")
						conn.Write([]byte{1, 1})
						return
					}
					conn.Write([]byte{1, 0})
				}
				if _, err := io.ReadFull(conn, header[:]); err != nil || header[0] != 5 || header[1] != 1 {
					return
				}
				size := 0
				switch header[3] {
				case 1:
					size = 4
				case 4:
					size = 16
				case 3:
					if _, err := io.ReadFull(conn, header[:1]); err != nil {
						return
					}
					size = int(header[0])
				default:
					return
				}
				address := make([]byte, size+2)
				if _, err := io.ReadFull(conn, address); err != nil {
					return
				}
				calls.Add(1)
				upstream, err := net.DialTimeout("tcp", target, time.Second)
				if err != nil {
					t.Error(err)
					return
				}
				defer upstream.Close()
				conn.Write([]byte{5, 0, 0, 1, 127, 0, 0, 1, 0, 0})
				conn.SetDeadline(time.Time{})
				go io.Copy(upstream, conn)
				io.Copy(conn, upstream)
			}()
		}
	}()
	return "socks5://" + listener.Addr().String(), calls
}

func TestAccountProxyRealTransports(t *testing.T) {
	for _, profile := range []string{"", "chrome_152"} {
		for _, scheme := range []string{"http", "https", "socks5", "socks5h"} {
			t.Run(profile+"/"+scheme, func(t *testing.T) {
				upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Header.Get("Proxy-Authorization") != "" || r.Header.Get("Authorization") != "Bearer upstream-secret" {
						t.Error("proxy authentication leaked to upstream or upstream credentials changed")
					}
					if r.URL.Path == "/redirect" {
						http.Redirect(w, r, "/destination", 302)
						return
					}
					if r.URL.Path == "/destination" {
						t.Error("authenticated redirect followed")
					}
					w.Header().Set("Content-Type", "text/event-stream")
					io.WriteString(w, "data: first\n\n")
					w.(http.Flusher).Flush()
					<-r.Context().Done()
				}))
				defer upstream.Close()
				roots := x509.NewCertPool()
				roots.AddCert(upstream.Certificate())
				var calls *atomic.Int32
				var proxyURL string
				if strings.HasPrefix(scheme, "socks5") {
					proxyURL, calls = accountTestSOCKSProxy(t, upstream.Listener.Addr().String(), "proxy-user", "proxy:secret")
					proxyURL = strings.Replace(proxyURL, "socks5:", scheme+":", 1)
				} else {
					proxy, count := accountTestCONNECTProxy(t, upstream.Listener.Addr().String(), "proxy-user:proxy:secret", scheme == "https")
					proxyURL, calls = proxy.URL, count
					if scheme == "https" {
						roots.AddCert(proxy.Certificate())
					}
				}
				u, _ := url.Parse(proxyURL)
				u.User = url.UserPassword("proxy-user", "proxy:secret")
				a := account{ID: "first", Fingerprint: profile, ProxyURL: u.String()}
				s, err := New(Config{AuthToken: "client-secret"})
				if err != nil {
					t.Fatal(err)
				}
				s.client.Transport.(*http.Transport).TLSClientConfig = &tls.Config{RootCAs: roots}
				defer s.client.CloseIdleConnections()
				s.fingerprints.newClient = func(profile, proxyURL string) (fingerprintClient, error) {
					options := []tlsclient.HttpClientOption{tlsclient.WithTransportOptions(&tlsclient.TransportOptions{RootCAs: roots})}
					if scheme == "https" {
						dialer, err := fingerprintProxyDialer(proxyURL, &net.Dialer{Timeout: time.Second}, &tls.Config{RootCAs: roots})
						if err != nil {
							return nil, err
						}
						options = append(options, tlsclient.WithDialContext(dialer))
					}
					client, err := newFingerprintClient(profile, proxyURL, options...)
					if err == nil {
						t.Cleanup(client.CloseIdleConnections)
					}
					return client, err
				}
				do := func(req *http.Request) (*http.Response, error) {
					req.Header.Set("Authorization", "Bearer upstream-secret")
					if profile != "" {
						return s.fingerprints.do(req, a)
					}
					return s.proxies.do(req, a, s.client)
				}
				req, _ := http.NewRequest("GET", upstream.URL+"/redirect", nil)
				resp, err := do(req)
				if err != nil || resp.StatusCode != 302 {
					t.Fatal("proxy redirect request failed", err)
				}
				resp.Body.Close()
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				req, _ = http.NewRequestWithContext(ctx, "POST", upstream.URL+"/stream", strings.NewReader("input"))
				resp, err = do(req)
				if err != nil {
					t.Fatal(err)
				}
				first := make([]byte, len("data: first\n\n"))
				if _, err := io.ReadFull(resp.Body, first); err != nil || string(first) != "data: first\n\n" {
					t.Fatal("stream buffered or corrupted", err)
				}
				cancel()
				if _, err := io.ReadAll(resp.Body); err == nil {
					t.Error("cancellation failed to stop proxied stream")
				}
				resp.Body.Close()
				if calls.Load() == 0 {
					t.Fatal("bound proxy was bypassed")
				}
				for _, entry := range s.proxies.clients {
					entry.client.CloseIdleConnections()
				}
			})
		}
	}
}

func TestAccountProxy429SwitchAndBatchBinding(t *testing.T) {
	var calls []string
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Header.Get("Authorization"))
		body, err := io.ReadAll(r.Body)
		if err != nil || string(body) != "multipart-body" {
			t.Error("retry changed request body", err)
		}
		if len(calls) == 1 {
			w.Header().Set("Retry-After", "120")
			w.WriteHeader(429)
		}
		io.WriteString(w, "ok")
	}))
	defer upstream.Close()
	s, err := New(Config{AuthToken: "client-secret"})
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(upstream.Certificate())
	s.client.Transport.(*http.Transport).TLSClientConfig = &tls.Config{RootCAs: roots, ServerName: "example.com"}
	first := addTestAccount(t, s, strings.Repeat("a", 32), "secret-a")
	second := addTestAccount(t, s, strings.Repeat("b", 32), "secret-b")
	proxyA, aCalls := accountTestCONNECTProxy(t, upstream.Listener.Addr().String(), "", false)
	proxyB, bCalls := accountTestCONNECTProxy(t, upstream.Listener.Addr().String(), "", false)
	for id, proxyURL := range map[string]string{first: proxyA.URL, second: proxyB.URL} {
		if _, err := s.accounts.update(id, accountUpdate{ProxyURL: &proxyURL}, false); err != nil {
			t.Fatal(err)
		}
	}
	defer func() {
		for _, entry := range s.proxies.clients {
			entry.client.CloseIdleConnections()
		}
	}()
	binding := &accountBinding{}
	ctx := context.WithValue(context.Background(), accountBindingKey{}, binding)
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	for i := 0; i < 2; i++ {
		resp, err := s.upstream(ctx, httptest.NewRecorder(), "/run/test", "multipart/form-data; boundary=test", "", []byte("multipart-body"), nil)
		if err != nil || resp.StatusCode != 200 {
			t.Fatal("proxy retry failed", err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if i == 0 {
			if _, err := s.accounts.update(second, accountUpdate{ProxyURL: ptr("http://127.0.0.1:1")}, false); err != nil {
				t.Fatal(err)
			}
		}
	}
	if strings.Join(calls, ",") != "Bearer secret-a,Bearer secret-b,Bearer secret-b" || binding.selected.ID != second || aCalls.Load() != 1 || bCalls.Load() != 1 {
		t.Fatal("proxy/account selection or connection reuse changed", calls, aCalls.Load(), bCalls.Load())
	}
}

func TestAccountProxyCacheIsolationAndNoFallback(t *testing.T) {
	base := &http.Client{Transport: http.DefaultTransport.(*http.Transport).Clone()}
	pool := &accountProxyPool{}
	defer func() {
		for _, entry := range pool.clients {
			entry.client.CloseIdleConnections()
		}
	}()
	a := account{ID: "first", ProxyURL: "http://user:one@localhost:7890"}
	first, err := pool.get(a, base)
	if err != nil {
		t.Fatal(err)
	}
	same, _ := pool.get(a, base)
	changed := a
	changed.ID = "second"
	second, _ := pool.get(changed, base)
	changed = a
	changed.ProxyURL = "http://user:two@localhost:7890"
	replaced, _ := pool.get(changed, base)
	if first != same || first == second || first == replaced || first.Transport == base.Transport {
		t.Fatal("connections shared across accounts or proxy credentials")
	}
	for i := 0; i < 110; i++ {
		pool.get(account{ID: fmt.Sprint(i), ProxyURL: a.ProxyURL}, base)
	}
	if len(pool.clients) != 100 {
		t.Fatal("unbounded proxy client cache")
	}
	var dials int
	base.Transport.(*http.Transport).DialContext = func(_ context.Context, _, address string) (net.Conn, error) {
		dials++
		if address != "127.0.0.1:1" {
			t.Error("proxy failure attempted direct fallback", address)
		}
		return nil, errors.New("proxy unavailable")
	}
	req, _ := http.NewRequest("GET", "https://api.cloudflare.com/", nil)
	a = account{ID: "failed", ProxyURL: "http://127.0.0.1:1"}
	if _, err := pool.do(req, a, base); err == nil || dials != 1 {
		t.Fatal("proxy error lost or retried", err, dials)
	}
}

func TestFingerprintBoundProxyAndSecrets(t *testing.T) {
	s, err := New(Config{AuthToken: "client-secret"})
	if err != nil {
		t.Fatal(err)
	}
	id := addTestAccount(t, s, strings.Repeat("a", 32), "secret-a")
	proxyURL := "https://proxy-user:proxy%3Asecret@proxy.example:443"
	if _, err := s.accounts.update(id, accountUpdate{ProxyURL: &proxyURL, Fingerprint: ptr("chrome_152")}, false); err != nil {
		t.Fatal(err)
	}
	leak := proxyURL + " proxy:secret proxy-user " + base64.StdEncoding.EncodeToString([]byte("proxy-user:proxy:secret"))
	s.fingerprints.newClient = func(profile, proxy string) (fingerprintClient, error) {
		if profile != "chrome_152" || proxy != proxyURL {
			t.Error("fingerprint did not use bound proxy", proxy)
		}
		return &fakeFingerprintClient{do: func(*fhttp.Request) (*fhttp.Response, error) {
			data, _ := json.Marshal(map[string]any{"error": map[string]string{"message": leak}})
			return &fhttp.Response{StatusCode: 400, Header: fhttp.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(string(data)))}, nil
		}}, nil
	}
	w := request(s, "/v1/chat/completions", `{"model":"@cf/test/model","messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != 400 {
		t.Fatal(w.Code, w.Body)
	}
	records := recordedLogs(s)
	if len(records) != 1 {
		t.Fatal("missing request log")
	}
	log, err := s.logs.get(records[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(log)
	for _, secret := range []string{"proxy-user", "proxy:secret", "proxy%3Asecret", base64.StdEncoding.EncodeToString([]byte("proxy-user:proxy:secret"))} {
		if strings.Contains(w.Body.String(), secret) || strings.Contains(string(data), secret) {
			t.Fatal("proxy credentials leaked in error or request log")
		}
	}
}

func TestFingerprintProxyHandshakeCancellationAndRejection(t *testing.T) {
	for _, scheme := range []string{"http", "socks5"} {
		t.Run(scheme, func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			accepted := make(chan net.Conn, 1)
			go func() {
				conn, err := listener.Accept()
				if err == nil {
					accepted <- conn
				}
			}()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			dialer, err := fingerprintProxyDialer(scheme+"://"+listener.Addr().String(), &net.Dialer{Timeout: time.Second}, nil)
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() {
				conn, err := dialer(ctx, "tcp", "example.com:443")
				if conn != nil {
					conn.Close()
				}
				done <- err
			}()
			var conn net.Conn
			select {
			case conn = <-accepted:
			case <-time.After(3 * time.Second):
				t.Fatal("proxy connection not established")
			}
			defer conn.Close()
			cancel()
			select {
			case err = <-done:
				if err == nil {
					t.Error("canceled handshake succeeded")
				}
			case <-time.After(time.Second):
				t.Fatal("cancellation did not interrupt proxy handshake")
			}
			conn.SetReadDeadline(time.Now().Add(time.Second))
			if _, err := io.Copy(io.Discard, conn); err != nil {
				t.Error("canceled tunnel connection was not closed", err)
			}
		})
	}
	rejected := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusProxyAuthRequired)
	}))
	defer rejected.Close()
	dialer, err := fingerprintProxyDialer(rejected.URL, &net.Dialer{Timeout: time.Second}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if conn, err := dialer(context.Background(), "tcp", "example.com:443"); err == nil {
		conn.Close()
		t.Fatal("rejected CONNECT accepted")
	}
	untrusted := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("untrusted HTTPS proxy received CONNECT")
	}))
	defer untrusted.Close()
	dialer, err = fingerprintProxyDialer(untrusted.URL, &net.Dialer{Timeout: time.Second}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if conn, err := dialer(context.Background(), "tcp", "example.com:443"); err == nil {
		conn.Close()
		t.Fatal("untrusted HTTPS proxy certificate accepted")
	}
}
