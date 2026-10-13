package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestResponsesClientToolsRoundTrip(t *testing.T) {
	tools := []any{
		map[string]any{"type": "custom", "name": "exec", "format": map[string]any{"type": "text"}},
		map[string]any{"type": "namespace", "name": "functions", "tools": []any{
			map[string]any{"type": "function", "name": "wait", "parameters": map[string]any{"type": "object"}},
		}},
		map[string]any{"type": "tool_search"},
		map[string]any{"type": "web_search"},
	}
	requestBody := map[string]any{"model": "test", "input": "hi", "tools": tools}
	s := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		chat := object(t, data)
		converted := chat["tools"].([]any)
		if len(converted) != 3 {
			t.Errorf("executable tools lost: %s", data)
		}
		calls := []any{
			map[string]any{"id": "exec1", "function": map[string]any{"name": "exec", "arguments": `{"input":"echo hello\n"}`}},
			map[string]any{"id": "wait1", "function": map[string]any{"name": "functions__wait", "arguments": `{"cell_id":"1"}`}},
			map[string]any{"id": "search1", "function": map[string]any{"name": "tool_search", "arguments": `{"query":"files"}`}},
		}
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "tool_calls": calls}, "finish_reason": "tool_calls"}}})
	})
	body, _ := json.Marshal(requestBody)
	w := request(s, "/v1/responses", string(body))
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	output := object(t, w.Body.Bytes())["output"].([]any)
	if len(output) != 3 {
		t.Fatal(w.Body)
	}
	custom := output[0].(map[string]any)
	if custom["type"] != "custom_tool_call" || custom["name"] != "exec" || custom["input"] != "echo hello\n" || custom["call_id"] != "exec1" {
		t.Fatal(custom)
	}
	namespace := output[1].(map[string]any)
	if namespace["type"] != "function_call" || namespace["name"] != "wait" || namespace["namespace"] != "functions" {
		t.Fatal(namespace)
	}
	search := output[2].(map[string]any)
	if search["type"] != "tool_search_call" || search["execution"] != "client" || search["arguments"].(map[string]any)["query"] != "files" {
		t.Fatal(search)
	}
	// Replay the exact wire outputs and client replies, including discoveries.
	input := append([]any{map[string]any{"role": "user", "content": "hi"}}, output...)
	input = append(input,
		map[string]any{"type": "custom_tool_call_output", "call_id": "exec1", "output": "hello"},
		map[string]any{"type": "function_call_output", "call_id": "wait1", "output": "done"},
		map[string]any{"type": "tool_search_output", "call_id": "search1", "status": "completed", "execution": "client", "tools": []any{map[string]any{"type": "function", "name": "inspect", "parameters": map[string]any{"type": "object"}}}},
	)
	requestBody["input"] = input
	body, _ = json.Marshal(requestBody)
	converted, _, _, err := convertResponsesToChat(body)
	if err != nil {
		t.Fatal(err)
	}
	chat := object(t, converted)
	if len(chat["tools"].([]any)) != 4 {
		t.Fatalf("discovered tool was lost: %s", converted)
	}
	messages := chat["messages"].([]any)
	if len(messages) != 5 || len(messages[1].(map[string]any)["tool_calls"].([]any)) != 3 {
		t.Fatalf("tool history was not grouped: %s", converted)
	}
	for index, name := range []string{"exec", "functions__wait", "tool_search"} {
		call := messages[1].(map[string]any)["tool_calls"].([]any)[index].(map[string]any)
		if call["function"].(map[string]any)["name"] != name {
			t.Fatal(call)
		}
	}
}

func TestResponsesClientToolStreamingLateName(t *testing.T) {
	for _, tc := range []struct {
		name, tool, chatName, firstName, arguments, outputType string
	}{
		{"custom", `{"type":"custom","name":"exec"}`, "exec", "", `{"input":"echo \"hi\"\n"}`, "custom_tool_call"},
		{"namespace", `{"type":"namespace","name":"functions","tools":[{"type":"function","name":"wait"}]}`, "functions__wait", "functions__", `{"cell_id":"1"}`, "function_call"},
		{"tool_search", `{"type":"tool_search"}`, "tool_search", "", `{"query":"files"}`, "tool_search_call"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := testServer(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				split := len(tc.arguments) / 2
				for _, function := range []map[string]any{
					{"name": tc.firstName, "arguments": tc.arguments[:split]},
					{"name": strings.TrimPrefix(tc.chatName, tc.firstName), "arguments": tc.arguments[split:]},
				} {
					chunk := map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": "call1", "function": function}}}}}}
					data, _ := json.Marshal(chunk)
					fmt.Fprintf(w, "data: %s\n\n", data)
				}
				io.WriteString(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n")
			})
			w := request(s, "/v1/responses", `{"model":"test","input":"hi","stream":true,"tools":[`+tc.tool+`]}`)
			if w.Code != 200 {
				t.Fatal(w.Code, w.Body)
			}
			added, done, completed := 0, 0, false
			var argumentDeltas, customDeltas strings.Builder
			for _, line := range strings.Split(w.Body.String(), "\n") {
				if !strings.HasPrefix(line, "data: ") {
					continue
				}
				event := object(t, []byte(strings.TrimPrefix(line, "data: ")))
				switch event["type"] {
				case "response.output_item.added", "response.output_item.done":
					item := event["item"].(map[string]any)
					if item["type"] != tc.outputType {
						t.Fatalf("wrong wire type: %s", line)
					}
					if tc.name == "namespace" && (item["name"] != "wait" || item["namespace"] != "functions") {
						t.Fatal(item)
					}
					if tc.name == "tool_search" && item["execution"] != "client" {
						t.Fatal(item)
					}
					if event["type"] == "response.output_item.added" {
						added++
					} else {
						done++
					}
				case "response.function_call_arguments.delta", "response.function_call_arguments.done":
					if tc.name != "namespace" {
						t.Fatalf("function events leaked for %s: %s", tc.name, line)
					}
					if event["type"] == "response.function_call_arguments.delta" {
						argumentDeltas.WriteString(stringValue(event["delta"]))
					}
				case "response.custom_tool_call_input.delta":
					customDeltas.WriteString(stringValue(event["delta"]))
				case "response.completed":
					completed = true
					item := event["response"].(map[string]any)["output"].([]any)[0].(map[string]any)
					if item["type"] != tc.outputType {
						t.Fatal(item)
					}
				}
			}
			if added != 1 || done != 1 || !completed {
				t.Fatalf("incomplete lifecycle: %s", w.Body)
			}
			if tc.name == "namespace" && argumentDeltas.String() != tc.arguments {
				t.Fatalf("buffered arguments lost: %s", w.Body)
			}
			if tc.name == "custom" && customDeltas.String() != "echo \"hi\"\n" {
				t.Fatalf("custom input not restored: %s", w.Body)
			}
		})
	}
}

func TestResponsesAdditionalToolsAndConflicts(t *testing.T) {
	valid := `{"model":"test","input":[{"type":"additional_tools","tools":[{"type":"custom","name":"exec"}]},{"role":"user","content":"hi"}],"tool_choice":{"type":"custom","name":"exec"}}`
	body, _, _, err := convertResponsesToChat([]byte(valid))
	if err != nil {
		t.Fatal(err)
	}
	chat := object(t, body)
	if len(chat["messages"].([]any)) != 1 || len(chat["tools"].([]any)) != 1 || chat["tool_choice"].(map[string]any)["function"].(map[string]any)["name"] != "exec" {
		t.Fatal(string(body))
	}
	for _, tools := range []string{
		`[{"type":"function","name":"exec"},{"type":"custom","name":"exec"}]`,
		`[{"type":"tool_search"},{"type":"function","name":"tool_search"}]`,
		`[{"type":"function","name":"functions__wait"},{"type":"namespace","name":"functions","tools":[{"type":"function","name":"wait"}]}]`,
		`[{"type":"namespace","name":"functions","tools":[{"type":"namespace","name":"nested","tools":[]}]}]`,
		`[{"type":"function","name":123}]`,
		`[123]`,
	} {
		if _, _, _, err := convertResponsesToChat([]byte(`{"model":"test","input":"hi","tools":` + tools + `}`)); err == nil {
			t.Fatalf("malformed or ambiguous tools accepted: %s", tools)
		}
	}
	longNS := strings.Repeat("namespace", 6)
	longName := strings.Repeat("function", 6)
	bridge := &responsesToolBridge{}
	raw := object(t, []byte(fmt.Sprintf(`{"tools":[{"type":"namespace","name":%q,"tools":[{"type":"function","name":%q}]}]}`, longNS, longName)))
	if err := bridge.prepare(raw); err != nil {
		t.Fatal(err)
	}
	flat := bridge.tools[0].(map[string]any)["function"].(map[string]any)["name"].(string)
	if len(flat) != 64 || bridge.output("call", flat, `{}`, "completed")["namespace"] != longNS {
		t.Fatal(flat)
	}
}

func TestResponsesToolBeforeTextHistoryKeepsOneAssistantTurn(t *testing.T) {
	body, _, _, err := convertResponsesToChat([]byte(`{"model":"test","input":[{"role":"user","content":"hi"},{"type":"reasoning","summary":[{"type":"summary_text","text":"thinking"}]},{"type":"function_call","name":"wait","call_id":"call1","arguments":"{}"},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"waiting"}]},{"type":"web_search_call"},{"type":"function_call_output","call_id":"call1","output":"done"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	messages := object(t, body)["messages"].([]any)
	if len(messages) != 3 || messages[2].(map[string]any)["role"] != "tool" {
		t.Fatalf("assistant text interrupted tool replies: %s", body)
	}
	assistant := messages[1].(map[string]any)
	if assistant["reasoning_content"] != "thinking" || assistant["content"].([]any)[0].(map[string]any)["text"] != "waiting" || len(assistant["tool_calls"].([]any)) != 1 {
		t.Fatal(string(body))
	}
}
