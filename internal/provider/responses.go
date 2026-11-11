package provider

import (
	"context"
	"encoding/json"
	"fmt"
)

func (c *Client) streamOnce(ctx context.Context, body map[string]any, fn func(StreamEvent) error) error {
	if c.api == APIChat {
		return c.streamChat(ctx, body, fn)
	}
	body["stream"] = true
	return c.sse(ctx, "/responses", body, func(data string) error {
		var event StreamEvent
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			return fmt.Errorf("provider stream: invalid event: %w", err)
		}
		return fn(event)
	})
}
