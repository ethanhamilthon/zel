package config

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// Expander runs a bash script (`bash -c script`) and returns its stdout.
type Expander func(ctx context.Context, script string) (string, error)

// Expand replaces every ${{ script }} in s with the output of the script, minus
// one trailing newline. "$${{" stands for a literal "${{". A failed expression
// expands to "" and is reported in the returned error; the rest of s is still
// expanded. With a nil run, s is returned unchanged.
func Expand(ctx context.Context, s string, run Expander) (string, error) {
	if run == nil || !strings.Contains(s, "${{") {
		return s, nil
	}
	var b strings.Builder
	var errs []error
	for {
		i := strings.Index(s, "${{")
		if i < 0 {
			b.WriteString(s)
			break
		}
		if i > 0 && s[i-1] == '$' { // escaped: $${{ -> ${{
			b.WriteString(s[:i-1] + "${{")
			s = s[i+3:]
			continue
		}
		b.WriteString(s[:i])
		j := strings.Index(s[i+3:], "}}")
		if j < 0 {
			b.WriteString(s[i:])
			errs = append(errs, fmt.Errorf("%s: missing }}", clip(s[i:], 43)))
			break
		}
		script := strings.TrimSpace(s[i+3 : i+3+j])
		s = s[i+3+j+2:]
		if script == "" {
			continue
		}
		out, err := runExpr(ctx, script, run)
		if err != nil {
			errs = append(errs, fmt.Errorf("${{ %s }}: %w", clip(script, 40), err))
			continue
		}
		b.WriteString(out)
	}
	return b.String(), errors.Join(errs...)
}

func runExpr(ctx context.Context, script string, run Expander) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, ExprTimeout)
	defer cancel()
	out, err := run(ctx, script)
	if err != nil {
		return "", err
	}
	out = strings.TrimSuffix(out, "\n")
	return strings.TrimSuffix(out, "\r"), nil
}

// clip shortens s to at most n runes.
func clip(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}
