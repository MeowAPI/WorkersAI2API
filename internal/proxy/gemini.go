package proxy

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
)

func streamChatAsNativeGemini(w http.ResponseWriter, resp *http.Response, modelID string) error {
	copyResponseHeaders(w.Header(), resp.Header, true)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher, ok := w.(http.Flusher)
	if !ok {
		return fmt.Errorf("streaming not supported")
	}
	responseID := fmt.Sprintf("resp_%d", time.Now().UnixNano())
	type toolState struct{ id, name, arguments string }
	tools := make(map[int]map[int]*toolState)
	finished := make(map[int]bool)
	seen := make(map[int]bool)
	write := func(payload map[string]interface{}) error {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", encoded); err != nil {
			return err
		}
		flusher.Flush()
		return nil
	}
	transportDone := false
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 4096), maxSSELineBytes)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			transportDone = true
			break
		}
		if data == "" {
			continue
		}
		var chunk map[string]interface{}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			return err
		}
		if upstreamError, ok := chunk["error"].(map[string]interface{}); ok {
			return fmt.Errorf("upstream error: %s", stringValue(upstreamError["message"]))
		}
		if id := stringValue(chunk["id"]); id != "" {
			responseID = id
		}
		if usage, ok := chunk["usage"].(map[string]interface{}); ok {
			if err := write(map[string]interface{}{
				"modelVersion": modelID, "responseId": responseID, "usageMetadata": chatUsageToGemini(usage),
			}); err != nil {
				return err
			}
		}
		choices, _ := chunk["choices"].([]interface{})
		for fallbackIndex, item := range choices {
			choice, _ := item.(map[string]interface{})
			candidateIndex := fallbackIndex
			if choice["index"] != nil {
				candidateIndex = intValue(choice["index"])
			}
			seen[candidateIndex] = true
			delta, _ := choice["delta"].(map[string]interface{})
			parts := make([]interface{}, 0)
			if reasoning := stringValue(delta["reasoning_content"]); reasoning != "" {
				parts = append(parts, map[string]interface{}{"text": reasoning, "thought": true})
			}
			if text := stringValue(delta["content"]); text != "" {
				parts = append(parts, map[string]interface{}{"text": text})
			}
			if calls, ok := delta["tool_calls"].([]interface{}); ok {
				if tools[candidateIndex] == nil {
					tools[candidateIndex] = make(map[int]*toolState)
				}
				for _, callValue := range calls {
					call, _ := callValue.(map[string]interface{})
					toolIndex := intValue(call["index"])
					state := tools[candidateIndex][toolIndex]
					if state == nil {
						state = &toolState{}
						tools[candidateIndex][toolIndex] = state
					}
					if id := stringValue(call["id"]); id != "" {
						state.id = id
					}
					function, _ := call["function"].(map[string]interface{})
					if name := stringValue(function["name"]); name != "" {
						state.name = name
					}
					state.arguments += stringValue(function["arguments"])
				}
			}
			finish := stringValue(choice["finish_reason"])
			if finish != "" {
				indices := make([]int, 0, len(tools[candidateIndex]))
				for index := range tools[candidateIndex] {
					indices = append(indices, index)
				}
				sort.Ints(indices)
				for _, index := range indices {
					state := tools[candidateIndex][index]
					if state == nil || state.name == "" {
						continue
					}
					if !json.Valid([]byte(state.arguments)) {
						return fmt.Errorf("invalid upstream tool arguments")
					}
					functionCall := map[string]interface{}{"name": state.name, "args": decodeGeminiArguments(state.arguments)}
					if state.id != "" {
						functionCall["id"] = state.id
					}
					parts = append(parts, map[string]interface{}{"functionCall": functionCall})
				}
				finished[candidateIndex] = true
			}
			if len(parts) == 0 && finish == "" {
				continue
			}
			candidate := map[string]interface{}{
				"index": candidateIndex, "content": map[string]interface{}{"role": "model", "parts": parts},
			}
			if finish != "" {
				candidate["finishReason"] = chatFinishReasonToGemini(finish)
			}
			if err := write(map[string]interface{}{
				"candidates": []interface{}{candidate}, "modelVersion": modelID, "responseId": responseID,
			}); err != nil {
				return err
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if !transportDone || len(seen) == 0 {
		return fmt.Errorf("upstream chat stream ended without a terminal event")
	}
	for index := range seen {
		if !finished[index] {
			return fmt.Errorf("upstream chat stream ended before candidate completion")
		}
	}
	return nil
}

func chatUsageToGemini(usage map[string]interface{}) map[string]interface{} {
	result := map[string]interface{}{
		"promptTokenCount": usage["prompt_tokens"], "candidatesTokenCount": usage["completion_tokens"],
		"totalTokenCount": usage["total_tokens"],
	}
	if details, ok := usage["prompt_tokens_details"].(map[string]interface{}); ok && details["cached_tokens"] != nil {
		result["cachedContentTokenCount"] = details["cached_tokens"]
	}
	if details, ok := usage["completion_tokens_details"].(map[string]interface{}); ok && details["reasoning_tokens"] != nil {
		result["thoughtsTokenCount"] = details["reasoning_tokens"]
	}
	return result
}

func chatFinishReasonToGemini(value interface{}) string {
	switch stringValue(value) {
	case "length":
		return "MAX_TOKENS"
	case "content_filter":
		return "SAFETY"
	default:
		return "STOP"
	}
}

func convertChatResponseToNativeGemini(body []byte, modelID string) ([]byte, error) {
	var raw map[string]interface{}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err
	}
	choices, _ := raw["choices"].([]interface{})
	if len(choices) == 0 {
		return nil, fmt.Errorf("chat response has no choices")
	}
	candidates := make([]interface{}, 0, len(choices))
	for fallbackIndex, item := range choices {
		choice, _ := item.(map[string]interface{})
		message, _ := choice["message"].(map[string]interface{})
		parts := chatMessageToNativeGeminiParts(message)
		candidate := map[string]interface{}{
			"index":        fallbackIndex,
			"content":      map[string]interface{}{"role": "model", "parts": parts},
			"finishReason": chatFinishReasonToGemini(choice["finish_reason"]),
		}
		if choice["index"] != nil {
			candidate["index"] = intValue(choice["index"])
		}
		candidates = append(candidates, candidate)
	}
	result := map[string]interface{}{
		"candidates": candidates, "modelVersion": modelID,
		"responseId": firstNonEmptyString(raw["id"], fmt.Sprintf("resp_%d", time.Now().UnixNano())),
	}
	if usage, ok := raw["usage"].(map[string]interface{}); ok {
		result["usageMetadata"] = chatUsageToGemini(usage)
	}
	return json.Marshal(result)
}

func chatMessageToNativeGeminiParts(message map[string]interface{}) []interface{} {
	parts := make([]interface{}, 0)
	if reasoning := stringValue(message["reasoning_content"]); reasoning != "" {
		parts = append(parts, map[string]interface{}{"text": reasoning, "thought": true})
	}
	switch content := message["content"].(type) {
	case string:
		if content != "" {
			parts = append(parts, map[string]interface{}{"text": content})
		}
	case []interface{}:
		for _, item := range content {
			part, _ := item.(map[string]interface{})
			if text := stringValue(part["text"]); text != "" {
				parts = append(parts, map[string]interface{}{"text": text})
			}
		}
	}
	if calls, ok := message["tool_calls"].([]interface{}); ok {
		for _, item := range calls {
			call, _ := item.(map[string]interface{})
			function, _ := call["function"].(map[string]interface{})
			name := stringValue(function["name"])
			if name == "" {
				continue
			}
			args := decodeGeminiArguments(function["arguments"])
			functionCall := map[string]interface{}{"name": name, "args": args}
			if id := stringValue(call["id"]); id != "" {
				functionCall["id"] = id
			}
			parts = append(parts, map[string]interface{}{"functionCall": functionCall})
		}
	}
	if len(parts) == 0 {
		parts = append(parts, map[string]interface{}{"text": ""})
	}
	return parts
}

func convertNativeGeminiToChat(modelID string, body []byte, stream bool) ([]byte, error) {
	var raw map[string]interface{}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err
	}
	removeUndefinedPlaceholders(raw)
	contents, ok := raw["contents"].([]interface{})
	if !ok || len(contents) == 0 {
		return nil, fmt.Errorf("contents is required")
	}

	messages := make([]interface{}, 0, len(contents)+1)
	if instruction, ok := raw["systemInstruction"].(map[string]interface{}); ok {
		if text := nativeGeminiPartsText(instruction["parts"]); text != "" {
			messages = append(messages, map[string]interface{}{"role": "system", "content": text})
		}
	}
	callIDs := make(map[string][]string)
	callNumber := 0
	for contentIndex, item := range contents {
		content, ok := item.(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("contents[%d] must be an object", contentIndex)
		}
		role := stringValue(content["role"])
		if role == "model" {
			role = "assistant"
		} else {
			role = "user"
		}
		parts, _ := content["parts"].([]interface{})
		chatParts := make([]interface{}, 0, len(parts))
		toolCalls := make([]interface{}, 0)
		toolResults := make([]interface{}, 0)
		for partIndex, partValue := range parts {
			part, ok := partValue.(map[string]interface{})
			if !ok {
				return nil, fmt.Errorf("contents[%d].parts[%d] must be an object", contentIndex, partIndex)
			}
			if text, ok := part["text"].(string); ok && !boolValue(part["thought"]) {
				chatParts = append(chatParts, map[string]interface{}{"type": "text", "text": text})
			}
			if image := nativeGeminiImagePart(part); image != nil {
				chatParts = append(chatParts, image)
			}
			if call, ok := part["functionCall"].(map[string]interface{}); ok {
				name := stringValue(call["name"])
				if name == "" {
					return nil, fmt.Errorf("contents[%d].parts[%d].functionCall.name is required", contentIndex, partIndex)
				}
				id := firstNonEmptyString(call["id"], fmt.Sprintf("call_%d", callNumber))
				callNumber++
				callIDs[name] = append(callIDs[name], id)
				arguments, err := json.Marshal(call["args"])
				if err != nil {
					return nil, err
				}
				if call["args"] == nil {
					arguments = []byte("{}")
				}
				toolCalls = append(toolCalls, map[string]interface{}{
					"id": id, "type": "function",
					"function": map[string]interface{}{"name": name, "arguments": string(arguments)},
				})
			}
			if result, ok := part["functionResponse"].(map[string]interface{}); ok {
				name := stringValue(result["name"])
				id := stringValue(result["id"])
				if id == "" && len(callIDs[name]) > 0 {
					id = callIDs[name][0]
				}
				if id == "" {
					return nil, fmt.Errorf("functionResponse %q has no matching functionCall", name)
				}
				for i, callID := range callIDs[name] {
					if callID == id {
						callIDs[name] = append(callIDs[name][:i], callIDs[name][i+1:]...)
						break
					}
				}
				value := result["response"]
				encoded, err := json.Marshal(value)
				if err != nil {
					return nil, err
				}
				toolResults = append(toolResults, map[string]interface{}{
					"role": "tool", "tool_call_id": id, "name": name, "content": string(encoded),
				})
			}
		}
		messages = append(messages, toolResults...)
		message := map[string]interface{}{"role": role, "content": nativeGeminiChatContent(chatParts)}
		if len(toolCalls) > 0 {
			message["tool_calls"] = toolCalls
		}
		if message["content"] != "" || len(toolCalls) > 0 {
			messages = append(messages, message)
		}
	}
	if len(messages) == 0 {
		return nil, fmt.Errorf("contents has no transferable content")
	}

	chat := map[string]interface{}{
		"model": modelID, "messages": messages, "stream": stream,
		"max_tokens": 4096,
	}
	if config, ok := raw["generationConfig"].(map[string]interface{}); ok {
		for source, target := range map[string]string{
			"maxOutputTokens": "max_tokens", "temperature": "temperature", "topP": "top_p", "topK": "top_k",
		} {
			if value := config[source]; value != nil {
				chat[target] = value
			}
		}
		if stop := config["stopSequences"]; stop != nil {
			chat["stop"] = stop
		}
		if thinking, ok := config["thinkingConfig"].(map[string]interface{}); ok {
			if effort := nativeGeminiThinkingEffort(thinking); effort != "" {
				chat["reasoning_effort"] = effort
			}
		}
		{
			if value := config["frequencyPenalty"]; value != nil {
				chat["frequency_penalty"] = value
			}
			if value := config["presencePenalty"]; value != nil {
				chat["presence_penalty"] = value
			}
			if mime := stringValue(config["responseMimeType"]); mime == "application/json" {
				chat["response_format"] = map[string]interface{}{"type": "json_object"}
			}
		}
	}
	if config, ok := raw["generationConfig"].(map[string]interface{}); ok {
		if schema := config["responseJsonSchema"]; schema != nil {
			chat["response_format"] = map[string]interface{}{"type": "json_schema", "json_schema": map[string]interface{}{"name": "response", "schema": schema}}
		}
	}
	if tools := nativeGeminiToolsToChat(raw["tools"]); len(tools) > 0 {
		chat["tools"] = tools
		if choice := nativeGeminiToolChoiceToChat(raw["toolConfig"]); choice != nil {
			chat["tool_choice"] = choice
		}
	}
	return json.Marshal(chat)
}

func nativeGeminiThinkingEffort(thinking map[string]interface{}) string {
	if enabled, exists := thinking["includeThoughts"].(bool); exists && !enabled {
		return ""
	}
	if level := strings.ToUpper(stringValue(thinking["thinkingLevel"])); level != "" {
		return map[string]string{"MINIMAL": "minimal", "LOW": "low", "MEDIUM": "medium", "HIGH": "high"}[level]
	}
	if budget := intValue(thinking["thinkingBudget"]); budget != 0 {
		return anthropicThinkingEffort(map[string]interface{}{"type": "enabled", "budget_tokens": budget})
	}
	return ""
}

func nativeGeminiToolsToChat(value interface{}) []interface{} {
	groups, _ := value.([]interface{})
	tools := make([]interface{}, 0)
	for _, groupValue := range groups {
		group, _ := groupValue.(map[string]interface{})
		declarations, _ := group["functionDeclarations"].([]interface{})
		for _, declarationValue := range declarations {
			declaration, _ := declarationValue.(map[string]interface{})
			name := stringValue(declaration["name"])
			if name == "" {
				continue
			}
			parameters := declaration["parameters"]
			if parameters == nil {
				parameters = declaration["parametersJsonSchema"]
			}
			if parameters == nil {
				parameters = map[string]interface{}{"type": "object"}
			}
			function := map[string]interface{}{"name": name, "parameters": parameters}
			if description := stringValue(declaration["description"]); description != "" {
				function["description"] = description
			}
			tools = append(tools, map[string]interface{}{"type": "function", "function": function})
		}
	}
	return tools
}

func nativeGeminiImagePart(part map[string]interface{}) map[string]interface{} {
	inline, _ := part["inlineData"].(map[string]interface{})
	if inline == nil {
		inline, _ = part["inline_data"].(map[string]interface{})
	}
	if inline == nil {
		return nil
	}
	mime := firstNonEmptyString(inline["mimeType"], stringValue(inline["mime_type"]))
	data := stringValue(inline["data"])
	if mime == "" || data == "" {
		return nil
	}
	if _, err := base64.StdEncoding.DecodeString(data); err != nil {
		return nil
	}
	return map[string]interface{}{"type": "image_url", "image_url": map[string]interface{}{"url": "data:" + mime + ";base64," + data}}
}

func nativeGeminiToolChoiceToChat(value interface{}) interface{} {
	toolConfig, _ := value.(map[string]interface{})
	calling, _ := toolConfig["functionCallingConfig"].(map[string]interface{})
	switch strings.ToUpper(stringValue(calling["mode"])) {
	case "NONE":
		return "none"
	case "ANY":
		allowed, _ := calling["allowedFunctionNames"].([]interface{})
		if len(allowed) == 1 && stringValue(allowed[0]) != "" {
			return map[string]interface{}{"type": "function", "function": map[string]interface{}{"name": stringValue(allowed[0])}}
		}
		return "required"
	case "AUTO":
		return "auto"
	default:
		return nil
	}
}

func nativeGeminiChatContent(parts []interface{}) interface{} {
	if len(parts) == 0 {
		return ""
	}
	if len(parts) == 1 {
		if part, ok := parts[0].(map[string]interface{}); ok && stringValue(part["type"]) == "text" {
			return stringValue(part["text"])
		}
	}
	return parts
}

func nativeGeminiPartsText(value interface{}) string {
	parts, _ := value.([]interface{})
	var text strings.Builder
	for _, item := range parts {
		part, _ := item.(map[string]interface{})
		if value, ok := part["text"].(string); ok && !boolValue(part["thought"]) {
			text.WriteString(value)
		}
	}
	return text.String()
}
