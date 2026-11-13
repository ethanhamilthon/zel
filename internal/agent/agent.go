// Package agent runs one model/tool loop without owning application state.
package agent

import (
	"context"
	"encoding/json"
	"zel/internal/entities"
)

type Host interface {
	AcceptSteering() bool
	Save() error
	Request(context.Context) ([]json.RawMessage, error)
	CommitResponse([]json.RawMessage) error
	Execute(context.Context, entities.ToolCall) error
	// Compact may shrink the context once all outputs of a step are committed.
	Compact() error
}

func Run(ctx context.Context, host Host) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		host.AcceptSteering()
		if err := host.Save(); err != nil {
			return err
		}
		items, err := host.Request(ctx)
		if err != nil {
			return err
		}
		if err := host.CommitResponse(items); err != nil {
			return err
		}
		calls := 0
		for _, item := range items {
			var call entities.ToolCall
			if err := json.Unmarshal(item, &call); err != nil {
				return err
			}
			if call.Type != "function_call" {
				continue
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			calls++
			if err := host.Execute(ctx, call); err != nil {
				return err
			}
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if calls > 0 {
			if err := host.Compact(); err != nil {
				return err
			}
		}
		if steered := host.AcceptSteering(); calls == 0 && !steered {
			return nil
		}
	}
}
