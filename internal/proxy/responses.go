package proxy

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

func intValue(value interface{}) int {
	switch number := value.(type) {
	case float64:
		return int(number)
	case int:
		return number
	case string:
		parsed, _ := strconv.Atoi(number)
		return parsed
	default:
		return 0
	}
}

func stringValue(value interface{}) string {
	if text, ok := value.(string); ok {
		return text
	}
	if value == nil {
		return ""
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	return string(encoded)
}

func streamChatAsResponses(w http.ResponseWriter, resp *http.Response, modelID string) error {
	copyResponseHeaders(w.Header(), resp.Header, true)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Del("Content-Length")
	w.WriteHeader(http.StatusOK)

	flusher, ok := w.(http.Flusher)
	if !ok {
		return fmt.Errorf("streaming not supported")
	}

	responseID := fmt.Sprintf("resp_%d", time.Now().UnixNano())
	createdAt := time.Now().Unix()
	responseModel := modelID
	messageID := fmt.Sprintf("msg_%s", responseID)
	messageStarted := false
	messageOutputIndex := -1
	reasoningID := fmt.Sprintf("rs_%s", responseID)
	reasoningStarted := false
	reasoningSummaryStarted := false
	reasoningOutputIndex := -1
	nextOutputIndex := 0
	type messagePartState struct {
		typ   string
		index int
	}
	messageParts := make([]messagePartState, 0, 2)
	textContentIndex := -1
	refusalContentIndex := -1
	text := strings.Builder{}
	refusal := strings.Builder{}
	reasoning := strings.Builder{}
	reasoningSignature := strings.Builder{}
	tools := make(map[int]*responsesStreamTool)
	toolOrder := make([]int, 0)
	usage := map[string]interface{}{}
	finishReason := "stop"
	terminalSeen := false
	transportDone := false
	sequenceNumber := 0

	writeEvent := func(eventType string, value interface{}) error {
		sequenceNumber++
		if object, ok := value.(map[string]interface{}); ok {
			object["sequence_number"] = sequenceNumber
		}
		encoded, err := json.Marshal(value)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", eventType, encoded); err != nil {
			return err
		}
		flusher.Flush()
		return nil
	}
	writeFailure := func(err error) error {
		return writeEvent("response.failed", map[string]interface{}{
			"type": "response.failed",
			"response": map[string]interface{}{
				"id": responseID, "object": "response", "created_at": createdAt,
				"status": "failed", "model": responseModel,
				"error": map[string]interface{}{"code": "upstream_protocol_error", "message": err.Error()},
			},
		})
	}
	startReasoning := func() error {
		if reasoningStarted {
			return nil
		}
		reasoningStarted = true
		reasoningOutputIndex = nextOutputIndex
		nextOutputIndex++
		if err := writeEvent("response.output_item.added", map[string]interface{}{
			"type": "response.output_item.added", "response_id": responseID,
			"output_index": reasoningOutputIndex,
			"item": map[string]interface{}{
				"type": "reasoning", "id": reasoningID, "status": "in_progress", "summary": []interface{}{},
			},
		}); err != nil {
			return err
		}
		return nil
	}
	startReasoningSummary := func() error {
		if err := startReasoning(); err != nil {
			return err
		}
		if reasoningSummaryStarted {
			return nil
		}
		reasoningSummaryStarted = true
		return writeEvent("response.reasoning_summary_part.added", map[string]interface{}{
			"type": "response.reasoning_summary_part.added", "response_id": responseID,
			"item_id": reasoningID, "output_index": reasoningOutputIndex, "summary_index": 0,
			"part": map[string]interface{}{"type": "summary_text", "text": ""},
		})
	}
	startMessagePart := func(partType string) (int, error) {
		if partType == "output_text" && textContentIndex >= 0 {
			return textContentIndex, nil
		}
		if partType == "refusal" && refusalContentIndex >= 0 {
			return refusalContentIndex, nil
		}
		if !messageStarted {
			messageStarted = true
			messageOutputIndex = nextOutputIndex
			nextOutputIndex++
			if err := writeEvent("response.output_item.added", map[string]interface{}{
				"type":         "response.output_item.added",
				"response_id":  responseID,
				"output_index": messageOutputIndex,
				"item": map[string]interface{}{
					"type":    "message",
					"id":      messageID,
					"status":  "in_progress",
					"role":    "assistant",
					"content": []interface{}{},
				},
			}); err != nil {
				return -1, err
			}
		}
		contentIndex := len(messageParts)
		messageParts = append(messageParts, messagePartState{typ: partType, index: contentIndex})
		if partType == "refusal" {
			refusalContentIndex = contentIndex
		} else {
			textContentIndex = contentIndex
		}
		part := map[string]interface{}{"type": partType}
		if partType == "refusal" {
			part["refusal"] = ""
		} else {
			part["text"] = ""
			part["annotations"] = []interface{}{}
		}
		if err := writeEvent("response.content_part.added", map[string]interface{}{
			"type":          "response.content_part.added",
			"response_id":   responseID,
			"output_index":  messageOutputIndex,
			"content_index": contentIndex,
			"item_id":       messageID,
			"part":          part,
		}); err != nil {
			return -1, err
		}
		return contentIndex, nil
	}

	if err := writeEvent("response.created", map[string]interface{}{
		"type": "response.created",
		"response": map[string]interface{}{
			"id":         responseID,
			"object":     "response",
			"created_at": createdAt,
			"status":     "in_progress",
			"model":      responseModel,
			"output":     []interface{}{},
		},
	}); err != nil {
		return err
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 4096), maxSSELineBytes)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" {
			continue
		}
		if payload == "[DONE]" {
			transportDone = true
			break
		}

		var chunk map[string]interface{}
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			return writeFailure(fmt.Errorf("invalid upstream OpenAI SSE event: %w", err))
		}
		if upstreamError, ok := chunk["error"].(map[string]interface{}); ok {
			return writeFailure(fmt.Errorf("upstream OpenAI error: %s", stringValue(upstreamError["message"])))
		}
		if value := stringValue(chunk["model"]); value != "" {
			responseModel = value
		}
		if chunkUsage, ok := chunk["usage"].(map[string]interface{}); ok {
			usage["input_tokens"] = chunkUsage["prompt_tokens"]
			usage["output_tokens"] = chunkUsage["completion_tokens"]
			usage["total_tokens"] = chunkUsage["total_tokens"]
			if details, ok := chunkUsage["prompt_tokens_details"].(map[string]interface{}); ok {
				usage["cached_tokens"] = details["cached_tokens"]
			}
			if details, ok := chunkUsage["completion_tokens_details"].(map[string]interface{}); ok {
				usage["reasoning_tokens"] = details["reasoning_tokens"]
			}
		}

		choices, _ := chunk["choices"].([]interface{})
		if len(choices) == 0 {
			continue
		}
		if len(choices) > 1 {
			return writeFailure(fmt.Errorf("Responses cannot represent multiple Chat Completions choices"))
		}
		choice, _ := choices[0].(map[string]interface{})
		if reason := stringValue(choice["finish_reason"]); reason != "" {
			finishReason = reason
			terminalSeen = true
		}
		delta, _ := choice["delta"].(map[string]interface{})
		if value := stringValue(delta["reasoning_content"]); value != "" {
			if err := startReasoningSummary(); err != nil {
				return err
			}
			reasoning.WriteString(value)
			if err := writeEvent("response.reasoning_summary_text.delta", map[string]interface{}{
				"type": "response.reasoning_summary_text.delta", "response_id": responseID,
				"item_id": reasoningID, "output_index": reasoningOutputIndex, "summary_index": 0, "delta": value,
			}); err != nil {
				return err
			}
		}
		if value := stringValue(delta["reasoning_signature"]); value != "" {
			if err := startReasoning(); err != nil {
				return err
			}
			reasoningSignature.WriteString(value)
		}
		if value := stringValue(delta["content"]); value != "" {
			contentIndex, err := startMessagePart("output_text")
			if err != nil {
				return err
			}
			text.WriteString(value)
			if err := writeEvent("response.output_text.delta", map[string]interface{}{
				"type":          "response.output_text.delta",
				"response_id":   responseID,
				"output_index":  messageOutputIndex,
				"content_index": contentIndex,
				"item_id":       messageID,
				"delta":         value,
			}); err != nil {
				return err
			}
		}
		if value := stringValue(delta["refusal"]); value != "" {
			contentIndex, err := startMessagePart("refusal")
			if err != nil {
				return err
			}
			refusal.WriteString(value)
			if err := writeEvent("response.refusal.delta", map[string]interface{}{
				"type":          "response.refusal.delta",
				"response_id":   responseID,
				"output_index":  messageOutputIndex,
				"content_index": contentIndex,
				"item_id":       messageID,
				"delta":         value,
			}); err != nil {
				return err
			}
		}

		toolCalls, _ := delta["tool_calls"].([]interface{})
		for _, item := range toolCalls {
			call, _ := item.(map[string]interface{})
			index := intValue(call["index"])
			state := tools[index]
			newTool := false
			if state == nil {
				state = &responsesStreamTool{
					id:          firstNonEmptyString(call["id"], fmt.Sprintf("call_%d", index)),
					outputIndex: nextOutputIndex,
				}
				nextOutputIndex++
				tools[index] = state
				toolOrder = append(toolOrder, index)
				newTool = true
			}
			function, _ := call["function"].(map[string]interface{})
			if name := stringValue(function["name"]); name != "" {
				state.name = name
			}
			if newTool {
				if err := writeEvent("response.output_item.added", map[string]interface{}{
					"type":         "response.output_item.added",
					"response_id":  responseID,
					"output_index": state.outputIndex,
					"item": map[string]interface{}{
						"type":      "function_call",
						"id":        state.id,
						"call_id":   state.id,
						"status":    "in_progress",
						"name":      state.name,
						"arguments": "",
					},
				}); err != nil {
					return err
				}
			}

			arguments := stringValue(function["arguments"])
			if arguments == "" {
				continue
			}
			deltaArguments := arguments
			state.arguments.WriteString(arguments)
			if deltaArguments == "" {
				continue
			}
			if err := writeEvent("response.function_call_arguments.delta", map[string]interface{}{
				"type":         "response.function_call_arguments.delta",
				"response_id":  responseID,
				"item_id":      state.id,
				"output_index": state.outputIndex,
				"delta":        deltaArguments,
			}); err != nil {
				return err
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return writeFailure(err)
	}
	if !terminalSeen || !transportDone {
		return writeFailure(fmt.Errorf("upstream OpenAI stream ended without a terminal event"))
	}
	for _, state := range tools {
		if state.name == "" || !json.Valid([]byte(state.arguments.String())) {
			return writeFailure(fmt.Errorf("upstream returned incomplete tool arguments or name"))
		}
	}
	output := make([]interface{}, nextOutputIndex)
	if reasoningStarted {
		summary := make([]interface{}, 0, 1)
		if reasoningSummaryStarted {
			part := map[string]interface{}{"type": "summary_text", "text": reasoning.String()}
			summary = append(summary, part)
			if err := writeEvent("response.reasoning_summary_text.done", map[string]interface{}{
				"type": "response.reasoning_summary_text.done", "response_id": responseID,
				"item_id": reasoningID, "output_index": reasoningOutputIndex, "summary_index": 0, "text": reasoning.String(),
			}); err != nil {
				return err
			}
			if err := writeEvent("response.reasoning_summary_part.done", map[string]interface{}{
				"type": "response.reasoning_summary_part.done", "response_id": responseID,
				"item_id": reasoningID, "output_index": reasoningOutputIndex, "summary_index": 0, "part": part,
			}); err != nil {
				return err
			}
		}
		reasoningOutput := map[string]interface{}{
			"type": "reasoning", "id": reasoningID, "status": "completed", "summary": summary,
		}
		if reasoningSignature.Len() > 0 {
			reasoningOutput["encrypted_content"] = reasoningSignature.String()
		}
		output[reasoningOutputIndex] = reasoningOutput
		if err := writeEvent("response.output_item.done", map[string]interface{}{
			"type": "response.output_item.done", "response_id": responseID,
			"output_index": reasoningOutputIndex, "item": reasoningOutput,
		}); err != nil {
			return err
		}
	}

	if messageStarted {
		contentParts := make([]interface{}, 0, len(messageParts))
		for _, state := range messageParts {
			contentPart := map[string]interface{}{"type": state.typ}
			if state.typ == "refusal" {
				contentPart["refusal"] = refusal.String()
			} else {
				contentPart["text"] = text.String()
				contentPart["annotations"] = []interface{}{}
			}
			contentParts = append(contentParts, contentPart)
		}
		messageOutput := map[string]interface{}{
			"type":    "message",
			"id":      messageID,
			"role":    "assistant",
			"status":  "completed",
			"content": contentParts,
		}
		output[messageOutputIndex] = messageOutput
		for _, state := range messageParts {
			contentPart := contentParts[state.index].(map[string]interface{})
			if state.typ == "refusal" {
				if err := writeEvent("response.refusal.done", map[string]interface{}{
					"type": "response.refusal.done", "response_id": responseID,
					"output_index": messageOutputIndex, "content_index": state.index, "item_id": messageID,
					"refusal": refusal.String(),
				}); err != nil {
					return err
				}
			} else {
				if err := writeEvent("response.output_text.done", map[string]interface{}{
					"type":          "response.output_text.done",
					"response_id":   responseID,
					"output_index":  messageOutputIndex,
					"content_index": state.index,
					"item_id":       messageID,
					"text":          text.String(),
				}); err != nil {
					return err
				}
			}
			if err := writeEvent("response.content_part.done", map[string]interface{}{
				"type":          "response.content_part.done",
				"response_id":   responseID,
				"output_index":  messageOutputIndex,
				"content_index": state.index,
				"item_id":       messageID,
				"part":          contentPart,
			}); err != nil {
				return err
			}
		}
		if err := writeEvent("response.output_item.done", map[string]interface{}{
			"type":         "response.output_item.done",
			"response_id":  responseID,
			"output_index": messageOutputIndex,
			"item":         messageOutput,
		}); err != nil {
			return err
		}
	}

	for _, index := range toolOrder {
		state := tools[index]
		toolOutput := map[string]interface{}{
			"type":      "function_call",
			"id":        state.id,
			"call_id":   state.id,
			"status":    "completed",
			"name":      state.name,
			"arguments": state.arguments.String(),
		}
		output[state.outputIndex] = toolOutput
		if err := writeEvent("response.function_call_arguments.done", map[string]interface{}{
			"type":         "response.function_call_arguments.done",
			"response_id":  responseID,
			"item_id":      state.id,
			"output_index": state.outputIndex,
			"arguments":    state.arguments.String(),
		}); err != nil {
			return err
		}
		if err := writeEvent("response.output_item.done", map[string]interface{}{
			"type":         "response.output_item.done",
			"response_id":  responseID,
			"output_index": state.outputIndex,
			"item":         toolOutput,
		}); err != nil {
			return err
		}
	}

	status := "completed"
	completedEvent := "response.completed"
	if finishReason == "length" || finishReason == "content_filter" {
		status = "incomplete"
		completedEvent = "response.incomplete"
	}
	response := map[string]interface{}{
		"id":          responseID,
		"object":      "response",
		"created_at":  createdAt,
		"status":      status,
		"model":       responseModel,
		"output":      output,
		"output_text": text.String(),
		"usage":       normalizedResponsesUsage(usage),
	}
	if status == "incomplete" {
		reason := "max_output_tokens"
		if finishReason == "content_filter" {
			reason = "content_filter"
		}
		response["incomplete_details"] = map[string]interface{}{"reason": reason}
	}
	return writeEvent(completedEvent, map[string]interface{}{
		"type":     completedEvent,
		"response": response,
	})
}

type responsesStreamTool struct {
	id          string
	name        string
	outputIndex int
	arguments   strings.Builder
}

func normalizedResponsesUsage(usage map[string]interface{}) map[string]interface{} {
	return map[string]interface{}{
		"input_tokens":  intValue(usage["input_tokens"]),
		"output_tokens": intValue(usage["output_tokens"]),
		"total_tokens":  intValue(usage["total_tokens"]),
		"input_tokens_details": map[string]interface{}{
			"cached_tokens": intValue(usage["cached_tokens"]),
		},
		"output_tokens_details": map[string]interface{}{
			"reasoning_tokens": intValue(usage["reasoning_tokens"]),
		},
	}
}

func convertChatJSONToResponses(body []byte, modelID string) ([]byte, error) {
	var raw map[string]interface{}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err
	}

	choices, ok := raw["choices"].([]interface{})
	if !ok || len(choices) == 0 {
		return nil, fmt.Errorf("chat completions response has no choices")
	}
	responseID := stringValue(raw["id"])
	if responseID == "" {
		responseID = fmt.Sprintf("resp_%d", time.Now().UnixNano())
	}
	output := make([]interface{}, 0)
	outputText := strings.Builder{}
	if choices, ok := raw["choices"].([]interface{}); ok && len(choices) > 0 {
		if len(choices) > 1 {
			return nil, fmt.Errorf("Responses cannot represent %d Chat Completions choices losslessly", len(choices))
		}
		choice, _ := choices[0].(map[string]interface{})
		message, _ := choice["message"].(map[string]interface{})
		text := stringValue(message["content"])
		content := make([]interface{}, 0, 1)
		if text != "" || stringValue(message["refusal"]) != "" {
			textPart := map[string]interface{}{"type": "output_text", "text": text, "annotations": []interface{}{}}
			if annotations, ok := message["annotations"].([]interface{}); ok {
				textPart["annotations"] = annotations
			}
			content = append(content, textPart)
		}
		messageOutput := map[string]interface{}{
			"type":    "message",
			"id":      fmt.Sprintf("msg_%s", responseID),
			"role":    "assistant",
			"status":  "completed",
			"content": content,
		}
		reasoning := stringValue(message["reasoning_content"])
		encryptedReasoning := stringValue(message["reasoning_signature"])
		if details, ok := message["reasoning_details"].([]interface{}); ok {
			for _, item := range details {
				detail, _ := item.(map[string]interface{})
				if reasoning == "" && stringValue(detail["type"]) == "thinking" {
					reasoning = stringValue(detail["thinking"])
				}
				if encryptedReasoning == "" {
					encryptedReasoning = firstNonEmptyString(stringValue(detail["signature"]), stringValue(detail["data"]))
				}
			}
		}
		if reasoning != "" || encryptedReasoning != "" {
			reasoningOutput := map[string]interface{}{
				"type": "reasoning", "id": fmt.Sprintf("rs_%s", responseID), "status": "completed",
				"summary": []interface{}{},
			}
			if reasoning != "" {
				reasoningOutput["summary"] = []interface{}{map[string]interface{}{"type": "summary_text", "text": reasoning}}
			}
			if encryptedReasoning != "" {
				reasoningOutput["encrypted_content"] = encryptedReasoning
			}
			output = append(output, reasoningOutput)
		}
		if refusal := stringValue(message["refusal"]); refusal != "" {
			messageOutput["content"] = []interface{}{map[string]interface{}{"type": "refusal", "refusal": refusal}}
		}
		if text != "" || stringValue(message["refusal"]) != "" {
			output = append(output, messageOutput)
		}
		if text != "" {
			outputText.WriteString(text)
		}

		if toolCalls, ok := message["tool_calls"].([]interface{}); ok {
			for _, item := range toolCalls {
				call, _ := item.(map[string]interface{})
				function, _ := call["function"].(map[string]interface{})
				callID := stringValue(call["id"])
				output = append(output, map[string]interface{}{
					"type":      "function_call",
					"id":        callID,
					"call_id":   callID,
					"status":    "completed",
					"name":      stringValue(function["name"]),
					"arguments": stringValue(function["arguments"]),
				})
			}
		}
	}

	response := map[string]interface{}{
		"id":          responseID,
		"object":      "response",
		"created_at":  int64Value(raw["created"]),
		"status":      "completed",
		"model":       modelID,
		"output":      output,
		"output_text": outputText.String(),
	}
	if choices, ok := raw["choices"].([]interface{}); ok && len(choices) == 1 {
		choice, _ := choices[0].(map[string]interface{})
		switch stringValue(choice["finish_reason"]) {
		case "length":
			response["status"] = "incomplete"
			response["incomplete_details"] = map[string]interface{}{"reason": "max_output_tokens"}
		case "content_filter":
			response["status"] = "incomplete"
			response["incomplete_details"] = map[string]interface{}{"reason": "content_filter"}
		}
	}
	if usage, ok := raw["usage"].(map[string]interface{}); ok {
		convertedUsage := map[string]interface{}{
			"input_tokens":  usage["prompt_tokens"],
			"output_tokens": usage["completion_tokens"],
			"total_tokens":  usage["total_tokens"],
		}
		if details, ok := usage["prompt_tokens_details"].(map[string]interface{}); ok {
			convertedUsage["input_tokens_details"] = map[string]interface{}{"cached_tokens": details["cached_tokens"]}
		}
		if details, ok := usage["completion_tokens_details"].(map[string]interface{}); ok {
			convertedUsage["output_tokens_details"] = map[string]interface{}{"reasoning_tokens": details["reasoning_tokens"]}
		}
		response["usage"] = convertedUsage
	}
	return json.Marshal(response)
}

func int64Value(value interface{}) int64 {
	switch number := value.(type) {
	case float64:
		return int64(number)
	case int64:
		return number
	case int:
		return int64(number)
	default:
		return time.Now().Unix()
	}
}

func convertResponsesToChat(body []byte) ([]byte, string, bool, error) {
	var raw map[string]interface{}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, "", false, err
	}
	removeUndefinedPlaceholders(raw)
	if err := validateResponsesForChat(raw); err != nil {
		return nil, "", false, err
	}

	modelID, _ := raw["model"].(string)
	if modelID == "" {
		return nil, "", false, fmt.Errorf("model is required")
	}

	messages := responsesInputToChatMessages(raw["input"])
	if len(messages) == 0 {
		return nil, "", false, fmt.Errorf("input is required")
	}

	chat := map[string]interface{}{
		"model":    modelID,
		"messages": messages,
		"stream":   boolValue(raw["stream"]),
	}
	if instructions, ok := raw["instructions"]; ok {
		messages = append([]interface{}{map[string]interface{}{"role": "system", "content": responsesContentToChat(instructions)}}, messages...)
		chat["messages"] = messages
	}

	for _, key := range []string{"temperature", "top_p", "stop", "seed", "response_format"} {
		if value, ok := raw[key]; ok {
			chat[key] = value
		}
	}
	if textConfig, ok := raw["text"].(map[string]interface{}); ok {
		if format, ok := textConfig["format"].(map[string]interface{}); ok {
			responseFormat := map[string]interface{}{"type": stringValue(format["type"])}
			if schema, ok := format["schema"]; ok {
				responseFormat["json_schema"] = map[string]interface{}{
					"name":        format["name"],
					"description": format["description"],
					"schema":      schema,
					"strict":      format["strict"],
				}
			}
			chat["response_format"] = responseFormat
		}
	}
	if value, ok := raw["max_output_tokens"]; ok {
		chat["max_completion_tokens"] = value
	}
	if value, ok := raw["tool_choice"]; ok {
		chat["tool_choice"] = responsesToolChoiceToChat(value)
	}
	if value, ok := raw["parallel_tool_calls"]; ok {
		chat["parallel_tool_calls"] = value
	}
	if reasoning, ok := raw["reasoning"].(map[string]interface{}); ok {
		if effort := stringValue(reasoning["effort"]); effort != "" {
			chat["reasoning_effort"] = effort
		}
	}
	if tools, ok := raw["tools"].([]interface{}); ok {
		if converted := responsesToolsToChat(tools); len(converted) > 0 {
			chat["tools"] = converted
		}
	}

	encoded, err := json.Marshal(chat)
	return encoded, modelID, boolValue(raw["stream"]), err
}

func responsesContentToChat(value interface{}) interface{} {
	if text, ok := value.(string); ok {
		return text
	}
	items, ok := value.([]interface{})
	if !ok {
		return ""
	}
	content := make([]interface{}, 0, len(items))
	for _, item := range items {
		part, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		partType, _ := part["type"].(string)
		switch partType {
		case "input_text", "output_text", "text":
			if text, ok := part["text"].(string); ok {
				content = append(content, map[string]interface{}{"type": "text", "text": text})
			}
		case "input_image", "image_url", "image":
			imageURL, ok := part["image_url"]
			if !ok {
				imageURL = part["url"]
			}
			if url, ok := imageURL.(string); ok {
				imageURL = map[string]interface{}{"url": url}
				if detail := part["detail"]; detail != nil {
					imageURL.(map[string]interface{})["detail"] = detail
				}
			}
			if imageURL != nil {
				content = append(content, map[string]interface{}{"type": "image_url", "image_url": imageURL})
			}
		}
	}
	return content
}

func responsesToolsToChat(tools []interface{}) []interface{} {
	converted := make([]interface{}, 0, len(tools))
	for _, item := range tools {
		tool, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		if toolType, _ := tool["type"].(string); toolType != "function" {
			continue
		}
		function := map[string]interface{}{}
		for _, key := range []string{"name", "description", "parameters", "strict"} {
			if value, ok := tool[key]; ok {
				function[key] = value
			}
		}
		if _, ok := function["name"]; !ok {
			continue
		}
		converted = append(converted, map[string]interface{}{
			"type":     "function",
			"function": function,
		})
	}
	return converted
}

func responsesInputToChatMessages(value interface{}) []interface{} {
	if text, ok := value.(string); ok && text != "" {
		return []interface{}{map[string]interface{}{"role": "user", "content": text}}
	}

	items, _ := value.([]interface{})
	messages := make([]interface{}, 0, len(items))
	for _, item := range items {
		entry, ok := item.(map[string]interface{})
		if !ok {
			continue
		}

		typeName, _ := entry["type"].(string)
		switch typeName {
		case "function_call":
			name, _ := entry["name"].(string)
			if name == "" {
				continue
			}
			callID, _ := entry["call_id"].(string)
			if callID == "" {
				callID, _ = entry["id"].(string)
			}
			messages = append(messages, map[string]interface{}{
				"role": "assistant",
				"tool_calls": []interface{}{map[string]interface{}{
					"id":   callID,
					"type": "function",
					"function": map[string]interface{}{
						"name":      name,
						"arguments": stringValue(entry["arguments"]),
					},
				}},
			})
		case "function_call_output":
			callID, _ := entry["call_id"].(string)
			messages = append(messages, map[string]interface{}{
				"role":         "tool",
				"tool_call_id": callID,
				"content":      stringValue(entry["output"]),
			})
		case "reasoning":
			reasoning := strings.Builder{}
			if summary, ok := entry["summary"].([]interface{}); ok {
				for _, item := range summary {
					part, _ := item.(map[string]interface{})
					reasoning.WriteString(stringValue(part["text"]))
				}
			}
			message := map[string]interface{}{
				"role":              "assistant",
				"content":           "",
				"reasoning_content": reasoning.String(),
			}
			if encrypted := stringValue(entry["encrypted_content"]); encrypted != "" {
				message["reasoning_signature"] = encrypted
			}
			messages = append(messages, message)
		default:
			role, _ := entry["role"].(string)
			if role == "" {
				if typeName == "message" {
					role = "assistant"
				} else {
					role = "user"
				}
			}
			// Responses uses developer for high-priority instructions. Both
			// Gemini and Anthropic adapters represent that role as a system
			// instruction instead of a user turn.
			if role == "developer" {
				role = "system"
			}
			messages = append(messages, map[string]interface{}{
				"role":    role,
				"content": responsesContentToChat(entry["content"]),
			})
		}
	}
	merged := make([]interface{}, 0, len(messages))
	for _, item := range messages {
		message := item.(map[string]interface{})
		if calls, ok := message["tool_calls"].([]interface{}); ok && len(merged) > 0 {
			previous := merged[len(merged)-1].(map[string]interface{})
			if stringValue(previous["role"]) == "assistant" {
				existing, _ := previous["tool_calls"].([]interface{})
				previous["tool_calls"] = append(existing, calls...)
				continue
			}
		}
		merged = append(merged, message)
	}
	return merged
}

func responsesToolChoiceToChat(value interface{}) interface{} {
	choice, ok := value.(map[string]interface{})
	if !ok {
		return value
	}
	if _, hasFunction := choice["function"]; hasFunction {
		return value
	}
	if name, ok := choice["name"].(string); ok && name != "" {
		return map[string]interface{}{
			"type": "function",
			"function": map[string]interface{}{
				"name": name,
			},
		}
	}
	return value
}
