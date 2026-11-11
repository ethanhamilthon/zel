package provider

import "encoding/json"

// Request is shared by both API adapters; callers never construct wire payloads.
type Request struct {
	Model, Instructions, Effort string
	Input                       []json.RawMessage
	Tools                       []Tool
	ReasoningSummary            bool
}
type Tool struct {
	Name, Description string
	Parameters        map[string]any
}

func (r Request) payload() map[string]any {
	tools := make([]any, 0, len(r.Tools))
	for _, t := range r.Tools {
		tools = append(tools, map[string]any{"type": "function", "name": t.Name, "description": t.Description, "parameters": t.Parameters, "strict": true})
	}
	body := map[string]any{"model": r.Model, "instructions": r.Instructions, "input": r.Input, "store": false, "tools": tools}
	reasoning := map[string]any{}
	if r.Effort != "" {
		reasoning["effort"] = r.Effort
	}
	if r.ReasoningSummary {
		reasoning["summary"] = "auto"
	}
	if len(reasoning) > 0 {
		body["reasoning"] = reasoning
	}
	return body
}
