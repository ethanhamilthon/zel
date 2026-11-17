package provider

import "encoding/json"

// Request is shared by both API adapters; callers never construct wire payloads.
type Request struct {
	Model, Instructions, Effort string
	Input                       []json.RawMessage
	Tools                       []Tool
}
type Tool struct {
	Name, Description string
	Parameters        map[string]any
	// Freeform names the single string argument of a tool whose input is raw
	// text. Where the API supports free-form (custom) tools the text travels
	// unescaped; elsewhere it is the JSON function argument of that name.
	Freeform string
}

func (r Request) payload(custom bool) map[string]any {
	tools := make([]any, 0, len(r.Tools))
	input := r.Input
	if custom {
		input = customInput(r.Input, r.Tools)
	}
	for _, t := range r.Tools {
		if custom && t.Freeform != "" {
			tools = append(tools, map[string]any{"type": "custom", "name": t.Name, "description": t.Description, "format": map[string]any{"type": "text"}})
			continue
		}
		tools = append(tools, map[string]any{"type": "function", "name": t.Name, "description": t.Description, "parameters": t.Parameters, "strict": true})
	}
	body := map[string]any{"model": r.Model, "input": input, "store": false, "tools": tools}
	if r.Instructions != "" {
		body["instructions"] = r.Instructions
	}
	reasoning := map[string]any{"summary": "auto"}
	if r.Effort != "" {
		reasoning["effort"] = r.Effort
	}
	body["reasoning"] = reasoning
	return body
}
