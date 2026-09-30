package proxy

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

func taskChatModel(id string) bool {
	switch shortName(id) {
	case "m2m100-1.2b", "indictrans2-en-indic-1b", "distilbert-sst-2-int8", "resnet-50", "llava-1.5-7b-hf":
		return true
	}
	return false
}

// Single-input translation/classification models use the standard Chat response
// envelope. They do not pretend to support conversation history or tool calls.
func (s *Server) taskChat(ctx context.Context, w http.ResponseWriter, id, model string, body []byte, stream bool) (*http.Response, error) {
	var raw map[string]any
	if json.Unmarshal(body, &raw) != nil {
		return nil, invalid("invalid chat request")
	}
	if err := mediaAllowed(raw, "model", "messages", "stream", "stream_options", "source_lang", "target_lang", "max_tokens", "max_completion_tokens", "temperature", "top_p"); err != nil {
		return nil, err
	}
	messages, _ := raw["messages"].([]any)
	if len(messages) != 1 {
		return nil, invalid("this model accepts one user message; conversation history is unsupported")
	}
	msg, _ := messages[0].(map[string]any)
	if stringValue(msg["role"]) != "user" {
		return nil, invalid("this model requires a user message")
	}
	text := ""
	var imageData []byte
	switch value := msg["content"].(type) {
	case string:
		text = value
	case []any:
		for _, v := range value {
			part, _ := v.(map[string]any)
			switch stringValue(part["type"]) {
			case "text":
				text += stringValue(part["text"])
			case "image_url":
				if imageData != nil {
					return nil, invalid("this model accepts one image")
				}
				source := stringValue(part["image_url"])
				if obj, ok := part["image_url"].(map[string]any); ok {
					source = stringValue(obj["url"])
				}
				prefix, data, ok := strings.Cut(source, ",")
				if !ok || !strings.HasPrefix(prefix, "data:image/") || !strings.HasSuffix(prefix, ";base64") {
					return nil, invalid("this model requires a base64 image data URL")
				}
				var err error
				imageData, err = base64.StdEncoding.DecodeString(data)
				if err != nil {
					return nil, invalid("invalid image data URL")
				}
				if err = validateImage(imageData); err != nil {
					return nil, err
				}
			default:
				return nil, invalid("unsupported content part")
			}
		}
	default:
		return nil, invalid("content must be text or an array of parts")
	}
	payload := map[string]any{}
	switch shortName(id) {
	case "m2m100-1.2b", "indictrans2-en-indic-1b":
		if text == "" || imageData != nil {
			return nil, invalid("translation requires text")
		}
		source, err := mediaString(raw, "source_lang", "english")
		if err != nil {
			return nil, err
		}
		target, err := mediaString(raw, "target_lang", "english")
		if err != nil {
			return nil, err
		}
		payload = map[string]any{"text": text, "source_lang": source, "target_lang": target}
	case "distilbert-sst-2-int8":
		if text == "" || imageData != nil {
			return nil, invalid("classification requires text")
		}
		payload["text"] = text
	case "resnet-50":
		if imageData == nil {
			return nil, invalid("image_url is required")
		}
		payload["image"] = byteNumbers(imageData)
	case "llava-1.5-7b-hf":
		if imageData == nil {
			return nil, invalid("image_url is required")
		}
		payload["image"] = byteNumbers(imageData)
		payload["prompt"] = text
		for _, key := range []string{"temperature", "top_p", "max_tokens"} {
			if raw[key] != nil {
				payload[key] = raw[key]
			}
		}
	}
	resp, err := s.runModel(ctx, w, id, payload)
	if err != nil {
		return nil, err
	}
	value, err := modelResult(resp)
	if err != nil {
		return nil, err
	}
	resultText := ""
	var usage any
	if obj, ok := value.(map[string]any); ok {
		usage = obj["usage"]
		resultText = stringValue(obj["translated_text"])
		if resultText == "" {
			resultText = stringValue(obj["description"])
		}
		if resultText == "" {
			resultText = stringValue(obj["response"])
		}
	}
	if shortName(id) == "distilbert-sst-2-int8" || shortName(id) == "resnet-50" {
		encoded, err := json.Marshal(value)
		if err != nil {
			return nil, err
		}
		resultText = string(encoded)
	}
	if resultText == "" {
		return nil, upstreamInvalid("upstream returned no result")
	}
	completion := map[string]any{"id": fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano()), "object": "chat.completion", "created": time.Now().Unix(), "model": model}
	contentType := "application/json"
	var encoded []byte
	if !stream {
		completion["choices"] = []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": resultText}, "finish_reason": "stop"}}
		if usage != nil {
			completion["usage"] = usage
		}
		encoded, _ = json.Marshal(completion)
	} else {
		contentType = "text/event-stream"
		var out bytes.Buffer
		completion["object"] = "chat.completion.chunk"
		completion["choices"] = []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant", "content": resultText}, "finish_reason": nil}}
		data, _ := json.Marshal(completion)
		fmt.Fprintf(&out, "data: %s\n\n", data)
		completion["choices"] = []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}}
		data, _ = json.Marshal(completion)
		fmt.Fprintf(&out, "data: %s\n\n", data)
		options, _ := raw["stream_options"].(map[string]any)
		if boolValue(options["include_usage"]) && usage != nil {
			completion["choices"] = []any{}
			completion["usage"] = usage
			data, _ = json.Marshal(completion)
			fmt.Fprintf(&out, "data: %s\n\n", data)
		}
		out.WriteString("data: [DONE]\n\n")
		encoded = out.Bytes()
	}
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {contentType}}, Body: io.NopCloser(bytes.NewReader(encoded))}, nil
}

func (s *Server) modelEndpoints(id string) []string {
	if taskChatModel(id) {
		return []string{"/v1/chat/completions", "/v1/responses", "/v1/messages"}
	}
	name := shortName(id)
	if name == "bge-reranker-base" {
		return []string{"/v1/rerank"}
	}
	out := []string{}
	for _, task := range s.modelTasks(id) {
		switch task {
		case "Text Generation":
			out = append(out, "/v1/chat/completions", "/v1/responses", "/v1/messages")
		case "Text Embeddings":
			out = append(out, "/v1/embeddings")
		case "Text-to-Image":
			out = append(out, "/v1/images/generations")
			if name == "stable-diffusion-v1-5-inpainting" {
				out = out[:0]
			}
			if strings.HasPrefix(name, "flux-2-") || strings.HasPrefix(name, "stable-diffusion") || name == "dreamshaper-8-lcm" {
				out = append(out, "/v1/images/edits")
			}
		case "Text-to-Speech":
			out = append(out, "/v1/audio/speech")
		case "Automatic Speech Recognition":
			if name != "flux" {
				out = append(out, "/v1/audio/transcriptions")
			}
			if name == "whisper-large-v3-turbo" {
				out = append(out, "/v1/audio/translations")
			}
		}
	}
	return out
}
