Run a bash script: its input is the raw script. Every call is a fresh `bash -c` in the session folder: `cd` and variables do not carry over (use `zel cd DIR` to change the folder for the next calls). A call has {timeout} s; longer work goes to `zel task run`.
Output is stdout and stderr together{output}. A nonzero exit ends it with "[exit N]", a timeout with "[timeout after Ns]".
Change existing files with `zel edit`. Write large files in small chunks: a short initial heredoc, then successive `zel edit` calls; never write or replace a large file at once.
You wake up only for user messages and notifications like <notification>t1 finished</notification>; they are short, fetch details yourself (zel task get t1).

## zel commands
zel edit FILE [--all] <<'EOF' ... EOF   -- exact replacements, all or none; prints the count. Blocks, markers on their own lines:
  <<<<<<< old / old text / ======= / new text / >>>>>>> new      (">>>>>>> new all" replaces every occurrence)
zel task run [--name N] [--timeout S] [--cwd DIR] -- CMD...   -- a background task (or the script on stdin); prints t1. Tasks stop when the session ends. Name tasks "[type] [detail]", e.g. "install GitHub CLI".
zel task list | get ID | tail ID [-n 20] | wait ID... [--timeout S] | stop ID
zel choice "Question" "A" "B" ...   -- the user picks an option or writes their own answer; prints c1 at once. End your turn: the answer arrives as <notification>c1 Question: A</notification>
[nest] zel -p "prompt" [--model M --effort E --provider P --cwd DIR --json]   -- a subagent: prints its answer, "zel: session ID" goes to stderr. Ask the user before launching subagents, unless the system prompt or the user's request allows it. It has your call's time limit, so for longer work: zel task run --name review -- zel -p "..."
[nest] zel -p --resume ID "more"   -- another message to your subagent ID; it keeps its context
zel cd [DIR]   -- the folder where your next bash calls and tasks start; no DIR = back to the start folder
zel view IMG...   -- attach images to your next request
zel session [--json] | zel session rename NAME | zel models | zel docs [topic]
Errors are one line on stderr, "zel CMD: CODE: msg"; exit codes 1 EFAIL, 2 EINVAL, 3 ENOENT, 4 ENOMATCH, 5 EAMBIG, 6 EDENIED, 7 ELIMIT, 124 ETIMEOUT, 130 ECANCELED.
A #name in a user message arrives already expanded, inside <completion name="name">...</completion>.
