package tui

// Committed hidden records are reconstructed from the runtime trace. Only live
// previews need a UI copy; keep chat history and turn summaries for scrollback.
func (m *model) releaseCommittedTrace() {
	kept := m.entries[:0]
	for _, e := range m.entries {
		switch e.Kind {
		case "user", "assistant", "compaction", "turn_end":
			kept = append(kept, e)
		default:
			if e.Preview {
				kept = append(kept, e)
			}
		}
	}
	if len(kept) != len(m.entries) {
		clear(m.entries[len(kept):])
		m.entries = kept
		m.cache = nil
		m.historyStale = true
	}
}
