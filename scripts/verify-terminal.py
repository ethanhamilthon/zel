"""Real PTY check of zel. Usage: python verify-terminal.py BINARY OUTPUT_DIRECTORY.
Requires pyte and Pillow. Uses only temporary homes and a local fake provider.
The PNG files are renderings of the captured virtual terminal, not OS screenshots.
"""
import codecs, fcntl, json, os, pathlib, pty, re, select, shlex, struct, subprocess, sys
import tempfile, termios, threading, time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import pyte
from PIL import Image, ImageDraw, ImageFont

BIN = str(pathlib.Path(sys.argv[1]).resolve())
OUT = pathlib.Path(sys.argv[2]).resolve(); OUT.mkdir(parents=True, exist_ok=True)
COLS, ROWS = 100, 30
requests, image_seen, failures = [], False, {}
LONG = 'sleep 6; echo ' + ' '.join(f'word{i}' for i in range(60))

def answer(text): return {'type': 'message', 'role': 'assistant', 'content': [{'type': 'output_text', 'text': text}]}
def lua(code, ident): return {'type': 'function_call', 'name': 'lua', 'call_id': ident, 'arguments': json.dumps({'code': code})}

class Provider(BaseHTTPRequestHandler):
    def log_message(self, *args): pass
    def do_GET(self):
        self.send_response(200); self.end_headers()
        self.wfile.write(json.dumps({'data': [{'id': 'fixture'}, {'id': 'second-model'}]}).encode())
    def do_POST(self):
        global image_seen
        body = json.loads(self.rfile.read(int(self.headers['Content-Length']))); requests.append(body)
        users, calls = [], {x.get('call_id') for x in body.get('input', [])}
        for item in body.get('input', []):
            if item.get('role') != 'user': continue
            content = item.get('content', '')
            if isinstance(content, list):
                users.append('\n'.join(p.get('text', '') for p in content))
                image_seen = image_seen or any(p.get('type') == 'input_image' and p['image_url'].startswith('data:image/') for p in content)
            else: users.append(content)
        last = users[-1] if users else ''
        if last == 'retry-check' and failures.setdefault('retry', 0) < 2:
            failures['retry'] += 1
            self.send_response(503); self.send_header('Retry-After', '1'); self.end_headers()
            self.wfile.write(b'{"error":{"message":"overloaded"}}'); return
        if last == 'slow-run' and 'slow' not in calls: output = [lua(f'return sh({json.dumps(LONG)}).out', 'slow')]
        elif last == 'attach-image' and 'image' not in calls: output = [lua(f'read({json.dumps(str(OUT / "fixture.png"))})', 'image')]
        elif last == 'view-relative' and 'rel' not in calls: output = [lua(f'cd({json.dumps(str(OUT / "relsub"))}); read("rel.png"); cd()', 'rel')]
        elif last == 'hook-check' and 'hook' not in calls: output = [lua('print("hooked")', 'hook')]
        elif last == 'trace-check' and 'tr1' not in calls: output = [lua('print("traced")', 'tr1'), lua('error("trace failure", 0)', 'tr2')]
        elif last == 'task-run' and 'task' not in calls: output = [lua('return task.spawn("bg-check", function() sh("sleep 90") end)', 'task')]
        elif last == 'spawn-sub' and 'spawn' not in calls: output = [lua(f'return sh({json.dumps(shlex.quote(BIN) + " -p sub-work")}).out', 'spawn')]
        elif last == 'sub-work' and 'subsleep' not in calls: output = [lua('sh("sleep 12")', 'subsleep')]
        elif last == 'choice-check' and 'pick' not in calls: output = [lua('return choice("Pick a letter", {"A", "B"})', 'pick')]
        elif last == 'runtime-check' and 'setup' not in calls:
            output = [lua('saved = configured{value = "persisted"}', 'setup'),
                      lua('print(saved); sh("sleep 3")', 'timeout'),
                      lua('return saved', 'reset'),
                      lua('return "HEAD" .. string.rep("x", 2048) .. "TAIL"', 'cut')]
        elif last == 'stream-check': output = [answer('Hello from the fake provider.\n\nA **bold** answer and `code`.')]
        elif last.startswith('<notification>'): output = [answer('Notification received: ' + last)]
        else: output = [answer('Finished ' + (last if not last.startswith('<prompt ') else 'prompt check') + '.')]
        self.send_response(200); self.send_header('Content-Type', 'text/event-stream'); self.end_headers()
        def emit(data): self.wfile.write(('data: ' + json.dumps(data) + '\n\n').encode()); self.wfile.flush()
        try:
            emit({'type': 'response.created'})
            for index, item in enumerate(output):
                initial = dict(item, arguments='') if item['type'] == 'function_call' else dict(item, content=[])
                emit({'type': 'response.output_item.added', 'output_index': index, 'item': initial})
                if item['type'] == 'function_call':
                    emit({'type': 'response.function_call_arguments.delta', 'output_index': index, 'delta': item['arguments']})
                elif item['type'] == 'message':
                    for i in range(0, len(item['content'][0]['text']), 4):
                        emit({'type': 'response.output_text.delta', 'output_index': index, 'delta': item['content'][0]['text'][i:i+4]})
            emit({'type': 'response.completed', 'response': {'output': output, 'usage': {'input_tokens': 200, 'output_tokens': 40}}})
        except (BrokenPipeError, ConnectionResetError): pass

server = ThreadingHTTPServer(('127.0.0.1', 0), Provider)
threading.Thread(target=server.serve_forever, daemon=True).start()
(OUT / 'relsub').mkdir(exist_ok=True)
for path in (OUT / 'fixture.png', OUT / 'relsub' / 'rel.png'):
    Image.new('RGB', (48, 32), (20, 80, 200)).save(path)
base_env = {k: v for k, v in os.environ.items() if not k.startswith(('ZEL_', 'REX_'))}


def init(settings='', hooks='', functions=''):
    return f'''local zel = require("zel")
local systems = {{}}
for _, mode in ipairs(zel.session.modes) do
    if mode == "plan" then systems.plan = "PLAN-MODE-RULE" end
end
return {{
    settings = {{colors = {{bg = "#0A0A0A", accent = "#FF3B30"}}, {settings}}},
    prompts = {{plan = "Expanded test plan."}},
    systems = systems,
    hooks = {{{hooks}}},
    functions = {{{functions}}},
}}
'''


def configure(home, code=None, model='fixture'):
    (home / 'init.lua').write_text(init() if code is None else code)
    (home / 'connect.json').write_text(json.dumps({
        'provider': 'fixture', 'model': model, 'effort': 'medium',
        'providers': {'fixture': {
            'base_url': f'http://127.0.0.1:{server.server_port}/v1',
            'api_key': 'local-test', 'api': 'responses',
        }},
    }))
log = []
def check(ok, label, detail=''):
    log.append(('PASS ' if ok else 'FAIL ') + label); print(log[-1], flush=True)
    if not ok: raise AssertionError(label + '\n' + str(detail))

class Term:
    def __init__(self, home, cwd, args=()):
        env = dict(base_env, ZEL_HOME=str(home), TERM='xterm-256color', COLORTERM='truecolor')
        self.master, slave = pty.openpty()
        fcntl.ioctl(slave, termios.TIOCSWINSZ, struct.pack('HHHH', ROWS, COLS, 0, 0))
        def tty(): os.setsid(); fcntl.ioctl(0, termios.TIOCSCTTY, 0)
        self.p = subprocess.Popen([BIN, *args], stdin=slave, stdout=slave, stderr=slave, cwd=cwd, env=env, preexec_fn=tty)
        os.close(slave)
        self.screen = pyte.Screen(COLS, ROWS); self.stream = pyte.Stream(self.screen)
        self.decoder = codecs.getincrementaldecoder('utf8')('replace'); self.raw = bytearray()
    def pump(self, seconds=.2):
        end = time.monotonic() + seconds
        while time.monotonic() < end:
            if select.select([self.master], [], [], .04)[0]:
                try: data = os.read(self.master, 65536)
                except OSError: return
                if not data: return
                self.raw.extend(data); self.stream.feed(self.decoder.decode(data))
    def send(self, data, wait=.25):
        os.write(self.master, data.encode() if isinstance(data, str) else data); self.pump(wait)
    def rows(self): return [''.join(self.screen.buffer[y][x].data or ' ' for x in range(COLS)).rstrip() for y in range(ROWS)]
    def view(self): return '\n'.join(self.rows())
    def wait(self, text, label, timeout=15):
        end = time.monotonic() + timeout
        while time.monotonic() < end:
            self.pump(.1)
            if text in self.view(): check(True, label); return
        check(False, label, self.view())
    def fg(self, x, y): return self.screen.buffer[y][x].fg
    def shot(self, name):
        (OUT / (name + '.txt')).write_text(self.view())
        font = ImageFont.truetype('/System/Library/Fonts/Menlo.ttc', 15)
        image = Image.new('RGB', (COLS * 10 + 24, ROWS * 21 + 24), '#0A0A0A'); draw = ImageDraw.Draw(image)
        names = {'default': '#EDEDED', 'black': '#222222', 'red': '#FF3B30', 'green': '#75C379', 'brown': '#DDC779', 'blue': '#7EA7DB', 'magenta': '#BD8ACF', 'cyan': '#7FC9C0', 'white': '#EDEDED'}
        for y in range(ROWS):
            for x in range(COLS):
                cell = self.screen.buffer[y][x]
                draw.text((12 + x * 10, 12 + y * 21), cell.data, font=font, fill=names.get(cell.fg, '#' + cell.fg if len(cell.fg) == 6 else '#EDEDED'))
        image.save(OUT / (name + '.png'))
    def close(self):
        self.send('\x11', .5)
        try: self.p.wait(5)
        except subprocess.TimeoutExpired: self.p.kill()
        return self.p.returncode

# 1. No provider: the app exits with an error, and ~/.config/zel gets its default files (only configs, no sessions).
home1, cwd = pathlib.Path(tempfile.mkdtemp(prefix='zelcheck-empty-')), pathlib.Path(tempfile.mkdtemp(prefix='zelcheck-project-'))
t = Term(home1 / 'new', cwd)
while t.p.poll() is None: t.pump(.05)
t.pump(.3)
check(t.p.returncode not in (None, 0) and 'no provider configured' in t.view(), 'no provider: exit with an error', t.view())
(OUT / '00-no-provider.txt').write_text(t.view())
CONFIG_FILES = {'.gitignore', 'init.lua', 'connect.json'}
check(set(os.listdir(home1 / 'new')) == CONFIG_FILES, 'default files (configs only)', os.listdir(home1 / 'new'))
check((home1 / 'new' / '.gitignore').read_text() == '.env\n', '.gitignore content')

# 2. The TUI with a provider.
home = pathlib.Path(tempfile.mkdtemp(prefix='zelcheck-home-')); configure(home)
(cwd / 'my file.go').write_text('sample\n')
(cwd / 'zel.lua').write_text('return {systems = {local_rule = "LOCAL-FILE-RULE"}}\n')
t = Term(home, cwd); t.pump(1.5); t.send('\x1b', .4); t.send('\x1b', .4)
check(t.screen.cursor.x == 0 and t.screen.cursor.y == 0, 'empty composer starts at the corner, no padding', (t.screen.cursor.x, t.screen.cursor.y))
t.send('draft'); check(t.screen.cursor.x == 5, 'typed text has no left padding'); t.shot('01-composer')
t.send('\x15'); t.send('stream-check\r')
t.wait('Hello from the fake provider.', 'answer streamed'); t.pump(.8)
rows = t.rows(); last = max(y for y, r in enumerate(rows) if r and y < t.screen.cursor.y)
check(t.screen.cursor.y - last == 2, 'exactly one empty line between the answer and the input', (last, t.screen.cursor.y)); t.shot('02-answer-and-composer')

t.send('slow-run\r'); t.pump(2.5)
STARS = '✶✸✹✺'
row = next((y for y, r in enumerate(t.rows()) if r and r[0] in STARS), None)
check(row is not None, 'working indicator shown', t.view())
text = t.rows()[row]
check(text.endswith('...') and len(text) <= COLS, 'the Lua code is one line cut with ...', text)
x_time, x_code = 2, text.index('return sh')
check(t.fg(x_time, row) == 'ff3b30' and t.fg(x_code, row) == '606060', 'time is accent, the Lua code is dark gray', (t.fg(x_time, row), t.fg(x_code, row)))
t.shot('03-working')
t.send('\x04', .8)  # stop the slow turn

t.send('@'); t.pump(.8); rows = t.rows(); y = t.screen.cursor.y
check(rows[y + 1] == '' and rows[y + 2].endswith(' ‹') and not rows[y + 2].startswith(' '), 'file completions: an empty line, no left padding, arrow on the right', rows[y:y + 5]); t.shot('04-file-completions')
t.send('\x1b', .4); t.send('\x15'); t.send('#'); t.pump(.5); t.shot('05-prompt-completions')
check('plan' in t.view(), '# lists the configured prompt', t.view())
t.send('\x1b', .4); t.send('\x15')
t.send('#plan'); t.send('\x1b', .4); t.send('\r')
t.wait('Finished prompt check.', 'answer to #plan')
prompt = '<prompt name="plan">\nExpanded test plan.\n</prompt>'
check(prompt in [i.get('content') for i in requests[-1]['input']], '#plan reaches the model as a prompt block', requests[-1]['input'][-2:])
check('#plan' in t.view() and 'Expanded test plan.' not in t.view(), 'chat shows #plan by default', t.view())

t.send('\x0b'); t.pump(.4); rows = t.rows(); body = '\n'.join(rows)
check('Providers' not in body and 'Settings' not in body and 'Esc' not in body and 'select' not in body, 'menu: no Providers, Settings or hints', body)
items = [(y, r) for y, r in enumerate(rows) if any(k in r for k in ('Resume', 'Quit', 'Models', 'Info'))]
descs = ('Open a past session', 'Save and exit', 'Switch model and effort', 'Model, context and spend')
ends = {r.index(k) + len(k) for y, r in items for k in descs if k in r}
box_x, box_w = rows[items[0][0]].index('Resume'), max(60, COLS // 2)
check(ends == {box_x + box_w - 2}, 'main menu: descriptions instead of hotkeys, flush with the right edge of the box', (ends, box_x, [r for y, r in items]))
check('Ctrl+' not in body and rows[items[0][0]].endswith('session \u2039') and len(rows[items[0][0]]) == box_x + box_w, 'the arrow is in the last column of the box', rows[items[0][0]])
def title_at(title):
    return next(((y, r.index(title)) for y, r in enumerate(t.rows()) if title in r), None)
main_at = title_at('Zel '); t.shot('06-menu')
t.send('info'); t.send('\r'); t.pump(.6); t.shot('07-info')
check('init.lua' in t.view(), 'Info shows init.lua', t.view())
check(title_at('Info') == main_at, 'the menu and Info are one box: the title stays in place', (main_at, title_at('Info'))); t.send('\x1b', .5)
t.send('models'); t.send('\r'); t.pump(1.2); t.shot('08-models')
before = json.loads((home / 'connect.json').read_text())
t.send('second'); t.send('\r'); t.pump(.5); t.shot('09-effort'); t.send('\r'); t.pump(1.0)
after = json.loads((home / 'connect.json').read_text())
check(after == dict(before, model='second-model'), 'choosing a model updates only the model selection in connect.json', after)
# The config is watched: an outside edit updates old prompts as well as colors.
(home / 'init.lua').write_text(init('show_prompt = true').replace('#FF3B30', '#30D158')); t.pump(1.8)
check('Expanded test plan.' in t.view(), 'show_prompt reveals existing prompt text on reload', t.view())
t.send('@'); t.pump(.8)
check(any(t.fg(x, y) == '30d158' for y in range(ROWS) for x in range(COLS)), 'an edit of init.lua changes the accent while running', t.view())
t.send('\x1b', .4); t.send('\x15')

# mouse is left to the terminal; links and selection are the terminal's business
check(not any(m in bytes(t.raw) for m in (b'\x1b[?1000h', b'\x1b[?1002h', b'\x1b[?1003h', b'\x1b[?1006h')), 'the mouse is not captured')

# Images: read() attaches an image, nothing is stored.
t.send('attach-image\r'); t.wait('Notification received', 'read() attached the image', 25); t.pump(.5)
check(image_seen, 'the provider received the image bytes')
t.send('\x0b'); t.send('tracing'); t.send('\r'); t.pump(.8); t.shot('10-tracing'); t.send('\x1b', .4); t.send('\x1b', .4)
check(t.close() == 0, 'clean quit')
state_home = home.parent / ('.zel-' + home.name)
session_files = list((state_home / 'sessions').rglob('session.jsonl')); data = ''.join(f.read_text() for f in session_files)
check('base64' not in data and '[an image was here]' in data, 'saved history has a placeholder, no image data', data[-400:])
check(not list(home.rglob('attachments')), 'no attachments folder anywhere', list(home.rglob('attachments')))
check(set(p.name for p in home.iterdir()) == CONFIG_FILES, 'config home holds only configs, no sessions', os.listdir(home))

# resume shows the placeholder where the image was (Tracing keeps the notification that carried it)
t = Term(home, cwd); t.pump(1); t.send('\x1b', .4); t.send('\x1b', .4); t.send('\x12'); t.pump(.8); t.send('\r'); t.pump(1.2)
t.send('\x0b'); t.send('tracing'); t.send('\r'); t.pump(.8)
t.send('notification attached'); t.send('\r', .6)
found = '[an image was here]' in t.view()
if found: t.shot('11-resumed-placeholder')
check(found, 'a resumed chat shows the placeholder where the image was', t.view())
for _ in range(3): t.send('\x1b', .4)

# retry: two 503 with Retry-After, then the answer
t.send('retry-check\r'); t.wait('Finished retry-check.', 'answer after two retries', 30)
check(failures['retry'] == 2, 'the provider failed twice, then answered', failures)
t.send('\x0b'); t.send('tracing'); t.send('\r'); t.pump(.8); t.send('Retrying', .6); t.shot('12-retry-tracing')
check('Retrying in' in t.view(), 'retries are shown in Tracing', t.view()); t.send('\x1b', .4); t.send('\x1b', .4)
check(t.close() == 0, 'clean quit again')

# --config replaces init.lua and skips zel.lua, but keeps connect.json.
other = pathlib.Path(tempfile.mkdtemp(prefix='zelcheck-other-')) / 'another.lua'
other.write_text('return {systems = {override = "ALTERNATE-INIT-RULE"}}\n')
def run(*args, env_extra=None):
    return subprocess.run([BIN, *args], cwd=cwd, env=dict(base_env, ZEL_HOME=str(home), **(env_extra or {})), capture_output=True, text=True, timeout=30)
r = run('config', '--config', str(other)); check(str(other) in r.stdout and 'override' in r.stdout and 'local_rule' not in r.stdout, 'zel config --config replaces init.lua and skips zel.lua', r.stdout)
r = run('config'); check('local_rule' in r.stdout and 'second-model' in r.stdout, 'zel config loads zel.lua and the selected model', r.stdout)
r = run('-p', 'stream-check', '--config', str(other)); request = requests[-1]
check(r.returncode == 0 and request['model'] == 'second-model' and 'ALTERNATE-INIT-RULE' in json.dumps(request) and 'LOCAL-FILE-RULE' not in json.dumps(request), 'zel -p --config uses the override and keeps connect.json', (r.stdout, r.stderr))
# The sole model tool describes the Lua API, with mode-specific guidance.
main_tool, = requests[0]['tools']
check(main_tool['name'] == 'lua', 'the model has one tool, lua', main_tool)
main_tool = main_tool['description']
check('choice(question,' in main_tool and 'Run Lua 5.1 code' in main_tool, 'the main description documents Lua and choice()', main_tool)
check(not any(tag in main_tool for tag in ('[main]', '[headless]', '[nest]')), 'no tool description template markers', main_tool)
check('{timeout}' not in main_tool and '{output}' not in main_tool, 'no template placeholder is left', main_tool)
check(requests[0]['reasoning'].get('summary') == 'auto', 'the reasoning summary is always requested', requests[0].get('reasoning'))
r = run('-p', 'stream-check', '--config', str(other))
head_tool = requests[-1]['tools'][0]['description']
check('no user to ask' in head_tool and 'choice(question,' not in head_tool, 'the headless description has no user and no choice()', head_tool)

other.write_text(init('timeout = 1, output = 1000', functions='''configured = {
    description = "return the value argument",
    fn = function(args) return args.value end,
}'''))
r = run('-p', 'runtime-check', '--config', str(other)); request = requests[-1]
outputs = {i['call_id']: i['output'] for i in request['input'] if i.get('type') == 'function_call_output'}
check(r.returncode == 0 and 'return the value argument' in request['tools'][0]['description'], 'configured functions are advertised to the model', r.stderr)
check(outputs.get('timeout') == 'persisted\n[timeout after 1s; VM reset]', 'Lua globals survive between calls and timeout covers sh()', outputs)
check(outputs.get('reset') == '[lua: fresh VM]\nnil', 'the call after timeout gets a fresh Lua VM', outputs)
cut = outputs.get('cut', '')
full_output = re.search(r'^======== full output: (.+)$', cut, re.M)
check('[output cut: 1056 of 2056 bytes omitted]' in cut and full_output is not None and pathlib.Path(full_output[1]).read_text() == 'HEAD' + 'x' * 2048 + 'TAIL', 'settings.output cuts the result and saves the full text', cut)

# removed settings are config errors now
old = pathlib.Path(tempfile.mkdtemp(prefix='zelcheck-old-')) / 'old.lua'
for key in ('slash_menu', 'reasoning_summary', 'bash_timeout', 'bash_output', 'show_bash'):
    old.write_text(init(f'{key} = true'))
    r = run('config', '--config', str(old))
    check('unknown' in r.stdout and key in r.stdout, f'settings.{key} is gone', r.stdout)

# "/" opens the menu; a Lua hook matches the code and appears in Tracing.
home3 = pathlib.Path(tempfile.mkdtemp(prefix='zelcheck-hook-'))
configure(home3, init(hooks='''{events = {"lua_after"}, fn = function(_, data)
    if data.code == 'print("hooked")' then return "HOOK-SAW-IT" end
end}'''))
t = Term(home3, cwd); t.pump(1.2); t.send('\x1b', .4); t.send('\x1b', .4)
t.send('/', .6); t.shot('13-slash-menu')
check('Resume' in t.view() and 'Models' in t.view(), '"/" opens the menu', t.view())
t.send('\x1b', .4)
t.send('hook-check\r'); t.wait('Finished hook-check.', 'answer after the hooked command', 20)
outputs = [i.get('output', '') for i in requests[-1].get('input', []) if i.get('type') in ('function_call_output', 'custom_tool_call_output')]
check(any('hooked\nHOOK-SAW-IT' in o for o in outputs), 'lua_after matched the code and appended output', outputs)
t.send('\x0b'); t.send('tracing'); t.send('\r'); t.pump(.8); t.shot('14-hook-in-tracing')
check(re.search(r'hook\s+lua_after: ', t.view()) is not None, 'the hook run is an entry in Tracing', t.view())
t.send('\x1b', .4); t.send('\x1b', .4)
t.send('view-relative\r'); t.wait('Notification received: view: rel.png attached', 'read() resolves a relative path after cd()', 25)
has_image = any(p.get('type') == 'input_image' for i in requests[-1].get('input', []) if isinstance(i.get('content'), list) for p in i['content'])
check(has_image, 'the image of a relative path reached the model', requests[-1].get('input', [])[-2:])
check(t.close() == 0, 'clean quit after hooks')

# Connection selection belongs in connect.json, not init.lua.
old.write_text('return {providers = {}}\n')
r = run('config', '--config', str(old)); check('unknown' in r.stdout and 'providers' in r.stdout, 'init.lua rejects providers', r.stdout)
(OUT / '20-config.txt').write_text(run('config').stdout)

# modes: --mode plan adds the plan instruction, headless alone does not
r = run('-p', 'stream-check', '--mode', 'plan'); plan_prompt = json.dumps(requests[-1].get('instructions', '')) + json.dumps(requests[-1].get('input', [])[:1])
check(r.returncode == 0 and 'PLAN-MODE-RULE' in plan_prompt, 'zel -p --mode plan gets the plan instruction', (r.stderr, plan_prompt[:300]))
r = run('-p', 'stream-check'); plain_prompt = json.dumps(requests[-1].get('instructions', '')) + json.dumps(requests[-1].get('input', [])[:1])
check('PLAN-MODE-RULE' not in plain_prompt, 'zel -p without --mode has no plan instruction', plain_prompt[:300])
r = run('-p', 'x', '--mode', 'main'); check(r.returncode != 0 and 'not a custom mode' in r.stderr, '--mode main is refused', r.stderr)

home4 = pathlib.Path(tempfile.mkdtemp(prefix='zelcheck-092-'))
configure(home4, init('editor = "sleep 2; perl -pi -e s/draft/EDITED/"'), model='second-model')
t = Term(home4, cwd, ('--mode', 'plan')); t.pump(1.5); t.send('\x1b', .4); t.send('\x1b', .4)
# the current model is the first row of the models menu
t.send('\x0b'); t.send('models'); t.send('\r'); t.pump(1.5)
rows = t.rows(); first = next((r for r in rows if 'second-model' in r or 'fixture  ' in r), '')
check('second-model' in first and '‹' in first, 'the current model is first and selected in the models menu', t.view()); t.shot('21-models-current-first')
t.send('\x1b', .4); t.send('\x1b', .4)
# Ctrl+E: the editor gets the input; zel keeps running; the edited text comes back
t.send('my draft text'); t.send('\x05', .5)
t.pump(1.0); check('my draft text' not in t.view(), 'while the editor runs zel does not draw', t.view())
t.pump(1.8)
check('my EDITED text' in t.view(), 'Ctrl+E: the editor changed the input and zel shows it', t.view()); t.shot('22-ctrl-e-result')
t.send('\x15')
# a real nvim: type into it, save and quit
(home4 / 'init.lua').write_text(init('editor = "nvim -u NONE -i NONE"')); t.pump(1.8)
t.send('from zel'); t.send('\x05', 1.5); t.shot('23-nvim-open')
check('from zel' in t.view() and '~' in t.view(), 'Ctrl+E opens nvim with the input', t.view())
t.send('A and nvim\x1b', .4); t.send(':wq\r', 1.2)
check('from zel and nvim' in t.view(), 'the text saved in nvim is the input now', t.view()); t.shot('24-after-nvim')
t.send('\x15')
# the task indicator: its own sprite, one empty line, then the working line
t.send('task-run\r'); t.wait('Finished task-run.', 'a background task started', 20)
t.send('slow-run\r'); t.pump(2.5)
rows = t.rows()
task_y = next((y for y, r in enumerate(rows) if r and r[0] in '◐◓◑◒' and 'bg-check' in r), None)
work_y = next((y for y, r in enumerate(rows) if r and r[0] in STARS), None)
check(task_y is not None and work_y == task_y + 2 and rows[task_y + 1] == '', 'task line, one empty line, working line', t.view())
check(t.fg(2, task_y) == 'ff3b30' and t.fg(rows[task_y].index('bg-check'), task_y) == '606060', 'the task line is styled like the working line', (t.fg(2, task_y),))
t.shot('25-task-and-working')
t.send('\x04', .8)
# Info lists a running subagent with its model, effort, context and spend
t.send('spawn-sub\r'); t.pump(3.5)
t.send('\x0b'); t.send('info'); t.send('\r'); t.pump(1.5)
t.send('\x1b[6~', .6)  # the box is half the screen: PgDn to the subagent
v = t.view()
check('Subagents (1)' in v and 'fixture · second-model · medium' in v and '200 in' in v and 'sub-work' in v, 'Info shows the subagent: model, effort, context, spend', v)
t.shot('26-info-subagent')
t.send('\x1b', .4); t.send('\x1b', .4)
t.wait('Finished spawn-sub.', 'the subagent finished and the agent answered', 30)
check(t.close() == 0, 'clean quit after the editor, task and subagent checks')

# Lua call counter, choice picker, input height, and the menu box.
home5 = pathlib.Path(tempfile.mkdtemp(prefix='zelcheck-input-'))
configure(home5, init('show_code = false'))
t = Term(home5, cwd); t.pump(1.5); t.send('\x1b', .4); t.send('\x1b', .4)
t.send('hook-check\r'); t.wait('Finished hook-check.', 'a turn with one Lua call', 20)
check('1 call' in t.view() and 'step' not in t.view(), 'the turn summary says "1 call", not steps', t.view()); t.shot('27-one-call')
t.send('\x1b[200~' + '\n'.join(f'line{i}' for i in range(1, 26)) + '\x1b[201~', .3)
t.pump(.8); rows = t.rows()
shown = [r for r in rows if r.startswith('line')]
check(len(shown) == 25, 'a 25-line input shows all 25 lines (a third of the screen was 10)', t.view()); t.shot('28-tall-input')
t.send('\x15', .4)
t.send('\x0b'); t.send('info'); t.send('\r'); t.pump(1.2)
rows = t.rows(); y = next((y for y, r in enumerate(rows) if r.strip() == 'Info'), None)
check(y == (ROWS - 16) // 2 and rows[y].index('Info') == (COLS - 60) // 2, 'Info: a centered half-screen box, title at its top left', (y, rows[y] if y is not None else t.view())); t.shot('29-info-box')
t.send('\x1b', .4); t.send('\x1b', .4)
t.send('slow-run\r'); t.pump(2.5)
working = next((r for r in t.rows() if r and r[0] in STARS), '')
check(working and 'return sh' not in working, 'show_code = false hides the code but keeps the working indicator', working)
t.send('\x04', .8)
t.send('choice-check\r'); t.wait('Pick a letter', 'choice() opens a picker')
t.shot('29-choice')
t.send('\x1b[B'); t.send('\r')
t.wait('Notification received: c1 Pick a letter: B', 'the selected answer reaches the model')
check(t.close() == 0, 'clean quit after the input and choice checks')

# Tracing by turn, Lua entries, navigation, and hook visibility.
home6 = pathlib.Path(tempfile.mkdtemp(prefix='zelcheck-trace-'))
noise = '{events = {"turn_start", "idle"}, fn = function() end}'
configure(home6, init(hooks=noise))
t = Term(home6, cwd); t.pump(1.5); t.send('\x1b', .4); t.send('\x1b', .4)
t.send('stream-check\r'); t.wait('Hello from the fake provider.', 'turn 1 answered')
t.send('trace-check\r'); t.wait('Finished trace-check.', 'turn 2: two Lua calls in one response, then an answer', 20); t.pump(.8)
t.send('\x14', 1.0); v = t.view(); t.shot('30-tracing-turns')
check('Tracing · All' in v and 'Turn 2 · trace-check' in v and v.rstrip().endswith('‹'), 'Ctrl+T: Tracing with a row per turn, at the latest entry', v)
t.send('\x1b[5~', .5); check('Turn 1 · stream-check' in t.view(), 'PgUp: the first turn has its row too', t.view()); t.send('\x1b[6~', .5)
check(re.search(r'hook\s+turn_start: ', v) is not None and re.search(r'lua\s+error\("trace failure", 0\)\s+error · <?\d+ms', v) is not None and 'output ' not in v, 'hooks shown, Lua error and duration, no separate output row', v)
t.send('\t', .5); v = t.view(); t.shot('31-tracing-lua')
check('Tracing · Lua' in v and 'print("traced")' in v and 'trace failure' in v and 'Hello from' not in v and 'turn_start' not in v, 'Tab: only Lua entries', v)
t.send('\r', .6); v = t.view(); t.shot('32-tracing-entry')
check('Tracing / lua · 2/2 · turn 2' in v and '── output' in v and 'error: trace failure' in v and re.search(r'error · <?\d+ms', v) is not None and re.search(r'request \d+ · .*200 in · 40 out', v) is not None, 'an entry: Lua code and output together, status, duration, request tokens', v)
t.send('\x1b[D', .5); v = t.view(); t.shot('33-tracing-previous')
check('Tracing / lua · 1/2 · turn 2' in v and 'print("traced")' in v and 'traced' in v.split('── output')[-1], 'Left: the previous Lua entry in the view', v)
t.send('\x1b', .6); rows = t.rows(); sel = next((r for r in rows if r.endswith('‹')), '')
check('Tracing · Lua' in '\n'.join(rows) and 'print("traced")' in sel, 'Esc: back on the same row', rows)
t.send('\t\t\t', .5); t.send('\x1b[104;5u', .6); v = t.view(); t.shot('34-tracing-hooks-hidden')
check('Tracing · All · hooks hidden' in v and 'turn_start' not in v and 'Turn 2' in v, 'Ctrl+H hides the hooks', v)
t.send('\x1b', .4); t.send('\x1b', .4)
check(t.close() == 0, 'clean quit after the Tracing checks')
(home6 / 'init.lua').write_text(init('trace_hooks = false', hooks=noise))
t = Term(home6, cwd); t.pump(1.5); t.send('\x1b', .4); t.send('\x1b', .4); t.send('\x12', .8); t.send('\r', 1.2)
t.send('\x14', 1.0); v = t.view(); t.shot('35-trace-hooks-false')
check('hooks hidden' in v and 'turn_start' not in v and 'Turn 2 · trace-check' in v, 'trace_hooks = false: a resumed session opens Tracing without hooks', v)
t.send('\x1b[104;5u', .6); check(re.search(r'hook\s+', t.view()) is not None, 'Ctrl+H shows them again', t.view())
t.send('\x1b', .4); t.send('\x1b', .4)
check(t.close() == 0, 'clean quit after trace_hooks')

(OUT / 'terminal-checks.log').write_text('\n'.join(log) + '\n'); print('ALL PASS')
