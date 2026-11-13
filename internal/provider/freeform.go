package provider

import (
	"encoding/json"
	"os"
	"strings"
)

// FreeformTools reports whether free-form (custom) tools are used on the wire:
// the Responses API of OpenAI-hosted endpoints. Everything else gets a JSON
// function with one string argument. ZEL_FREEFORM=0|1 overrides the guess.
func (c *Client) FreeformTools() bool {
	switch os.Getenv("ZEL_FREEFORM") {
	case "0":
		return false
	case "1":
		return c.api == APIResponses
	}
	return c.api == APIResponses && (strings.Contains(c.base, "openai.com") || strings.Contains(c.base, "chatgpt.com"))
}

func freeformFields(tools []Tool) map[string]string {
	fields := map[string]string{}
	for _, t := range tools {
		if t.Freeform != "" {
			fields[t.Name] = t.Freeform
		}
	}
	return fields
}

// customInput rewrites history function calls of freeform tools (and their
// outputs) into custom_tool_call items. The history itself always keeps the
// uniform function_call form.
func customInput(input []json.RawMessage, tools []Tool) []json.RawMessage {
	fields := freeformFields(tools)
	if len(fields) == 0 {
		return input
	}
	custom := map[string]bool{}
	out := make([]json.RawMessage, 0, len(input))
	for _, item := range input {
		var v struct {
			Type, Name, Arguments, Output string
			CallID                        string `json:"call_id"`
		}
		if json.Unmarshal(item, &v) != nil {
			out = append(out, item)
			continue
		}
		switch {
		case v.Type == "function_call" && fields[v.Name] != "":
			var args map[string]any
			_ = json.Unmarshal([]byte(v.Arguments), &args)
			text, _ := args[fields[v.Name]].(string)
			custom[v.CallID] = true
			data, _ := json.Marshal(map[string]any{"type": "custom_tool_call", "call_id": v.CallID, "name": v.Name, "input": text})
			out = append(out, data)
		case v.Type == "function_call_output" && custom[v.CallID]:
			data, _ := json.Marshal(map[string]any{"type": "custom_tool_call_output", "call_id": v.CallID, "output": v.Output})
			out = append(out, data)
		default:
			out = append(out, item)
		}
	}
	return out
}

// customToFunction turns a custom_tool_call output item into a function_call.
func customToFunction(item json.RawMessage, fields map[string]string) json.RawMessage {
	var m map[string]any
	if json.Unmarshal(item, &m) != nil || m["type"] != "custom_tool_call" {
		return item
	}
	name, _ := m["name"].(string)
	field := fields[name]
	if field == "" {
		field = "input"
	}
	text, _ := m["input"].(string)
	args, _ := json.Marshal(map[string]any{field: text})
	delete(m, "input")
	m["type"], m["arguments"] = "function_call", string(args)
	data, _ := json.Marshal(m)
	return data
}

// normalizeCustom maps the stream events of custom tool calls to the function
// call events the rest of zel handles. It returns extra events to deliver
// before the (possibly rewritten) event; an empty Type drops the event.
func normalizeCustom(event StreamEvent, fields map[string]string, inputs map[int64]string) ([]StreamEvent, StreamEvent) {
	switch event.Type {
	case "response.output_item.added", "response.output_item.done":
		event.Item = customToFunction(event.Item, fields)
	case "response.custom_tool_call_input.delta":
		// The first fragment opens the JSON object: {"code":"<escaped text>
		piece, _ := json.Marshal(event.Delta)
		fragment := string(piece[1 : len(piece)-1])
		if _, started := inputs[event.OutputIndex]; !started {
			fragment = `{"` + fieldFor(fields) + `":"` + fragment
		}
		inputs[event.OutputIndex] += event.Delta
		return nil, StreamEvent{Type: "response.function_call_arguments.delta", OutputIndex: event.OutputIndex, Delta: fragment}
	case "response.custom_tool_call_input.done":
		args, _ := json.Marshal(map[string]any{fieldFor(fields): event.Input})
		return nil, StreamEvent{Type: "response.function_call_arguments.done", OutputIndex: event.OutputIndex, Arguments: string(args)}
	case "response.completed":
		for i, item := range event.Response.Output {
			event.Response.Output[i] = customToFunction(item, fields)
		}
	}
	return nil, event
}

func fieldFor(fields map[string]string) string {
	for _, f := range fields {
		return f
	}
	return "input"
}
