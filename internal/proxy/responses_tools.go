package proxy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"unicode/utf8"
)

// The bridge belongs to a single request: Chat tool names must be restored
// using that request's declarations, never shared across clients or turns.
type responsesToolBridge struct {
	tools []interface{}
	names map[string]responsesToolIdentity
}

type responsesToolIdentity struct {
	typ       string
	name      string
	namespace string
}

func flattenResponsesToolName(namespace, name string) string {
	full := namespace + "__" + name
	if len(full) <= 64 {
		return full
	}
	sum := sha256.Sum256([]byte(full))
	suffix := "__" + hex.EncodeToString(sum[:4])
	prefix := full[:64-len(suffix)]
	// Never split a UTF-8 character when shortening a namespace.
	for !utf8.ValidString(prefix) && len(prefix) > 0 {
		prefix = prefix[:len(prefix)-1]
	}
	return prefix + suffix
}

func (b *responsesToolBridge) prepare(raw map[string]interface{}) error {
	b.names = make(map[string]responsesToolIdentity)
	b.tools = nil
	static, _ := raw["tools"].([]interface{})
	tools := append([]interface{}(nil), static...)
	items, _ := raw["input"].([]interface{})
	for index, item := range items {
		entry, _ := item.(map[string]interface{})
		if stringValue(entry["type"]) != "additional_tools" {
			continue
		}
		if err := validateResponsesToolsForChat(entry["tools"]); err != nil {
			return fmt.Errorf("input[%d] additional_tools: %w", index, err)
		}
		additional := entry["tools"].([]interface{})
		tools = append(tools, additional...)
	}
	for _, item := range tools {
		if err := b.add(item.(map[string]interface{}), "", false); err != nil {
			return err
		}
	}
	// A completed client tool search can discover tools that are only present
	// in its output. Promote them so later turns can call those tools.
	for index, item := range items {
		entry, _ := item.(map[string]interface{})
		if stringValue(entry["type"]) != "tool_search_output" || stringValue(entry["status"]) != "completed" || stringValue(entry["execution"]) != "client" {
			continue
		}
		discovered, ok := entry["tools"].([]interface{})
		if !ok {
			continue
		}
		if err := validateResponsesToolsForChat(discovered); err != nil {
			return fmt.Errorf("input[%d] discovered tools: %w", index, err)
		}
		for _, tool := range discovered {
			if err := b.add(tool.(map[string]interface{}), "", true); err != nil {
				return err
			}
		}
	}
	return nil
}

func (b *responsesToolBridge) add(tool map[string]interface{}, namespace string, dedupe bool) error {
	typ, _ := tool["type"].(string)
	name, _ := tool["name"].(string)
	if typ == "namespace" {
		children, _ := tool["tools"].([]interface{})
		if children == nil {
			children, _ = tool["children"].([]interface{})
		}
		for _, child := range children {
			if err := b.add(child.(map[string]interface{}), name, true); err != nil {
				return err
			}
		}
		return nil
	}
	if typ != "function" && typ != "custom" && typ != "tool_search" {
		// Workers AI cannot execute Responses provider-side tools.
		return nil
	}
	function := map[string]interface{}{}
	if typ == "tool_search" {
		name = "tool_search"
		function = map[string]interface{}{
			"name":        name,
			"description": "Search and load client tools, plugins, connectors, and MCP namespaces for the current task.",
			"parameters": map[string]interface{}{
				"type": "object", "properties": map[string]interface{}{
					"query": map[string]interface{}{"type": "string"},
					"limit": map[string]interface{}{"type": "integer"},
				}, "required": []string{"query"},
			},
		}
		dedupe = true
	} else {
		for _, key := range []string{"name", "description", "parameters", "strict"} {
			if value, ok := tool[key]; ok {
				function[key] = value
			}
		}
		if typ == "custom" {
			delete(function, "strict")
			function["parameters"] = map[string]interface{}{
				"type": "object", "properties": map[string]interface{}{
					"input": map[string]interface{}{"type": "string", "description": "The raw input for this tool, passed through verbatim."},
				}, "required": []string{"input"},
			}
		}
	}
	identity := responsesToolIdentity{typ: typ, name: name, namespace: namespace}
	chatName := name
	if namespace != "" {
		chatName = flattenResponsesToolName(namespace, name)
	}
	function["name"] = chatName
	converted := map[string]interface{}{"type": "function", "function": function}
	if previous, exists := b.names[chatName]; exists {
		if dedupe && previous == identity {
			for _, existing := range b.tools {
				fn := existing.(map[string]interface{})["function"].(map[string]interface{})
				if fn["name"] == chatName {
					a, _ := json.Marshal(existing)
					c, _ := json.Marshal(converted)
					if string(a) == string(c) {
						return nil
					}
					break
				}
			}
		}
		return fmt.Errorf("Responses tools conflict at Chat function name %q; cannot disambiguate declarations", chatName)
	}
	b.names[chatName] = identity
	b.tools = append(b.tools, converted)
	return nil
}

func (b *responsesToolBridge) identity(name string) responsesToolIdentity {
	if b != nil {
		if identity, exists := b.names[name]; exists {
			return identity
		}
	}
	return responsesToolIdentity{typ: "function", name: name}
}

func customResponsesToolInput(arguments string) string {
	var object map[string]interface{}
	if json.Unmarshal([]byte(arguments), &object) == nil {
		if input, ok := object["input"].(string); ok {
			return input
		}
		if len(object) == 0 {
			return ""
		}
	}
	return arguments
}

func (b *responsesToolBridge) output(id, name, arguments, status string) map[string]interface{} {
	identity := b.identity(name)
	output := map[string]interface{}{"id": id, "call_id": id, "status": status}
	if identity.namespace != "" {
		output["namespace"] = identity.namespace
	}
	switch identity.typ {
	case "custom":
		output["type"] = "custom_tool_call"
		output["name"] = identity.name
		output["input"] = customResponsesToolInput(arguments)
	case "tool_search":
		output["type"] = "tool_search_call"
		output["execution"] = "client"
		if arguments == "" {
			arguments = "{}"
		}
		if json.Valid([]byte(arguments)) {
			output["arguments"] = json.RawMessage(arguments)
		} else {
			// Preserve malformed model output for client validation instead of
			// manufacturing a different query.
			output["arguments"] = arguments
		}
	default:
		output["type"] = "function_call"
		output["name"] = identity.name
		output["arguments"] = arguments
	}
	return output
}

func (b *responsesToolBridge) choice(value interface{}) interface{} {
	if choice, ok := value.(map[string]interface{}); ok {
		typ := stringValue(choice["type"])
		name := stringValue(choice["name"])
		if typ == "tool_search" {
			identity, ok := b.names["tool_search"]
			if !ok || identity.typ != "tool_search" {
				return nil
			}
			value = map[string]interface{}{"type": "function", "name": "tool_search"}
		} else if namespace := stringValue(choice["namespace"]); namespace != "" && name != "" {
			value = map[string]interface{}{"type": typ, "name": flattenResponsesToolName(namespace, name)}
		}
	}
	return responsesToolChoiceToChat(value, responsesChatToolNames(b.tools))
}
