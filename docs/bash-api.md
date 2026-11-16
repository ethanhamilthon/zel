# zel 0.7 reference

Zel gives the model one tool, `bash`: its input is a bash script. Background
tasks, questions to the user, images and edits are `zel` commands the script
runs. You configure Zel with `zel.toml` files: settings, providers,
instructions, hooks and prompt completions.

## The bash tool

- Every call is a fresh `bash -c` in the session folder, in its own process
  group: `cd` and variables do not carry over, so keep what must last in files.
- A call has `settings.bash_timeout` seconds (default 30). At the timeout the
  whole group is killed and the output ends with `[timeout after 30s]`. Longer
  work goes to `zel task run`.
- Output is stdout and stderr together, never truncated; only a 1 MiB safety
  cap (head and tail kept) protects memory. A nonzero exit ends the output with
  `[exit N]`.
- stdin is empty; pagers are off (`PAGER=cat`, `GIT_PAGER=cat`).
- The agent wakes up only for user messages and short notifications such as
  `<notification>t1 finished</notification>`.
- Environment of every call, task, hook and `${{ }}`: `ZEL_SESSION_ID`,
  `ZEL_MODE` (`main` or `headless`), `ZEL_DEPTH`, `ZEL_PARENT` (for a child),
  `ZEL_SOCKET`, `ZEL_TOKEN`, `ZEL_CONFIG` (the config layers, `:`-separated),
  `ZEL_PROVIDER`, `ZEL_MODEL`, `ZEL_EFFORT`, `ZEL_TASK` inside a task,
  `ZEL_HOOK` inside a hook, the `[env]` of the config, and `PATH` with
  `./.zel/bin` and `~/.zel/bin` in front. API keys are never exported.

There is no sandbox: bash runs with your user's permissions and network access.

## Commands

Inside a session (they talk to it through its socket):

```sh
zel task run [--name N] [--timeout S] -- CMD...   # background task; the script may come on stdin; prints t1
zel task list                                     # t1 running build 12s
zel task get ID                                   # state, exit code, command, last 40 lines
zel task tail ID [-n 20]
zel task wait ID... [--timeout S]                 # exit 124 if some still run
zel task stop ID                                  # SIGTERM to the group, SIGKILL after 2 s
zel choice "Question" "A" "B" ...                 # prints c1 at once; the answer arrives as a notification
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
(several questions queue up); the user picks one or presses Esc, and the agent
gets `<notification>c1 Question: A</notification>` (or `cancelled`). It works
in the main session, its tasks and hooks; `zel -p` has no user (EDENIED).

A subagent is `zel -p` run from the agent's bash. It becomes a child of the
session: `ZEL_MODE=headless`, the parent's model and config layers unless
given others, and a row in the parent's chat while it runs. Its answer goes
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
accent            = "#c3e88d"
slash_menu        = true
bash_timeout      = 30         # seconds per bash call

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
  current turn; a broken file keeps the previous config. The system prompt is
  rebuilt only when the instructions changed. Files read by `${{ }}` are not
  watched.
- The menus write back into the layer a value comes from: picking a model
  changes this session and the `provider`/`model`/`effort` lines of the file
  they come from (`~/.zel/zel.toml` when no file sets them). Other open
  sessions keep their model; so does an open session when you edit `model` by hand.
- `zel config` prints the merged config and where every key comes from.

Your own tools are scripts in `~/.zel/bin` or `./.zel/bin`, which lead PATH;
tell the agent about them in `instructions`.

## Hooks

`[hooks]` maps an event to a list of commands. An entry is a string (the
command) or a table `{run, match, timeout, async, main, headless}`:
`match` is a regexp on the command (bash_*) or the text (message,
notification); `timeout` is in seconds (default 60); `async = true` starts the
hook and does not wait; hooks run in the main session only unless
`headless = true` (`main = false` leaves the main session out).

A hook is `bash -c` with the session environment and `ZEL_HOOK`; the event
data comes as JSON on stdin. Exit 0: stdout is used as below. Exit 2 blocks a
blockable event, with stderr as the reason. Any other exit is logged in
Tracing and the hook is skipped. Hooks run one by one, in layer order;
rewriting hooks chain.

| event | when | stdin | stdout (exit 0) | exit 2 |
|---|---|---|---|---|
| `session_created` | a new session | `{}` | | |
| `session_resumed` | a session is resumed | `{}` | | |
| `session_closed` | quit, new or resume | `{"reason"}` | | |
| `config_changed` | after hot reload | `{"ok","error","files"}` | | |
| `message` | the user sent a message | `{"text"}` | new text | the message is dropped |
| `notification` | before a notification reaches the agent | `{"text"}` | new text | it is dropped |
| `turn_start` | the agent starts a turn | `{"text"}` | | |
| `turn_end` | the agent answered | `{"answer","running":[{"id","name","ms"}]}` | a notification; the turn goes on (at most 3 times in a row) | |
| `idle` | the turn ended and no task or child runs | `{}` | | |
| `bash_before` | before a bash call | `{"command"}` | new command | not run; the agent sees the reason |
| `bash_after` | after a bash call | `{"command","out","code","ms","timed_out"}` | added to the output | |
| `compaction` | after auto compaction | `{"before","after"}` | | |
| `model_changed` | the session's model or effort changed | `{"provider","model","effort"}` | | |
| `error` | a turn failed | `{"kind","message"}` | | |

`session_closed` runs within what is left of the 5 s shutdown budget, after
the session's tasks were stopped.

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

## Examples

See the examples topic: a global zel.toml, a project zel.toml and a script
tool for `~/.zel/bin`.
