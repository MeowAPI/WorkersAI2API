package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAccountUserAgentOnWireAndLogs(t *testing.T) {
	for _, http2 := range []bool{false, true} {
		for _, ua := range []string{"", "Mozilla/5.0 (Account A) Custom/1.0"} {
			t.Run(fmt.Sprintf("http2=%t/ua=%s", http2, ua), func(t *testing.T) {
				want := ua
				proto := 1
				if http2 {
					proto = 2
				}
				if want == "" {
					want = fmt.Sprintf("Go-http-client/%d.0", proto)
					if proto == 1 {
						want = "Go-http-client/1.1"
					}
				}
				upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.ProtoMajor != proto {
						t.Errorf("upstream protocol = %d, want %d", r.ProtoMajor, proto)
					}
					if r.UserAgent() != want || len(r.Header.Values("User-Agent")) != 1 {
						t.Errorf("upstream User-Agent = %q, want %q", r.Header.Values("User-Agent"), want)
					}
					w.Header().Set("Content-Type", "application/json")
					io.WriteString(w, chatJSON)
				}))
				upstream.EnableHTTP2 = http2
				upstream.StartTLS()
				t.Cleanup(upstream.Close)
				s, err := New(Config{AuthToken: "client-secret"})
				if err != nil {
					t.Fatal(err)
				}
				id := addTestAccount(t, s, strings.Repeat("a", 32), "secret-a")
				if _, err := s.accounts.update(id, accountUpdate{UserAgent: &ua}, false); err != nil {
					t.Fatal(err)
				}
				target, err := url.Parse(upstream.URL)
				if err != nil {
					t.Fatal(err)
				}
				transport := upstream.Client().Transport
				s.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
					r = r.Clone(r.Context())
					r.URL.Scheme, r.URL.Host = target.Scheme, target.Host
					return transport.RoundTrip(r)
				})
				r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"@cf/test/model","messages":[{"role":"user","content":"hi"}]}`))
				r.Header.Set("Authorization", "Bearer client-secret")
				r.Header.Set("User-Agent", "Untrusted-Client/9.0")
				w := httptest.NewRecorder()
				s.ServeHTTP(w, r)
				if w.Code != http.StatusOK {
					t.Fatal(w.Code, w.Body)
				}
				records := recordedLogs(s)
				if len(records) != 1 {
					t.Fatal(records)
				}
				entry, err := s.logs.get(records[0].ID)
				if err != nil {
					t.Fatal(err)
				}
				if len(entry.Upstream) != 1 || http.Header(entry.Upstream[0].Request.Headers).Get("User-Agent") != ua {
					t.Fatal("upstream log lost the configured User-Agent", entry.Upstream)
				}
			})
		}
	}
}

func TestAccountUserAgentBindingSurvivesEdits(t *testing.T) {
	s, err := New(Config{AuthToken: "client-secret"})
	if err != nil {
		t.Fatal(err)
	}
	id := addTestAccount(t, s, strings.Repeat("a", 32), "secret-a")
	if _, err := s.accounts.update(id, accountUpdate{UserAgent: ptr("Bound/1.0")}, false); err != nil {
		t.Fatal(err)
	}
	calls := 0
	s.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		want := "Bound/1.0"
		if calls == 3 {
			want = "Updated/2.0"
		}
		if r.UserAgent() != want || len(r.Header.Values("User-Agent")) != 1 {
			t.Errorf("call %d User-Agent = %q, want %q", calls, r.Header.Values("User-Agent"), want)
		}
		if calls == 1 {
			if _, err := s.accounts.update(id, accountUpdate{UserAgent: ptr("Updated/2.0")}, false); err != nil {
				t.Fatal(err)
			}
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("ok"))}, nil
	})
	bound := context.WithValue(context.Background(), accountBindingKey{}, &accountBinding{})
	for _, ctx := range []context.Context{bound, bound, context.Background()} {
		resp, err := s.upstream(ctx, httptest.NewRecorder(), "/run/@cf/test/model", "application/json", "", nil, http.Header{"User-Agent": {"Extra-Header/9.0"}})
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	if calls != 3 {
		t.Fatal(calls)
	}
}

func TestAdminAccountUserAgentPersistenceAndClearing(t *testing.T) {
	cfg := Config{AuthToken: "client-secret", AccountsFile: filepath.Join(t.TempDir(), "accounts.json")}
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	cookie, csrf := loginAdmin(t, s)
	w := adminRequest(s, "POST", "/admin/api/accounts", `{"name":"first","account_id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","api_token":"secret-a","user_agent":"  Account A/1.0 (test)  "}`, cookie, csrf)
	if w.Code != http.StatusCreated {
		t.Fatal(w.Code, w.Body)
	}
	view := object(t, w.Body.Bytes())
	id := view["id"].(string)
	want := "Account A/1.0 (test)"
	if view["user_agent"] != want {
		t.Fatal(view)
	}
	w = adminRequest(s, "PATCH", "/admin/api/accounts/"+id, `{"name":"renamed","api_token":"","enabled":false}`, cookie, csrf)
	if w.Code != http.StatusOK || object(t, w.Body.Bytes())["user_agent"] != want {
		t.Fatal("unrelated edit changed User-Agent", w.Code, w.Body)
	}
	restarted, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	a := restarted.accounts.accounts[0]
	if a.UserAgent != want || a.APIToken != "secret-a" || a.Enabled {
		t.Fatal("restart lost account settings")
	}
	cookie, csrf = loginAdmin(t, restarted)
	w = adminRequest(restarted, "GET", "/admin/api/state", "", cookie, "")
	if w.Code != http.StatusOK || object(t, w.Body.Bytes())["accounts"].([]any)[0].(map[string]any)["user_agent"] != want {
		t.Fatal("admin state lost User-Agent", w.Code, w.Body)
	}
	w = adminRequest(restarted, "PATCH", "/admin/api/accounts/"+id, `{"user_agent":"Updated/2.0"}`, cookie, csrf)
	if w.Code != http.StatusOK || object(t, w.Body.Bytes())["user_agent"] != "Updated/2.0" {
		t.Fatal(w.Code, w.Body)
	}
	w = adminRequest(restarted, "PATCH", "/admin/api/accounts/"+id, `{"user_agent":""}`, cookie, csrf)
	if w.Code != http.StatusOK || object(t, w.Body.Bytes())["user_agent"] != "" {
		t.Fatal("User-Agent could not be cleared", w.Code, w.Body)
	}
	restarted, err = New(cfg)
	if err != nil || restarted.accounts.accounts[0].UserAgent != "" {
		t.Fatal("cleared User-Agent reappeared after restart", err)
	}
}

func TestAccountUserAgentValidation(t *testing.T) {
	s, err := New(Config{AuthToken: "client-secret"})
	if err != nil {
		t.Fatal(err)
	}
	id := addTestAccount(t, s, strings.Repeat("a", 32), "secret-a")
	if _, err := s.accounts.update(id, accountUpdate{UserAgent: ptr("Original/1.0")}, false); err != nil {
		t.Fatal(err)
	}
	cookie, csrf := loginAdmin(t, s)
	for _, ua := range []string{"Agent\r\nX-Injected: value", "\nAgent", "Agent\r", "Agent\t1.0", "Agent\x00", "Agent\x1f", "Agent\x7f", "Agent\u00e9", strings.Repeat("a", 1025)} {
		body, err := json.Marshal(map[string]string{"user_agent": ua})
		if err != nil {
			t.Fatal(err)
		}
		w := adminRequest(s, "PATCH", "/admin/api/accounts/"+id, string(body), cookie, csrf)
		if w.Code != http.StatusBadRequest || s.accounts.views()[0].UserAgent != "Original/1.0" {
			t.Fatalf("invalid User-Agent %q changed account: %d %s", ua, w.Code, w.Body)
		}
	}
	ua := strings.Repeat("a", 1024)
	if _, err := s.accounts.update(id, accountUpdate{UserAgent: &ua}, false); err != nil {
		t.Fatal("maximum valid User-Agent rejected", err)
	}
}

func TestLegacyAccountStoreWithoutUserAgent(t *testing.T) {
	file := filepath.Join(t.TempDir(), "accounts.json")
	data := `{"version":1,"accounts":[{"id":"` + strings.Repeat("1", 64) + `","name":"legacy","account_id":"` + strings.Repeat("a", 32) + `","api_token":"secret-a","enabled":true}]}`
	if err := os.WriteFile(file, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := Config{AuthToken: "client-secret", AccountsFile: file}
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	a, err := s.accounts.selectAccount(httptest.NewRecorder())
	if err != nil || a.UserAgent != "" {
		t.Fatal("legacy account did not keep the default User-Agent", err)
	}
	if _, err := s.accounts.update(a.ID, accountUpdate{UserAgent: ptr("Legacy/1.0")}, false); err != nil {
		t.Fatal(err)
	}
	restarted, err := New(cfg)
	if err != nil || restarted.accounts.views()[0].UserAgent != "Legacy/1.0" {
		t.Fatal("legacy account upgrade lost User-Agent", err)
	}
}
