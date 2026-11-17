# Changelog

## 0.9.1

Cleanup release. 0.9.0 is the baseline: there is no migration from older
versions and `zel update` only replaces the binary.

- The bash tool description has two texts, one for the main session and one for
  `zel -p` (headless): no `zel choice`, no question about subagents, only what
  the agent cannot guess by itself.
- `settings.slash_menu` and `settings.reasoning_summary` are gone: both are
  always on.
- A model stream is no longer cut after 10 minutes: the limit is on the
  response headers, not on the whole body.
- Fewer layers in the runtime: the `agent` loop lives in `core`, the
  controller talks to the session directly, one mode name (`main`/`headless`)
  and one place that redacts API keys.
- The repository holds only what ships: old plans, audits, release evidence
  and the update script are removed.
- Documentation was checked against the code.

## 0.9.0

- One config: `~/.zel/config.toml`. No per-folder config, layers or merging.
  `--config FILE` replaces it. Everything Zel writes is in `~/.zel/.sessions/`.
- Providers, Settings and onboarding are gone from the UI. Without a provider
  the TUI exits with an error. The app only writes `provider`/`model`/`effort`
  when you choose a model.
- No image handling in the app: `zel view IMG` only, in memory. Saved history
  keeps `[an image was here]`.
- Interface: lists share one design, menus are centered, the mouse is left to
  the terminal, the working command is one dark gray line.
- Retry: six attempts, exponential backoff with jitter, `Retry-After`.
- Production Go code went from 13,981 to 12,936 lines.

## 0.8.0

The terminal interface runs on tcell instead of Bubble Tea: one shared editor
and searchable list, direct terminal rendering, no second frontend.

## 0.7.6

Answered choices stay in the chat as `question: answer`, the custom answer
edits like the composer, steadier screen updates, completed `#name` and
`@path` tokens in the composer.

## 0.7.5

Performance and simpler internals: cached history projections, incremental
saves, less render work, custom answers for `zel choice`, complete commands
in Tracing.
