package defaults

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	lua "github.com/yuin/gopher-lua"
)

func setupDefaultsVM(t *testing.T, withRG bool) (*lua.LState, string, string) {
	t.Helper()
	home := t.TempDir()
	work := t.TempDir()
	bin := t.TempDir()
	logFile := filepath.Join(t.TempDir(), "zel.log")

	// Write embedded defaults into <home>/defaults/
	defDir := filepath.Join(home, "defaults")
	for rel, content := range Files() {
		dst := filepath.Join(defDir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dst, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}

	// Fake zel script on PATH that logs calls and simulates CLI subcommands.
	fakeZel := `#!/usr/bin/env bash
set -euo pipefail
LOG="${FAKE_ZEL_LOG:?}"
printf '%s\n' "$*" >> "$LOG"
cmd="${1:-}"
shift || true
case "$cmd" in
  status)
    if [ "${FAKE_ZEL_FAIL_STATUS:-0}" = "1" ]; then
      exit 1
    fi
    exit 0
    ;;
  exec)
    timeout=""
    cwd="."
    envs=()
    while [ $# -gt 0 ]; do
      case "$1" in
        --timeout) timeout="$2"; shift 2 ;;
        --cwd) cwd="$2"; shift 2 ;;
        --env) envs+=("$2"); shift 2 ;;
        --) shift; break ;;
        *) shift ;;
      esac
    done
    # Expect: bash -c CMD
    if [ "${1:-}" = "bash" ] && [ "${2:-}" = "-c" ]; then
      script="$3"
    else
      script="$*"
    fi
    out_file="$(mktemp)"
    code=0
    (
      cd "$cwd"
      for kv in "${envs[@]+"${envs[@]}"}"; do
        export "$kv"
      done
      export FAKE_EXEC_TIMEOUT="$timeout"
      bash -c "$script" > "$out_file" 2>&1
    ) || code=$?
    python3 - "$code" "$out_file" <<'PY'
import json, sys
code = int(sys.argv[1])
with open(sys.argv[2], "r", encoding="utf-8", errors="replace") as f:
    out = f.read()
print(json.dumps({"ok": code == 0, "code": code, "out": out, "timed_out": False, "ms": 7}))
PY
    rm -f "$out_file"
    ;;
  steer)
    if [ "${FAKE_ZEL_FAIL_STEER:-0}" = "1" ]; then
      echo "EFAIL: steer failed" >&2
      exit 1
    fi
    exit 0
    ;;
  choice)
    if [ "${ZEL_MODE:-main}" = "headless" ]; then
      echo "zel choice: EDENIED: headless mode" >&2
      exit 6
    fi
    echo "c42"
    ;;
  docs)
    if [ $# -eq 0 ]; then
      echo "zel docs index"
    elif [ "$1" = "unknown" ]; then
      echo "zel docs: ENOENT: unknown topic" >&2
      exit 3
    else
      echo "topic:$1"
    fi
    ;;
  models)
    echo '{"models":[{"Provider":"openai","Model":"gpt-5","ContextWindow":128000}],"failures":{"anthropic":"timeout"}}'
    ;;
  session)
    echo '{"id":"s1","name":"demo","cwd":"'"$PWD"'","root":"'"$PWD"'","mode":"main","modes":["main"],"depth":0,"model":"gpt-5","effort":"medium","provider":"openai"}'
    ;;
  rename)
    exit 0
    ;;
  *)
    echo "unknown fake zel subcommand: $cmd" >&2
    exit 2
    ;;
esac
`
	if err := os.WriteFile(filepath.Join(bin, "zel"), []byte(fakeZel), 0755); err != nil {
		t.Fatal(err)
	}

	// Build minimal PATH with standard tools + optional rg
	pathParts := []string{bin}
	if withRG {
		if rgPath, err := exec.LookPath("rg"); err == nil {
			pathParts = append(pathParts, filepath.Dir(rgPath))
		}
	}
	// Copy/symlink only standard POSIX tools if without rg, or append system dirs if rg isn't in them
	if withRG {
		pathParts = append(pathParts, "/usr/bin", "/bin", "/usr/local/bin", "/opt/homebrew/bin")
	} else {
		// Create a clean bin dir with standard tools (without rg)
		for _, tool := range []string{"bash", "sh", "pwd", "mkdir", "rm", "test", "stat", "find", "grep", "printf", "cat", "mktemp", "python3", "env"} {
			if p, err := exec.LookPath(tool); err == nil {
				dst := filepath.Join(bin, tool)
				if _, err := os.Stat(dst); err != nil {
					_ = os.Symlink(p, dst)
				}
			}
		}
	}

	t.Setenv("PATH", strings.Join(pathParts, string(os.PathListSeparator)))
	t.Setenv("HOME", home)
	t.Setenv("FAKE_ZEL_LOG", logFile)
	t.Setenv("ZEL_SESSION_ID", "test-session")
	t.Setenv("ZEL_MODE", "main")
	t.Chdir(work)

	L := lua.NewState()
	t.Cleanup(L.Close)

	pkg := L.GetGlobal("package").(*lua.LTable)
	pkg.RawSetString("path", lua.LString(filepath.Join(defDir, "?.lua")+";"+filepath.Join(defDir, "?", "init.lua")))

	return L, work, logFile
}

func runLua(t *testing.T, L *lua.LState, code string) {
	t.Helper()
	if err := L.DoString(code); err != nil {
		t.Fatalf("lua error:\n%v\ncode:\n%s", err, code)
	}
}

func TestInitLuaMainAndHeadless(t *testing.T) {
	for _, tc := range []struct {
		mode       string
		wantChoice bool
	}{
		{"main", true},
		{"headless", false},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			L, _, _ := setupDefaultsVM(t, false)
			t.Setenv("ZEL_MODE", tc.mode)
			if err := L.DoString(InitLua()); err != nil {
				t.Fatalf("InitLua failed: %v", err)
			}
			tbl := L.Get(-1).(*lua.LTable)
			fns := tbl.RawGetString("functions").(*lua.LTable)
			for _, name := range []string{"sh", "read", "write", "edit", "slice", "list", "glob", "grep", "exists", "cd", "docs", "models", "session", "rename"} {
				entry, ok := fns.RawGetString(name).(*lua.LTable)
				if !ok {
					t.Fatalf("missing function %s", name)
				}
				if _, ok := entry.RawGetString("description").(lua.LString); !ok {
					t.Fatalf("%s missing description", name)
				}
				if _, ok := entry.RawGetString("fn").(*lua.LFunction); !ok {
					t.Fatalf("%s missing fn", name)
				}
			}
			hasChoice := fns.RawGetString("choice") != lua.LNil
			if hasChoice != tc.wantChoice {
				t.Fatalf("mode %s: hasChoice=%v, want %v", tc.mode, hasChoice, tc.wantChoice)
			}
		})
	}
}

func TestDefaultsCallableAndErrors(t *testing.T) {
	L, work, _ := setupDefaultsVM(t, false)
	if err := os.WriteFile(filepath.Join(work, "hello.txt"), []byte("line1\nline2\n"), 0644); err != nil {
		t.Fatal(err)
	}
	runLua(t, L, `
		local d = require("defaults")
		assert(d.read("hello.txt") == "line1\nline2\n")
		local ok, err = pcall(d.read, "missing.txt")
		assert(not ok)
		assert(err:match("^read: ENOENT:"), "got: " .. tostring(err))
		local ok2, err2 = pcall(d.read, 123)
		assert(not ok2 and err2:match("^read: EINVAL:"), "got: " .. tostring(err2))
	`)
}

func TestDefaultsFilesEditSliceExistsCd(t *testing.T) {
	L, work, logFile := setupDefaultsVM(t, false)
	t.Setenv("FAKE_ZEL_FAIL_STATUS", "1") // failing zel status must be ignored
	_ = work
	runLua(t, L, `
		local d = require("defaults")
		-- write creates parent dirs + append
		d.write("sub/a.txt", "alpha\nbeta\nalpha\n")
		d.write("sub/a.txt", "gamma\n", {append = true})
		assert(d.read("sub/a.txt") == "alpha\nbeta\nalpha\ngamma\n")

		-- slice
		assert(d.slice("a\nb\nc\n", 2, 1) == "b\n")
		assert(d.slice("a\nb\nc\n", -2) == "b\nc\n")
		assert(d.slice("a\nb\nc\n", -10, 2) == "a\nb\n")
		assert(d.slice("", 1) == "")
		local ok0, err0 = pcall(d.slice, "a", 0)
		assert(not ok0 and err0:match("^slice: EINVAL:"))

		-- edit: EAMBIG, ENOMATCH, all-or-nothing, literal %() characters
		local okA, errA = pcall(d.edit, "sub/a.txt", "alpha", "X")
		assert(not okA and errA:match("^edit: EAMBIG:"), tostring(errA))
		local okM, errM = pcall(d.edit, "sub/a.txt", "nope", "X")
		assert(not okM and errM:match("^edit: ENOMATCH:"), tostring(errM))

		-- multi-block rollback if second block fails
		local okR, errR = pcall(d.edit, "sub/a.txt", {{"beta", "BETA"}, {"missing", "Y"}})
		assert(not okR and errR:match("^edit: ENOMATCH:"))
		assert(d.read("sub/a.txt") == "alpha\nbeta\nalpha\ngamma\n", "rollback failed")

		-- successful multi-block + literal special chars
		local n = d.edit("sub/a.txt", {{"alpha", "100% (ok)", true}, {"beta", "BETA"}})
		assert(n == 3, "count=" .. tostring(n))
		assert(d.read("sub/a.txt") == "100% (ok)\nBETA\n100% (ok)\ngamma\n")

		-- exists & tilde expansion
		d.write("~/in_home.txt", "home")
		local e1, k1 = d.exists("~/in_home.txt")
		assert(e1 == true and k1 == "file")
		local e2, k2 = d.exists("sub")
		assert(e2 == true and k2 == "dir")
		local e3, k3 = d.exists("missing")
		assert(e3 == false and k3 == nil)

		-- cd and reset
		d.cd("sub")
		assert(d.read("a.txt"):sub(1, 3) == "100")
		d.cd()
		assert(d.exists("sub/a.txt"))
		local okcd, errcd = pcall(d.cd, "sub/a.txt")
		assert(not okcd and errcd:match("^cd: EINVAL:"))
		local okcd2, errcd2 = pcall(d.cd, "no_such_dir")
		assert(not okcd2 and errcd2:match("^cd: ENOENT:"))

		-- list
		local entries = d.list()
		local seen = {}
		for _, it in ipairs(entries) do seen[it.name] = it end
		assert(seen["sub"] and seen["sub"].dir == true)
		d.write("empty.txt", "")
		local e_empty, k_empty = d.exists("empty.txt")
		assert(e_empty == true and k_empty == "file")
		-- directory read fails with EINVAL
		local okd, errd = pcall(d.read, "sub")
		assert(not okd and errd:match("^read: EINVAL:"), tostring(errd))
	`)

	logBytes, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(logBytes), "status read sub/a.txt") {
		t.Fatalf("expected zel status calls in log, got:\n%s", logBytes)
	}
}

func TestDefaultsReadImageAndLimit(t *testing.T) {
	L, work, logFile := setupDefaultsVM(t, false)
	img := filepath.Join(work, "Pic.PNG")
	if err := os.WriteFile(img, []byte("fakepng"), 0644); err != nil {
		t.Fatal(err)
	}
	big := filepath.Join(work, "big.txt")
	if err := os.WriteFile(big, make([]byte, (16<<20)+1), 0644); err != nil {
		t.Fatal(err)
	}
	runLua(t, L, `
		local d = require("defaults")
		local res = d.read("Pic.PNG")
		assert(res == nil, "image read must return nil")
		local ok, err = pcall(d.read, "big.txt")
		assert(not ok and err:match("^read: ELIMIT:"), tostring(err))
	`)
	logBytes, _ := os.ReadFile(logFile)
	if !strings.Contains(string(logBytes), "steer --file "+img) {
		t.Fatalf("expected zel steer --file %s in log, got:\n%s", img, logBytes)
	}
}

func TestDefaultsGlobAndGrep(t *testing.T) {
	run := func(t *testing.T, withRG bool) {
		L, work, _ := setupDefaultsVM(t, withRG)
		if err := os.MkdirAll(filepath.Join(work, ".git"), 0755); err != nil {
			t.Fatal(err)
		}
		_ = os.WriteFile(filepath.Join(work, ".git", "config"), []byte("matchme\n"), 0644)
		_ = os.WriteFile(filepath.Join(work, ".gitignore"), []byte("ignored.go\n"), 0644)
		_ = os.MkdirAll(filepath.Join(work, "a", "b", "c"), 0755)
		_ = os.WriteFile(filepath.Join(work, "top.go"), []byte("package main\n// matchme\n"), 0644)
		_ = os.WriteFile(filepath.Join(work, "ignored.go"), []byte("matchme\n"), 0644)
		_ = os.WriteFile(filepath.Join(work, "a", "one.go"), []byte("hello matchme\nliteral a+b\n"), 0644)
		_ = os.WriteFile(filepath.Join(work, "a", "b", "c", "deep.go"), []byte("deep matchme\n"), 0644)
		_ = os.WriteFile(filepath.Join(work, "a", "note.txt"), []byte("text matchme\n"), 0644)

		runLua(t, L, fmt.Sprintf(`
			local d = require("defaults")
			-- top-level glob must not descend
			local top = d.glob("*.go")
			local top_set = {}
			for _, p in ipairs(top) do top_set[p] = true end
			assert(top_set["top.go"], "missing top.go")
			assert(not top_set["a/one.go"], "top-level glob must not match nested")

			-- recursive ** glob
			local deep = d.glob("a/**/*.go")
			local deep_set = {}
			for _, p in ipairs(deep) do deep_set[p] = true end
			assert(deep_set["a/one.go"], "a/**/ should match 0 dirs")
			assert(deep_set["a/b/c/deep.go"], "a/**/ should match nested dirs")
			assert(not deep_set["a/note.txt"])

			-- grep basic & skips .git
			local hits = d.grep("matchme")
			local hit_files = {}
			for _, h in ipairs(hits) do
				assert(type(h.line) == "number" and h.line > 0)
				assert(not h.file:find(".git/", 1, true), "must skip .git")
				hit_files[h.file] = true
			end
			assert(hit_files["top.go"] and hit_files["a/one.go"] and hit_files["a/b/c/deep.go"])
			if %v then
				assert(not hit_files["ignored.go"], "rg must skip .gitignore'd files")
			end

			-- grep fixed, glob, max, path
			local fhits = d.grep("a+b", {fixed = true, glob = "*.go"})
			assert(#fhits == 1 and fhits[1].file == "a/one.go" and fhits[1].line == 2)

			local mhits = d.grep("matchme", {max = 1})
			assert(#mhits == 1)

			local phits = d.grep("matchme", {path = "a/note.txt"})
			assert(#phits == 1 and phits[1].file == "a/note.txt")

			local ok_bad, err_bad = pcall(d.grep, "[", {})
			assert(not ok_bad and err_bad:match("^grep: EINVAL:"), tostring(err_bad))
			local ok_miss, err_miss = pcall(d.grep, "x", {path = "nope"})
			assert(not ok_miss and err_miss:match("^grep: ENOENT:"), tostring(err_miss))
		`, withRG))
	}

	t.Run("fallback_find_grep", func(t *testing.T) { run(t, false) })
	if _, err := exec.LookPath("rg"); err == nil {
		t.Run("with_rg", func(t *testing.T) { run(t, true) })
	}
}

func TestDefaultsShAndCLIWrappers(t *testing.T) {
	L, work, _ := setupDefaultsVM(t, false)
	if err := os.MkdirAll(filepath.Join(work, "sub"), 0755); err != nil {
		t.Fatal(err)
	}
	runLua(t, L, `
		local d = require("defaults")
		local r = d.sh("printf 'hi:%s:' \"$FOO\"; cat", {
			cwd = "sub",
			env = { FOO = "bar" },
			stdin = "world\n",
			timeout = 12,
		})
		assert(r.ok == true and r.code == 0 and r.timed_out == false)
		assert(r.out == "hi:bar:world\n", "out=" .. tostring(r.out))

		-- JSON escapes, unicode and surrogate pairs
		local jr = d.sh([[printf 'quote:"\\" slash:\\\\ tab:\t nl:\n smile:\xF0\x9F\x98\x80']])
		assert(jr.out == 'quote:"\\" slash:\\\\ tab:\t nl:\n smile:😀', "jr=" .. tostring(jr.out))

		local okc, errc = pcall(d.sh, "echo boom; exit 3", { check = true })
		assert(not okc and errc:match("^sh: EFAIL: exit status 3: boom"), tostring(errc))

		-- choice
		local cid = d.choice("Pick one?", {"-a", "b"})
		assert(cid == "c42", "cid=" .. tostring(cid))

		-- docs
		assert(d.docs():find("zel docs index", 1, true))
		assert(d.docs("hooks") == "topic:hooks")
		local okd, errd = pcall(d.docs, "unknown")
		assert(not okd and errd:match("^docs: ENOENT:"), tostring(errd))

		-- models
		local mlist, mfail = d.models()
		assert(#mlist == 1 and mlist[1].provider == "openai" and mlist[1].model == "gpt-5" and mlist[1].context_window == 128000)
		assert(mfail.anthropic == "timeout")

		-- session & rename
		local s = d.session()
		assert(s.id == "s1" and s.mode == "main")
		d.rename("-new name")
	`)

	// Headless choice returns EDENIED
	t.Setenv("ZEL_MODE", "headless")
	runLua(t, L, `
		local d = require("defaults")
		local ok, err = pcall(d.choice, "Q?", {"A"})
		assert(not ok and err:match("^choice: EDENIED:"), tostring(err))
	`)
}
