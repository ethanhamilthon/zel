Run a bash script in current dir.
Run `zel cd DIR` to change dir for the next calls.
A call has {timeout} s; longer work goes to `zel task run`.
Change existing files with `zel edit`. Write large files in small chunks: a short initial heredoc, then successive `zel edit` calls; never write or replace a large file at once.

## zel commands
`zel edit FILE [--all] <<'EOF' ... EOF` - exact replacements, all or none; prints the count. Blocks, markers on their own lines:
  <<<<<<< old / old text / ======= / new text / >>>>>>> new      (">>>>>>> new all" replaces every occurrence)
`zel task run [--name N] [--timeout S] [--cwd DIR]` - spawn a background task. Name tasks "[type] [detail]", e.g. "install GitHub CLI" or "worker agent gemini-3.8-flash/high"
`zel task list | get ID | tail ID [-n 20] | wait ID... [--timeout S] | stop ID` - work with existing tasks
`zel choice "Question" "A" "B" ...` - sends a picker to UI. Non blocking, answer arrives in `<notification>` block. Prefer it instead of asking user in final answer
[nest] `zel -p "prompt" [--model M --effort E --provider P --cwd DIR --json]` - launches zel in headless mode. Use for subagents and automations. Blocking, prints its answer at the end. Wrap it in `zel task`. "zel: session ID" goes to stderr. `zel -p --resume ID "more"` - another message to keep its context.
`zel cd [DIR]` - the folder where your next bash calls and tasks start; no DIR = back to the start folder
`zel view IMG...` - attach images to your next request, so you can see it.
`zel session [--json] | zel session rename NAME | zel models`
Errors are one line on stderr, "zel CMD: CODE: msg"; exit codes 1 EFAIL, 2 EINVAL, 3 ENOENT, 4 ENOMATCH, 5 EAMBIG, 6 EDENIED, 7 ELIMIT, 124 ETIMEOUT, 130 ECANCELED.
`zel docs [TOPIC]` - use it if user asks about zel itself, config, agent and etc.
