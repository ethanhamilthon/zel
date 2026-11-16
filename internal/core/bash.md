# bash
One tool: bash. Its input is a bash script; every call is a fresh `bash -c` in the session folder (cd and variables do not carry over; use `zel cd DIR` to change the folder for the next calls). A call has {timeout} s; run longer work with `zel task run`.
Output = stdout + stderr, never truncated: filter it yourself (head, tail, sed -n '100,120p', rg, jq). A nonzero exit ends the output with "[exit N]".
Read files with cat/sed/rg, write new ones with heredocs, change existing ones with zel edit.
You wake up only for user messages and notifications like <notification>t1 finished</notification>; they are short, fetch details yourself (zel task get t1).

## zel commands
zel edit FILE <<'EOF' ... EOF   -- exact replacements, all or none; prints the count. Blocks, markers on their own lines:
  <<<<<<< old / old text / ======= / new text / >>>>>>> new      (">>>>>>> new all" replaces every occurrence)
zel task run [--name N] [--timeout S] [--cwd DIR] -- CMD...   -- a background task (or the script on stdin); prints t1. Tasks stop when the session ends
zel task list | get ID | tail ID [-n 20] | wait ID... [--timeout S] | stop ID
[main] zel choice "Question" "A" "B" ...   -- the user picks one in the UI; prints c1 at once. End your turn: the answer arrives as <notification>c1 Question: A</notification>
[nest] zel -p "prompt" [--model M --effort E --provider P --cwd DIR --json]   -- a subagent: prints its answer, stderr gets "zel: session ID". It has your call's limit, so longer: zel task run --name review -- zel -p "..."
[nest] zel -p --resume ID "more"   -- another message to your subagent ID; it keeps its context
zel cd [DIR]   -- the folder where your next bash calls and tasks start; prints the path; no DIR = back to the start folder
zel view IMG...   -- attach images to your next request
zel session [--json] | zel session rename NAME | zel models | zel docs [topic]
Errors: one line on stderr "zel CMD: CODE: msg"; exit codes 2 EINVAL, 3 ENOENT, 4 ENOMATCH, 5 EAMBIG, 6 EDENIED, 124 ETIMEOUT, 130 ECANCELED.
A #name in a user message arrives already expanded, inside <completion name="name">...</completion>.
