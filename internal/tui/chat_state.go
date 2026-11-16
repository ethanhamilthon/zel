package tui

// Committed hidden records are reconstructed from the runtime trace. Only live
// previews need a UI copy; keep chat history and turn summaries for scrollback.
func (m *model) releaseCommittedTrace() {
	kept, cached := m.entries[:0], m.cache[:0]
	for i, e := range m.entries {
		keep := e.Preview
		switch e.Kind {
		case "user", "assistant", "choice", "compaction", "turn_end":
			keep = true
		}
		if keep {
			kept = append(kept, e)
			if i < len(m.cache) {
				cached = append(cached, m.cache[i])
			}
		}
	}
	clear(m.entries[len(kept):])
	clear(m.cache[len(cached):])
	m.entries, m.cache = kept, cached
	// Removed records had no chat rows. Preserve rendered chat and its index
	// instead of invalidating every old answer at the end of each tool turn.
}
