package proxy

import "fmt"

func validateNativeGeminiRequest(raw map[string]any) error {
	if err := rejectUnsupportedFields(raw, "Gemini", "contents", "systemInstruction", "generationConfig", "tools", "toolConfig"); err != nil {
		return err
	}
	contents, err := requireJSONArray(raw, "contents")
	if err != nil {
		return err
	}
	for i, item := range contents {
		content, ok := item.(map[string]any)
		if !ok {
			return fmt.Errorf("contents[%d] must be an object", i)
		}
		role := stringValue(content["role"])
		if role != "" && role != "user" && role != "model" {
			return fmt.Errorf("contents[%d].role must be user or model", i)
		}
		parts, err := requireJSONArray(content, "parts")
		if err != nil {
			return err
		}
		for _, item := range parts {
			part, ok := item.(map[string]any)
			if !ok {
				return fmt.Errorf("Gemini parts must be objects")
			}
			if err := rejectUnsupportedFields(part, "Gemini part", "text", "inlineData", "inline_data", "functionCall", "functionResponse", "thought"); err != nil {
				return err
			}
			if boolValue(part["thought"]) {
				return fmt.Errorf("Gemini thought history cannot be transferred; omit thought parts")
			}
			if part["inlineData"] != nil || part["inline_data"] != nil {
				if nativeGeminiImagePart(part) == nil {
					return fmt.Errorf("invalid Gemini inline image")
				}
			}
			if part["text"] == nil && part["inlineData"] == nil && part["inline_data"] == nil && part["functionCall"] == nil && part["functionResponse"] == nil {
				return fmt.Errorf("empty Gemini part")
			}
		}
	}
	if v := raw["systemInstruction"]; v != nil {
		instruction, ok := v.(map[string]any)
		if !ok {
			return fmt.Errorf("systemInstruction must be an object")
		}
		parts, err := requireJSONArray(instruction, "parts")
		if err != nil {
			return err
		}
		for _, item := range parts {
			part, ok := item.(map[string]any)
			if !ok {
				return fmt.Errorf("systemInstruction parts must be text objects")
			}
			if err := rejectUnsupportedFields(part, "systemInstruction part", "text"); err != nil {
				return err
			}
		}
	}
	if v := raw["generationConfig"]; v != nil {
		config, ok := v.(map[string]any)
		if !ok {
			return fmt.Errorf("generationConfig must be an object")
		}
		if err := rejectUnsupportedFields(config, "Gemini generationConfig", "maxOutputTokens", "temperature", "topP", "topK", "stopSequences", "thinkingConfig", "frequencyPenalty", "presencePenalty", "responseMimeType", "responseJsonSchema"); err != nil {
			return err
		}
		if mime := stringValue(config["responseMimeType"]); mime != "" && mime != "text/plain" && mime != "application/json" {
			return fmt.Errorf("unsupported responseMimeType")
		}
	}
	if v := raw["tools"]; v != nil {
		groups, ok := v.([]any)
		if !ok {
			return fmt.Errorf("tools must be an array")
		}
		for _, item := range groups {
			group, ok := item.(map[string]any)
			if !ok {
				return fmt.Errorf("tool must be an object")
			}
			if err := rejectUnsupportedFields(group, "Gemini tools", "functionDeclarations"); err != nil {
				return err
			}
			decls, err := requireJSONArray(group, "functionDeclarations")
			if err != nil {
				return err
			}
			for _, item := range decls {
				decl, ok := item.(map[string]any)
				if !ok {
					return fmt.Errorf("function declaration must be an object")
				}
				if _, err := requireString(decl, "name"); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
