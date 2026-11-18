#!/usr/bin/env python3
"""0.12 real-PTY acceptance suite (requires pyte and Pillow).

Usage: python3 scripts/verify-terminal.py BINARY OUTPUT_DIRECTORY
PNG files render captured terminal cells, not OS windows.
"""
import json
import re

from verify_support import inputs, lua, message, outputs, suite, user_texts

MENU, ALT_MENU, TRACE, RELOAD, ESC, DOWN, PGDN = (
    '\x0b', '\x1bk', '\x14', '\x12', '\x1b', '\x1b[B', '\x1b[6~')
STARS = '✶✸✹✺'
GATE = 'os.execute("while [ ! -f release-turn ]; do sleep .05; done")'


def respond(body):
    users = list(user_texts(body))
    latest = users[-1] if users else ''
    if latest.startswith('<notification>') and ('Pick a letter' in latest or 'steer-marker' in latest):
        return [message('Steering and notifications received.')]
    items = inputs(body)
    if items and items[-1].get('type') == 'function_call_output':
        ident = items[-1]['call_id']
        return [message(f'Finished {ident}.')]
    if 'Expanded test plan.' in latest:
        return [message('Finished prompt check.')]
    if latest == 'identity':
        return [lua('session()', 'identity')]
    if latest == 'plain-working':
        return [lua('return (function() ' + GATE + '; return "' + 'x' * 150 + '" end)()', 'plain')]
    if latest == 'active-status':
        return [lua('os.execute("zel status fixture-call-status"); ' + GATE + '; return "released"', 'held')]
    if latest == 'trace-check':
        return [lua('return "traced"', 'trace1'), lua('error("trace failure", 0)', 'trace2')]
    return [message('Hello from the fake provider.')]


def boot(f, name='terminal', *args):
    t = f.term(name, *args)
    t.wait('second-model', 'startup model picker loads from the loopback provider')
    t.send(ESC)
    t.wait('Open a past session', 'Esc from startup Models returns to the main menu')
    t.send(ESC)
    t.until(lambda: 'Open a past session' not in t.view(), 'Esc closes the main menu')
    return t


def open_item(t, name, key=MENU, start_open=False):
    if not start_open:
        t.send(key)
        t.wait('Open a past session', f'opens menu for {name}')
    t.send(name)
    t.send('\r')


def leave_page(t):
    t.send(ESC)
    t.send(ESC)


def no_provider(f):
    t = f.term('first-run')
    t.until(lambda: t.p.poll() is not None, 'provider-less TUI exits instead of waiting')
    t.wait('no provider configured', 'provider-less TUI gives a diagnostic')
    f.check(t.close() != 0 and not f.requests, 'provider-less TUI fails without contacting a provider')
    f.check({p.name for p in f.zel_home.iterdir()} == {'.gitignore', 'init.lua', 'connect.json', 'defaults'},
            'first-run config directory contains init, connect and defaults')


def composer_and_models(f):
    f.configure()
    (f.cwd / 'my file.go').write_text('sample\n')
    (f.cwd / 'zel.lua').write_text('''return {
  prompts = {plan = "Expanded test plan."},
  settings = {colors = {accent = "#ff3b30", dark_gray = "#606060"}},
  systems = {project = "LOCAL-FILE-RULE"},
}
''')
    t = boot(f)
    f.check((t.screen.cursor.x, t.screen.cursor.y) == (0, 0), 'empty composer starts at the corner', t.view())
    t.send('draft')
    f.check(t.screen.cursor.x == 5, 'typed text has no left padding', t.view())
    t.shot('01-composer')
    t.send('\x15')
    t.send('stream-check\r')
    t.wait('Hello from the fake provider.', 'answer streams into the chat')
    y = t.screen.cursor.y
    last = max((i for i, r in enumerate(t.rows()[:y]) if r), default=-1)
    f.check(y - last == 2, 'one empty line between answer and composer', t.view())
    t.shot('02-answer')
    t.send('plain-working\r')
    t.until(lambda: any(r.startswith(tuple(STARS)) for r in t.rows()), 'working line appears during Lua')
    row = next(i for i, r in enumerate(t.rows()) if r.startswith(tuple(STARS)))
    text = t.rows()[row]
    f.check('return (function()' in text and text.endswith('...') and len(text) <= t.COLS,
            'working code stays on one clipped line', text)
    if 'return ' in text:
        f.check(t.screen.buffer[row][2].fg == 'ff3b30' and
                t.screen.buffer[row][text.index('return ')].fg == '606060',
                'working timer uses accent and code uses dark gray', text)
    t.shot('03-working')
    t.send('\x04')
    t.until(lambda: not any(r.startswith(tuple(STARS)) for r in t.rows()), 'Ctrl+D stops the running call')

    t.send('@')
    t.wait('my file.go', '@ lists files with spaces')
    y, rows = t.screen.cursor.y, t.rows()
    f.check(rows[y + 1] == '' and rows[y + 2].endswith(' ‹') and not rows[y + 2].startswith(' '),
            'file completions: blank line, no padding, selection arrow', rows[y:y + 6])
    t.shot('04-file-completions')
    t.send(ESC)
    t.send('\x15#pl')
    t.wait('plan', '# lists the configured prompt')
    t.shot('05-prompt-completions')
    t.send('\r')
    t.send('\r')
    t.wait('Finished prompt check.', 'prompt token sends a turn')
    prompt = '<prompt name="plan">\nExpanded test plan.\n</prompt>'
    f.check(prompt in list(user_texts(f.requests[-1])) and
            'LOCAL-FILE-RULE' in f.requests[-1].get('instructions', ''),
            'provider sees expanded prompt and project system rule', f.requests[-1])
    f.check('#plan' in t.view() and 'Expanded test plan.' not in t.view(),
            'chat shows the prompt token, not its expansion by default', t.view())

    t.send('/')
    t.wait('Open a past session', '/ on empty input opens the main menu')
    rows = t.rows()
    selected = next((r for r in rows if 'Open a past session' in r), '')
    f.check(selected.endswith(' ‹') and 'Providers' not in t.view(), 'main menu selection arrow and current items', t.view())
    t.shot('06-main-menu')
    before = json.loads(f.connect.read_text())
    open_item(t, 'Models', start_open=True)
    t.wait('second-model', 'model picker lists the local provider models')
    t.shot('07-models')
    t.send('second')
    t.send('\r')
    t.wait('Effort', 'model picker opens effort selection')
    t.send('\r')
    t.until(lambda: json.loads(f.connect.read_text()).get('model') == 'second-model',
            'model selection persists to connect.json')
    f.check(json.loads(f.connect.read_text()) == dict(before, model='second-model'),
            'model selection preserves provider and API key reference', f.connect.read_text())
    f.check(t.close() == 0, 'composer/model session quits cleanly')


def config_and_commands(f):
    f.configure()
    init = f.zel_home / 'init.lua'
    init.write_text('''return {
  systems = { style = "HOME-STYLE" },
  settings = { keys = { menu = "alt+k" } },
  hooks = {{events = {"config_changed"}, async = true, fn = function(_, data)
    local file = assert(io.open("reload-hook.txt", "a"))
    file:write(tostring(data.ok), "\\n"); file:close()
  end}},
}
''')
    project = f.cwd / 'zel.lua'
    project.write_text('''return {
  prompts = { plan = "Expanded test plan." }, systems = { style = "PROJECT-STYLE" },
  functions = { probe = {description = "`probe()` - fixture probe", fn = function() return "ok" end} },
}
''')
    extra = f.cwd / 'extra.lua'

    def config(rev):
        return f'''return {{
  settings = {{show_prompt = {str(rev == 2).lower()}}},
  systems = {{ style = "FIXTURE-STYLE-{rev}" }},
  commands = {{
    {{ name = "Echo fixture", key = "alt+g", description = "Print a fixture line", fn = function()
      return "command-result-v{rev}"
    end }},
    {{ name = "Fail fixture", fn = function() error("fixture command error", 0) end }},
  }},
}}
'''

    extra.write_text(config(1))
    t = boot(f, 'config', '--config', 'extra.lua')
    t.send(MENU)
    f.check('Open a past session' not in t.view(), 'rebound menu no longer opens with Ctrl+K', t.view())
    t.send(ALT_MENU)
    t.wait('Open a past session', 'Alt+K opens the rebound menu')
    t.wait('Print a fixture line (alt+g)', 'menu lists commands with their key hints')
    t.shot('01-command-menu')
    t.send(MENU)
    f.check('Open a past session' in t.view(), 'Ctrl+K does not close the rebound menu', t.view())
    t.send(ALT_MENU)
    t.until(lambda: 'Open a past session' not in t.view(), 'Alt+K closes the rebound menu')
    open_item(t, 'Echo fixture', ALT_MENU)
    t.wait('command-result-v1', 'Enter runs the selected command and prints its result')
    f.check(not f.requests, 'running a menu command does not call the provider')
    open_item(t, 'Fail fixture', ALT_MENU)
    t.wait('fixture command error', 'command error is shown in chat')
    err_y = next(y for y, r in enumerate(t.rows()) if 'fixture command error' in r)
    f.check(t.screen.buffer[err_y][t.rows()[err_y].index('fixture command error')].fg == 'e27878',
            'command error uses error color', t.rows()[err_y])
    open_item(t, 'Config', ALT_MENU)
    t.wait('Ctrl+R to reload', 'Config opens with reload hint')
    f.check(all(s in t.view() for s in ('Warnings (2)', 'replaces a definition', 'Files (3)',
                                       'init.lua', 'zel.lua', 'extra.lua', 'style ·')),
            'Config shows load files, overrides and system source', t.view())
    t.shot('02-config-sources')
    t.send(PGDN)
    t.wait('probe()','Config shows function description')
    t.send(PGDN)
    t.wait('Echo fixture · alt+g ·', 'Config shows command binding and source')
    f.check(all(s in t.view() for s in ('plan ·', 'config_changed (async)', 'extra.lua')),
            'Config shows prompt and async hook sources', t.view())
    t.shot('03-config-entries')
    t.send(ALT_MENU)
    t.send('#plan'); t.send('\r'); t.send('\r')
    t.wait('Finished prompt check.', 'turn with original config')
    extra.write_text(config(2))
    t.pump(1.5)
    t.send('\x1bg')
    t.until(lambda: t.view().count('command-result-v1') == 2,
            'command hotkey works in chat and file edits do not auto-reload')
    f.check('Expanded test plan.' not in t.view(), 'edited show_prompt does not apply without reload', t.view())
    open_item(t, 'Config', ALT_MENU)
    t.send(RELOAD)
    t.wait('Reloaded', 'Ctrl+R in Config reloads the edited config')
    t.shot('04-config-reloaded')
    t.send(ALT_MENU)
    t.wait('Expanded test plan.', 'show_prompt reveals existing prompt text after reload')
    t.send('\x1bg')
    t.wait('command-result-v2', 'hotkey runs the new command closure after reload')
    t.send('reloaded\r')
    t.wait('Hello from the fake provider.', 'turn after reload reaches provider')
    f.check('FIXTURE-STYLE-2' in f.requests[-1].get('instructions', ''), 'reload rebuilds provider instructions')
    extra.write_text('return { commands =\n')
    open_item(t, 'Config', ALT_MENU)
    t.send(RELOAD)
    t.wait('Errors (1)', 'failed reload reports a config error')
    joined = ''.join(r.strip() for r in t.rows())
    f.check('extra.lua' in t.view() and 'at EOF' in t.view() and 'syntax error' in joined,
            'reload error names the broken file and syntax error', t.view())
    t.shot('05-config-broken')
    t.send(ALT_MENU)
    t.send('\x1bg')
    t.until(lambda: t.view().count('command-result-v2') == 2, 'failed reload keeps the previous command')
    hook = f.cwd / 'reload-hook.txt'
    t.until(lambda: hook.exists() and hook.read_text() == 'true\nfalse\n',
            'async config_changed hook sees both results in order')
    (f.out / 'reload-hook.txt').write_text(hook.read_text())
    f.check(t.close() == 0, 'Config and commands session quits cleanly')


def live_session(f, t):
    t.send('identity\r')
    t.wait('Finished identity.', 'default session() can query the live session')
    record = json.loads(outputs(f.requests[-1])['identity'])
    ident = record['id']
    f.check(record['mode'] == 'main' and record['model'] == 'fixture' and
            record['cwd'] == str(f.cwd.resolve()), 'session exposes live cwd, mode and model', record)
    return ident


def status_and_steer(f):
    from PIL import Image

    f.configure()
    (f.cwd / 'zel.lua').write_text('''return {settings = {
      show_code = false, colors = {accent = "#ff3b30", dark_gray = "#606060"},
    }}''')
    t = boot(f, 'live')
    ident = live_session(f, t)

    def cmd(name, *args, stdin=None):
        p = f.run(name, '-s', ident, *args, stdin=stdin)
        f.require(p.returncode == 0, f'{name} {args!r} succeeds for explicit session', p)
        return p.stdout.strip()

    indicator = cmd('status', 'new', 'indicator-initial')
    f.require(bool(indicator), 'status new returns an indicator ID')
    t.wait('indicator-initial', 'idle indicator is visible')
    cmd('status', '-i', indicator, 'indicator-updated')
    t.wait('indicator-updated', 'status update redraws the indicator')
    t.send('active-status\r')
    t.wait('fixture-call-status', 'explicit Lua call status is visible even with show_code disabled')
    rows = t.rows()
    i = next(y for y, row in enumerate(rows) if 'indicator-updated' in row)
    w = next(y for y, row in enumerate(rows) if 'fixture-call-status' in row)
    f.check(rows[i].startswith(tuple('◐◓◑◒')) and rows[w].startswith(tuple(STARS)) and
            w == i + 2 and rows[i + 1] == '', 'indicator and call status are separate lines with a blank separator', rows)
    f.check('os.execute' not in rows[w], 'call status replaces Lua code', rows[w])
    t.shot('01-status')
    Image.new('RGB', (24, 16), (20, 80, 200)).save(f.cwd / 'fixture.png')
    cmd('steer', '--file', 'fixture.png', '-', stdin='steer-marker-active\n')
    f.check(not any('steer-marker-active' in text for body in f.requests for text in user_texts(body)),
            'active steer stays queued while Lua is running')
    (f.cwd / 'release-turn').touch()
    t.wait('Steering and notifications received.', 'active steer is delivered when Lua finishes')
    body = f.requests[-1]
    items = inputs(body)
    out_pos = next((i for i, item in enumerate(items) if item.get('call_id') == 'held' and
                    item.get('type') == 'function_call_output'), -1)
    steer_pos = next((i for i, item in enumerate(items) if item.get('role') == 'user' and
                      'steer-marker-active' in json.dumps(item)), -1)
    parts = items[steer_pos].get('content') if steer_pos >= 0 else None
    f.check(0 <= out_pos < steer_pos and outputs(body).get('held') == 'released' and
            isinstance(parts, list) and any(p.get('type') == 'input_image' for p in parts),
            'active steer follows the tool result and carries its image', items[-4:])
    f.check('fixture-call-status' not in t.view() and 'indicator-updated' in t.view(),
            'call status clears without clearing the independent indicator', t.view())
    cmd('status', '-i', indicator, '--done')
    t.until(lambda: 'indicator-updated' not in t.view(), 'status --done removes the indicator')

    count = len(f.requests)
    cmd('print', '-', stdin='fixture-print-one\nfixture-print-two\n')
    t.wait('fixture-print-two', 'print reads stdin and renders multiple lines')
    print_y = next((y for y, r in enumerate(t.rows()) if r == 'fixture-print-one'), None)
    f.check(print_y is not None and t.rows()[print_y + 1] == 'fixture-print-two' and
            t.screen.buffer[print_y][0].fg == '606060', 'print has no header and uses dark gray', t.view())
    f.check(len(f.requests) == count, 'print does not start a model turn')
    t.shot('02-print')
    cmd('steer', 'steer-marker-idle')
    t.until(lambda: len(f.requests) > count and 'steer-marker-idle' in '\n'.join(user_texts(f.requests[-1])),
            'idle steer wakes a session without keyboard input')
    f.check(not any('fixture-print-' in json.dumps(body) for body in f.requests), 'print is never sent to the provider')
    choice_id = cmd('choice', 'Pick a letter', 'A', 'B')
    f.check(bool(choice_id), 'choice returns an ID without waiting for the answer')
    t.wait('Pick a letter', 'choice opens a picker')
    t.shot('03-choice')
    t.send(DOWN)
    count = len(f.requests)
    t.send('\r')
    t.until(lambda: len(f.requests) > count, 'choice answer wakes the model')
    f.check(any('Pick a letter: B' in s and choice_id in s and '<notification>' in s
                for s in user_texts(f.requests[-1])), 'choice answer contains the ID, question and selected B',
            list(user_texts(f.requests[-1])))
    cmd('rename', 'acceptance-resume')
    t.send(TRACE)
    t.wait('Tracing · All', 'Tracing opens for live session')
    t.shot('04-print-tracing')
    if 'fixture-print-one' not in t.view():
        t.send('\x1b[5~')
        t.shot('05-print-tracing-previous')
    t.wait('fixture-print-one', 'Tracing includes local print records', required=False, timeout=2)
    leave_page(t)
    f.check(t.close() == 0, 'live session quits cleanly')
    resumed = boot(f, 'resumed')
    resumed.send(RELOAD)
    resumed.wait('acceptance-resume', 'session picker lists the renamed session')
    resumed.send('acceptance-resume')
    resumed.send('\r')
    resumed.wait('fixture-print-two', 'print history survives resume through the session picker')
    resumed.shot('01-print-history')
    f.check(resumed.close() == 0, 'resumed session quits cleanly')


def tracing(f):
    f.configure()
    (f.cwd / 'zel.lua').write_text('return {hooks = {{events = {"turn_start", "idle"}, fn = function() end}}}')
    t = boot(f, 'trace')
    t.send('stream-check\r')
    t.wait('Hello from the fake provider.', 'first tracing turn answered')
    t.send('trace-check\r')
    t.wait('Finished trace2.', 'second tracing turn has two Lua calls')
    f.check(outputs(f.requests[-1]).get('trace1') == 'traced' and
            'error: trace failure' in outputs(f.requests[-1]).get('trace2', ''),
            'provider gets both returns and Lua errors', outputs(f.requests[-1]))
    t.send(TRACE)
    t.wait('Turn 2 · trace-check', 'Tracing shows per-turn headings')
    f.check('Tracing · All' in t.view() and re.search(r'hook\s+turn_start', t.view()),
            'All view includes hook entries', t.view())
    t.shot('01-all')
    t.send('\x1b[5~')
    t.wait('Turn 1 · stream-check', 'PgUp reaches the first turn')
    t.send(PGDN)
    t.send('\t')
    t.wait('Tracing · Lua', 'Tab filters tracing to Lua')
    f.check(all(x in t.view() for x in ('return "traced"', 'trace failure')) and
            'turn_start' not in t.view(), 'Lua view contains calls but not hooks', t.view())
    t.shot('02-lua')
    t.send('\r')
    t.wait('Tracing / lua · 2/2', 'Enter opens the latest Lua entry')
    f.check(all(x in t.view() for x in ('── output', 'error: trace failure', '200 in · 40 out')),
            'Lua entry includes error output and request tokens', t.view())
    t.shot('03-error-entry')
    t.send('\x1b[D')
    t.wait('Tracing / lua · 1/2', 'Left opens the previous Lua entry')
    f.check('traced' in t.view().split('── output')[-1], 'previous entry shows returned value', t.view())
    t.shot('04-previous-entry')
    t.send(ESC)
    t.wait('Tracing · Lua', 'Esc goes back to the filtered Lua list')
    t.send('\t\t\t')
    t.send('\x1b[104;5u')
    t.wait('hooks hidden', 'Ctrl+H toggles hook rows')
    f.check('turn_start' not in t.view(), 'hook rows are hidden after toggle', t.view())
    t.shot('05-hooks-hidden')
    leave_page(t)
    f.check(t.close() == 0, 'Tracing session quits cleanly')


if __name__ == '__main__':
    raise SystemExit(suite('terminal-checks.log', [no_provider, composer_and_models,
                          config_and_commands, status_and_steer, tracing], respond))
