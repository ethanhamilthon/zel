Run Lua 5.1 code in a VM that lives as long as the session: globals survive between calls; a timeout or a config reload resets it.
The REPL exposes only pure Lua (`string`, `table`, `math`, `coroutine`, safe base functions) plus the configured functions: no `io`, `os`, `debug`, `package`, `require`, `dofile`, `loadfile`, `getfenv`/`setfenv`, or `print`.
A call has {timeout} s.
The output is what the code returns: `return a, b`, or the result of the last call (`read("a.go")`). Tables come back as JSON.
