package proxy

import (
	"encoding/json"
	"fmt"
)

// GLM's image limit applies to the entire request, including conversation
// history. Preserve every image in the current user turn and evict only the
// oldest historical images when the combined request exceeds that limit.
func limitChatImagesForModel(body []byte, model string) ([]byte, int, error) {
	if model != "@cf/zai-org/glm-5.3-flash" {
		return body, 0, nil
	}
	const limit = 8
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, 0, err
	}
	var messages []map[string]json.RawMessage
	if err := json.Unmarshal(raw["messages"], &messages); err != nil {
		return nil, 0, err
	}
	contents := make([][]json.RawMessage, len(messages))
	lastUser := -1
	total := 0
	for index, message := range messages {
		var role string
		_ = json.Unmarshal(message["role"], &role)
		if role == "user" {
			lastUser = index
		}
		if json.Unmarshal(message["content"], &contents[index]) != nil {
			continue // String or null content cannot contain image parts.
		}
		for _, part := range contents[index] {
			if chatPartIsImage(part) {
				total++
			}
		}
	}
	if total <= limit {
		return body, 0, nil
	}
	current := 0
	for index, parts := range contents {
		if lastUser >= 0 && index < lastUser {
			continue
		}
		for _, part := range parts {
			if chatPartIsImage(part) {
				current++
			}
		}
	}
	if current > limit {
		return nil, 0, fmt.Errorf("glm-5.3-flash accepts at most %d images per request; the current turn contains %d images, please send them in batches of at most %d", limit, current, limit)
	}
	drop := total - limit
	omitted := drop
	placeholder := json.RawMessage(`{"type":"text","text":"[Earlier image omitted to fit glm-5.3-flash's 8-image request limit.]"}`)
	for index := 0; index < lastUser && drop > 0; index++ {
		changed := false
		for partIndex, part := range contents[index] {
			if drop > 0 && chatPartIsImage(part) {
				contents[index][partIndex] = placeholder
				drop--
				changed = true
			}
		}
		if changed {
			messages[index]["content"], _ = json.Marshal(contents[index])
		}
	}
	raw["messages"], _ = json.Marshal(messages)
	encoded, err := json.Marshal(raw)
	return encoded, omitted, err
}

func chatPartIsImage(part json.RawMessage) bool {
	var discriminator struct {
		Type string `json:"type"`
	}
	return json.Unmarshal(part, &discriminator) == nil && discriminator.Type == "image_url"
}
