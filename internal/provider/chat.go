package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// The agent keeps its history in Responses API items. For a provider with
// api = "chat" the request is translated to Chat Completions messages and the
// Stream chunks come back as the Responses events the agent already handles.

// chatMessages converts the system prompt and Responses input items to chat messages.
func chatMessages(instructions string, input []json.RawMessage) []map[string]any {
	messages := []map[string]any{}
	if instructions != "" {
		messages = append(messages, map[string]any{"role": "system", "content": instructions})
	}
	for _, item := range input {
		var v struct {
			Role, Type, Name, Arguments, Output string
			CallID                              string `json:"call_id"`
			Content                             json.RawMessage
		}
		if json.Unmarshal(item, &v) != nil {
			continue
		}
		switch {
		case v.Type == "function_call":
			call := map[string]any{"id": v.CallID, "type": "function", "function": map[string]any{"name": v.Name, "arguments": v.Arguments}}
			if last := messages[len(messages)-1]; last["role"] == "assistant" {
				last["tool_calls"] = append(last["tool_calls"].([]any), call)
			} else {
				messages = append(messages, map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{call}})
			}
		case v.Type == "function_call_output":
			messages = append(messages, map[string]any{"role": "tool", "tool_call_id": v.CallID, "content": v.Output})
		case v.Role != "":
			role := v.Role
			if role == "developer" {
				role = "system"
			}
			var parts []struct {
				Type, Text, Refusal string
				ImageURL            string `json:"image_url"`
			}
			var text string
			var content any
			if json.Unmarshal(v.Content, &text) == nil {
				content = text
			} else if json.Unmarshal(v.Content, &parts) == nil {
				var out []any
				var texts []string
				for _, p := range parts {
					if p.Type == "input_image" {
						out = append(out, map[string]any{"type": "image_url", "image_url": map[string]any{"url": p.ImageURL}})
					} else if t := p.Text + p.Refusal; t != "" {
						out = append(out, map[string]any{"type": "text", "text": t})
						texts = append(texts, t)
					}
				}
				content = strings.Join(texts, "\n")
				if len(out) > len(texts) {
					content = out // images need the part list
				}
			}
			msg := map[string]any{"role": role, "content": content}
			if role == "assistant" {
				msg["tool_calls"] = []any{}
			}
			messages = append(messages, msg)
		}
	}
	for _, m := range messages {
		if calls, ok := m["tool_calls"].([]any); ok && len(calls) == 0 {
			delete(m, "tool_calls")
		}
	}
	return messages
}

// chatBody builds a Chat Completions request from a Responses payload.
func chatBody(body map[string]any) map[string]any {
	input, _ := body["input"].([]json.RawMessage)
	instructions, _ := body["instructions"].(string)
	out := map[string]any{
		"model": body["model"], "messages": chatMessages(instructions, input),
		"stream": true, "stream_options": map[string]any{"include_usage": true},
	}
	if tools, _ := body["tools"].([]any); len(tools) > 0 {
		var chatTools []any
		for _, t := range tools {
			tool, _ := t.(map[string]any)
			chatTools = append(chatTools, map[string]any{"type": "function", "function": map[string]any{
				"name": tool["name"], "description": tool["description"], "parameters": tool["parameters"], "strict": tool["strict"],
			}})
		}
		out["tools"] = chatTools
	}
	if reasoning, _ := body["reasoning"].(map[string]any); reasoning["effort"] != nil {
		out["reasoning_effort"] = reasoning["effort"]
	}
	return out
}

// chatChunk is one Chat Completions Stream chunk.
type chatChunk struct {
	Choices []struct {
		Delta struct {
			Content          string
			ReasoningContent string `json:"reasoning_content"`
			Reasoning        string
			Refusal          string
			ToolCalls        []struct {
				Index    int
				ID       string
				Function struct{ Name, Arguments string }
			} `json:"tool_calls"`
		}
	}
	Usage *struct {
		PromptTokens        int64 `json:"prompt_tokens"`
		CompletionTokens    int64 `json:"completion_tokens"`
		PromptTokensDetails *struct {
			CachedTokens *int64 `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
	}
	Error *struct {
		Code    any
		Message string
	}
}

// Output indexes of the synthesized Responses items.
const (
	chatReasoningIndex = 0
	chatTextIndex      = 1
	chatCallIndex      = 2 // first tool call; the next ones follow
)

func (c *Client) streamChat(ctx context.Context, body map[string]any, fn func(StreamEvent) error) error {
	var text, reasoning strings.Builder
	type call struct {
		id, name string
		args     strings.Builder
	}
	calls := map[int]*call{}
	var usage json.RawMessage
	emit := func(v map[string]any) error {
		var event StreamEvent
		if err := json.Unmarshal(raw(v), &event); err != nil {
			return err
		}
		return fn(event)
	}
	err := c.sse(ctx, "/chat/completions", chatBody(body), func(data string) error {
		var chunk chatChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			return fmt.Errorf("provider stream: invalid event: %w", err)
		}
		if chunk.Error != nil {
			return &StreamError{Type: "error", Code: fmt.Sprint(chunk.Error.Code), Message: chunk.Error.Message}
		}
		if u := chunk.Usage; u != nil {
			details := map[string]any{}
			if u.PromptTokensDetails != nil && u.PromptTokensDetails.CachedTokens != nil {
				details["cached_tokens"] = *u.PromptTokensDetails.CachedTokens
			}
			usage = raw(map[string]any{"input_tokens": u.PromptTokens, "output_tokens": u.CompletionTokens, "input_tokens_details": details})
		}
		for _, choice := range chunk.Choices {
			d := choice.Delta
			if r := d.ReasoningContent + d.Reasoning; r != "" {
				reasoning.WriteString(r)
				if err := fn(StreamEvent{Type: "response.reasoning_text.delta", OutputIndex: chatReasoningIndex, Delta: r}); err != nil {
					return err
				}
			}
			if t := d.Content; t != "" {
				text.WriteString(t)
				if err := fn(StreamEvent{Type: "response.output_text.delta", OutputIndex: chatTextIndex, Delta: t}); err != nil {
					return err
				}
			}
			if t := d.Refusal; t != "" {
				text.WriteString(t)
				if err := fn(StreamEvent{Type: "response.refusal.delta", OutputIndex: chatTextIndex, Delta: t}); err != nil {
					return err
				}
			}
			for _, tc := range d.ToolCalls {
				cl := calls[tc.Index]
				if cl == nil {
					cl = &call{}
					calls[tc.Index] = cl
				}
				if tc.ID != "" {
					cl.id = tc.ID
				}
				index := chatCallIndex + tc.Index
				if tc.Function.Name != "" && cl.name == "" {
					cl.name = tc.Function.Name
					item := map[string]any{"type": "function_call", "name": cl.name}
					if err := fn(StreamEvent{Type: "response.output_item.added", OutputIndex: int64(index), Item: raw(item)}); err != nil {
						return err
					}
				}
				if a := tc.Function.Arguments; a != "" {
					cl.args.WriteString(a)
					if err := fn(StreamEvent{Type: "response.function_call_arguments.delta", OutputIndex: int64(index), Delta: a}); err != nil {
						return err
					}
				}
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	var output []any
	if reasoning.Len() > 0 {
		output = append(output, map[string]any{"type": "reasoning", "summary": []any{map[string]any{"type": "summary_text", "text": reasoning.String()}}})
	}
	if text.Len() > 0 {
		output = append(output, map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": text.String()}}})
	}
	indexes := make([]int, 0, len(calls))
	for i := range calls {
		indexes = append(indexes, i)
	}
	sort.Ints(indexes)
	for _, i := range indexes {
		cl := calls[i]
		if cl.name == "" {
			return errors.New("provider stream: tool call without a name")
		}
		id := cl.id
		if id == "" {
			id = fmt.Sprintf("call_%d", i)
		}
		output = append(output, map[string]any{"type": "function_call", "call_id": id, "name": cl.name, "arguments": cl.args.String()})
	}
	if output == nil && usage == nil {
		return ErrIncompleteStream
	}
	response := map[string]any{"output": output}
	if usage != nil {
		response["usage"] = usage
	}
	if err := emit(map[string]any{"type": "response.completed", "response": response}); !errors.Is(err, ErrStreamDone) {
		return err
	}
	return nil
}

func raw(v any) json.RawMessage { data, _ := json.Marshal(v); return data }
