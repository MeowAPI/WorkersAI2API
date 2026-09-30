package proxy

import "encoding/json"

func decodeJSONValue(value interface{}) interface{} {
	if text, ok := value.(string); ok {
		var decoded interface{}
		if json.Unmarshal([]byte(text), &decoded) == nil {
			return decoded
		}
	}
	if value == nil {
		return map[string]interface{}{}
	}
	return value
}

func decodeGeminiArguments(value interface{}) interface{} {
	if text, ok := value.(string); ok {
		var decoded interface{}
		if json.Unmarshal([]byte(text), &decoded) == nil {
			return decoded
		}
		return map[string]interface{}{"text": text}
	}
	if value == nil {
		return map[string]interface{}{}
	}
	return value
}

func firstNonEmptyString(value interface{}, fallback string) string {
	if text, ok := value.(string); ok && text != "" {
		return text
	}
	return fallback
}

func boolValue(value interface{}) bool {
	result, _ := value.(bool)
	return result
}

func removeUndefinedPlaceholders(value interface{}) {
	switch current := value.(type) {
	case map[string]interface{}:
		for key, child := range current {
			if text, ok := child.(string); ok {
				// Cherry Studio can emit these diagnostic placeholders for
				// JavaScript values that were undefined or circular.
				if text == "[undefined]" || (key == "extra_content" && text == "[Circular]") {
					delete(current, key)
					continue
				}
			}
			removeUndefinedPlaceholders(child)
		}
	case []interface{}:
		for _, child := range current {
			removeUndefinedPlaceholders(child)
		}
	}
}
