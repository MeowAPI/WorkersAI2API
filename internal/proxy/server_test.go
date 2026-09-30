package proxy

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func testServer(t *testing.T, handler http.HandlerFunc) *Server {
	t.Helper()
	return testServerForPath(t, handler, "/v1/chat/completions")
}

func testServerForPath(t *testing.T, handler http.HandlerFunc, path string) *Server {
	t.Helper()
	upstream := httptest.NewServer(handler)
	t.Cleanup(upstream.Close)
	cfg := Config{AuthToken: "client-secret"}
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	addTestAccount(t, s, strings.Repeat("a", 32), "upstream-secret")
	s.catalog.routes = map[string]string{"test": "@cf/test/model", "model": "@cf/test/model"}
	s.requestTimeout = 5 * time.Second
	target, _ := url.Parse(upstream.URL)
	transport := http.DefaultTransport.(*http.Transport).Clone()
	t.Cleanup(transport.CloseIdleConnections)
	s.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != "https://api.cloudflare.com/client/v4/accounts/"+strings.Repeat("a", 32)+"/ai"+path {
			t.Errorf("unexpected upstream: %s", r.URL)
		}
		if r.Header.Get("Authorization") != "Bearer upstream-secret" {
			t.Error("wrong upstream auth")
		}
		if r.Header.Get("X-Api-Key") != "" || r.Header.Get("Cookie") != "" || r.Header.Get("X-Goog-Api-Key") != "" {
			t.Error("client credentials leaked")
		}
		copy := r.Clone(r.Context())
		u := *r.URL
		u.Host = target.Host
		u.Scheme = target.Scheme
		copy.URL = &u
		return transport.RoundTrip(copy)
	})
	return s
}

func request(s *Server, path, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer client-secret")
	r.Header.Set("Cookie", "private=yes")
	r.Header.Set("X-Api-Key", "client-secret")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

func object(t *testing.T, data []byte) map[string]any {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatalf("invalid JSON %s: %v", data, err)
	}
	return v
}

const chatJSON = `{"id":"chatcmpl-test","object":"chat.completion","created":123,"model":"@cf/test/model","choices":[{"index":0,"message":{"role":"assistant","content":"你好","tool_calls":[{"id":"call_1","type":"function","function":{"name":"weather","arguments":"{\"city\":\"北京\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":12,"completion_tokens":7,"total_tokens":19}}`

func TestProtocolRequestsAndResponses(t *testing.T) {
	cases := []struct{ name, path, body string }{
		{"chat", "/v1/chat/completions", `{"model":"test","messages":[{"role":"user","content":"你好"}],"options":{"rejectIfBusy":true}}`},
		{"responses", "/v1/responses", `{"model":"test","input":"你好","instructions":"help","max_output_tokens":42,"tools":[{"type":"function","name":"weather","parameters":{"type":"object"}}]}`},
		{"anthropic", "/v1/messages", `{"model":"test","system":"help","max_tokens":42,"messages":[{"role":"user","content":"你好"}],"tools":[{"name":"weather","input_schema":{"type":"object"}}]}`},
		{"gemini", "/v1beta/models/test:generateContent", `{"systemInstruction":{"parts":[{"text":"help"}]},"contents":[{"role":"user","parts":[{"text":"你好"}]}],"generationConfig":{"maxOutputTokens":42},"tools":[{"functionDeclarations":[{"name":"weather","parameters":{"type":"object"}}]}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := testServer(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "POST" {
					t.Error("wrong method")
				}
				body, _ := io.ReadAll(r.Body)
				raw := object(t, body)
				if raw["model"] != "@cf/test/model" {
					t.Errorf("alias not resolved: %s", body)
				}
				if tc.name != "chat" {
					if raw["max_tokens"] != float64(42) {
						t.Errorf("token limit: %s", body)
					}
					if raw["max_completion_tokens"] != nil {
						t.Error("unadapted token limit")
					}
					if len(raw["tools"].([]any)) != 1 {
						t.Error("tools lost")
					}
				}
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Set-Cookie", "upstream=secret")
				w.Header().Set("Cf-Ray", "trace")
				io.WriteString(w, chatJSON)
			})
			w := request(s, tc.path, tc.body)
			if w.Code != 200 {
				t.Fatalf("%d %s", w.Code, w.Body)
			}
			if w.Header().Get("Set-Cookie") != "" || w.Header().Get("Cf-Ray") != "trace" {
				t.Error("incorrect header filtering")
			}
			raw := object(t, w.Body.Bytes())
			switch tc.name {
			case "chat":
				if w.Body.String() != chatJSON {
					t.Error("chat response changed")
				}
			case "responses":
				if raw["object"] != "response" || raw["status"] != "completed" {
					t.Fatal(raw)
				}
				if len(raw["output"].([]any)) != 2 {
					t.Fatal(raw)
				}
			case "anthropic":
				if raw["type"] != "message" || raw["stop_reason"] != "tool_use" {
					t.Fatal(raw)
				}
				if raw["usage"].(map[string]any)["output_tokens"] != float64(7) {
					t.Fatal(raw)
				}
			case "gemini":
				c := raw["candidates"].([]any)[0].(map[string]any)
				if c["finishReason"] != "STOP" {
					t.Fatal(raw)
				}
			}
		})
	}
}

func TestChatPassthroughPreservesBody(t *testing.T) {
	body := `{ "model":"@cf/test/model", "messages":[{"role":"user","content":"hi"}], "seed":9007199254740993,"options":{"rejectIfBusy":true}}`
	s := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		if string(data) != body {
			t.Errorf("body changed: %s", data)
		}
		io.WriteString(w, chatJSON)
	})
	if w := request(s, "/v1/chat/completions", body); w.Code != 200 {
		t.Fatal(w.Body)
	}
}

func chatSSE(t *testing.T, done bool) string {
	t.Helper()
	var b strings.Builder
	deltas := []map[string]any{
		{"role": "assistant", "content": "你"}, {"content": "好"},
		{"tool_calls": []any{map[string]any{"index": 0, "id": "call_1", "type": "function", "function": map[string]any{"name": "weather", "arguments": "{\"city\":\""}}}},
		{"tool_calls": []any{map[string]any{"index": 0, "function": map[string]any{"arguments": "a"}}}},
		{"tool_calls": []any{map[string]any{"index": 0, "function": map[string]any{"arguments": "a"}}}},
		{"tool_calls": []any{map[string]any{"index": 0, "function": map[string]any{"arguments": "\"}"}}}},
	}
	for _, delta := range deltas {
		data, _ := json.Marshal(map[string]any{"id": "chatcmpl-test", "choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": nil}}})
		fmt.Fprintf(&b, "data: %s\r\n\r\n", data)
	}
	if done {
		b.WriteString("data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\ndata: {\"choices\":[],\"usage\":{\"prompt_tokens\":12,\"completion_tokens\":7,\"total_tokens\":19}}\n\ndata: [DONE]\n\n")
	}
	return b.String()
}

func TestStreamingConversion(t *testing.T) {
	for _, protocol := range []string{"chat", "responses", "anthropic", "gemini"} {
		for _, complete := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/complete=%v", protocol, complete), func(t *testing.T) {
				upstream := chatSSE(t, complete)
				s := testServer(t, func(w http.ResponseWriter, r *http.Request) {
					body, _ := io.ReadAll(r.Body)
					raw := object(t, body)
					if raw["stream"] != true {
						t.Error("stream lost")
					}
					if protocol != "chat" && raw["stream_options"].(map[string]any)["include_usage"] != true {
						t.Error("usage not requested")
					}
					w.Header().Set("Content-Type", "text/event-stream")
					io.WriteString(w, upstream)
				})
				path, body := streamRequest(protocol)
				w := request(s, path, body)
				if w.Code != 200 {
					t.Fatalf("%d %s", w.Code, w.Body)
				}
				if protocol == "chat" {
					if w.Body.String() != upstream {
						t.Error("SSE passthrough changed")
					}
					return
				}
				ending := map[string]string{"responses": "event: response.completed", "anthropic": "event: message_stop", "gemini": "\"finishReason\":\"STOP\""}[protocol]
				if complete && !strings.Contains(w.Body.String(), ending) {
					t.Fatalf("missing terminal event: %s", w.Body)
				}
				if !complete {
					if strings.Contains(w.Body.String(), ending) || !strings.Contains(w.Body.String(), "error") {
						t.Fatalf("truncation not reported: %s", w.Body)
					}
					return
				}
				// Repeated argument fragments must be retained ("a" + "a" = "aa").
				var arguments strings.Builder
				scanner := bufio.NewScanner(strings.NewReader(w.Body.String()))
				for scanner.Scan() {
					line := scanner.Text()
					if !strings.HasPrefix(line, "data: ") {
						continue
					}
					v := object(t, []byte(strings.TrimPrefix(line, "data: ")))
					if v["type"] == "response.function_call_arguments.done" {
						arguments.WriteString(stringValue(v["arguments"]))
					}
					if v["type"] == "content_block_delta" {
						d := v["delta"].(map[string]any)
						if d["type"] == "input_json_delta" {
							arguments.WriteString(stringValue(d["partial_json"]))
						}
					}
					if protocol == "gemini" {
						if candidates, ok := v["candidates"].([]any); ok {
							for _, c := range candidates {
								content := c.(map[string]any)["content"].(map[string]any)
								for _, p := range content["parts"].([]any) {
									if call, ok := p.(map[string]any)["functionCall"].(map[string]any); ok {
										data, _ := json.Marshal(call["args"])
										arguments.Write(data)
									}
								}
							}
						}
					}
				}
				if arguments.String() != `{"city":"aa"}` {
					t.Fatalf("corrupt tool args: %s\n%s", arguments.String(), w.Body)
				}
			})
		}
	}
}

func streamRequest(protocol string) (string, string) {
	switch protocol {
	case "responses":
		return "/v1/responses", `{"model":"test","input":"hi","stream":true}`
	case "anthropic":
		return "/v1/messages", `{"model":"test","max_tokens":20,"messages":[{"role":"user","content":"hi"}],"stream":true}`
	case "gemini":
		return "/v1beta/models/test:streamGenerateContent?alt=sse", `{"contents":[{"parts":[{"text":"hi"}]}]}`
	default:
		return "/v1/chat/completions", `{"model":"test","messages":[{"role":"user","content":"hi"}],"stream":true}`
	}
}

func TestInvalidRequestsNeverReachUpstream(t *testing.T) {
	var count atomic.Int32
	s := testServer(t, func(w http.ResponseWriter, r *http.Request) { count.Add(1); w.WriteHeader(500) })
	for _, tc := range []struct{ path, body string }{
		{"/v1/chat/completions", "null"}, {"/v1/chat/completions", "{}"}, {"/v1/chat/completions", `{"model":"test","messages":[{}],"stream":"true"}`},
		{"/v1/responses", `{"model":"test","input":"hi","previous_response_id":"resp_old"}`},
		{"/v1/responses", `{"model":"test","input":"hi","tools":[{"type":"web_search"}]}`},
		{"/v1/messages", `{"model":"test","messages":[{"role":"admin","content":"hi"}]}`},
		{"/v1beta/models/test:generateContent", `{"contents":[{"parts":[{"fileData":{"fileUri":"gs://secret"}}]}]}`},
	} {
		w := request(s, tc.path, tc.body)
		if w.Code != 400 {
			t.Errorf("%s => %d %s", tc.body, w.Code, w.Body)
		}
	}
	r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Error(w.Code)
	}
	if count.Load() != 0 {
		t.Fatal("invalid request reached upstream")
	}
}

func TestErrorsAndRedirects(t *testing.T) {
	for _, status := range []int{302, 401, 429, 500} {
		for _, protocol := range []string{"chat", "responses", "anthropic", "gemini"} {
			t.Run(fmt.Sprintf("%d/%s", status, protocol), func(t *testing.T) {
				var count atomic.Int32
				s := testServer(t, func(w http.ResponseWriter, r *http.Request) {
					count.Add(1)
					w.Header().Set("Retry-After", "3")
					w.Header().Set("Location", "/elsewhere")
					w.WriteHeader(status)
					io.WriteString(w, `{"success":false,"errors":[{"code":1000,"message":"upstream failed"}]}`)
				})
				path, body := streamRequest(protocol)
				w := request(s, path, body)
				want := status
				if status == 302 {
					want = 502
				}
				if w.Code != want {
					t.Fatalf("got %d", w.Code)
				}
				if count.Load() != 1 {
					t.Error("unexpected retry or redirect")
				}
				if w.Header().Get("Retry-After") != "3" {
					t.Error("retry metadata lost")
				}
				raw := object(t, w.Body.Bytes())
				if raw["error"].(map[string]any)["message"] != "upstream failed" {
					t.Fatal(raw)
				}
			})
		}
	}
}

func TestCatalogAndAuth(t *testing.T) {
	s := testServer(t, func(w http.ResponseWriter, r *http.Request) { t.Error("catalog must be local") })
	for _, path := range []string{"/v1/models", "/v1/models/model", "/v1beta/models", "/v1beta/models/test?key=client-secret"} {
		r := httptest.NewRequest("GET", path, nil)
		if !strings.Contains(path, "?key=") {
			r.Header.Set("x-api-key", "client-secret")
		}
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body)
		}
	}
}

func TestStreamingFlushAndCancellation(t *testing.T) {
	started := make(chan struct{})
	cancelled := make(chan struct{})
	s := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n")
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
		close(cancelled)
	})
	front := httptest.NewServer(s)
	defer front.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "POST", front.URL+"/v1/responses", strings.NewReader(`{"model":"test","input":"hi","stream":true}`))
	req.Header.Set("Authorization", "Bearer client-secret")
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	<-started
	reader := bufio.NewReader(resp.Body)
	line, err := reader.ReadString('\n')
	if err != nil || !strings.HasPrefix(line, "event:") {
		t.Fatalf("not flushed: %s %v", line, err)
	}
	cancel()
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("upstream was not cancelled")
	}
}

func TestRequestTimeout(t *testing.T) {
	s := testServer(t, func(w http.ResponseWriter, r *http.Request) { io.Copy(io.Discard, r.Body); <-r.Context().Done() })
	s.requestTimeout = 30 * time.Millisecond
	w := request(s, "/v1/chat/completions", `{"model":"test","messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != 504 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
}
