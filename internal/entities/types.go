package entities

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"time"
)

type Usage struct {
	Input  int64
	Output int64
	Cached int64 // -1 means the provider omitted cached-token data.
}

type Metadata struct {
	Duration     time.Duration
	Usage        *Usage
	ContextInput *int64
	Steps        int       `json:",omitempty"`
	RequestID    string    `json:"-"`
	RequestSeq   int       `json:",omitempty"`
	TurnSeq      int       `json:",omitempty"`
	At           time.Time `json:",omitzero"`
}

type Record struct {
	Preview bool
	Kind    string
	Text    string
	ID      string
	Meta    *Metadata
}

type Event struct {
	Kind        string
	Text        string
	ID          string
	Meta        *Metadata
	Final       bool
	Provisional bool
}

type Input struct {
	QueueID uint64 // Optional composer identity, used to withdraw a queued draft

	Text   string
	Images []Image
	SentAt time.Time `json:"-"`
	Notice bool
}

func (input Input) Display() string {
	if len(input.Images) == 0 {
		return input.Text
	}
	text := input.Text
	for _, image := range input.Images {
		text += "\n[Image: " + filepath.Base(image.Path) + "]"
	}
	return strings.TrimSpace(text)
}

type TraceEntry struct {
	Kind string // system, user, assistant, thinking, bash, output, error, notice, task, event
	Text string
	At   time.Time
}

type SessionSummary struct {
	ID      string
	Title   string
	Project string
	Updated time.Time
}

type Image struct {
	Path string `json:"path,omitempty"`
	URL  string `json:"url"`
}

func ApplyEvent(records []Record, e Event) []Record {
	kind := e.Kind
	switch kind {
	case "delta", "refusal_delta":
		kind = "assistant"
	case "reasoning_delta":
		kind = "reasoning"
	}
	if e.ID != "" {
		for i := len(records) - 1; i >= 0; i-- {
			if records[i].ID == e.ID {
				if e.Final {
					records[i].Text = e.Text
				} else {
					records[i].Text += e.Text
				}
				records[i].Kind = kind
				records[i].Preview = e.Provisional
				if e.Meta != nil {
					records[i].Meta = e.Meta
				}
				return records
			}
		}
	} else if e.Kind == "delta" && len(records) > 0 && records[len(records)-1].Kind == kind {
		records[len(records)-1].Text += e.Text
		return records
	}
	return append(records, Record{Kind: kind, Text: e.Text, ID: e.ID, Meta: e.Meta, Preview: e.Provisional})
}

func RecordsForItem(item json.RawMessage) []Record {
	var v struct {
		Role, Type, Name, Arguments, Output, Text string
		Content                                   json.RawMessage
		Summary                                   []struct{ Text string }
	}
	if json.Unmarshal(item, &v) != nil {
		return nil
	}
	switch {
	case v.Role == "user" || v.Role == "assistant" || v.Type == "message":
		var text string
		if json.Unmarshal(v.Content, &text) != nil {
			var parts []struct {
				Type    string `json:"type"`
				Text    string `json:"text"`
				Refusal string `json:"refusal"`
			}
			if json.Unmarshal(v.Content, &parts) == nil {
				var texts []string
				for _, p := range parts {
					if p.Text != "" {
						texts = append(texts, p.Text)
					}
					if p.Refusal != "" {
						texts = append(texts, p.Refusal)
					}
				}
				text = strings.Join(texts, "\n")
			}
		}
		kind := v.Role
		if kind == "" {
			kind = "assistant"
		}
		if IsNotice(text) {
			kind = "notice"
		}
		if text != "" {
			if kind == "user" {
				text, _, _ = strings.Cut(text, OriginalImage)
			}
			return []Record{{Kind: kind, Text: text}}
		}
	case v.Type == "reasoning":
		var texts []string
		for _, p := range v.Summary {
			if p.Text != "" {
				texts = append(texts, p.Text)
			}
		}
		var parts []struct{ Text string }
		_ = json.Unmarshal(v.Content, &parts)
		for _, p := range parts {
			if p.Text != "" {
				texts = append(texts, p.Text)
			}
		}
		if len(texts) == 0 {
			var c string
			if json.Unmarshal(v.Content, &c) == nil && c != "" {
				texts = append(texts, c)
			}
		}
		if len(texts) > 0 {
			return []Record{{Kind: "reasoning", Text: strings.Join(texts, "\n")}}
		}
	case v.Type == "compaction":
		return []Record{{Kind: "compaction", Text: "Context compacted"}}
	case v.Type == "turn_end":
		return []Record{{Kind: "turn_end", Text: v.Text}}
	case v.Type == "function_call":
		var args struct{ Command string }
		_ = json.Unmarshal([]byte(v.Arguments), &args)
		switch v.Name {
		case "bash":
			return []Record{{Kind: "code", Text: args.Command}}

		}
		return []Record{{Kind: "code", Text: v.Name + "\n" + v.Arguments}}
	case v.Type == "function_call_output":
		if strings.HasPrefix(v.Output, "error: ") {
			return []Record{{Kind: "error", Text: v.Output}}
		}
		return []Record{{Kind: "output", Text: v.Output}}
	default:
		// Provider-managed tool events remain visible even without a specialized UI.
		if v.Type != "" && v.Type != "instructions" {
			return []Record{{Kind: "output", Text: string(item)}}
		}
	}
	return nil
}

// IsNotice reports whether a user-role message is a notification rather
// than something the user typed. The other prefixes come from sessions saved
// before 0.5.2.
func IsNotice(text string) bool {
	return strings.HasPrefix(text, "<notification>") || strings.HasPrefix(text, "<task-result ") ||
		strings.HasPrefix(text, "<agent-result ") || strings.HasPrefix(text, "<view>")
}

const OriginalImage = "\nOriginal image (for crops): "

type ToolCall struct {
	Type, Name, Arguments string
	CallID                string `json:"call_id"`
}
type ModelChoice struct {
	Provider, Model string
	ContextWindow   int64
}
