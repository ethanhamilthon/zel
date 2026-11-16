package core

import (
	"slices"

	"zel/internal/entities"
)

func (a *Runtime) Records() []Record { return a.session().Records() }

func (s *session) Records() []Record {
	s.histMu.RLock()
	defer s.histMu.RUnlock()
	var records []Record
	next := 0
	choicesBefore := func(seq int) {
		for ; next < len(s.choices) && s.choices[next].Seq <= seq; next++ {
			records = append(records, entities.ChoiceRecord(s.choices[next].Title, s.choices[next].Answer))
		}
	}
	for seq := range s.History {
		choicesBefore(seq)
		meta := s.itemMeta[seq]
		for _, r := range s.projection(seq).records {
			if meta != nil {
				m := *meta
				if r.Kind == "assistant" {
					m = s.answerMetadata(seq)
				}
				r.Meta = &m
			}
			records = append(records, r)
		}
	}
	choicesBefore(len(s.History))
	return append(records, s.imageWarnings...)
}

// answerMetadata derives turn totals without counting a request once per item.
// Legacy answers already contain turn totals and retain their original metadata.
func (s *session) answerMetadata(seq int) Metadata {
	m := *s.itemMeta[seq]
	if m.RequestSeq == 0 {
		return m
	}
	m.Duration, m.Usage = 0, nil
	var total Usage
	known := true
	for i := m.TurnSeq; i <= seq; i++ {
		for _, r := range s.projection(i).records {
			if r.Kind == "code" || r.Kind == "reasoning" {
				m.Steps++
			}
		}
		part := s.itemMeta[i]
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

func (a *Runtime) SessionUsage() (Usage, int) { return a.session().SessionUsage() }

// SessionUsage sums the token usage of every model request in the history.
// requests counts the requests that reported usage; Cached is -1 when one omitted it.
func (s *session) SessionUsage() (total Usage, requests int) {
	s.histMu.RLock()
	defer s.histMu.RUnlock()
	for seq, m := range s.itemMeta {
		if m == nil || m.Usage == nil || seq >= len(s.History) {
			continue
		}
		// One request owns several items; count it once, on its first item.
		// Legacy metadata (no RequestSeq) keeps turn totals on the answer only.
		if m.RequestSeq != 0 && m.RequestSeq != seq+1 {
			continue
		}
		if m.RequestSeq == 0 && !slices.ContainsFunc(s.projection(seq).records, func(r Record) bool { return r.Kind == "assistant" }) {
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

func (a *Runtime) UserMessages() int { return a.session().UserMessages() }

// UserMessages counts the user messages in the history.
func (s *session) UserMessages() (n int) {
	s.histMu.RLock()
	defer s.histMu.RUnlock()
	for seq := range s.History {
		for _, r := range s.projection(seq).records {
			if r.Kind == "user" {
				n++
			}
		}
	}
	return n
}
