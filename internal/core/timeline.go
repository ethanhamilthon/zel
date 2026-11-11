package core

import (
	"slices"
)

func (a *Runtime) Records() []Record {
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
func (a *Runtime) answerMetadata(seq int) Metadata {
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
func (a *Runtime) SessionUsage() (total Usage, requests int) {
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
func (a *Runtime) UserMessages() (n int) {
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
