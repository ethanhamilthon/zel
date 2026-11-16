package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFreeformRoundTrip(t *testing.T) {
	t.Setenv("ZEL_FREEFORM", "1")
	var sent map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(data, &sent)
		w.Header().Set("Content-Type", "text/event-stream")
		for _, ev := range []string{
			`{"type":"response.output_item.added","output_index":0,"item":{"type":"custom_tool_call","call_id":"c1","name":"script","input":""}}`,
			`{"type":"response.custom_tool_call_input.delta","output_index":0,"delta":"print(\"a"}`,
			`{"type":"response.custom_tool_call_input.delta","output_index":0,"delta":"\"\n)"}`,
			`{"type":"response.custom_tool_call_input.done","output_index":0,"input":"print(\"a\"\n)"}`,
			`{"type":"response.output_item.done","output_index":0,"item":{"type":"custom_tool_call","call_id":"c1","name":"script","input":"print(\"a\"\n)"}}`,
			`{"type":"response.completed","response":{"output":[{"type":"custom_tool_call","call_id":"c1","name":"script","input":"print(\"a\"\n)"}]}}`,
		} {
			fmt.Fprintf(w, "data: %s\n\n", ev)
		}
	}))
	defer server.Close()
	client := New(server.URL, "k", APIResponses)
	if !client.FreeformTools() {
		t.Fatal("freeform should be on")
	}
	history := []json.RawMessage{
		json.RawMessage(`{"role":"user","content":"hi"}`),
		json.RawMessage(`{"type":"function_call","call_id":"p1","name":"script","arguments":"{\"code\":\"x = 1\"}"}`),
		json.RawMessage(`{"type":"function_call_output","call_id":"p1","output":"ok"}`),
	}
	tools := []Tool{{Name: "script", Description: "d", Freeform: "code", Parameters: map[string]any{"type": "object"}}}
	var args string
	var done json.RawMessage
	err := client.Stream(context.Background(), Request{Model: "m", Input: history, Tools: tools}, func(e StreamEvent) error {
		switch e.Type {
		case "response.function_call_arguments.delta":
			args += e.Delta
		case "response.completed":
			done = e.Response.Output[0]
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid([]byte(args + `"}`)) {
		t.Errorf("delta fragments do not form a JSON prefix: %q", args)
	}
	var arguments struct{ Code string }
	var call struct{ Type, Arguments string }
	_ = json.Unmarshal(done, &call)
	_ = json.Unmarshal([]byte(call.Arguments), &arguments)
	if call.Type != "function_call" || arguments.Code != "print(\"a\"\n)" {
		t.Errorf("completed item not normalized: %s", done)
	}
	body, _ := json.Marshal(sent)
	for _, want := range []string{`"type":"custom"`, `"custom_tool_call"`, `"input":"x = 1"`, `"custom_tool_call_output"`} {
		if !strings.Contains(string(body), want) {
			t.Errorf("request lacks %s: %s", want, body)
		}
	}
	if strings.Contains(string(body), `"strict"`) {
		t.Errorf("freeform tool must not be a strict function: %s", body)
	}
}

func TestFreeformOffForChatAndPlainHosts(t *testing.T) {
	if New("http://localhost:1", "", APIResponses).FreeformTools() {
		t.Error("unknown host must use JSON functions")
	}
	if New("https://api.openai.com/v1", "", APIChat).FreeformTools() {
		t.Error("chat api has no custom tools")
	}
	if !New("https://api.openai.com/v1", "", APIResponses).FreeformTools() {
		t.Error("openai responses should use custom tools")
	}
}
