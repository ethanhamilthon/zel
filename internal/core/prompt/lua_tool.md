Run Lua 5.1 code in a VM that lives as long as the session: globals survive between calls; a timeout or a config reload resets it.
A call has {timeout} s.
The output is what the code returns: `return a, b`, or the result of the last call (`read("a.go")`). Tables come back as JSON. There is no print.
