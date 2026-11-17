# Changelog

## 0.10.0

Breaking release. No migration or rollback of the old config or sessions.

- Settings now live in `~/.config/zel/settings.toml`, with hot-reloaded
  `hooks/`, `prompts/` and `instructions/` snippets beside them.
- `[keys]` rebinds UI shortcuts; Ctrl/Alt letters work on Russian layouts.
- New sessions clear the old history immediately and open the model picker.
- Bash totals count tool calls, not reasoning blocks, including live calls.
- Prompt completion rows show a gray preview without executing expansions.
- Enter turns an @ path into a token; only Tab enters a directory.


## 0.9.5

Tracing is easier to find your way in.

- The list is grouped by turn: a row per turn with its first message and how
  long it took. It opens at the latest entry.
- A bash call and its output are one entry: the command, then the output.
- Rows show a failure (`exit 1`, `timeout`) and the duration of a bash call,
  hook or model request; hook rows lose the repeated `-> exit 0, 5ms`.
- An entry starts with its time, exit and duration, and the model request it
  came from: its number, time and tokens (in, cached, out). Its title says
  where you are: `Tracing / bash · 3/12 · turn 4`.
- Tab (Shift+Tab back) cycles the view: All, Bash, Agent and Errors. Left and
  Right in an entry go through the entries of the view only; Esc returns to
  the same row of the list.
- `h` hides or shows the hook runs; `settings.trace_hooks = false` opens
  Tracing without them.

## 0.9.4

- Menu lists fill the whole box: the details (session dates, providers,
  trace times) are flush with its right edge, the arrow of the selected row in
  its last column.
- The main menu says what each item does instead of its hotkey; Help still
  lists the hotkeys.

## 0.9.3

- The input grows with its text up to the whole screen, not a third of it.
- New, shorter description of the bash tool and the zel commands, for main and
  headless sessions; it adds `zel docs [TOPIC]`.
- The turn summary and the working line count `bashs` instead of `steps`.
- Menu pages are a box of half the screen (at least 60×16), centered, with the
  content at its top left.
- Docs: group hooks as one `[hook.NAME]` with a list of events, told apart by
  `$ZEL_HOOK`; the example config does so for the progress bar.

## 0.9.2

**The config format changed**; a 0.9.1 file is refused with a message that
names the replacement of each old key.

- Providers, instructions, hooks and completions are named maps:
  `[provider.NAME]`, `[instruction.NAME]`, `[hook.NAME]` (with `event`, which
  may be a list, and `run`), `[completion.NAME]`. An instruction or completion
  may be just a string.
- `model = "PROVIDER/MODEL"` replaces the top-level `provider` and `model`
  (TOML cannot have `provider` as a string and a table).
- Modes: every entry has `modes = [...]`. A session runs in `main` or
  `headless`, and `--mode NAME` (TUI and `zel -p`) adds a custom mode, so
  `modes = ["plan"]` entries apply only to `--mode plan` sessions. Defaults:
  `["main"]` for instructions, hooks and completions, every mode for providers.
  `ZEL_CUSTOM_MODE` holds the custom mode.
- The models menu (Ctrl+M) shows the current model first.
- Every menu page is one box of the same size; the title (and the search line)
  stays while the list or the text scrolls.
- Ctrl+E opens the composer text in your editor (`[settings] editor`, default
  `nvim`); the session keeps working meanwhile and the saved text comes back.
- A running task has its own line like the working line (`◐ 6s · name`),
  one empty line above the working line.
- Info lists the running subagents with their model, effort, context and spend
  (input, output, cache).

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
- A `bash_after` hook with `match` now works (it never matched before); `match`
  on events with nothing to match is a config error.
- `zel view` and `zel cd` resolve a relative path from the folder of the call
  (a script that did `cd sub`), as documented; they used the session folder.
- Writing the model into `config.toml` is a small top-level edit (the file
  keeps its comments and tables); the old general TOML editor is gone.
- Ctrl+K opens the menu everywhere, also in a custom answer.
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
