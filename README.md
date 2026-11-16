# Zel — Minimalist AI Coding Agent

A focused terminal coding agent with one tool: bash. The model writes bash;
background tasks, questions to you, images, edits and subagents are `zel`
commands it runs. You configure Zel with `zel.toml`: providers, settings,
instructions, hooks and prompt completions. Your own tools are scripts.

> **0.7.0 is a breaking release.** The Lua tool and init.lua are gone: the
> agent's tool is `bash`, the config is `~/.zel/zel.toml` (and `./zel.toml`,
> `--config FILE`), and history is JSONL files instead of SQLite. Sessions,
> providers and preferences of 0.6 are not migrated. See
> [docs/bash-api.md](docs/bash-api.md).

## Install

```sh
curl -fsSL https://raw.githubusercontent.com/ethanhamilthon/zel/main/scripts/install.sh | sh
```

Installs the latest release for macOS or Linux (arm64/amd64) into
`~/.local/bin` (override with `ZEL_BINDIR`; pin with `ZEL_VERSION=v0.7.0`).
`zel update` later replaces the binary with the latest GitHub release.

## Build from source

```sh
make install     # installs ~/.local/bin/zel
zel              # opens first-run onboarding if no provider exists
make dev         # isolated ~/.zeldev data
make test        # formatting, vet, race tests
```

Add `~/.local/bin` to `PATH`. Go 1.27.3+ and Make are required.

Zel keeps its data in `~/.zel` (override with `ZEL_HOME`):

```
~/.zel/zel.toml              the global config
~/.zel/bin/                  your scripts, on the agent's PATH
~/.zel/sessions/<project>/<session>/
    session.jsonl            the history, append-only
    meta.json                title and model, for the session list
    tasks.jsonl, tasks/      background tasks and their output
    attachments/, trace.log  images, notices and hook runs
~/.zel/run/                  sockets of the running sessions
```

## Onboarding and settings

The first launch opens a provider form: API type (`responses` or `chat`),
Base URL, API key, Name; it writes `~/.zel/zel.toml`. Then choose a model and
reasoning effort (default `medium`).

Ctrl+K opens the menu (typing `/` into an empty composer does too; Settings →
Slash opens menu turns that off). Providers adds, edits or deletes providers;
Settings controls auto compaction, reasoning summaries, the accent color and
the bash timeout. The menus write into the zel.toml layer the value comes from:
picking a model switches this session and updates the `model` line of the file
that set it. API keys are stored as you type them; `${{ echo $VAR }}` reads one
from the environment, `${{ security find-generic-password -s NAME -w }}` from
the Keychain.

## The bash tool

The model has exactly one tool, `bash`; its input is a bash script. Every call
is a fresh `bash -c` in the session folder with a 30-second limit
(`settings.bash_timeout`); output is stdout and stderr together, never
truncated (a 1 MiB safety cap keeps head and tail). Longer work runs as a task.

```sh
zel task run --name build -- make test     # background task: prints t1
zel task list | get t1 | tail t1 | wait t1 --timeout 20 | stop t1
zel choice "Which color" "Blue" "Red"      # you pick in the UI; the answer comes as a notification
zel edit main.go <<'EOF'                   # exact replacements, all or none
<<<<<<< old
func main() {
=======
func main() { // start
>>>>>>> new
EOF
zel -p "review the diff"                   # a subagent: its answer on stdout
zel view shot.png                          # attach an image to the next request
zel session | zel models | zel docs [topic]
```

The agent wakes up only for your messages and short notifications such as
`<notification>t1 finished</notification>`. Messages you type during a call
wait until the call ends. Full reference: [docs/bash-api.md](docs/bash-api.md)
(the agent reads it with `zel docs`).

There is no sandbox, permission prompt or trust dialog: bash runs with your
user permissions and network access, so only use config files you trust.

## Configuration: zel.toml

Zel reads `~/.zel/zel.toml`, then `./zel.toml` of the working folder, then each
`--config FILE`; later layers win by key, instructions and hooks add up.

```toml
provider = "openai"
model    = "gpt-5"
instructions = [
  "Read code first.",
  "${{ cat AGENTS.md }}",
  { text = "You are a subagent: answer short.", main = false, headless = true },
]

[providers.openai]
base_url = "https://api.openai.com/v1"
api_key  = "${{ echo $OPENAI_API_KEY }}"

[hooks]
turn_end    = ['test -z "$(git status --porcelain)" || echo "uncommitted changes, commit them"']
bash_before = [{ match = 'git push (-f|--force)', run = "echo 'no force push' >&2; exit 2" }]
idle        = [{ run = "say done", async = true }]

[completions]
plan = "${{ cat PLAN.md }}"
```

- **`${{ script }}`** in any string is replaced by the output of `bash -c
  script` (10 s); completions expand it when you send the message.
- **instructions** go into the system prompt; plain strings are for the main
  session, `{text, main, headless}` choose where an entry applies.
- **hooks** run a command on an event with its data as JSON on stdin: 14
  events from `session_created` to `bash_before`, `turn_end` and `idle`. Exit 2
  blocks a message, notification or bash call; `match`, `timeout` and `async`
  tune an entry. See `zel docs hooks`.
- **completions** register `#plan`: type `#` to list them, and the text,
  wrapped in `<completion name="plan">`, replaces the token when you send the
  message (main session only).
- **tools** are scripts: put them in `~/.zel/bin` or `./.zel/bin` (both lead
  the agent's PATH) and describe them in `instructions`.

Changes apply by themselves: Zel watches the layer files and reloads after the
current turn; a broken file keeps the previous config and shows the error.
`zel config` prints the merged result. Examples are in
[docs/examples](docs/examples/).

### Auto compaction

When the context reaches the compaction threshold (default 400000 tokens; 0
turns it off), Zel compacts it between model steps. Counting back from the
latest message, the last quarter of the threshold is sent unchanged; the next
quarter keeps its commands but cuts their outputs to the first line; anything
older keeps only user and assistant messages, with a marker telling the model
where the compaction happened. Saved history is never rewritten, so Tracing
still shows everything, and the chat shows a "context compacted" divider.

## Chat and Tracing

Chat shows user messages, agent messages and the working indicator. The
indicator shows `thinking...` until the agent does something, then the first
line of its bash call (`$ go test ./...`) until the next one; configured API
keys are hidden. Each running background task and each running `zel -p` child
has a row with how long it runs.

Bash calls, thoughts, errors, notices and hook runs belong in Tracing. Tracing
search accepts a leading type, optionally followed by text: `:user`,
`:assistant fix tests`, `:bash go test`, `:error`. A turn that fails or is
stopped with Ctrl+D ends in the chat with a red line of time and steps, then
the reason.

Typing `@` in the composer completes file paths; `#` lists prompt completions.

### Shortcuts

- Ctrl+K: Menu
- Ctrl+M: Models
- Ctrl+R: Resume
- Ctrl+N: New session
- Ctrl+I: Info
- Ctrl+T: Tracing (Enter opens an entry; Left/Right switch entries)
- Ctrl+D: Stop the active turn or a prompt completion still expanding
- Ctrl+Q: Save and quit
- Esc: back
- Up / Down move through the composer's rows, then scroll the chat; PgUp / PgDn
  scroll the timeline

Ctrl+M and Ctrl+I need the Kitty keyboard protocol; otherwise use the menu.
Mouse reporting is off, so selection and copying are your terminal's. A queued
message stays editable in the composer; editing withdraws it.

## Headless

```sh
zel -p "Review this project" --provider NAME --model MODEL --effort high
echo "long prompt" | zel -p --json
zel -p --resume SESSION "and now fix it"
```

`zel: session ID` goes to stderr before any model request; the final answer
goes to stdout (`--json` prints `{ok, answer, session, error}`). Success exits
0, errors nonzero, cancellation 130. Run from the agent's bash, `zel -p` is a
subagent: it uses the parent's model and config, shows as a row in the
parent's chat and nests at most 3 deep.

## Contributing

See [Architecture](docs/ARCHITECTURE.md) for package boundaries and ownership.

Keep code, UI, docs, and comments in English. Run `make test` before committing
features or fixes. Prefer focused unit tests, at most two test files per package.

## License

[MIT](LICENSE)
