Run Lua 5.1; globals survive calls, except after a timeout (%d s) or reset. Output is `print()` followed by returns (tables as JSON). Use `task.spawn` for longer work. Standard libraries are available. Paths are relative to `session.cwd`; API errors are catchable with `pcall`.

- `sh(cmd, {timeout, cwd, env, stdin, check}?) -> {ok, code, out, timed_out, ms}`: run bash; use `sh("zel -p ...")` for a subagent. `check=true` raises on failure.
- `read(path) -> text` (16 MiB max; images attach and return nil); `slice(s, from, count?)` selects lines (negative from counts from the end).
- `write(path, s, {append}?)`; `edit(path, old, new, {all}?) -> n` or `edit(path, {{old, new, all?}, ...})` (all or nothing).
- `list(dir?) -> {name, dir, size}[]`; `glob(pattern) -> paths` (supports `**`); `grep(re, {path, glob, fixed, max}?) -> {file, line, text}[]` (RE2, skips ignored and binary files); `exists(path) -> bool, kind`.
- `task.spawn(name, fn, {timeout}?) -> id`: fresh VM with copied upvalues, not caller globals. `print()` goes to the log. Unwaited tasks notify on finish.
- `task.list()`; `task.get(id)`; `task.wait(id_or_ids, timeout?) -> result_or_results`; `task.tail(id, n?)`; `task.stop(id)`.
- `cd(dir?)` changes cwd for later calls (omit to reset); `docs(topic?)`; `models() -> rows, failures`.
- `session.id/name/cwd/root/mode/modes/depth/model/effort/provider`; `session.rename(name)`.
