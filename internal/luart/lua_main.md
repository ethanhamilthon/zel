Run Lua 5.1 code. One VM per session: globals survive between calls; a timeout or config reload resets it.
A call has {timeout} s; longer work goes to `task.spawn`. Output is what you `print()`, then the returned values (tables as JSON).
[headless]You run headless (zel -p): there is no user to ask, and your final message is the answer the caller gets.
Change existing files with `edit`. Write large files in small chunks: a short `write`, then successive `edit` calls; never write or replace a large file at once.
Errors raise "fn: CODE: msg" (catch with pcall); codes EINVAL ENOENT ENOMATCH EAMBIG ELIMIT EFAIL EDENIED ETIMEOUT ECANCELED. Paths are relative to `session.cwd`. io/os are open.

## API (globals)
`sh(cmd, {timeout, cwd, env, stdin, check}?) -> {ok, code, out, timed_out, ms}` - bash -c in its own process group; out = stdout+stderr; check=true raises on failure
`read(path) -> text` - images (png/jpg/gif/webp) attach to your next request so you can see them, and return nil
`slice(s, from, count?) -> text` - lines from..from+count-1; negative from counts from the end
`write(path, s, {append}?)`, `edit(path, old, new, {all}?) -> n`, `edit(path, {{old, new, all?}, ...}) -> n` - exact text, all or none
`list(dir?) -> {name, dir, size}[]`, `glob("internal/**/*.go") -> paths`, `exists(path) -> bool, "file"|"dir"`
`grep(re, {path, glob, fixed, max}?) -> {file, line, text}[]` - RE2, skips .gitignore'd and binary files, max 200 by default
`task.spawn(name, fn, {timeout}?) -> id` - fn runs in the background in a fresh VM with copies of its upvalues (not your globals); its print() goes to the log, its return value is the result; you get a notification when it ends. Name tasks "[type] [detail]", e.g. "install GitHub CLI" or "worker agent gemini-3.8-flash/high"
`task.list()`, `task.get(id)`, `task.wait(id or {ids}, timeout?)`, `task.tail(id, n?)`, `task.stop(id)`
[main]`choice(question, {options...}) -> id` - sends a picker to the UI. Non blocking, the answer arrives in a `<notification>` block. Prefer it to asking the user in the final answer
[nest]`sh("zel -p 'prompt' [--model M --effort E --provider P --cwd DIR --json]")` - a headless subagent for delegated work. Blocking, prints its answer at the end; run it inside `task.spawn`. "zel: session ID" goes to stderr; `zel -p --resume ID "more"` continues it.
`cd(dir?)` - the folder of your next calls; no dir = back to the start folder
`session.id/name/cwd/root/mode/depth/model/effort/provider`, `session.rename(name)`, `models()`
`docs(topic?)` - use it if the user asks about zel itself, its config, hooks and so on.
