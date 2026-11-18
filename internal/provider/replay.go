package provider

import (
	"bytes"
	"encoding/json"
	"strings"
)

// PortableItem projects another model's output into ordinary conversation
// input. Provider-local item IDs and reasoning state are not portable; call_id
// is a conversation key and must still match the corresponding tool result.
// A nil result means the item should not be sent to the new model.
func PortableItem(item json.RawMessage) json.RawMessage {
	var fields map[string]json.RawMessage
	if json.Unmarshal(item, &fields) != nil {
		return item
	}
	switch string(fields["type"]) {
	case `"reasoning"`:
		return nil
	case `"function_call"`:
		return raw(map[string]json.RawMessage{
			"type": fields["type"], "call_id": fields["call_id"], "name": fields["name"], "arguments": fields["arguments"],
		})
	case `"message"`, "":
		if string(fields["role"]) == `"assistant"` {
			return portableMessage(fields["content"])
		}
	}
	return item
}

func portableMessage(content json.RawMessage) json.RawMessage {
	// Easy input accepts a string, not output_text or refusal parts.
	var text string
	if json.Unmarshal(content, &text) != nil {
		var parts []struct{ Type, Text, Refusal string }
		if json.Unmarshal(content, &parts) == nil {
			var texts []string
			for _, part := range parts {
				switch part.Type {
				case "input_text", "output_text":
					texts = append(texts, part.Text)
				case "refusal":
					texts = append(texts, part.Refusal)
				}
			}
			text = strings.Join(texts, "\n")
		}
	}
	return raw(map[string]string{"role": "assistant", "content": text})
}

// Responses output is not always valid Responses input. Strict endpoints
// reject status on replayed reasoning items. Keep the reasoning payload itself
// (including any encrypted content) and preserve status on native messages and
// function calls. An ID-less assistant message is an input message instead.
func responsesInput(input []json.RawMessage) []json.RawMessage {
	out := make([]json.RawMessage, 0, len(input))
	for _, item := range input {
		// Skip decoding items without a status field, notably inline images.
		var fields map[string]json.RawMessage
		if bytes.Contains(item, []byte(`"status"`)) && json.Unmarshal(item, &fields) == nil && fields["status"] != nil {
			switch string(fields["type"]) {
			case `"reasoning"`:
				delete(fields, "status")
				item = raw(fields)
			case `"message"`, "":
				if string(fields["role"]) == `"assistant"` && (len(fields["id"]) == 0 || string(fields["id"]) == `""` || string(fields["id"]) == "null") {
					item = portableMessage(fields["content"])
				}
			}
		}
		out = append(out, item)
	}
	return out
}
