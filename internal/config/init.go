package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

const initTemplate = `local zel = require("zel")

-- Project instructions for the agent. Plain text or a file:
%s
-- Functions the agent can call (they show up in the system prompt):
-- zel.fn{
--   name = "proj.test", summary = "run the tests, return only failures",
--   params = {{"pkg", "string?", "package pattern", default = "./..."}},
--   returns = "{ok:boolean, out:string}",
--   run = function(pkg) local r = sh("go test " .. pkg); return {ok = r.ok, out = r.out} end,
-- }

-- Hooks: session_start, session_end, message, turn_end, subagent_start, subagent_end
-- (session_start and turn_end: returned text becomes a <notification>; message: returned
-- text replaces the user's text; the other hooks return nothing)
-- zel.on("turn_end", function(ev) end)

-- Prompt completions: a #plan token in your message is replaced by what this returns when you send it
-- zel.complete("plan", function() return "Instructions for the agent go here" end)
`

// InitProject creates ./.zel/init.lua without replacing an existing file. When
// the directory has an AGENTS.md, the new file includes it as instructions.
func InitProject(dir string) (created bool, err error) {
	dir, err = filepath.Abs(dir)
	if err != nil {
		return false, err
	}
	path := filepath.Join(dir, ".zel", "init.lua")
	if _, err := os.Stat(path); err == nil {
		return false, nil
	}
	instructions := "-- zel.instructions [[Read code first. Verify before the final answer.]]\n"
	if _, err := os.Stat(filepath.Join(dir, "AGENTS.md")); err == nil {
		instructions = "zel.instructions{file = \"AGENTS.md\"}\n"
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if errors.Is(err, os.ErrExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	_, err = file.WriteString(fmt.Sprintf(initTemplate, instructions))
	if cerr := file.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(path)
		return false, err
	}
	return true, nil
}
