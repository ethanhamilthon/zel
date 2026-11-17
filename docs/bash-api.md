# zel 0.9 reference

Zel gives the model one tool, `bash`: its input is a bash script. Background
tasks, questions to the user, images and edits are `zel` commands the script
runs. You configure Zel with one file, `~/.zel/config.toml`: settings, providers,
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
  ======== full output: /Users/…/.sessions/…/outputs/b12.txt
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
  (`12s · 3 bashs`), with no command and no `thinking...`, also after a call
  ends. Tracing and task rows are unchanged. The command is cut to one line
  with `...` at its end, in the dark gray color.
- A running background task has a line of its own above the working line, in
  the same style with another sprite: `◐ 6s · test background`, the time in the
  accent color and the task's name in dark gray; one empty line separates the
  tasks from the working line.
- stdin is empty; pagers are off (`PAGER=cat`, `GIT_PAGER=cat`).
- The agent wakes up only for user messages and short notifications such as
  `<notification>t1 finished</notification>`.
- Environment of every call, task, hook and `${{ }}`: `ZEL_SESSION_ID`,
  `ZEL_MODE` (`main` or `headless`), `ZEL_CUSTOM_MODE` (the `--mode`, when set), `ZEL_DEPTH`, `ZEL_PARENT` (for a child),
  `ZEL_SOCKET`, `ZEL_TOKEN`, `ZEL_CONFIG` (the config file),
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
zel docs [topic] | zel models [--json] | zel config [--config FILE]
zel -p "prompt" [--model M --effort E --provider P --cwd DIR --timeout S --json --config FILE]
zel -p --resume ID "more"                         # continue a session
```

`zel cd DIR` sets the working folder of the session: every next bash call and
`zel task run` (unless `--cwd DIR` is given) starts there, so the agent does not
repeat `cd DIR && ...`. DIR is relative to the caller's folder, `~` works; no
argument goes back to the folder zel was started in. It prints the new absolute
path; a missing folder is ENOENT, a file EINVAL, and nothing changes. Running
tasks keep their folder. Zel itself stays where it started: the project, config,
session list, hooks and `${{ }}` are not touched. A
`zel -p` called from a bash call starts in that call's folder, so after
`zel cd ~/some` a subagent works in `~/some`. The
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
with its lines (Alt+Enter, Shift+Enter or Ctrl+J insert a newline), and up to
4096 characters fit; Enter sends a non-empty answer,
and Esc returns to the options
without losing the custom text. The normal composer draft is preserved. Each
answer stays in the chat as `question: answer` (saved in the session's
`choices.jsonl`, so it is shown again on resume). The agent gets
`<notification>c1 Question: A</notification>` (or the custom text, or `cancelled`). It works
in the main session, its tasks and hooks; `zel -p` has no user (EDENIED).

Images are only what `zel view IMG...` attaches: Zel stores nothing of them and
has no image paste of its own. A preview (PNG or JPEG, shrunk to the request
limits) stays in memory while the session runs and goes to the model with the
next request. It is not saved: in `session.jsonl` an image becomes the text
`[an image was here]`, which a resumed session shows instead. The original file
is not copied, so the agent can still read it by its path. Tracing shows complete
generated commands, not partial argument fragments.

A subagent is `zel -p` run from the agent's bash. It becomes a child of the
session: `ZEL_MODE=headless`, the parent's model and config file unless
given others. Child tracking participates in idle/lifecycle handling. Its answer goes
to stdout, `zel: session ID` to stderr (`--json` prints `{ok, answer,
session, error}`). `zel -p --resume ID` continues one of your own children
only. Children nest at most 3 deep. In a bash call a child has the same time
limit, so longer work runs as a task: `zel task run --name review -- zel -p "..."`.

## config.toml

One file: `~/.zel/config.toml` (or `$ZEL_HOME/config.toml`). `--config FILE`
(one path) replaces it: Zel works by that file alone, with no inheritance or
merging. There is no per-folder config. Zel creates `~/.zel` with a commented
`config.toml` and a `.gitignore` (`.env`, `.sessions/`) when they are missing.
Everything else Zel writes lives in `~/.zel/.sessions/`. Without a configured
provider the TUI exits with an error. The app never edits this file, except
that choosing a model writes `provider`, `model` and `effort`; Providers and
Settings are not in the UI, you edit the file and Zel follows it.

```toml
# top-level keys first: in TOML, keys after a [table] belong to that table
model  = "openai/gpt-5"        # PROVIDER/MODEL, default for new sessions; the model menu writes here
effort = "medium"              # none minimal low medium high xhigh max

[settings]
compaction        = 400000     # tokens, 0 = off
bash_timeout      = 30         # seconds per bash call
bash_output       = 8000       # bytes of one bash call the agent gets, 0 = no cut
show_bash         = true       # false: the working line has no command and no `thinking...`
editor            = "nvim"     # Ctrl+E opens the composer text in it

[colors]                       # the TUI theme; see `zel docs colors`
accent = "#c3e88d"

[provider.openai]
base_url = "https://api.openai.com/v1"
api      = "responses"         # responses | chat
api_key  = "${{ echo $OPENAI_API_KEY }}"

[env]                          # for bash, tasks, hooks and ${{ }}
EDITOR = "true"

[instruction.style]
text  = "You are zel. Short answers."
modes = ["main", "headless"]   # default ["main"]

[instruction]
agents = "${{ cat AGENTS.md }}" # a string is the text; main only

[hook.uncommitted]
event = "turn_end"
run   = 'test -z "$(git status --porcelain)" || echo "uncommitted changes, commit them"'

[completion]
plan = "${{ cat PLAN.md }}"
```

- Providers, instructions, hooks and completions are maps: `[provider.NAME]`,
  `[instruction.NAME]`, `[hook.NAME]`, `[completion.NAME]` (or one
  `[instruction]` table with `NAME = ...` lines). Names are 1 to 64 letters,
  digits, `-` or `_`. Instructions and hooks keep file order.
  Name a hook after what it does and give it all its events
  (`event = [...]`, the script switches on `$ZEL_HOOK`), see Hooks.
- `model = "PROVIDER/MODEL"` picks the provider and the model in one key; the
  model ID may contain `/` itself (`openrouter/anthropic/claude`). Without a `/`
  the only configured provider is meant.

### Modes

Every session runs in one base mode: `main` (the TUI) or `headless` (`zel -p`).
`--mode NAME` (`zel --mode plan`, `zel -p --mode plan "..."`) adds one custom
mode, so the session runs in `main` + `plan` or `headless` + `plan`. A
`zel -p` started by the agent does not inherit the custom mode; pass `--mode`.

`modes = [...]` on an entry says in which modes it applies; it applies when one
of them is a mode of the session:

| entry | default `modes` | outside its modes |
|---|---|---|
| `[instruction.NAME]` | `["main"]` | not in the system prompt |
| `[hook.NAME]` | `["main"]` | does not run |
| `[completion.NAME]` | `["main"]` | `#NAME` is not offered or expanded |
| `[provider.NAME]` | every mode | not in the model menu; selecting it is an error |

So `modes = ["plan"]` is an entry only for `--mode plan` sessions, and
`modes = ["main", "headless"]` is one for every session. `zel session` shows the
modes as `main+plan`.

- `${{ script }}` in any string (not in hook `run`) is replaced by the output
  of `bash -c script`, one trailing newline trimmed; `$${{` is a literal `${{`.
  Each expression has 10 s; a failing one becomes empty and leaves a notice.
  Instructions, providers, settings and env are expanded when the session
  starts and whenever the config changes; completions when they are used.
- `[instruction.NAME]` is a string (the text) or a table `{text, modes}`.
- Unknown keys and wrong types make the file invalid: it is skipped with a
  notice naming the file (and the line, for syntax errors). The keys of the
  0.9.1 format (`provider = "..."`, `[providers.*]`, `instructions`, `[hooks]`,
  `[completions]`) are errors that name their replacement.
- Hot reload: Zel watches the file. A change is applied after the
  current turn; a broken file keeps the previous config. The system prompt is
  only your `instructions`: Zel adds nothing of its own to it. It is rebuilt
  only when the instructions changed. Everything Zel itself tells the model
  (how the bash tool and the zel commands work) is the description of the bash
  tool, built fresh for every request; the main session and `zel -p` get
  different texts (a `zel -p` agent has no user to ask and no `zel choice`).
  Files read by `${{ }}` are not watched.
- Picking a model changes this session and the `model`/`effort` lines of the
  file. Other open sessions keep their model; so does an open
  session when you edit `model` by hand.
- `zel config` prints the config and its problems.

Tell the agent about your own tools in an instruction. Invoke scripts by their
paths, or install them on your normal `PATH`; Zel does not manage script folders.

## Hooks

`[hook.NAME]` is a table `{event, run, match, timeout, async, modes}`:
`event` is one event or a list of them; `run` is the command;
`match` is a regexp on the command (`bash_before`, `bash_after`) or the text
(`message`, `notification`, `turn_start`); the other events have nothing to match,
so `match` there is a config error; `timeout` is in seconds (default 60); `async = true` starts the
hook and does not wait; `modes` (default `["main"]`) are the session modes it
runs in, see Modes above.

```toml
[hook.notify]
event = "idle"
run   = "osascript -e 'display notification \"done\"'"
async = true

[hook.no-force-push]
event = "bash_before"
match = 'git push (-f|--force)'
run   = "echo 'no force push' >&2; exit 2"
```

Group hooks by purpose, not by event: one `[hook.<namespace>]` with a list of
events and one script that tells them apart by `$ZEL_HOOK`, not a `[hook.X]`
block per event. When you (the agent) add hooks to a config, do it this way.

```toml
[hook.progress]
event = ["turn_start", "turn_end", "error", "session_closed"]
run   = '''
case "$ZEL_HOOK" in
  turn_start) printf "\033]9;4;3\a" ;;
  error)      printf "\033]9;4;2;100\a" ;;
  *)          printf "\033]9;4;0\a" ;;
esac > /dev/tty
'''
```

A hook is `bash -c` with the session environment and `ZEL_HOOK` (the event's
name); the event data comes as JSON on stdin. Exit 0: stdout is used as below. Exit 2 blocks a
blockable event, with stderr as the reason. Any other exit is recorded in
session diagnostics and the hook is skipped. Every hook run (event, command, exit
code, time) is a `hook` entry in Tracing, not a row in the chat. Hooks run one
by one, in file order; rewriting hooks chain.

| event | when | stdin | stdout (exit 0) | exit 2 |
|---|---|---|---|---|
| `session_created` | a new session | `{}` | | |
| `session_resumed` | a session is resumed | `{}` | | |
| `session_closed` | quit, new or resume | `{"reason"}` | | |
| `config_changed` | after hot reload | `{"ok","error","file"}` | | |
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
to `/dev/tty`; the example config shows the terminal progress bar (OSC 9;4)
on `turn_start`, `turn_end`, `error` and `session_closed`.

## Prompt completions

`[completion] name = "text"` (or `[completion.name]` with `text` and
`modes`) registers `#name` in the sessions of its modes (default main). Typing
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
key keeps the default. A change applies on reload, and a
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
  choice).
- `accent`: the composer rail, the `*` header over answers, completed `#name`
  and `@path` tokens in the composer, answered choice questions, choice and menu
  markers, status icons, headings, inline code and links in
  answers, keywords in highlighted bash.
- `agent`: the text of answers, the main text of menus, Info values, the
  choice title and the rail text of forms.
- `user`: your messages and choice answers in the chat and the composer text; `user_highlight`:
  the composer line under the cursor.
- `gray`: time and steps, the working line, inactive menu rows, Info
  keys, the `+ N more` row, the composer placeholder, punctuation in bash.
- `dark_gray`: the "context compacted" divider, the command in the working
  line, the name in a task line, hotkeys in menus, Tracing row numbers, bash comments, the pulse of a
  queued composer.
- `error`: failed turns, prompt errors, form errors.
- `trace.*`: the kind labels and bodies in Tracing, one per kind; `trace.bash`
  is also the plain text of highlighted bash.

Strings and numbers in highlighted bash keep fixed tones.

## Examples

See the examples topic: a complete config.toml and a standalone web-search script.
