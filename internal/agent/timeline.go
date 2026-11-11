package agent

import (
	"encoding/json"
	"slices"
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
		Role, Type, Name, Arguments, Output string
		Content                             json.RawMessage
		Summary                             []struct{ Text string }
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
				text, _, _ = strings.Cut(text, originalImage)
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
	case v.Type == "function_call":
		var args struct{ Command string }
		_ = json.Unmarshal([]byte(v.Arguments), &args)
		if v.Name == "bash" {
			return []Record{{Kind: "code", Text: args.Command}}
		}
		return []Record{{Kind: "code", Text: v.Name + "\n" + v.Arguments}}
	case v.Type == "function_call_output":
		var result struct {
			Output string
			Error  bool
		}
		if json.Unmarshal([]byte(v.Output), &result) == nil {
			kind := "output"
			if result.Error {
				kind = "error"
			}
			return []Record{{Kind: kind, Text: result.Output}}
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

func (a *Agent) Records() []Record {
	a.histMu.RLock()
	defer a.histMu.RUnlock()
	var records []Record
	for seq, item := range a.History {
		meta := a.itemMeta[seq]
		for _, r := range RecordsForItem(item) {
			if meta != nil {
				m := *meta
				if r.Kind == "assistant" {
					m = a.answerMetadata(seq)
				}
				r.Meta = &m
			}
			records = append(records, r)
		}
	}
	return append(records, a.imageWarnings...)
}

// answerMetadata derives turn totals without counting a request once per item.
// Legacy answers already contain turn totals and retain their original metadata.
func (a *Agent) answerMetadata(seq int) Metadata {
	m := *a.itemMeta[seq]
	if m.RequestSeq == 0 {
		return m
	}
	m.Duration, m.Usage = 0, nil
	var total Usage
	known := true
	for i := m.TurnSeq; i <= seq; i++ {
		for _, r := range RecordsForItem(a.History[i]) {
			if r.Kind == "code" || r.Kind == "reasoning" {
				m.Steps++
			}
		}
		part := a.itemMeta[i]
		if part == nil || (part.RequestSeq != 0 && part.RequestSeq != i+1) {
			continue
		}
		m.Duration += part.Duration
		if part.RequestSeq == 0 {
			continue
		}
		if part.Usage == nil {
			known = false
			continue
		}
		total.Input += part.Usage.Input
		total.Output += part.Usage.Output
		if total.Cached < 0 || part.Usage.Cached < 0 {
			total.Cached = -1
		} else {
			total.Cached += part.Usage.Cached
		}
	}
	if known {
		m.Usage = &total
	}
	return m
}

// SessionUsage sums the token usage of every model request in the history.
// requests counts the requests that reported usage; Cached is -1 when one omitted it.
func (a *Agent) SessionUsage() (total Usage, requests int) {
	a.histMu.RLock()
	defer a.histMu.RUnlock()
	for seq, m := range a.itemMeta {
		if m == nil || m.Usage == nil || seq >= len(a.History) {
			continue
		}
		// One request owns several items; count it once, on its first item.
		// Legacy metadata (no RequestSeq) keeps turn totals on the answer only.
		if m.RequestSeq != 0 && m.RequestSeq != seq+1 {
			continue
		}
		if m.RequestSeq == 0 && !slices.ContainsFunc(RecordsForItem(a.History[seq]), func(r Record) bool { return r.Kind == "assistant" }) {
			continue
		}
		requests++
		total.Input += m.Usage.Input
		total.Output += m.Usage.Output
		if total.Cached < 0 || m.Usage.Cached < 0 {
			total.Cached = -1
		} else {
			total.Cached += m.Usage.Cached
		}
	}
	return total, requests
}

// UserMessages counts the user messages in the history.
func (a *Agent) UserMessages() (n int) {
	a.histMu.RLock()
	defer a.histMu.RUnlock()
	for _, item := range a.History {
		for _, r := range RecordsForItem(item) {
			if r.Kind == "user" {
				n++
			}
		}
	}
	return n
}
