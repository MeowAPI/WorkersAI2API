package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func ptr[T any](v T) *T { return &v }
func addTestAccount(t *testing.T, s *Server, id, token string) string {
	t.Helper()
	key, err := s.accounts.update("", accountUpdate{Name: ptr("账号 " + id[:4]), AccountID: &id, APIToken: &token}, false)
	if err != nil {
		t.Fatal(err)
	}
	return key
}
func TestRoundRobinAcrossChatAndMedia(t *testing.T) {
	s, err := New(Config{AuthToken: "client-secret"})
	if err != nil {
		t.Fatal(err)
	}
	addTestAccount(t, s, strings.Repeat("a", 32), "secret-a")
	addTestAccount(t, s, strings.Repeat("b", 32), "secret-b")
	s.catalog.routes = map[string]string{"model": "@cf/test/model"}
	var calls []string
	s.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		id := strings.Split(r.URL.Path, "/")[4]
		calls = append(calls, id)
		expected := "secret-" + string(id[0])
		if r.Header.Get("Authorization") != "Bearer "+expected {
			t.Error("account credential mismatch")
		}
		data := chatJSON
		if strings.HasSuffix(r.URL.Path, "/embeddings") {
			data = `{"object":"list","data":[]}`
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(data))}, nil
	})
	for i := 0; i < 4; i++ {
		path, body := "/v1/chat/completions", `{"model":"model","messages":[{"role":"user","content":"hi"}]}`
		if i%2 == 1 {
			path, body = "/v1/embeddings", `{"model":"model","input":"hello"}`
		}
		w := request(s, path, body)
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body)
		}
	}
	if strings.Join(calls, ",") != strings.Repeat("a", 32)+","+strings.Repeat("b", 32)+","+strings.Repeat("a", 32)+","+strings.Repeat("b", 32) {
		t.Fatal(calls)
	}
	for _, v := range s.accounts.views() {
		if v.Requests != 2 {
			t.Fatal(v)
		}
	}
}
func TestAccountCooldownAndNoReplay(t *testing.T) {
	s, err := New(Config{AuthToken: "client-secret"})
	if err != nil {
		t.Fatal(err)
	}
	addTestAccount(t, s, strings.Repeat("a", 32), "secret-a")
	addTestAccount(t, s, strings.Repeat("b", 32), "secret-b")
	calls := 0
	s.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 429, Header: http.Header{"Retry-After": []string{"120"}}, Body: io.NopCloser(strings.NewReader(`{"error":{"message":"quota secret-a secret-b"}}`))}, nil
	})
	body := `{"model":"@cf/test/model","messages":[{"role":"user","content":"hi"}]}`
	for i := 0; i < 2; i++ {
		w := request(s, "/v1/chat/completions", body)
		if w.Code != 429 || w.Header().Get("Retry-After") != "120" {
			t.Fatal(w.Code, w.Header())
		}
		usedToken := "secret-" + string(rune('a'+i))
		if strings.Contains(w.Body.String(), usedToken) {
			t.Fatal("active token leaked")
		}
	}
	w := request(s, "/v1/chat/completions", body)
	if calls != 2 || w.Code != 503 || w.Header().Get("Retry-After") == "" {
		t.Fatal(calls, w.Code, w.Body)
	}
	for _, v := range s.accounts.views() {
		if v.Failures != 1 || v.CooldownUntil == nil {
			t.Fatal(v)
		}
	}
}
func TestAccountStorePersistenceDisableAndAtomicFailure(t *testing.T) {
	file := filepath.Join(t.TempDir(), "data", "accounts.json")
	cfg := Config{AuthToken: "client-secret", AccountsFile: file}
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	first := addTestAccount(t, s, strings.Repeat("a", 32), "seed-secret")
	second := addTestAccount(t, s, strings.Repeat("b", 32), "second-secret")
	_, err = s.accounts.update(first, accountUpdate{Enabled: ptr(false)}, false)
	if err != nil {
		t.Fatal(err)
	}
	next, err := s.accounts.selectAccount(httptest.NewRecorder())
	if err != nil || next.ID != second {
		t.Fatal(next.ID, err)
	}
	t.Setenv("CLOUDFLARE_API_TOKEN", "ignored-legacy-value")
	restart, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(restart.accounts.views()) != 2 || restart.accounts.accounts[0].APIToken != "seed-secret" || restart.accounts.views()[0].Enabled {
		t.Fatal("account persistence lost")
	}
	stat, err := os.Stat(file)
	if err != nil || stat.Mode().Perm() != 0600 {
		t.Fatal(stat, err)
	}
	parent := filepath.Join(t.TempDir(), "not-a-directory")
	os.WriteFile(parent, []byte("x"), 0600)
	restart.accounts.file = filepath.Join(parent, "accounts.json")
	if _, err := restart.accounts.update(second, accountUpdate{Enabled: ptr(false)}, false); err == nil {
		t.Fatal("failed disk write accepted")
	}
	if !restart.accounts.views()[1].Enabled {
		t.Fatal("failed transaction changed live state")
	}
	s.accounts.update(first, accountUpdate{}, true)
	s.accounts.update(second, accountUpdate{}, true)
	restart, err = New(cfg)
	if err != nil || len(restart.accounts.views()) != 0 {
		t.Fatal("deleted account reappeared", err)
	}
	os.WriteFile(file, []byte("invalid secret-do-not-log"), 0600)
	if _, err = New(cfg); err == nil || strings.Contains(err.Error(), "secret-do-not-log") {
		t.Fatal(err)
	}
}
func TestAccountConcurrentSelectionAndUpdates(t *testing.T) {
	s, err := New(Config{AuthToken: "client-secret"})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		addTestAccount(t, s, fmt.Sprintf("%032x", i+1), fmt.Sprintf("secret-%d", i))
	}
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				a, err := s.accounts.selectAccount(httptest.NewRecorder())
				if err != nil {
					t.Error(err)
					return
				}
				s.accounts.record(a, &http.Response{StatusCode: 200}, nil)
				s.accounts.views()
			}
		}()
	}
	wg.Wait()
	for _, v := range s.accounts.views() {
		if v.Requests != 200 {
			t.Fatal(v.Requests)
		}
	}
}
func TestImagesBatchUsesSingleAccount(t *testing.T) {
	s, err := New(Config{AuthToken: "client-secret"})
	if err != nil {
		t.Fatal(err)
	}
	addTestAccount(t, s, strings.Repeat("a", 32), "secret-a")
	addTestAccount(t, s, strings.Repeat("b", 32), "secret-b")
	ctx := context.WithValue(context.Background(), accountBindingKey{}, &accountBinding{})
	s.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Authorization") != "Bearer secret-a" {
			t.Error("request changed accounts")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("ok"))}, nil
	})
	for i := 0; i < 3; i++ {
		resp, err := s.upstream(ctx, httptest.NewRecorder(), "/run/@cf/test/model", "application/json", "", nil)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	next, _ := s.accounts.selectAccount(httptest.NewRecorder())
	if next.APIToken != "secret-b" {
		t.Fatal("unexpected next account")
	}
}
func TestSafeAccountViews(t *testing.T) {
	s, _ := New(Config{AuthToken: "client-secret"})
	addTestAccount(t, s, strings.Repeat("a", 32), "top-secret-api-token")
	data, _ := json.Marshal(s.accounts.views())
	if strings.Contains(string(data), "top-secret") || strings.Contains(string(data), "api_token") {
		t.Fatal("credentials leaked")
	}
	a, _ := s.accounts.selectAccount(httptest.NewRecorder())
	s.accounts.record(a, &http.Response{StatusCode: 500}, nil)
	s.accounts.mu.Lock()
	expired := time.Now().Add(-time.Second)
	s.accounts.stats[a.ID].CooldownUntil = &expired
	s.accounts.mu.Unlock()
	if _, err := s.accounts.selectAccount(httptest.NewRecorder()); err != nil {
		t.Fatal("expired cooldown still blocks", err)
	}
}
