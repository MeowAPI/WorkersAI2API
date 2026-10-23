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
	"testing/iotest"
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
func TestAccountCooldownRetriesUntilExhausted(t *testing.T) {
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
	w := request(s, "/v1/chat/completions", body)
	if calls != 2 || w.Code != 429 || w.Header().Get("Retry-After") != "120" {
		t.Fatal(calls, w.Code, w.Header())
	}
	if strings.Contains(w.Body.String(), "secret-a") || strings.Contains(w.Body.String(), "secret-b") {
		t.Fatal("attempted account token leaked")
	}
	w = request(s, "/v1/chat/completions", body)
	if calls != 2 || w.Code != 503 || w.Header().Get("Retry-After") == "" {
		t.Fatal(calls, w.Code, w.Body)
	}
	for _, v := range s.accounts.views() {
		if v.Failures != 1 || v.CooldownUntil == nil {
			t.Fatal(v)
		}
	}
}

type accountResponseBody struct {
	io.Reader
	closed bool
}

func (b *accountResponseBody) Close() error {
	b.closed = true
	return nil
}

func TestAccount429RetriesAcrossProtocolsAndMedia(t *testing.T) {
	cases := []struct {
		name, path, body, response, contentType string
	}{
		{"chat", "/v1/chat/completions", `{"model":"@cf/test/model","messages":[{"role":"user","content":"hi"}],"seed":9007199254740993}`, chatJSON, "application/json"},
		{"responses", "/v1/responses", `{"model":"@cf/test/model","input":"hi","prompt_cache_key":"shared-session"}`, chatJSON, "application/json"},
		{"anthropic", "/v1/messages", `{"model":"@cf/test/model","max_tokens":32,"messages":[{"role":"user","content":"hi"}]}`, chatJSON, "application/json"},
		{"gemini", "/v1beta/models/model:generateContent", `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`, chatJSON, "application/json"},
		{"embeddings", "/v1/embeddings", `{"model":"@cf/test/model","input":"hello"}`, `{"object":"list","data":[]}`, "application/json"},
	}
	for _, protocol := range []string{"chat", "responses", "anthropic", "gemini"} {
		path, body := streamRequest(protocol)
		cases = append(cases, struct {
			name, path, body, response, contentType string
		}{protocol + "-stream", path, body, chatSSE(t, true), "text/event-stream"})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, err := New(Config{AuthToken: "client-secret"})
			if err != nil {
				t.Fatal(err)
			}
			var lastID string
			for _, letter := range []string{"a", "b", "c"} {
				lastID = addTestAccount(t, s, strings.Repeat(letter, 32), "secret-"+letter)
			}
			s.catalog.routes = map[string]string{"model": "@cf/test/model", "test": "@cf/test/model"}
			calls := 0
			var firstBody, firstPath, firstContentType, firstAccept string
			var responses []*accountResponseBody
			s.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				if calls > 3 {
					t.Fatal("retried an account twice")
				}
				if calls > 1 && !responses[len(responses)-1].closed {
					t.Error("previous response was not closed before retry")
				}
				data, err := io.ReadAll(r.Body)
				if err != nil {
					t.Fatal(err)
				}
				if calls == 1 {
					firstBody = string(data)
					firstPath = strings.TrimPrefix(r.URL.Path, "/client/v4/accounts/"+strings.Repeat("a", 32)+"/ai")
					firstContentType, firstAccept = r.Header.Get("Content-Type"), r.Header.Get("Accept")
				}
				letter := string(rune('a' + calls - 1))
				wantPath := "/client/v4/accounts/" + strings.Repeat(letter, 32) + "/ai" + firstPath
				if r.Method != http.MethodPost || r.URL.Path != wantPath || r.Header.Get("Authorization") != "Bearer secret-"+letter {
					t.Error("retry used the wrong account or route", r.URL.Path)
				}
				if string(data) != firstBody || r.Header.Get("Content-Type") != firstContentType || r.Header.Get("Accept") != firstAccept {
					t.Error("retry changed the request")
				}
				if tc.name == "responses" && r.Header.Get("X-Session-Affinity") != "shared-session" {
					t.Error("retry lost session affinity")
				}
				status, contentType, body := 200, tc.contentType, tc.response
				headers := http.Header{"Cf-Ray": []string{"success-ray"}}
				if calls < 3 {
					status, contentType, body = 429, "application/json", `{"error":{"message":"rate limited secret-a secret-b"}}`
					headers.Set("Retry-After", "120")
					headers.Set("Cf-Ray", "rejected-ray")
				}
				headers.Set("Content-Type", contentType)
				response := &accountResponseBody{Reader: strings.NewReader(body)}
				if calls < 3 {
					response.Reader = iotest.ErrReader(io.ErrUnexpectedEOF)
				}
				responses = append(responses, response)
				return &http.Response{StatusCode: status, Header: headers, Body: response}, nil
			})
			w := request(s, tc.path, tc.body)
			if calls != 3 || w.Code != 200 || w.Header().Get("Retry-After") != "" || strings.Contains(w.Body.String(), "rate limited") {
				t.Fatal(calls, w.Code, w.Header(), w.Body)
			}
			for _, response := range responses {
				if !response.closed {
					t.Error("response body was not closed")
				}
			}
			views := s.accounts.views()
			for i, view := range views {
				if view.Requests != 1 || i < 2 && (view.Failures != 1 || view.CooldownUntil == nil) || i == 2 && (view.Failures != 0 || view.CooldownUntil != nil) {
					t.Fatal(view)
				}
			}
			records := recordedLogs(s)
			if len(records) != 1 || records[0].Outcome != "success" || records[0].UpstreamCalls != 3 || records[0].AccountID != lastID {
				t.Fatal(records)
			}
			entry, err := s.logs.get(records[0].ID)
			if err != nil {
				t.Fatal(err)
			}
			for i, step := range entry.Upstream {
				if i < 2 && step.Status != 429 || i == 2 && step.Status != 200 {
					t.Fatal(step)
				}
				if strings.Contains(step.Response.Text, "secret-") {
					t.Error("retry log leaked an account token")
				}
			}
		})
	}
}

func TestAccount429RetrySkipsUnavailableAndAttemptedAccounts(t *testing.T) {
	s, err := New(Config{AuthToken: "client-secret"})
	if err != nil {
		t.Fatal(err)
	}
	first := addTestAccount(t, s, strings.Repeat("a", 32), "secret-a")
	disabled := addTestAccount(t, s, strings.Repeat("b", 32), "secret-b")
	cooling := addTestAccount(t, s, strings.Repeat("c", 32), "secret-c")
	addTestAccount(t, s, strings.Repeat("d", 32), "secret-d")
	if _, err := s.accounts.update(disabled, accountUpdate{Enabled: ptr(false)}, false); err != nil {
		t.Fatal(err)
	}
	until := time.Now().Add(time.Hour)
	s.accounts.stats[cooling] = &accountStats{CooldownUntil: &until}
	var calls []string
	s.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls = append(calls, r.Header.Get("Authorization"))
		if len(calls) > 2 {
			t.Fatal("retried an unavailable or previously attempted account")
		}
		if len(calls) == 2 {
			// An expired cooldown or concurrent admin update must not replay an attempt.
			s.accounts.mu.Lock()
			s.accounts.stats[first].CooldownUntil = nil
			s.accounts.mu.Unlock()
		}
		return &http.Response{StatusCode: 429, Body: io.NopCloser(strings.NewReader(`{"error":{"message":"quota"}}`))}, nil
	})
	w := request(s, "/v1/chat/completions", `{"model":"@cf/test/model","messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != 429 || strings.Join(calls, ",") != "Bearer secret-a,Bearer secret-d" {
		t.Fatal(calls, w.Code, w.Body)
	}
}

func TestAccountDoesNotRetryOtherFailuresOrCanceledRequests(t *testing.T) {
	for _, status := range []int{0, 400, 401, 403, 500, 429} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			s, err := New(Config{AuthToken: "client-secret"})
			if err != nil {
				t.Fatal(err)
			}
			addTestAccount(t, s, strings.Repeat("a", 32), "secret-a")
			addTestAccount(t, s, strings.Repeat("b", 32), "secret-b")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls := 0
			s.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				if status == 0 {
					return nil, fmt.Errorf("connection failed")
				}
				if status == 429 {
					cancel()
				}
				return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader("failed"))}, nil
			})
			resp, err := s.upstream(ctx, httptest.NewRecorder(), "/v1/chat/completions", "application/json", "", nil, nil)
			if resp != nil {
				resp.Body.Close()
			}
			if calls != 1 || (err != nil) != (status == 0) {
				t.Fatal(calls, err)
			}
		})
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
		resp, err := s.upstream(ctx, httptest.NewRecorder(), "/run/@cf/test/model", "application/json", "", nil, nil)
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

func TestAccount429RetryKeepsBatchBindingAndMultipartBody(t *testing.T) {
	s, err := New(Config{AuthToken: "client-secret"})
	if err != nil {
		t.Fatal(err)
	}
	addTestAccount(t, s, strings.Repeat("a", 32), "secret-a")
	second := addTestAccount(t, s, strings.Repeat("b", 32), "secret-b")
	addTestAccount(t, s, strings.Repeat("c", 32), "secret-c")
	binding := &accountBinding{}
	ctx := context.WithValue(context.Background(), accountBindingKey{}, binding)
	body := []byte("--test-boundary\r\nContent-Disposition: form-data; name=\"prompt\"\r\n\r\nhello\r\n--test-boundary--\r\n")
	contentType := "multipart/form-data; boundary=test-boundary"
	calls := 0
	s.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		wantToken := "secret-a"
		if calls > 2 {
			wantToken = "secret-b"
		}
		data, err := io.ReadAll(r.Body)
		if err != nil || string(data) != string(body) || r.Header.Get("Content-Type") != contentType || r.Header.Get("Authorization") != "Bearer "+wantToken {
			t.Fatal("batch retry changed the multipart body or account", err)
		}
		status := 200
		if calls == 2 {
			status = 429
		}
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader("ok"))}, nil
	})
	for i := 0; i < 3; i++ {
		resp, err := s.upstream(ctx, httptest.NewRecorder(), "/run/@cf/test/model", contentType, "", body, nil)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != 200 {
			t.Fatal(resp.StatusCode)
		}
		resp.Body.Close()
	}
	if calls != 4 || binding.selected.ID != second {
		t.Fatal(calls, binding.selected)
	}
	next, err := s.accounts.selectAccount(httptest.NewRecorder())
	if err != nil || next.APIToken != "secret-c" {
		t.Fatal("retry disrupted round-robin selection", err)
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
