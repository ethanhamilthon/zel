package provider

import (
	"bytes"
	"encoding/json"
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
	var parts []json.RawMessage
	if json.Unmarshal(content, &parts) == nil {
		for i, part := range parts {
			var p struct{ Type, Text, Refusal string }
			_ = json.Unmarshal(part, &p)
			switch p.Type {
			case "input_text", "output_text":
				parts[i] = raw(map[string]any{"type": "output_text", "text": p.Text, "annotations": []any{}})
			case "refusal":
				parts[i] = raw(map[string]string{"type": "refusal", "refusal": p.Refusal})
			}
		}
		content = raw(parts)
	}
	return raw(map[string]json.RawMessage{"role": json.RawMessage(`"assistant"`), "content": content})
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
