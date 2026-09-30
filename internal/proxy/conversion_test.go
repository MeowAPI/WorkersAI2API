package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"testing"
)

func TestResponsesToolHistoryAndImage(t *testing.T) {
	body, _, _, err := convertResponsesToChat([]byte(`{"model":"test","input":[{"role":"user","content":[{"type":"input_image","image_url":"https://example.com/image.png","detail":"low"}]},{"type":"function_call","call_id":"a","name":"weather","arguments":"{}"},{"type":"function_call","call_id":"b","name":"weather","arguments":"{}"},{"type":"function_call_output","call_id":"a","output":"one"},{"type":"function_call_output","call_id":"b","output":"two"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	messages := object(t, body)["messages"].([]any)
	if len(messages) != 4 {
		t.Fatalf("parallel calls not grouped: %s", body)
	}
	calls := messages[1].(map[string]any)["tool_calls"].([]any)
	if len(calls) != 2 {
		t.Fatal(calls)
	}
	part := messages[0].(map[string]any)["content"].([]any)[0].(map[string]any)
	image := part["image_url"].(map[string]any)
	if image["url"] != "https://example.com/image.png" || image["detail"] != "low" {
		t.Fatal(image)
	}
}

func TestAnthropicToolResultsPrecedeText(t *testing.T) {
	body, _, _, err := convertAnthropicMessagesToChat([]byte(`{"model":"test","messages":[{"role":"assistant","content":[{"type":"tool_use","id":"a","name":"weather","input":{}}]},{"role":"user","content":[{"type":"text","text":"continue"},{"type":"tool_result","tool_use_id":"a","content":"sunny"}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	messages := object(t, body)["messages"].([]any)
	if len(messages) != 3 || messages[1].(map[string]any)["role"] != "tool" || messages[2].(map[string]any)["role"] != "user" {
		t.Fatal(string(body))
	}
}

func TestGeminiRepeatedToolNames(t *testing.T) {
	body, err := convertNativeGeminiToChat("test", []byte(`{"contents":[{"role":"model","parts":[{"functionCall":{"name":"weather","args":{"city":"A"}}},{"functionCall":{"name":"weather","args":{"city":"B"}}}]},{"role":"user","parts":[{"functionResponse":{"name":"weather","response":{"value":1}}},{"functionResponse":{"name":"weather","response":{"value":2}}}]}]}`), false)
	if err != nil {
		t.Fatal(err)
	}
	messages := object(t, body)["messages"].([]any)
	first := messages[1].(map[string]any)["tool_call_id"]
	second := messages[2].(map[string]any)["tool_call_id"]
	if first == second || first != "call_0" || second != "call_1" {
		t.Fatal(string(body))
	}
}

func TestAliasPreservesLargeInteger(t *testing.T) {
	s := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		var body map[string]json.RawMessage
		json.Unmarshal(data, &body)
		if string(body["seed"]) != "9007199254740993" {
			t.Errorf("integer changed: %s", data)
		}
		io.WriteString(w, chatJSON)
	})
	w := request(s, "/v1/chat/completions", `{"model":"test","messages":[{"role":"user","content":"hi"}],"seed":9007199254740993}`)
	if w.Code != 200 {
		t.Fatal(w.Body)
	}
}

func TestInvalidUpstreamResponse(t *testing.T) {
	s := testServer(t, func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, `{"success":false}`) })
	w := request(s, "/v1/responses", `{"model":"test","input":"hi"}`)
	if w.Code != 502 {
		t.Fatalf("malformed response accepted: %d %s", w.Code, w.Body)
	}
}

func TestLoadConfig(t *testing.T) {
	for name, value := range map[string]string{"AUTH_TOKEN": "client-key", "PORT": "8080", "BIND_ADDRESS": "127.0.0.1"} {
		t.Setenv(name, value)
	}
	// Removed options are ignored, including stale invalid values from old .env files.
	for _, key := range []string{"CLOUDFLARE_ACCOUNT_ID", "CLOUDFLARE_API_TOKEN", "REQUEST_TIMEOUT", "AUTO_MODELS", "MODEL_REFRESH_INTERVAL", "MODELS", "MODEL_ALIASES", "PUBLIC_BASE_URL"} {
		t.Setenv(key, "invalid-old-value")
	}
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Address != "127.0.0.1:8080" || cfg.AccountsFile != "data/accounts.json" {
		t.Fatal(cfg)
	}
	for _, tc := range []struct{ name, value string }{{"AUTH_TOKEN", ""}, {"PORT", "65536"}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(tc.name, tc.value)
			if _, err := LoadConfig(); err == nil {
				t.Error("invalid config accepted")
			}
		})
	}
}
