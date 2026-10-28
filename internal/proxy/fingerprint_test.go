package proxy

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
	"github.com/bogdanfinn/fhttp/http2"
	"github.com/bogdanfinn/fhttp/http2/hpack"
	tlsclient "github.com/bogdanfinn/tls-client"
)

type fingerprintWireResult struct {
	ciphers  []uint16
	settings []http2.Setting
	pseudo   []string
	window   uint32
	err      error
}

// Read the decrypted HTTP/2 handshake instead of inferring it from a profile name.
func captureFingerprint(conn *tls.Conn, result fingerprintWireResult) fingerprintWireResult {
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		result.err = err
		return result
	}
	preface := make([]byte, len(http2.ClientPreface))
	if _, err := io.ReadFull(conn, preface); err != nil || string(preface) != http2.ClientPreface {
		result.err = fmt.Errorf("invalid HTTP/2 preface: %w", err)
		return result
	}
	framer := http2.NewFramer(conn, conn)
	if err := framer.WriteSettings(); err != nil {
		result.err = err
		return result
	}
	for {
		frame, err := framer.ReadFrame()
		if err != nil {
			result.err = err
			return result
		}
		switch frame := frame.(type) {
		case *http2.SettingsFrame:
			if !frame.IsAck() {
				frame.ForeachSetting(func(s http2.Setting) error {
					result.settings = append(result.settings, s)
					return nil
				})
				if err := framer.WriteSettingsAck(); err != nil {
					result.err = err
					return result
				}
			}
		case *http2.WindowUpdateFrame:
			if frame.StreamID == 0 {
				result.window = frame.Increment
			}
		case *http2.HeadersFrame:
			block := append([]byte{}, frame.HeaderBlockFragment()...)
			ended := frame.HeadersEnded()
			for !ended {
				next, err := framer.ReadFrame()
				if err != nil {
					result.err = err
					return result
				}
				continuation, ok := next.(*http2.ContinuationFrame)
				if !ok {
					result.err = fmt.Errorf("expected header continuation")
					return result
				}
				block = append(block, continuation.HeaderBlockFragment()...)
				ended = continuation.HeadersEnded()
			}
			fields, err := hpack.NewDecoder(65536, nil).DecodeFull(block)
			if err != nil {
				result.err = err
				return result
			}
			for _, field := range fields {
				if strings.HasPrefix(field.Name, ":") {
					result.pseudo = append(result.pseudo, field.Name)
				}
			}
			var headers bytes.Buffer
			encoder := hpack.NewEncoder(&headers)
			encoder.WriteField(hpack.HeaderField{Name: ":status", Value: "200"})
			encoder.WriteField(hpack.HeaderField{Name: "content-type", Value: "text/plain"})
			if err := framer.WriteHeaders(http2.HeadersFrameParam{StreamID: frame.StreamID, EndHeaders: true, BlockFragment: headers.Bytes()}); err != nil {
				result.err = err
				return result
			}
			result.err = framer.WriteData(frame.StreamID, true, []byte("ok"))
			return result
		}
	}
}

func withoutGrease(values []uint16) []uint16 {
	var result []uint16
	for _, value := range values {
		if value&0x0f0f != 0x0a0a {
			result = append(result, value)
		}
	}
	return result
}

func TestFingerprintTLSAndHTTP2OnWire(t *testing.T) {
	certificateServer := httptest.NewTLSServer(http.NotFoundHandler())
	roots := x509.NewCertPool()
	roots.AddCert(certificateServer.Certificate())
	certificates := certificateServer.TLS.Certificates
	certificateServer.Close()
	for _, profile := range fingerprintProfiles {
		t.Run(profile.ID, func(t *testing.T) {
			var ciphers []uint16
			listener, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
				Certificates: certificates, NextProtos: []string{"h2"},
				GetConfigForClient: func(info *tls.ClientHelloInfo) (*tls.Config, error) {
					ciphers = append([]uint16{}, info.CipherSuites...)
					return nil, nil
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			done := make(chan fingerprintWireResult, 1)
			go func() {
				conn, err := listener.Accept()
				if err != nil {
					done <- fingerprintWireResult{err: err}
					return
				}
				tlsConn := conn.(*tls.Conn)
				if err := tlsConn.Handshake(); err != nil {
					conn.Close()
					done <- fingerprintWireResult{err: err}
					return
				}
				done <- captureFingerprint(tlsConn, fingerprintWireResult{ciphers: ciphers})
			}()
			client, err := newFingerprintClient(profile.ID, "", tlsclient.WithTransportOptions(&tlsclient.TransportOptions{RootCAs: roots}))
			if err != nil {
				t.Fatal(err)
			}
			defer client.CloseIdleConnections()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			req, _ := http.NewRequestWithContext(ctx, "GET", "https://"+listener.Addr().String()+"/", nil)
			req.Header.Set("User-Agent", profile.UserAgent)
			resp, err := doFingerprintRequest(client, req)
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			if err != nil || string(body) != "ok" || resp.ProtoMajor != 2 {
				t.Fatal("fingerprint request failed", err, string(body))
			}
			wire := <-done
			if wire.err != nil {
				t.Fatal(wire.err)
			}
			spec, err := profile.profile.GetClientHelloSpec()
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(withoutGrease(wire.ciphers), withoutGrease(spec.CipherSuites)) {
				t.Fatalf("TLS cipher order differs from profile: %v", wire.ciphers)
			}
			var settings []http2.Setting
			for _, id := range profile.profile.GetSettingsOrder() {
				settings = append(settings, http2.Setting{ID: id, Val: profile.profile.GetSettings()[id]})
			}
			if !reflect.DeepEqual(wire.settings, settings) || !reflect.DeepEqual(wire.pseudo, profile.profile.GetPseudoHeaderOrder()) || wire.window != profile.profile.GetConnectionFlow() {
				t.Fatalf("HTTP/2 fingerprint differs from profile: %#v", wire)
			}
		})
	}
}

func TestFingerprintStreamingCancellationRedirectsAndCertificates(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, "/destination", 302)
			return
		}
		if r.URL.Path == "/destination" {
			t.Error("authenticated redirect followed")
		}
		if r.Header.Get("Cookie") != "" {
			t.Error("fingerprint client retained cookies")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Set-Cookie", "upstream_session=private")
		io.WriteString(w, "data: first\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()
	defer close(release)
	untrusted, err := newFingerprintClient("chrome_152", "")
	if err != nil {
		t.Fatal(err)
	}
	defer untrusted.CloseIdleConnections()
	req, _ := http.NewRequest("GET", server.URL+"/redirect", nil)
	if resp, err := doFingerprintRequest(untrusted, req); err == nil {
		resp.Body.Close()
		t.Fatal("untrusted certificate accepted")
	}
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	client, err := newFingerprintClient("chrome_152", "", tlsclient.WithTransportOptions(&tlsclient.TransportOptions{RootCAs: roots}))
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections()
	resp, err := doFingerprintRequest(client, req)
	if err != nil || resp.StatusCode != http.StatusFound {
		t.Fatal("redirect not preserved", err)
	}
	resp.Body.Close()
	for i := 0; i < 2; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		req, _ = http.NewRequestWithContext(ctx, "POST", server.URL+"/stream", strings.NewReader("input"))
		resp, err = doFingerprintRequest(client, req)
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		first := make([]byte, len("data: first\n\n"))
		if _, err := io.ReadFull(resp.Body, first); err != nil || string(first) != "data: first\n\n" {
			cancel()
			t.Fatal("stream buffered or corrupted", err)
		}
		cancel()
		if _, err := io.ReadAll(resp.Body); err == nil {
			t.Error("request cancellation did not interrupt stream")
		}
		resp.Body.Close()
	}
}

type fakeFingerprintClient struct {
	do     func(*fhttp.Request) (*fhttp.Response, error)
	closed atomic.Int32
}

func (c *fakeFingerprintClient) Do(r *fhttp.Request) (*fhttp.Response, error) { return c.do(r) }
func (c *fakeFingerprintClient) CloseIdleConnections()                        { c.closed.Add(1) }

func TestFingerprintAccountIsolationCacheAndError(t *testing.T) {
	created := 0
	pool := fingerprintPool{newClient: func(string, string) (fingerprintClient, error) {
		created++
		return &fakeFingerprintClient{}, nil
	}}
	a := account{ID: "first", Fingerprint: "chrome_152"}
	first, _ := pool.get(a, "")
	same, _ := pool.get(a, "")
	b := a
	b.ID = "second"
	second, _ := pool.get(b, "")
	changed := a
	changed.Fingerprint = "firefox_147"
	differentProfile, _ := pool.get(changed, "")
	proxied, _ := pool.get(a, "http://proxy.example:8080")
	if same != first || first == second || differentProfile == first || proxied == first || created != 4 {
		t.Fatal("fingerprint connections not isolated by account/profile/proxy")
	}
	for i := 0; i < 110; i++ {
		pool.get(account{ID: fmt.Sprint(i), Fingerprint: "chrome_152"}, "")
	}
	if len(pool.clients) != 100 || first.(*fakeFingerprintClient).closed.Load() != 1 {
		t.Fatal("fingerprint cache is unbounded or idle connections not closed")
	}
	wantErr := errors.New("network failed")
	client := &fakeFingerprintClient{do: func(*fhttp.Request) (*fhttp.Response, error) { return nil, wantErr }}
	req, _ := http.NewRequest("GET", "https://example.com/", nil)
	if _, err := doFingerprintRequest(client, req); !errors.Is(err, wantErr) {
		t.Fatal("network error misclassified as timeout", err)
	}
}

func TestFingerprintRetryAndAccountBinding(t *testing.T) {
	s, err := New(Config{AuthToken: "client-secret"})
	if err != nil {
		t.Fatal(err)
	}
	first := addTestAccount(t, s, strings.Repeat("a", 32), "secret-a")
	second := addTestAccount(t, s, strings.Repeat("b", 32), "secret-b")
	if _, err := s.accounts.update(first, accountUpdate{Fingerprint: ptr("chrome_152"), ProxyURL: ptr("http://first.example:8080")}, false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.accounts.update(second, accountUpdate{Fingerprint: ptr("firefox_147"), ProxyURL: ptr("socks5://second.example:1080")}, false); err != nil {
		t.Fatal(err)
	}
	var profiles, proxies []string
	s.fingerprints.newClient = func(profileID, proxyURL string) (fingerprintClient, error) {
		return &fakeFingerprintClient{do: func(r *fhttp.Request) (*fhttp.Response, error) {
			profiles = append(profiles, profileID)
			proxies = append(proxies, proxyURL)
			status := 200
			token, ua := "secret-b", findFingerprint(profileID).UserAgent
			if len(profiles) == 1 {
				status, token = 429, "secret-a"
			}
			if r.Header.Get("User-Agent") != ua || r.Header.Get("Authorization") != "Bearer "+token {
				t.Error("fingerprint/account/UA pairing changed")
			}
			body, _ := io.ReadAll(r.Body)
			if string(body) != "multipart-body" || r.Header.Get("Content-Type") != "multipart/form-data; boundary=test" {
				t.Error("fingerprint request changed multipart data")
			}
			return &fhttp.Response{StatusCode: status, Header: fhttp.Header{}, Body: io.NopCloser(strings.NewReader("ok"))}, nil
		}}, nil
	}
	ctx := context.WithValue(context.Background(), accountBindingKey{}, &accountBinding{})
	trace := &requestTrace{}
	ctx = context.WithValue(ctx, traceKey{}, trace)
	for i := 0; i < 3; i++ {
		resp, err := s.upstream(ctx, httptest.NewRecorder(), "/run/@cf/test/model", "multipart/form-data; boundary=test", "", []byte("multipart-body"), nil)
		if err != nil || resp.StatusCode != 200 {
			t.Fatal(err)
		}
		resp.Body.Close()
		if i == 0 {
			if _, err := s.accounts.update(second, accountUpdate{Fingerprint: ptr("safari_ios_18_0"), ProxyURL: ptr("http://changed.example:8080")}, false); err != nil {
				t.Fatal(err)
			}
		}
	}
	if strings.Join(profiles, ",") != "chrome_152,firefox_147,firefox_147,firefox_147" {
		t.Fatal("retry or live edit changed bound fingerprint", profiles)
	}
	if strings.Join(proxies, ",") != "http://first.example:8080,socks5://second.example:1080,socks5://second.example:1080,socks5://second.example:1080" {
		t.Fatal("retry or live edit changed bound proxy", proxies)
	}
	if len(trace.steps) != len(profiles) {
		t.Fatal("fingerprint calls missing from request trace")
	}
	for i, step := range trace.steps {
		if step.fingerprint != profiles[i] {
			t.Fatal("request trace used a later account configuration", i, step.fingerprint)
		}
	}
}

func TestFingerprintCatalogValidationAndPersistence(t *testing.T) {
	cfg := Config{AuthToken: "client-secret", AccountsFile: filepath.Join(t.TempDir(), "accounts.json")}
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	cookie, csrf := loginAdmin(t, s)
	w := adminRequest(s, "GET", "/admin/api/state", "", cookie, "")
	catalog := object(t, w.Body.Bytes())["client_profiles"].(map[string]any)
	if len(catalog["user_agents"].([]any)) != 2 || len(catalog["fingerprints"].([]any)) != 3 || len(catalog["presets"].([]any)) != 5 {
		t.Fatal("client profile catalog missing", w.Body)
	}
	for _, template := range userAgentTemplates {
		if err := validateAccount(account{Name: "template", AccountID: strings.Repeat("a", 32), APIToken: "secret", UserAgent: template.UserAgent}); err != nil {
			t.Fatal("invalid built-in UA template", template.ID, err)
		}
	}
	body := `{"name":"first","account_id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","api_token":"secret-a","fingerprint":"chrome_152"}`
	w = adminRequest(s, "POST", "/admin/api/accounts", body, cookie, csrf)
	if w.Code != http.StatusCreated || object(t, w.Body.Bytes())["fingerprint"] != "chrome_152" {
		t.Fatal(w.Code, w.Body)
	}
	id := s.accounts.accounts[0].ID
	if got := s.accounts.accounts[0].effectiveUserAgent(); got != findFingerprint("chrome_152").UserAgent {
		t.Fatal("fingerprint default UA not applied", got)
	}
	w = adminRequest(s, "PATCH", "/admin/api/accounts/"+id, `{"user_agent":"Custom/1.0"}`, cookie, csrf)
	if w.Code != 200 || object(t, w.Body.Bytes())["fingerprint"] != "" || s.accounts.accounts[0].effectiveUserAgent() != "Custom/1.0" {
		t.Fatal("custom UA retained a browser fingerprint", w.Code, w.Body)
	}
	w = adminRequest(s, "PATCH", "/admin/api/accounts/"+id, `{"fingerprint":"chrome_152"}`, cookie, csrf)
	if w.Code != 200 || object(t, w.Body.Bytes())["user_agent"] != findFingerprint("chrome_152").UserAgent {
		t.Fatal("fingerprint selection did not bind its UA", w.Code, w.Body)
	}
	w = adminRequest(s, "PATCH", "/admin/api/accounts/"+id, `{"fingerprint":"claude-code"}`, cookie, csrf)
	if w.Code != http.StatusBadRequest || s.accounts.views()[0].Fingerprint != "chrome_152" {
		t.Fatal("unverified app fingerprint accepted", w.Code, w.Body)
	}
	restarted, err := New(cfg)
	if err != nil || restarted.accounts.views()[0].Fingerprint != "chrome_152" || restarted.accounts.views()[0].UserAgent != findFingerprint("chrome_152").UserAgent {
		t.Fatal("fingerprint lost after restart", err)
	}
	w = adminRequest(s, "PATCH", "/admin/api/accounts/"+id, `{"fingerprint":"","user_agent":""}`, cookie, csrf)
	if w.Code != 200 || s.accounts.accounts[0].effectiveUserAgent() != "" {
		t.Fatal("native transport could not be restored", w.Code, w.Body)
	}
	restarted, err = New(cfg)
	if err != nil || restarted.accounts.accounts[0].Fingerprint != "" {
		t.Fatal("cleared fingerprint reappeared", err)
	}
	invalidStore, _ := json.Marshal(struct {
		Version  int       `json:"version"`
		Accounts []account `json:"accounts"`
	}{1, []account{{ID: strings.Repeat("a", 64), Name: "invalid", AccountID: strings.Repeat("a", 32), APIToken: "secret", Fingerprint: "unknown"}}})
	if err := os.WriteFile(cfg.AccountsFile, invalidStore, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := New(cfg); err == nil {
		t.Fatal("invalid persisted fingerprint accepted")
	}
}

func TestClientPresetBindingAndMismatchRejection(t *testing.T) {
	for _, preset := range clientPresets() {
		t.Run(preset.ID, func(t *testing.T) {
			cfg := Config{AuthToken: "client-secret", AccountsFile: filepath.Join(t.TempDir(), "accounts.json")}
			s, err := New(cfg)
			if err != nil {
				t.Fatal(err)
			}
			cookie, csrf := loginAdmin(t, s)
			body, _ := json.Marshal(map[string]any{
				"name": preset.Name, "account_id": strings.Repeat("a", 32), "api_token": "secret-a",
				"user_agent": preset.UserAgent, "fingerprint": preset.Fingerprint,
			})
			w := adminRequest(s, "POST", "/admin/api/accounts", string(body), cookie, csrf)
			if w.Code != http.StatusCreated {
				t.Fatal(w.Code, w.Body)
			}
			id := s.accounts.accounts[0].ID
			before, err := os.ReadFile(cfg.AccountsFile)
			if err != nil {
				t.Fatal(err)
			}
			badProfile := "chrome_152"
			if preset.Fingerprint == badProfile {
				badProfile = "firefox_147"
			}
			body, _ = json.Marshal(map[string]string{"user_agent": preset.UserAgent, "fingerprint": badProfile})
			w = adminRequest(s, "PATCH", "/admin/api/accounts/"+id, string(body), cookie, csrf)
			if w.Code != http.StatusBadRequest {
				t.Fatal("mixed client identity accepted", w.Code, w.Body)
			}
			after, err := os.ReadFile(cfg.AccountsFile)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("rejected mismatch changed persisted account", err)
			}
			view := s.accounts.views()[0]
			if view.Fingerprint != preset.Fingerprint || view.UserAgent != preset.UserAgent {
				t.Fatal("rejected mismatch changed live account", view)
			}
			restarted, err := New(cfg)
			if err != nil {
				t.Fatal(err)
			}
			a := restarted.accounts.accounts[0]
			if a.Fingerprint != preset.Fingerprint || a.effectiveUserAgent() != preset.UserAgent || a.APIToken != "secret-a" {
				t.Fatal("client binding lost after restart")
			}
		})
	}
}

func TestLegacyMixedFingerprintPreservesUserAgent(t *testing.T) {
	cfg := Config{AuthToken: "client-secret", AccountsFile: filepath.Join(t.TempDir(), "accounts.json")}
	accounts := []account{
		{Name: "legacy-claude", UserAgent: userAgentTemplates[1].UserAgent, Fingerprint: "firefox_147"},
		{Name: "legacy-custom", UserAgent: "Custom/1.0", Fingerprint: "chrome_152"},
		{Name: "legacy-matched", UserAgent: findFingerprint("chrome_152").UserAgent, Fingerprint: "chrome_152"},
		{Name: "legacy-default", Fingerprint: "firefox_147"},
	}
	for i := range accounts {
		accounts[i].ID = fmt.Sprintf("%064x", i+1)
		accounts[i].AccountID = fmt.Sprintf("%032x", i+1)
		accounts[i].APIToken = fmt.Sprintf("secret-%d", i)
		accounts[i].Enabled = true
	}
	data, err := json.Marshal(struct {
		Version  int       `json:"version"`
		Accounts []account `json:"accounts"`
	}{1, accounts})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg.AccountsFile, data, 0600); err != nil {
		t.Fatal(err)
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatal("legacy mixed pair prevented startup", err)
	}
	for i, a := range s.accounts.accounts {
		wantFingerprint := accounts[i].Fingerprint
		if i < 2 {
			wantFingerprint = ""
		}
		if a.Fingerprint != wantFingerprint || a.UserAgent != accounts[i].UserAgent || a.APIToken != accounts[i].APIToken || !a.Enabled {
			t.Fatal("legacy normalization changed account identity", i)
		}
	}
	unchanged, err := os.ReadFile(cfg.AccountsFile)
	if err != nil || !bytes.Equal(unchanged, data) {
		t.Fatal("loading rewrote the account store", err)
	}
	if _, err := s.accounts.update(accounts[0].ID, accountUpdate{Name: ptr("renamed")}, false); err != nil {
		t.Fatal(err)
	}
	restarted, err := New(cfg)
	if err != nil || restarted.accounts.accounts[0].Fingerprint != "" || restarted.accounts.accounts[0].UserAgent != accounts[0].UserAgent {
		t.Fatal("legacy normalization was not preserved on save", err)
	}
}
