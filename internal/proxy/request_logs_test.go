package proxy

import (
	"bytes"
	"context"
	"encoding/json"
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

func recordedLogs(s *Server) []requestSummary {
	records, _ := s.logs.matching(logFilter{since: time.Now().Add(-logRetention)})
	return records
}
func TestRequestLogBodiesUsageAndCredentialRedaction(t *testing.T) {
	s := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Request-ID", "upstream-id")
		w.Header().Set("Set-Cookie", "upstream-private=value")
		io.WriteString(w, chatJSON)
	})
	raw := `{"model":"test","messages":[{"role":"user","content":"hello upstream-secret client-secret"}],"api_key":"another-user-secret"}`
	r := httptest.NewRequest("POST", "/v1/chat/completions?api_key=hidden-query", strings.NewReader(raw))
	r.Header.Set("Authorization", "Bearer client-secret")
	r.Header.Set("Cookie", "private-cookie=value")
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 200 || w.Body.String() != chatJSON {
		t.Fatal(w.Code, w.Body)
	}
	records := recordedLogs(s)
	if len(records) != 1 {
		t.Fatal(records)
	}
	record := records[0]
	if record.Usage.Total != 19 || record.Usage.Input != 12 || record.Usage.Output != 7 || !record.Usage.Reported || record.Outcome != "success" || record.UpstreamCalls != 1 || record.AccountName == "" || record.Model != "test" || record.UpstreamModel != "@cf/test/model" || record.TTFB == nil {
		t.Fatal(record)
	}
	if w.Header().Get("X-Request-ID") != record.ID {
		t.Fatal("client request ID does not match log ID")
	}
	entry, err := s.logs.get(record.ID)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(entry)
	for _, secret := range []string{"upstream-secret", "client-secret", "another-user-secret", "hidden-query", "private-cookie=value", "upstream-private=value"} {
		if bytes.Contains(data, []byte(secret)) {
			t.Fatalf("credential leaked: %s", secret)
		}
	}
	if !strings.Contains(entry.Request.Text, "hello") || !strings.Contains(entry.Response.Text, "你好") || len(entry.Upstream) != 1 || entry.Upstream[0].Response.Headers["X-Request-Id"][0] != "upstream-id" {
		t.Fatal(entry)
	}
	if entry.Request.Size != int64(len(raw)) || entry.Response.Size != int64(len(chatJSON)) {
		t.Fatal("incorrect payload size")
	}
}
func TestLogsForRejectionAndInterruptedSSE(t *testing.T) {
	s := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n")
	})
	for _, body := range []string{`{"model":"test","messages":[{"role":"user","content":"hello"}]}`, `{"model":"test","messages":[{"role":"user","content":"hello"}],"stream":true}`} {
		w := request(s, "/v1/chat/completions", body)
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body)
		}
	}
	r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"test","messages":[{"role":"user","content":"hello"}]}`))
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatal(w.Code)
	}
	w = request(s, "/v1/chat/completions", `{invalid`)
	if w.Code != 400 {
		t.Fatal(w.Code)
	}
	records := recordedLogs(s)
	if len(records) != 4 {
		t.Fatal(len(records))
	}
	counts := map[string]int{}
	for _, record := range records {
		counts[record.Outcome]++
	}
	if counts["error"] != 2 || counts["interrupted"] != 2 {
		t.Fatal(counts)
	}
}
func TestStreamingUsageAcrossChunksAndCaptureLimit(t *testing.T) {
	c := bodyCapture{contentType: "text/event-stream"}
	first := []byte("data: {\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":2}}\n\n")
	for _, b := range first {
		if n, _ := c.Write([]byte{b}); n != 1 {
			t.Fatal("incorrect writer count")
		}
	}
	c.Write(bytes.Repeat([]byte(": keepalive\n"), maxLogBody/11+1))
	c.Write([]byte("data: {\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":8,\"total_tokens\":18,\"prompt_tokens_details\":{\"cached_tokens\":4},\"completion_tokens_details\":{\"reasoning_tokens\":3}}}\n\ndata: [DONE]\n\n"))
	usage := c.tokens()
	if usage.Input != 10 || usage.Output != 8 || usage.Total != 18 || usage.Cached != 4 || usage.Reasoning != 3 || !usage.Reported || !c.done {
		t.Fatal(usage, c.done)
	}
	if len(c.data) != maxLogBody || c.size <= maxLogBody {
		t.Fatal("capture not bounded")
	}
}
func TestLoggedSSEIsNotBufferedBeforeClient(t *testing.T) {
	firstRead := make(chan struct{})
	release := make(chan struct{})
	s := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"first\"}}]}\n\n")
		w.(http.Flusher).Flush()
		<-release
		io.WriteString(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2,\"total_tokens\":5}}\n\ndata: [DONE]\n\n")
	})
	front := httptest.NewServer(s)
	defer front.Close()
	go func() {
		defer close(firstRead)
		req, _ := http.NewRequest("POST", front.URL+"/v1/chat/completions", strings.NewReader(`{"model":"test","messages":[{"role":"user","content":"hello"}],"stream":true}`))
		req.Header.Set("Authorization", "Bearer client-secret")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Error(err)
			return
		}
		defer resp.Body.Close()
		buf := make([]byte, 1)
		if _, err = resp.Body.Read(buf); err != nil {
			t.Error(err)
		}
		close(release)
		io.Copy(io.Discard, resp.Body)
	}()
	select {
	case <-firstRead:
	case <-time.After(3 * time.Second):
		close(release)
		t.Fatal("logging buffered streaming response")
	}
	deadline := time.Now().Add(time.Second)
	for len(recordedLogs(s)) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	records := recordedLogs(s)
	if len(records) != 1 || records[0].Usage.Total != 5 || records[0].Outcome != "success" {
		t.Fatal(records)
	}
}
func TestMediaPayloadSnapshotsOmitBinaryAndPreserveFields(t *testing.T) {
	input := bodyCapture{contentType: "application/json"}
	input.Write([]byte(`{"model":"m","prompt":"a cat","image_b64":"very-large-base64","audio":[1,2,3],"messages":[{"content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,hidden-image-data"}}]}]}`))
	payload := payloadSnapshot(&input, nil, nil)
	if strings.Contains(payload.Text, "very-large-base64") || strings.Contains(payload.Text, "hidden-image-data") || !strings.Contains(payload.Text, "a cat") {
		t.Fatal(payload.Text)
	}
	binary := bodyCapture{contentType: "audio/wav"}
	binary.Write([]byte{0xff, 0, 1})
	snap := payloadSnapshot(&binary, nil, nil)
	if snap.Kind != "binary" || snap.Size != 3 {
		t.Fatal(snap)
	}
	s := testServerForPath(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"result":{"text":"hello"}}`)
	}, "/run/@cf/openai/whisper-large-v3-turbo")
	s.catalog.routes["whisper-large-v3-turbo"] = "@cf/openai/whisper-large-v3-turbo"
	w := uploadRequest(t, s, "/v1/audio/transcriptions", map[string]string{"model": "whisper-large-v3-turbo", "response_format": "json"}, map[string][]byte{"file": []byte("test-audio-binary")})
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	entry, _ := s.logs.get(recordedLogs(s)[0].ID)
	if entry.Request.Kind != "multipart" || !strings.Contains(entry.Request.Text, "response_format") || strings.Contains(entry.Request.Text, "test-audio-binary") || entry.Model != "whisper-large-v3-turbo" {
		t.Fatal(entry.Request)
	}
}
func TestRequestLogsPersistFilterAndExport(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "logs")
	store, err := newRequestLogStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for i := 0; i < 4; i++ {
		status, outcome := 200, "success"
		if i == 3 {
			status, outcome = 429, "error"
		}
		entry := requestLog{requestSummary: requestSummary{ID: randomID(), Started: now.Add(time.Duration(-i) * time.Minute), Finished: now, Model: "model", AccountID: "account", Status: status, Outcome: outcome, DurationMS: float64(i+1) * 100, Usage: tokenUsage{Reported: i < 2, Input: 10, Output: 5, Total: 15}}, Upstream: []upstreamLog{}}
		if err = store.add(entry); err != nil {
			t.Fatal(err)
		}
	}
	files, _ := os.ReadDir(dir)
	if len(files) != 4 {
		t.Fatal(len(files))
	}
	stat, _ := files[0].Info()
	if stat.Mode().Perm() != 0600 {
		t.Fatal(stat.Mode())
	}
	restart, err := newRequestLogStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	s, _ := New(Config{AuthToken: "client-secret"})
	s.logs = restart
	cookie, _ := loginAdmin(t, s)
	w := adminRequest(s, "GET", "/admin/api/logs?page=2&page_size=2&range=all", "", cookie, "")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	data := object(t, w.Body.Bytes())
	if data["total"] != float64(4) || len(data["data"].([]any)) != 2 {
		t.Fatal(data)
	}
	w = adminRequest(s, "GET", "/admin/api/logs?status=error", "", cookie, "")
	data = object(t, w.Body.Bytes())
	if data["total"] != float64(1) {
		t.Fatal(data)
	}
	id := data["data"].([]any)[0].(map[string]any)["id"].(string)
	w = adminRequest(s, "GET", "/admin/api/logs/"+id, "", cookie, "")
	if w.Code != 200 || object(t, w.Body.Bytes())["request"] == nil {
		t.Fatal(w.Code, w.Body)
	}
	w = adminRequest(s, "GET", "/admin/api/logs/export?status=error", "", cookie, "")
	if w.Code != 200 || w.Header().Get("Content-Type") != "application/x-ndjson" || bytes.Count(w.Body.Bytes(), []byte("\n")) != 1 {
		t.Fatal(w.Code, w.Body)
	}
	w = adminRequest(s, "GET", "/admin/api/stats?range=7d", "", cookie, "")
	data = object(t, w.Body.Bytes())
	summary := data["summary"].(map[string]any)
	if summary["requests"] != float64(4) || summary["success_rate"] != float64(75) || summary["usage_reported_requests"] != float64(2) || summary["p95_duration_ms"] != float64(400) {
		t.Fatal(summary)
	}
	sum := float64(0)
	for _, bucket := range data["timeline"].([]any) {
		sum += bucket.(map[string]any)["requests"].(float64)
	}
	if sum != 4 {
		t.Fatal("timeline totals mismatch")
	}
	for _, path := range []string{"/admin/api/logs", "/admin/api/logs/" + id, "/admin/api/logs/export", "/admin/api/stats"} {
		w = adminRequest(s, "GET", path, "", nil, "")
		if w.Code != 401 {
			t.Fatal("logs are not protected", path, w.Code)
		}
	}
	for _, query := range []string{"range=bad", "page=-1", "page_size=99999", "status=unknown"} {
		w = adminRequest(s, "GET", "/admin/api/logs?"+query, "", cookie, "")
		if w.Code != 400 {
			t.Fatal(query, w.Code)
		}
	}
}
func TestLogRotationAndDiskFailureDoesNotBreakInference(t *testing.T) {
	store, _ := newRequestLogStore(t.TempDir())
	old := requestLog{requestSummary: requestSummary{ID: randomID(), Started: time.Now().Add(-8 * 24 * time.Hour), Finished: time.Now().Add(-8 * 24 * time.Hour)}}
	if err := store.add(old); err != nil {
		t.Fatal(err)
	}
	records, _ := store.matching(logFilter{})
	if len(records) != 0 {
		t.Fatal("expired record retained")
	}
	s := testServer(t, func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, chatJSON) })
	parent := filepath.Join(t.TempDir(), "not-a-dir")
	os.WriteFile(parent, []byte("x"), 0600)
	s.logs.dir = filepath.Join(parent, "logs")
	w := request(s, "/v1/chat/completions", `{"model":"test","messages":[{"role":"user","content":"hello"}]}`)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	_, storage := s.logs.matching(logFilter{})
	if storage.LastError == "" {
		t.Fatal("storage failure hidden")
	}
}
func TestLoggingConcurrentRequestsAndCancellation(t *testing.T) {
	s := testServer(t, func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, chatJSON) })
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := request(s, "/v1/chat/completions", `{"model":"test","messages":[{"role":"user","content":"hello"}]}`)
			if w.Code != 200 {
				t.Error(w.Code)
			}
		}()
	}
	wg.Wait()
	if len(recordedLogs(s)) != 20 || s.activeRequests.Load() != 0 {
		t.Fatal("lost requests")
	}
	r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"test","messages":[{"role":"user","content":"hello"}]}`))
	r.Header.Set("Authorization", "Bearer client-secret")
	ctx, cancel := context.WithCancel(r.Context())
	cancel()
	r = r.WithContext(ctx)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	records := recordedLogs(s)
	if len(records) != 21 || records[0].Outcome != "canceled" {
		t.Fatal(records[0])
	}
}
func TestNoCloudflareEnvironmentBootstrap(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("AUTH_TOKEN", "client-secret")
	t.Setenv("CLOUDFLARE_ACCOUNT_ID", "invalid-legacy-id")
	t.Setenv("CLOUDFLARE_API_TOKEN", "invalid old token")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.accounts.views()) != 0 {
		t.Fatal("environment credentials seeded account")
	}
}

func TestNonTokenUsageIsNotCountedAsReportedTokens(t *testing.T) {
	for _, body := range []string{`{"usage":{}}`, `{"result":{"usage":{"unit":"audio_seconds","value":4.5}}}`, `{"usage":{"prompt_tokens":null}}`} {
		capture := bodyCapture{contentType: "application/json"}
		capture.Write([]byte(body))
		if capture.tokens().Reported {
			t.Fatal("non-token usage counted as token report", body)
		}
	}
	capture := bodyCapture{contentType: "application/json"}
	capture.Write([]byte(`{"usage":{"prompt_tokens":0,"completion_tokens":0}}`))
	if !capture.tokens().Reported {
		t.Fatal("actual zero usage not reported")
	}
}

func TestIncompleteJSONAndMultipartValuesDoNotLeakCredentials(t *testing.T) {
	capture := bodyCapture{contentType: "application/json"}
	capture.Write([]byte(`{"password":"third-party-secret",`))
	payload := payloadSnapshot(&capture, nil, nil)
	if strings.Contains(payload.Text, "third-party-secret") {
		t.Fatal("invalid JSON leaked credential")
	}
	value := cleanValue(map[string]any{"prompt": []string{"first secret-value", "second secret-value"}}, []string{"secret-value"})
	data, _ := json.Marshal(value)
	if bytes.Contains(data, []byte("secret-value")) {
		t.Fatal("repeated form values not redacted")
	}
	headers := http.Header{"X-Auth-Token": []string{"other-token"}, "Cf-Access-Client-Secret": []string{"other-secret"}}
	data, _ = json.Marshal(safeHeaders(headers, nil))
	if bytes.Contains(data, []byte("other-")) {
		t.Fatal("credential headers not redacted")
	}
}
