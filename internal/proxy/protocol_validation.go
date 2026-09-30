package proxy

import (
	"fmt"
	"sort"
	"strings"
)

func validateAnthropicRequestForChat(raw map[string]interface{}) error {
	if err := rejectUnsupportedFields(raw, "Anthropic compatibility", "model", "messages", "system", "max_tokens", "stream", "temperature", "top_p", "stop_sequences", "tools", "tool_choice", "top_k", "thinking"); err != nil {
		return err
	}
	if _, err := requireString(raw, "model"); err != nil {
		return err
	}
	messages, err := requireJSONArray(raw, "messages")
	if err != nil {
		return err
	}
	for index, item := range messages {
		message, ok := item.(map[string]interface{})
		if !ok {
			return fmt.Errorf("messages[%d] must be an object", index)
		}
		if err := rejectUnsupportedFields(message, fmt.Sprintf("Anthropic messages[%d]", index), "role", "content"); err != nil {
			return err
		}
		if role := stringValue(message["role"]); role != "user" && role != "assistant" {
			return fmt.Errorf("messages[%d].role must be user or assistant", index)
		}
		if err := validateAnthropicBlocks(message["content"], fmt.Sprintf("messages[%d].content", index)); err != nil {
			return err
		}
	}
	if thinking, ok := raw["thinking"].(map[string]interface{}); ok {
		typ := stringValue(thinking["type"])
		if typ != "enabled" && typ != "disabled" {
			return fmt.Errorf("anthropic thinking type %q cannot be represented by OpenAI/Gemini", typ)
		}
		if typ == "enabled" {
			budget, ok := thinking["budget_tokens"]
			if !ok || intValue(budget) < 1 {
				return fmt.Errorf("thinking.budget_tokens is required when thinking is enabled")
			}
		}
	}
	if err := validateAnthropicToolChoice(raw["tool_choice"]); err != nil {
		return err
	}
	return nil
}

func validateAnthropicBlocks(value interface{}, path string) error {
	if _, ok := value.(string); ok {
		return nil
	}
	items, ok := value.([]interface{})
	if !ok {
		return fmt.Errorf("%s must be a string or block array", path)
	}
	for index, item := range items {
		block, ok := item.(map[string]interface{})
		if !ok {
			return fmt.Errorf("%s[%d] must be an object", path, index)
		}
		switch stringValue(block["type"]) {
		case "text", "input_text", "output_text", "image", "tool_use", "tool_result", "thinking", "redacted_thinking":
		case "image_url", "input_image":
			if fileID, exists := block["file_id"]; exists && fileID != nil {
				return fmt.Errorf("%s[%d].file_id cannot be represented by the compatibility path", path, index)
			}
			if block["image_url"] == nil && block["url"] == nil {
				return fmt.Errorf("%s[%d] %s requires image_url or url", path, index, stringValue(block["type"]))
			}
		default:
			return fmt.Errorf("%s[%d] block type %q cannot be represented", path, index, stringValue(block["type"]))
		}
	}
	return nil
}

func rejectUnsupportedFields(raw map[string]interface{}, protocol string, allowed ...string) error {
	allow := make(map[string]struct{}, len(allowed))
	for _, key := range allowed {
		allow[key] = struct{}{}
	}
	unsupported := make([]string, 0)
	for key, value := range raw {
		if value == nil {
			continue
		}
		if _, ok := allow[key]; !ok {
			unsupported = append(unsupported, key)
		}
	}
	if len(unsupported) == 0 {
		return nil
	}
	sort.Strings(unsupported)
	return fmt.Errorf("%s compatibility path cannot preserve field(s): %s", protocol, strings.Join(unsupported, ", "))
}

func validateAnthropicToolChoice(value interface{}) error {
	if value == nil {
		return nil
	}
	choice, ok := value.(map[string]interface{})
	if !ok {
		return fmt.Errorf("anthropic tool_choice must be an object")
	}
	typeName := stringValue(choice["type"])
	if typeName != "auto" && typeName != "any" && typeName != "tool" && typeName != "none" {
		return fmt.Errorf("anthropic tool_choice type %q cannot be represented", typeName)
	}
	if disabled, exists := choice["disable_parallel_tool_use"]; exists && disabled != nil {
		if _, ok := disabled.(bool); !ok {
			return fmt.Errorf("anthropic tool_choice.disable_parallel_tool_use must be boolean")
		}
	}
	if typeName == "tool" && stringValue(choice["name"]) == "" {
		return fmt.Errorf("anthropic tool_choice.name is required for type tool")
	}
	return nil
}

func requireString(raw map[string]interface{}, field string) (string, error) {
	value, ok := raw[field].(string)
	if !ok || strings.TrimSpace(value) == "" {
		return "", fmt.Errorf("%s is required", field)
	}
	return value, nil
}

func requireJSONArray(raw map[string]interface{}, field string) ([]interface{}, error) {
	value, ok := raw[field].([]interface{})
	if !ok || len(value) == 0 {
		return nil, fmt.Errorf("%s is required", field)
	}
	return value, nil
}

func validateResponsesForChat(raw map[string]interface{}) error {
	if err := rejectUnsupportedFields(raw, "Chat Responses", "model", "input", "instructions", "max_output_tokens", "tools", "tool_choice", "temperature", "top_p", "stream", "text", "reasoning", "parallel_tool_calls", "include", "store"); err != nil {
		return err
	}
	consumeResponsesCompatibilityHints(raw)
	if _, err := requireString(raw, "model"); err != nil {
		return err
	}
	if _, ok := raw["input"]; !ok {
		return fmt.Errorf("input is required")
	}
	if err := validateResponsesInputForChat(raw["input"]); err != nil {
		return err
	}
	if tools, ok := raw["tools"].([]interface{}); ok {
		for index, item := range tools {
			tool, ok := item.(map[string]interface{})
			if !ok || stringValue(tool["type"]) != "function" || stringValue(tool["name"]) == "" {
				return fmt.Errorf("chat Responses tools[%d] built-in or malformed tool cannot be represented", index)
			}
		}
	}
	if textValue, exists := raw["text"]; exists && textValue != nil {
		text, ok := textValue.(map[string]interface{})
		if !ok {
			return fmt.Errorf("Chat Responses text must be an object")
		}
		if err := rejectUnsupportedFields(text, "Chat Responses text", "format"); err != nil {
			return err
		}
		if formatValue, exists := text["format"]; exists && formatValue != nil {
			if _, ok := formatValue.(map[string]interface{}); !ok {
				return fmt.Errorf("Chat Responses text.format must be an object")
			}
		}
	}
	return nil
}

func validateResponsesInputForChat(value interface{}) error {
	if _, ok := value.(string); ok {
		return nil
	}
	items, ok := value.([]interface{})
	if !ok {
		return fmt.Errorf("input must be a string or item array")
	}
	for index, item := range items {
		entry, ok := item.(map[string]interface{})
		if !ok {
			return fmt.Errorf("input[%d] must be an object", index)
		}
		switch typ := stringValue(entry["type"]); typ {
		case "", "message":
			if typ == "" && stringValue(entry["role"]) == "" {
				return fmt.Errorf("input[%d] message role is required", index)
			}
			if err := validateResponsesChatContent(entry["content"], fmt.Sprintf("input[%d].content", index)); err != nil {
				return err
			}
		case "function_call":
			if stringValue(entry["name"]) == "" {
				return fmt.Errorf("input[%d].name is required", index)
			}
		case "function_call_output":
			if stringValue(entry["call_id"]) == "" {
				return fmt.Errorf("input[%d].call_id is required", index)
			}
		case "reasoning":
			// Preserve this through the shared Responses-to-Chat converter.
		default:
			return fmt.Errorf("input[%d] type %q cannot be represented by Chat Completions", index, typ)
		}
	}
	return nil
}

func validateResponsesChatContent(value interface{}, path string) error {
	if _, ok := value.(string); ok {
		return nil
	}
	items, ok := value.([]interface{})
	if !ok {
		return fmt.Errorf("%s must be a string or content array", path)
	}
	for index, item := range items {
		part, ok := item.(map[string]interface{})
		if !ok {
			return fmt.Errorf("%s[%d] must be an object", path, index)
		}
		switch stringValue(part["type"]) {
		case "input_text", "output_text", "text", "input_image", "image_url", "image":
			if partType := stringValue(part["type"]); partType == "input_image" {
				if fileID, exists := part["file_id"]; exists && fileID != nil {
					return fmt.Errorf("%s[%d].file_id cannot be represented by Chat Completions", path, index)
				}
				if part["image_url"] == nil && part["url"] == nil {
					return fmt.Errorf("%s[%d] input_image requires image_url or url", path, index)
				}
			}
		default:
			return fmt.Errorf("%s[%d] type %q cannot be represented by Chat Completions", path, index, stringValue(part["type"]))
		}
	}
	return nil
}

func consumeResponsesCompatibilityHints(raw map[string]interface{}) {
	// These options control OpenAI-side persistence and optional response
	// decorations. Compatibility providers are stateless, so accepting and
	// dropping them gives clients such as Cherry Studio the most useful result
	// without changing the generated model input.
	delete(raw, "store")
	delete(raw, "include")
}
