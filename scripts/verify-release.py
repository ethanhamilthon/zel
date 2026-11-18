#!/usr/bin/env python3
"""0.12 release contracts against a real binary and a loopback Responses provider.

Usage: python3 scripts/verify-release.py BINARY OUTPUT_DIRECTORY
"""
import json
from pathlib import Path
import re

from verify_support import inputs, lua, message, outputs, suite, user_texts


def respond(body):
    items = inputs(body)
    latest = list(user_texts(body))[-1].strip()
    if items[-1].get("type") == "function_call_output":
        return [message("Fixture finished.")]
    if latest == "runtime":
        return [
            lua('x = 41; return type(print), type(task), type(sh), pcall(require, "zel")', 'bare'),
            lua('io.write("sink-marker"); os.execute("echo child-marker"); return x', 'sink'),
            lua('return configured(), bump()', 'shared'),
            lua('return string.rep("x", 2048)', 'cut'),
            lua('os.execute("sleep 10")', 'timeout'),
            lua('return type(x)', 'reset'),
        ]
    if latest == "defaults":
        return [lua(r'''
write("sub/notes.txt", "one\ntwo\n")
cd("sub")
local n = edit("notes.txt", {{"one", "uno"}, {"two", "dos"}})
local ok = pcall(edit, "notes.txt", {{"uno", "bad"}, {"absent", "no"}})
local r = sh("cat; printf '%s' \"$GREETING\"", {stdin="stdin:", env={GREETING="hello"}})
return n, ok, read("notes.txt"), #grep("dos", {path = "notes.txt"}), r.out, type(choice)
''', 'files')]
    return [message("Fixture finished.")]


def bootstrap(f):
    first = f.run('-p', 'unused')
    f.check(first.returncode != 0 and 'no provider configured' in first.stderr.lower() and not f.requests,
            'no provider fails without a model request', first)
    home = f.zel_home
    f.require(home.is_dir(), 'creates Zel home on first use')
    f.check({p.name for p in home.iterdir()} == {'.gitignore', 'init.lua', 'connect.json', 'defaults'},
            'first use creates only bundled configuration files', list(home.iterdir()))
    defaults = home / 'defaults' / 'defaults.lua'
    f.require(defaults.is_file(), 'installs the defaults module')
    original = defaults.read_bytes()
    init = home / 'init.lua'
    f.check((home / '.gitignore').read_text() == '.env\n', 'ignores .env')
    init.write_text('return {} -- user init\n')
    defaults.write_text('-- user defaults\n')
    (home / 'defaults' / 'extra.lua').touch()
    connect = f.connect.read_bytes()
    f.run('-p', 'unused')
    f.check(defaults.read_text() == '-- user defaults\n' and init.read_text() == 'return {} -- user init\n',
            'normal startup preserves user edits')
    reset = f.run('defaults', '--reset')
    f.check(reset.returncode == 0 and defaults.read_bytes() == original and
            not (home / 'defaults' / 'extra.lua').exists() and f.connect.read_bytes() == connect and
            init.read_text() == 'return {} -- user init\n', 'reset replaces only defaults/')


def config_layers(f):
    f.configure()
    (f.zel_home / 'init.lua').write_text('return {systems={home="HOME-RULE", style="OLD-RULE"}}')
    (f.cwd / 'zel.lua').write_text('return {systems={project="PROJECT-RULE"}}')
    (f.home / 'extra.lua').write_text('return {systems={style="MIDDLE-RULE"}}')
    (f.cwd / 'sub').mkdir()
    (f.cwd / 'sub' / 'extra.lua').write_text('return {loads={"loaded.lua"}, systems={style="FINAL-RULE"}}')
    (f.cwd / 'sub' / 'loaded.lua').write_text('return {systems={loaded="LOADED-RULE"}}')
    flags = ('--config', '~/extra.lua', '--config', 'sub/extra.lua')
    r = f.run('config', *flags)
    paths = (f.zel_home / 'init.lua', f.cwd / 'zel.lua', f.home / 'extra.lua',
             f.cwd / 'sub' / 'extra.lua', f.cwd / 'sub' / 'loaded.lua')
    displayed = (str(paths[0]), str(paths[1].resolve()), '~/extra.lua',
                 str(paths[3].resolve()), str(paths[4].resolve()))
    positions = [r.stdout.find(p) for p in displayed]
    f.check(r.returncode == 0 and min(positions) >= 0 and positions == sorted(positions),
            'config lists default, explicit and relative load files in order', r.stdout + r.stderr)
    f.check('replaces a definition' in r.stdout, 'config reports overridden definitions', r.stdout)
    for skip in (False, True):
        r = f.run('-p', 'layers', *flags, *(['--no-default-configs'] if skip else []))
        f.require(r.returncode == 0 and f.requests, 'layered config can send a headless turn', r.stderr)
        body = f.requests[-1]
        prompt = body.get('instructions', '')
        f.check(all(s in prompt for s in ('FINAL-RULE', 'LOADED-RULE')) and
                all(s not in prompt for s in ('OLD-RULE', 'MIDDLE-RULE')) and
                all((s in prompt) != skip for s in ('HOME-RULE', 'PROJECT-RULE')) and body['model'] == 'fixture',
                f'prompt honors layer precedence and connect.json (skip defaults={skip})', prompt)
    missing = f.run('-p', 'unused', '--config', 'missing.lua')
    f.check(missing.returncode != 0 and 'missing.lua' in missing.stderr,
            'a missing explicit config is an error', missing)


def runtime(f):
    f.configure()
    (f.cwd / 'custom.lua').write_text('''local n = 0
return { settings = {timeout = 1, output = 1000}, functions = {
  configured = {description = "Fixture increment", fn = function() n = n + 1; return n end},
  bump = {description = "Fixture read shared state", fn = function() return n end},
} }
''')
    r = f.run('-p', 'runtime', '--no-default-configs', '--config', 'custom.lua')
    f.require(r.returncode == 0 and f.requests, 'headless runtime turn succeeds', r.stderr)
    body = f.requests[-1]
    tool, = body['tools']
    desc = tool['description']
    prompt = body.get('instructions', '')
    f.check(tool['name'] == 'lua' and 'Run Lua 5.1 code' in desc and '1 s' in desc and
            not any(s in desc for s in ('{timeout}', '[main]', '[headless]', '[nest]', 'Fixture increment')),
            'one Lua tool with static documentation and substituted timeout', tool)
    f.check('## Functions' in prompt and prompt.find('Fixture read shared state') < prompt.find('Fixture increment')
            and 'Fixture read shared state' in prompt, 'configured functions are sorted in system prompt', prompt)
    f.check(body.get('reasoning', {}).get('summary') == 'auto', 'reasoning summary requested', body.get('reasoning'))
    result = outputs(body)
    f.check(result.get('bare', '').startswith('nil\nnil\nnil\nfalse'), 'no injected print, task, sh or zel module', result)
    f.check(result.get('sink') == '41' and not any(s in r.stdout + r.stderr for s in ('sink-marker', 'child-marker')),
            'Lua globals persist and stdio does not leak', (result, r.stdout, r.stderr))
    f.check(result.get('shared') == '1\n1', 'copied functions share a closure upvalue', result)
    f.check(result.get('timeout', '').endswith('[timeout after 1s; VM reset]') and
            result.get('reset') == '[lua: fresh VM]\nnil', 'subprocess timeout resets Lua globals', result)
    cut = result.get('cut', '')
    full = re.search(r'^======== full output: (.+)$', cut, re.M)
    text = Path(full[1]).read_text() if full else ''
    if full:
        (f.out / 'full-lua-result.txt').write_text(text)
    f.check('[output cut:' in cut and len(cut) < 2048 and text == 'x' * 2048,
            'output cap leaves a full-result artifact', cut)


def defaults(f):
    f.configure()
    r = f.run('-p', 'defaults')
    f.require(r.returncode == 0 and f.requests, 'default init works headless', r.stderr)
    body = f.requests[-1]
    result = outputs(body)
    f.check(result.get('files') == '2\nfalse\nuno\ndos\n\n1\nstdin:hello\nnil' and
            (f.cwd / 'sub' / 'notes.txt').read_text() == 'uno\ndos\n',
            'defaults share cwd, batch edits are atomic, grep and sh work, headless has no choice', result)
    p = f.run('exec', '--timeout', '1', '--', 'bash', '-c', 'printf started; sleep 10')
    data = json.loads(p.stdout)
    f.check(not data['ok'] and data['timed_out'] and data['out'] == 'started' and data['ms'] < 3000,
            'zel exec reports timeout, partial output and elapsed time', p)
    p = f.run('print', 'no session')
    f.check(p.returncode == 1 and 'session' in p.stderr.lower(), 'socket command without a session fails clearly', p)


if __name__ == '__main__':
    raise SystemExit(suite('release-checks.log', [bootstrap, config_layers, runtime, defaults], respond))
