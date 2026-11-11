package provider

import (
	"context"
	"sort"
	"zel/internal/entities"
)

func AllModels(ctx context.Context, providers map[string]*Client) (choices []entities.ModelChoice, errs map[string]error) {
	type result struct {
		name   string
		models []ModelInfo
		err    error
	}
	results := make(chan result, len(providers))
	for name, p := range providers {
		go func(name string, p *Client) {
			models, err := p.Models(ctx)
			results <- result{name, models, err}
		}(name, p)
	}
	errs = map[string]error{}
	for range providers {
		r := <-results
		if r.err != nil {
			errs[r.name] = r.err
			continue
		}
		for _, m := range r.models {
			choices = append(choices, entities.ModelChoice{Provider: r.name, Model: m.ID, ContextWindow: m.Window()})
		}
	}
	sort.Slice(choices, func(i, j int) bool {
		if choices[i].Provider != choices[j].Provider {
			return choices[i].Provider < choices[j].Provider
		}
		return choices[i].Model < choices[j].Model
	})
	return choices, errs
}

// SelectSession changes the model of this session and makes it the default for new
// sessions by persisting the selection in SQLite (see SaveDefault).
// Saved sessions keep their own model. Call between turns. Failed validation or
// persistence of the session restores the old selection.
