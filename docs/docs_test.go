package docs

import (
	"strings"
	"testing"
)

// Every section of lua-api.md has a topic and every topic returns text, so the
// agent can reach the whole reference through docs().
func TestEverySectionIsReachable(t *testing.T) {
	for _, h := range Sections() {
		if h == "Examples" {
			continue // examples has its own topic
		}
		if _, ok := sections[h]; !ok {
			t.Errorf("section %q of lua-api.md has no topic", h)
		}
	}
	for _, topic := range Topics() {
		text, ok := Get(topic.Name)
		if !ok || strings.TrimSpace(text) == "" {
			t.Errorf("topic %q is empty", topic.Name)
		}
		if !strings.Contains(Index(), `"`+topic.Name+`"`) {
			t.Errorf("index misses %q", topic.Name)
		}
	}
	if _, ok := Get("nope"); ok {
		t.Error("unknown topic resolved")
	}
	if text, _ := Get("hooks"); !strings.Contains(text, "turn_end") || strings.Contains(text, "Prompt completions") {
		t.Errorf("hooks topic is wrong: %q", text)
	}
}
