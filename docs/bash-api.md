# zel 0.7 reference

Zel gives the model one tool, `bash`: its input is a bash script. Background
tasks, questions to the user, images and edits are `zel` commands the script
runs. You configure Zel with `zel.toml` files: settings, providers,
instructions, hooks and prompt completions.

## The bash tool

- Every call is a fresh `bash -c` in the session folder, in its own process
  group: `cd` and variables do not carry over (keep what must last in files, or
  use `zel cd DIR` to change the folder for the next calls).
- A call has `settings.bash_timeout` seconds (default 30). At the timeout the
  whole group is killed and the output ends with `[timeout after 30s]`. Longer
  work goes to `zel task run`.
- Output is stdout and stderr together. A nonzero exit ends it with
  `[exit N]`, a timeout with `[timeout after Ns]`.
- The text the agent gets is cut to `settings.bash_output` bytes (default 8000;
  `0` turns the cut off; values 1-999 are a config error). The limit counts the
  whole text: the output, the endings and what `bash_after` hooks add. A longer
  text becomes its first and last half of the limit, with a marker between
  (not counted in the limit); the endings stay at the end:

  ```
  …first 4000 bytes…

  ======== [output cut: 5000 of 13000 bytes omitted]
  ======== full output: /Users/…/sessions/…/outputs/b12.txt
  ======== read it with sed -n '100,200p', rg, head or tail

  …last 4000 bytes…
  ```

  A cut never splits a UTF-8 character and moves to a line break that is close
  (within ~10% of the half). The full output is written to
  `outputs/b<N>.txt` in the session folder, only when it is cut (64 MiB cap,
  with a note at its end when hit). `bash_after` hooks still get the full
  output in `out`. Only the agent's bash calls are cut: task output, hook output
  and `${{ }}` are not (a `zel -p` subagent runs through bash, so its answer is).
  Behind it, a 1 MiB in-memory safety cap keeps head and tail.
- `settings.show_bash` (default `true`) shows the running command in the working
  line. With `false` the line keeps only time, steps and queued input
  (`12s · 3 steps`), with no command and no `thinking...`, also after a call
  ends. Tracing and task rows are unchanged. The Settings menu has a "Show bash"
  row for it.
- stdin is empty; pagers are off (`PAGER=cat`, `GIT_PAGER=cat`).
- The agent wakes up only for user messages and short notifications such as
  `<notification>t1 finished</notification>`.
- Environment of every call, task, hook and `${{ }}`: `ZEL_SESSION_ID`,
  `ZEL_MODE` (`main` or `headless`), `ZEL_DEPTH`, `ZEL_PARENT` (for a child),
  `ZEL_SOCKET`, `ZEL_TOKEN`, `ZEL_CONFIG` (the config layers, `:`-separated),
  `ZEL_PROVIDER`, `ZEL_MODEL`, `ZEL_EFFORT`, `ZEL_TASK` inside a task,
  `ZEL_HOOK` inside a hook, the `[env]` of the config, and the inherited `PATH`
  with the running Zel binary's directory in front so `zel` finds itself.
  No special script folders are added. API keys are never exported.

There is no sandbox: bash runs with your user's permissions and network access.

## Commands

Inside a session (they talk to it through its socket):

```sh
zel task run [--name N] [--timeout S] [--cwd DIR] -- CMD... # background task; the script may come on stdin; prints t1
zel task list                                     # t1 running build 12s
zel task get ID                                   # state, exit code, command, last 40 lines
zel task tail ID [-n 20]
zel task wait ID... [--timeout S]                 # exit 124 if some still run
zel task stop ID                                  # SIGTERM to the group, SIGKILL after 2 s
zel choice "Question" "A" "B" ...                 # prints c1 at once; the answer arrives as a notification
zel cd [DIR]                                      # folder of the next bash calls and tasks; no DIR = start folder
zel view IMG...                                   # attach images to the next model request
zel session [--json] | zel session rename NAME
```

Anywhere:

```sh
zel edit FILE [--all] <<'EOF' ... EOF             # exact replacements, all or none; prints the count
zel docs [topic] | zel models [--json] | zel config [--config FILE]...
zel -p "prompt" [--model M --effort E --provider P --cwd DIR --timeout S --json --config FILE]
zel -p --resume ID "more"                         # continue a session
```

`zel cd DIR` sets the working folder of the session: every next bash call and
`zel task run` (unless `--cwd DIR` is given) starts there, so the agent does not
repeat `cd DIR && ...`. DIR is relative to the caller's folder, `~` works; no
argument goes back to the folder zel was started in. It prints the new absolute
path; a missing folder is ENOENT, a file EINVAL, and nothing changes. Running
tasks keep their folder. Zel itself stays where it started: the project, config
layers (`./zel.toml`), session list, hooks and `${{ }}` are not touched. A
`zel -p` called from a bash call starts in that call's folder, so after
`zel cd ~/some` a subagent works in `~/some` and loads `~/some/zel.toml`. The
folder is saved with the session: a resumed session's bash calls start in it
(if it is gone, in the start folder, with a notice). `zel session` shows it as
`bash_cwd` when it differs from `cwd`. Each session, subagents too, has its
own; a new subagent starts without it.

`zel edit` reads blocks from stdin, markers on their own lines:

```
<<<<<<< old
old text
=======
new text
>>>>>>> new
```

End a block with `>>>>>>> new all` to replace every occurrence. Old text that
is missing fails with ENOMATCH (exit 4), found more than once with EAMBIG (exit 5).

Errors are one line on stderr, `zel CMD: CODE: message`. Exit codes: 1 EFAIL,
2 EINVAL, 3 ENOENT, 4 ENOMATCH, 5 EAMBIG, 6 EDENIED, 7 ELIMIT, 124 ETIMEOUT,
130 ECANCELED.

Tasks run until they end, are stopped, or the session closes. A task that
ends while nobody waits for it wakes the agent with `<notification>t1
finished</notification>` (or `failed`, `stopped`, `timed out`).

`zel choice` does not wait. The UI shows the options instead of the input
(several questions queue up); the user picks one, chooses **Write your own
answer…**, or presses Esc. The custom answer edits like the composer: it grows
with its lines (Alt+Enter, Shift+Enter or Ctrl+J insert a newline), a click
moves the cursor (mouse reporting is on only while it is open), and up to 4096
characters fit; Enter sends a non-empty answer, and Esc returns to the options
without losing the custom text. The normal composer draft is preserved. Each
answer stays in the chat as `question: answer` (saved in the session's
`choices.jsonl`, so it is shown again on resume). The agent gets
`<notification>c1 Question: A</notification>` (or the custom text, or `cancelled`). It works
in the main session, its tasks and hooks; `zel -p` has no user (EDENIED).

New image previews are saved in the session's `attachments` folder; history
stores versioned references and loads preview bytes only for requests that need
them. Keep that folder with `session.jsonl` when copying a session. Old chats
with embedded images still work and are not migrated. An old Zel binary cannot
read the new references. Missing or corrupt preview files produce an explicit
error rather than silently omitting the image. Original image copies remain
available for crops. Tracing shows complete generated commands, not partial
argument fragments.

A subagent is `zel -p` run from the agent's bash. It becomes a child of the
session: `ZEL_MODE=headless`, the parent's model and config layers unless
given others. Child tracking participates in idle/lifecycle handling. Its answer goes
to stdout, `zel: session ID` to stderr (`--json` prints `{ok, answer,
session, error}`). `zel -p --resume ID` continues one of your own children
only. Children nest at most 3 deep. In a bash call a child has the same time
limit, so longer work runs as a task: `zel task run --name review -- zel -p "..."`.

## zel.toml

Layers, in merge order: `~/.zel/zel.toml` (or `$ZEL_HOME/zel.toml`),
`./zel.toml` of the working folder only, then each `--config FILE`. Settings
and tables merge by key (a later layer wins); instructions and hooks add up in
layer order; completions replace by name (with a warning in Tracing).

```toml
# top-level keys first: in TOML, keys after a [table] belong to that table
provider = "openai"            # default for new sessions; the model menu writes here
model    = "gpt-5"
effort   = "medium"            # none minimal low medium high xhigh max

instructions = [
  "You are zel. Short answers.",                              # main session only
  "${{ cat AGENTS.md }}",                                     # text from a command
  { text = "You are a subagent.", main = false, headless = true },
]

[settings]
compaction        = 400000     # tokens, 0 = off
reasoning_summary = true
slash_menu        = true
bash_timeout      = 30         # seconds per bash call
bash_output       = 8000       # bytes of one bash call the agent gets, 0 = no cut
show_bash         = true       # false: the working line has no command and no `thinking...`

[colors]                       # the TUI theme; see `zel docs colors`
accent = "#c3e88d"

[providers.openai]
base_url = "https://api.openai.com/v1"
api      = "responses"         # responses | chat
api_key  = "${{ echo $OPENAI_API_KEY }}"

[env]                          # for bash, tasks, hooks and ${{ }}
EDITOR = "true"

[hooks]
turn_end    = ['test -z "$(git status --porcelain)" || echo "uncommitted changes, commit them"']
bash_before = [{ match = 'git push (-f|--force)', run = "echo 'no force push' >&2; exit 2" }]

[completions]
plan = "${{ cat PLAN.md }}"
```

- `${{ script }}` in any string (not in hook `run`) is replaced by the output
  of `bash -c script`, one trailing newline trimmed; `$${{` is a literal `${{`.
  Each expression has 10 s; a failing one becomes empty and leaves a notice.
  Instructions, providers, settings and env are expanded when the session
  starts and whenever the config changes; completions when they are used.
- `instructions` entries are strings (main session only) or tables
  `{text, main = true, headless = false}`; `[[instructions]]` tables work too.
- Unknown keys and wrong types make the whole layer invalid: it is skipped
  with a notice naming the file (and the line, for syntax errors).
- Hot reload: Zel watches the layer files. A change is applied after the
  current turn; a broken file keeps the previous config. The system prompt
  itself is empty: the intro, `instructions` and the built-in zel text all go
  into the description of the bash tool. That text is
  rebuilt only when the instructions changed. Files read by `${{ }}` are not
  watched.
- The menus write back into the layer a value comes from: picking a model
  changes this session and the `provider`/`model`/`effort` lines of the file
  they come from (`~/.zel/zel.toml` when no file sets them). Other open
  sessions keep their model; so does an open session when you edit `model` by hand.
- `zel config` prints the merged config and where every key comes from.
- `settings.accent` of 0.7.0 is read as `colors.accent`, with a notice;
  picking an accent in Settings moves it there.

Tell the agent about your own tools in `instructions`. Invoke scripts by their
paths, or install them on your normal `PATH`; Zel does not manage script folders.

## Hooks

`[hooks]` maps an event to a list of commands. An entry is a string (the
command) or a table `{run, match, timeout, async, main, headless}`:
`match` is a regexp on the command (bash_*) or the text (message,
notification); `timeout` is in seconds (default 60); `async = true` starts the
hook and does not wait; hooks run in the main session only unless
`headless = true` (`main = false` leaves the main session out).

A hook is `bash -c` with the session environment and `ZEL_HOOK`; the event
data comes as JSON on stdin. Exit 0: stdout is used as below. Exit 2 blocks a
blockable event, with stderr as the reason. Any other exit is recorded in
session diagnostics and the hook is skipped. Hook execution records are hidden
from the Tracing view. Hooks run one by one, in layer order;
rewriting hooks chain.

| event | when | stdin | stdout (exit 0) | exit 2 |
|---|---|---|---|---|
| `session_created` | a new session | `{}` | | |
| `session_resumed` | a session is resumed | `{}` | | |
| `session_closed` | quit, new or resume | `{"reason"}` | | |
| `config_changed` | after hot reload | `{"ok","error","files"}` | | |
| `message` | the user sent a message | `{"text"}` | new text | the message is dropped |
| `notification` | before a notification reaches the agent | `{"text"}` (plain, no tags) | new text; zel wraps it in `<notification>` | it is dropped |
| `turn_start` | the agent starts a turn | `{"text"}` | | |
| `turn_end` | the agent answered | `{"answer","running":[{"id","name","ms"}]}` | a notification; the turn goes on (at most 3 times in a row) | |
| `idle` | the turn ended and no task or child runs | `{}` | | |
| `bash_before` | before a bash call | `{"command"}` | new command | not run; the agent sees the reason |
| `bash_after` | after a bash call | `{"command","out","code","ms","timed_out"}` | added to the output | |
| `compaction` | after auto compaction | `{"before","after"}` | | |
| `model_changed` | the session's model or effort changed | `{"provider","model","effort"}` | | |
| `error` | a turn failed | `{"kind","message"}` | | |

### Steering during bash

When a typed steering message arrives during a bash call, the already-running
process is moved into a background task without restarting it. Its existing
output is retained, its original timeout still applies, and normal task get,
stop and completion notifications work. The tool result gives the task ID.
Bash calls from the same model response that have not started are returned as
`[skipped: superseded by steering]`; they are not executed. The steering text
is delivered after these tool results, before the next model request.
Notifications alone do not move a running bash call into a task.

### What the agent sees

Only five events send hook output to the agent; the output of all others
(`session_*`, `config_changed`, `turn_start`, `idle`, `compaction`,
`model_changed`, `error`) is retained only in session diagnostics, not shown in
Tracing. Hiding hook execution records does not change which hooks run or what
they send to the agent.

**bash_after**: stdout of each hook is added on its own line after the output,
after `[exit N]`:

```
--- FAIL: TestParse (0.00s)
[exit 1]
uncommitted changes, commit them
```

**bash_before**: stdout replaces the command; the agent sees only the output of
the new command, with no note. Exit 2: the call does not run and the agent gets
only:

```
[blocked by a hook: no force push]
```

**turn_end**: stdout of all hooks, joined by new lines, comes to the agent as a
new message, and the turn goes on (at most 3 times in a row):

```
<notification>uncommitted changes, commit them</notification>
```

**message**: stdout replaces the whole user message; the agent gets only the
new text. Exit 2: the agent gets nothing, the chat shows
`Message blocked by a hook: <reason>`.

**notification**: the hook gets the plain text, `{"text":"t1 finished"}`; its
stdout replaces it and zel wraps it back (output that already is one
`<notification>…</notification>` block is not wrapped again):

```
<notification>t1 finished, run make test</notification>
```

Exit 2: the notification is dropped without a trace in the chat.

`session_closed` runs within what is left of the 5 s shutdown budget, after
the session's tasks were stopped. Asynchronous hooks are cancelled and joined
before session resources are released; they do not outlive the session.

zel writes no window title or progress to the terminal. A hook can, by writing
to `/dev/tty`; the global example shows the terminal progress bar (OSC 9;4)
on `turn_start`, `turn_end`, `error` and `session_closed`.

## Prompt completions

`[completions] name = "text"` registers `#name` in the main session. Typing
`#` in the composer lists the names; when you send a message, each `#name`
is replaced by its text with `${{ }}` expanded (10 s), wrapped in a tag so the
agent sees where it starts and ends:

```
fix this #plan
```
is sent as
```
fix this <completion name="plan">
...text of plan...
</completion>
```

An empty or failing completion keeps your draft and shows why; Ctrl+D cancels
one still running. Names are 1 to 64 letters, digits, `-` or `_`; unknown
`#names` stay as typed, with no tag.

A `#name` or `@path` picked from a completion list is one token in the
composer: it shows in the accent color, the cursor jumps over it, and one
Backspace (right after it) or Delete (right before it) removes it whole.

## Colors

`[colors]` sets every color of the TUI. Values are `#rrggbb` hex; a missing
key keeps the default. Layers merge by key, a change applies on reload, and a
bad value is a config error: the previous config stays and the error is shown.

```toml
[colors]
bg             = ""         # empty: the terminal's own background
accent         = "#d97757"
agent          = "#d6d6d6"
user           = "#606060"
user_highlight = "#8c8c8c"
gray           = "#929292"
dark_gray      = "#606060"
error          = "#e27878"

[colors.trace]
system  = "#606060"
user    = "#d97757"
agent   = "#ffffff"
bash    = "#a0a0a0"
notice  = "#808080"   # notices, compaction, tasks
output  = "#404040"
error   = "#e27878"
```

- `bg`: the background of the whole screen (chat, composer, menus, Tracing,
  choice, onboarding).
- `accent`: the composer rail, the `*` header over answers, completed `#name`
  and `@path` tokens in the composer, answered choice questions, choice and menu
  markers, status icons, onboarding title, headings, inline code and links in
  answers, keywords in highlighted bash. Settings → Accent color picks one from
  a palette and writes `colors.accent` into the layer it comes from; a hex
  outside the palette shows as "Custom".
- `agent`: the text of answers, the main text of menus, Info values, the
  choice title and the rail text of forms.
- `user`: your messages and choice answers in the chat and the composer text; `user_highlight`:
  the composer line under the cursor.
- `gray`: hints, time and steps, the working line, inactive menu rows, Info
  keys, the `+ N more` row, the composer placeholder, punctuation in bash.
- `dark_gray`: the "context compacted" divider, Tracing row numbers, onboarding
  subtitle and hints, bash comments, the pulse of a queued composer.
- `error`: failed turns, prompt errors, onboarding form errors.
- `trace.*`: the kind labels and bodies in Tracing, one per kind; `trace.bash`
  is also the plain text of highlighted bash.

Strings and numbers in highlighted bash keep fixed tones.

## Examples

See the examples topic: a global zel.toml, a project zel.toml and a standalone
web-search script.
