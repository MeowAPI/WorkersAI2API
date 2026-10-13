package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

func imageParts(count int, typ, prefix string) []any {
	parts := []any{map[string]any{"type": "text", "text": "compare these images"}}
	for index := 0; index < count; index++ {
		parts = append(parts, map[string]any{"type": typ, "image_url": prefix + strings.Repeat("x", index+1)})
	}
	return parts
}

func TestGLMImageHistoryBudget(t *testing.T) {
	for _, protocol := range []string{"chat", "responses"} {
		t.Run(protocol, func(t *testing.T) {
			s := testServer(t, func(w http.ResponseWriter, r *http.Request) {
				data, _ := io.ReadAll(r.Body)
				chat := object(t, data)
				messages := chat["messages"].([]any)
				count := 0
				for _, item := range messages {
					message := item.(map[string]any)
					for _, content := range message["content"].([]any) {
						part := content.(map[string]any)
						if part["type"] == "image_url" {
							count++
							url := part["image_url"]
							if image, ok := url.(map[string]any); ok {
								url = image["url"]
								if image["detail"] == "low" {
									t.Error("unexpected detail rewriting")
								}
							}
							if url == "https://old/x" || url == "https://old/xx" {
								t.Errorf("oldest image retained: %s", data)
							}
						}
					}
				}
				if count != 8 || len(messages[1].(map[string]any)["content"].([]any)) != 3 {
					t.Errorf("current images changed or image count wrong: %s", data)
				}
				if protocol == "chat" {
					var exact map[string]json.RawMessage
					json.Unmarshal(data, &exact)
					if string(exact["seed"]) != "9007199254740993" {
						t.Error("large integer changed")
					}
				}
				io.WriteString(w, chatJSON)
			})
			s.catalog.routes["glm-5.3-flash"] = "@cf/zai-org/glm-5.3-flash"
			typ := "image_url"
			path, field := "/v1/chat/completions", "messages"
			if protocol == "responses" {
				typ = "input_image"
				path, field = "/v1/responses", "input"
			}
			body := map[string]any{"model": "glm-5.3-flash", field: []any{
				map[string]any{"role": "user", "content": imageParts(8, typ, "https://old/")},
				map[string]any{"role": "user", "content": imageParts(2, typ, "https://current/")},
			}}
			if protocol == "chat" {
				body["seed"] = json.Number("9007199254740993")
			}
			data, _ := json.Marshal(body)
			w := request(s, path, string(data))
			if w.Code != 200 || w.Header().Get("X-Images-Omitted") != "2" {
				t.Fatal(w.Code, w.Header(), w.Body)
			}
		})
	}
}

func TestGLMCurrentImageLimitAndOtherModels(t *testing.T) {
	var calls atomic.Int32
	s := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		io.WriteString(w, chatJSON)
	})
	s.catalog.routes["glm-5.3-flash"] = "@cf/zai-org/glm-5.3-flash"
	for _, tc := range []struct {
		model  string
		count  int
		status int
	}{
		{"glm-5.3-flash", 8, 200},
		{"glm-5.3-flash", 10, 400},
		{"test", 10, 200},
	} {
		data, _ := json.Marshal(map[string]any{"model": tc.model, "messages": []any{map[string]any{"role": "user", "content": imageParts(tc.count, "image_url", "https://current/")}}})
		w := request(s, "/v1/chat/completions", string(data))
		if w.Code != tc.status {
			t.Fatal(tc, w.Code, w.Body)
		}
		if tc.status == 400 && !strings.Contains(w.Body.String(), "send them in batches") {
			t.Fatal("unclear current-turn limit error", w.Body)
		}
	}
	if calls.Load() != 2 {
		t.Fatal("oversized current turn reached upstream", calls.Load())
	}
}

func TestGLMImageBudgetPreservesSmallRequestBytes(t *testing.T) {
	body := []byte(`{ "model":"@cf/zai-org/glm-5.3-flash", "messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://image","detail":"high"}}]}], "seed":9007199254740993 }`)
	converted, omitted, err := limitChatImagesForModel(body, "@cf/zai-org/glm-5.3-flash")
	if err != nil || omitted != 0 || string(converted) != string(body) {
		t.Fatal(string(converted), omitted, err)
	}
}
