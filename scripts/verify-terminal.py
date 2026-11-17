"""Real PTY check of zel. Usage: python verify-terminal.py BINARY OUTPUT_DIRECTORY.
Requires pyte and Pillow. Uses only temporary homes and a local fake provider.
The PNG files are renderings of the captured virtual terminal, not OS screenshots.
"""
import codecs, fcntl, json, os, pathlib, pty, re, select, struct, subprocess, sys
import tempfile, termios, threading, time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import pyte
from PIL import Image, ImageDraw, ImageFont

BIN = str(pathlib.Path(sys.argv[1]).resolve())
OUT = pathlib.Path(sys.argv[2]); OUT.mkdir(parents=True, exist_ok=True)
COLS, ROWS = 100, 30
requests, image_seen, failures = [], False, {}
LONG = 'echo ' + ' '.join(f'word{i}' for i in range(60))

def answer(text): return {'type': 'message', 'role': 'assistant', 'content': [{'type': 'output_text', 'text': text}]}
def command(text, ident): return {'type': 'function_call', 'name': 'bash', 'call_id': ident, 'arguments': json.dumps({'command': text})}

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
        if last == 'slow-run' and 'slow' not in calls: output = [command('sleep 6; ' + LONG, 'slow')]
        elif last == 'attach-image' and 'image' not in calls: output = [command(f'{BIN} view {OUT}/fixture.png', 'image')]
        elif last == 'view-relative' and 'rel' not in calls: output = [command(f'mkdir -p {OUT}/relsub && cp {OUT}/fixture.png {OUT}/relsub/rel.png && cd {OUT}/relsub && {BIN} view rel.png', 'rel')]
        elif last == 'hook-check' and 'hook' not in calls: output = [command('echo hooked', 'hook')]
        elif last == 'stream-check': output = [answer('Hello from the fake provider.\n\nA **bold** answer and `code`.')]
        elif last.startswith('<notification>'): output = [answer('Notification received: ' + last)]
        else: output = [answer('Ready for the next check.')]
        self.send_response(200); self.send_header('Content-Type', 'text/event-stream'); self.end_headers()
        def emit(data): self.wfile.write(('data: ' + json.dumps(data) + '\n\n').encode()); self.wfile.flush()
        try:
            emit({'type': 'response.created'})
            for index, item in enumerate(output):
                initial = dict(item, arguments='') if item['type'] == 'function_call' else dict(item, content=[])
                emit({'type': 'response.output_item.added', 'output_index': index, 'item': initial})
                if item['type'] == 'function_call':
                    emit({'type': 'response.function_call_arguments.delta', 'output_index': index, 'delta': item['arguments']})
                else:
                    for i in range(0, len(item['content'][0]['text']), 4):
                        emit({'type': 'response.output_text.delta', 'output_index': index, 'delta': item['content'][0]['text'][i:i+4]})
            emit({'type': 'response.completed', 'response': {'output': output, 'usage': {'input_tokens': 200, 'output_tokens': 40}}})
        except (BrokenPipeError, ConnectionResetError): pass

server = ThreadingHTTPServer(('127.0.0.1', 0), Provider)
threading.Thread(target=server.serve_forever, daemon=True).start()
Image.new('RGB', (48, 32), (20, 80, 200)).save(OUT / 'fixture.png')
base_env = {k: v for k, v in os.environ.items() if not k.startswith(('ZEL_', 'REX_'))}
CONFIG = f'''provider = "fixture"
model = "fixture"

[providers.fixture]
base_url = "http://127.0.0.1:{server.server_port}/v1"
api_key = "local-test"

[completions]
plan = "Expanded test plan."

[colors]
bg = "#0A0A0A"
accent = "#FF3B30"
'''
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

# 1. No provider: the app exits with an error, and ~/.zel gets its default files.
home1, cwd = pathlib.Path(tempfile.mkdtemp(prefix='zelcheck-empty-')), pathlib.Path(tempfile.mkdtemp(prefix='zelcheck-project-'))
t = Term(home1 / 'new', cwd)
while t.p.poll() is None: t.pump(.05)
t.pump(.3)
check(t.p.returncode not in (None, 0) and 'no provider configured' in t.view(), 'no provider: exit with an error', t.view())
(OUT / '00-no-provider.txt').write_text(t.view())
check(sorted(os.listdir(home1 / 'new')) == ['.gitignore', '.sessions', 'config.toml'] or sorted(os.listdir(home1 / 'new')) == ['.gitignore', 'config.toml'], 'default files', os.listdir(home1 / 'new'))
check((home1 / 'new' / '.gitignore').read_text() == '.env\n.sessions/\n', '.gitignore content')

# 2. The TUI with a provider.
home = pathlib.Path(tempfile.mkdtemp(prefix='zelcheck-home-')); (home / 'config.toml').write_text(CONFIG)
(cwd / 'my file.go').write_text('sample\n'); (cwd / 'zel.toml').write_text('model = "from-local-file"\n')
t = Term(home, cwd); t.pump(1.5)
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
check(text.endswith('...') and len(text) <= COLS, 'the command is one line cut with ...', text)
x_time, x_cmd = 2, text.index('echo')
check(t.fg(x_time, row) == 'ff3b30' and t.fg(x_cmd, row) == '606060', 'time is accent, the command is dark gray', (t.fg(x_time, row), t.fg(x_cmd, row)))
t.shot('03-working')
t.send('\x04', .8)  # stop the slow turn

t.send('@'); t.pump(.8); rows = t.rows(); y = t.screen.cursor.y
check(rows[y + 1] == '' and rows[y + 2].endswith(' ‹') and not rows[y + 2].startswith(' '), 'file completions: an empty line, no left padding, arrow on the right', rows[y:y + 5]); t.shot('04-file-completions')
t.send('\x1b', .4); t.send('\x15'); t.send('#'); t.pump(.5); t.shot('05-prompt-completions'); t.send('\x1b', .4); t.send('\x15')

t.send('\x0b'); t.pump(.4); rows = t.rows(); body = '\n'.join(rows)
check('Providers' not in body and 'Settings' not in body and 'Esc' not in body and 'select' not in body, 'menu: no Providers, Settings or hints', body)
items = [(y, r) for y, r in enumerate(rows) if any(k in r for k in ('Resume', 'Quit', 'Models', 'Info'))]
cols = {r.index(k) for y, r in items for k in ('Ctrl+R', 'Ctrl+Q', 'Ctrl+M', 'Ctrl+I') if k in r}
check(len(cols) == 1, 'hotkeys are one aligned column', cols)
used = [y for y, r in enumerate(rows) if r.strip()]
check(abs(min(used) - (ROWS - 1 - max(used))) <= 1, 'menu is centered vertically', (min(used), max(used)))
left = min(len(r) - len(r.lstrip()) for r in rows if r.strip()); right = max(len(r) for r in rows)
check(abs(left - (COLS - right)) <= 3, 'menu is centered horizontally', (left, COLS - right)); t.shot('06-menu')
t.send('info'); t.send('\r'); t.pump(.6); t.shot('07-info')
check('config.toml' in t.view(), 'Info shows config.toml', t.view()); t.send('\x1b', .5)
t.send('models'); t.send('\r'); t.pump(1.2); t.shot('08-models')
before = (home / 'config.toml').read_text()
t.send('second'); t.send('\r'); t.pump(.5); t.shot('09-effort'); t.send('\r'); t.pump(1.0)
after = (home / 'config.toml').read_text()
changed = [l for l in after.splitlines() if l not in before.splitlines()]
check(changed == ['model = "second-model"'], 'choosing a model writes only the model line', changed)
check('zel.toml' not in os.listdir(home), 'only config.toml is used')
# the file is watched: an outside edit is applied
(home / 'config.toml').write_text(after.replace('#FF3B30', '#30D158')); t.pump(1.8)
t.send('@'); t.pump(.8)
check(any(t.fg(x, y) == '30d158' for y in range(ROWS) for x in range(COLS)), 'an edit of config.toml is applied while running (the arrow is the new accent)', t.view())
t.send('\x1b', .4); t.send('\x15')

# mouse is left to the terminal; links and selection are the terminal's business
check(not any(m in bytes(t.raw) for m in (b'\x1b[?1000h', b'\x1b[?1002h', b'\x1b[?1003h', b'\x1b[?1006h')), 'the mouse is not captured')

# images: zel view works, nothing is stored
t.send('attach-image\r'); t.wait('Notification received', 'zel view image reached the model', 25); t.pump(.5)
check(image_seen, 'the provider received the image bytes')
t.send('\x0b'); t.send('tracing'); t.send('\r'); t.pump(.8); t.shot('10-tracing'); t.send('\x1b', .4); t.send('\x1b', .4)
check(t.close() == 0, 'clean quit')
session_files = list((home / '.sessions').rglob('session.jsonl')); data = ''.join(f.read_text() for f in session_files)
check('base64' not in data and '[an image was here]' in data, 'saved history has a placeholder, no image data', data[-400:])
check(not list(home.rglob('attachments')), 'no attachments folder anywhere', list(home.rglob('attachments')))
check(sorted(p.name for p in home.iterdir()) == ['.gitignore', '.sessions', 'config.toml'], 'home holds only config, .gitignore and .sessions', os.listdir(home))

# resume shows the placeholder where the image was (Tracing keeps the notification that carried it)
t = Term(home, cwd); t.pump(1); t.send('\x12'); t.pump(.8); t.send('\r'); t.pump(1.2)
t.send('\x0b'); t.send('tracing'); t.send('\r'); t.pump(.8)
t.send('notification attached'); t.send('\r', .6)
found = '[an image was here]' in t.view()
if found: t.shot('11-resumed-placeholder')
check(found, 'a resumed chat shows the placeholder where the image was', t.view())
for _ in range(3): t.send('\x1b', .4)

# retry: two 503 with Retry-After, then the answer
t.send('retry-check\r'); t.wait('Ready for the next check', 'answer after two retries', 30)
check(failures['retry'] == 2, 'the provider failed twice, then answered', failures)
t.send('\x0b'); t.send('tracing'); t.send('\r'); t.pump(.8); t.shot('12-retry-tracing')
check('Retrying in' in t.view(), 'retries are shown in Tracing', t.view()); t.send('\x1b', .4); t.send('\x1b', .4)
check(t.close() == 0, 'clean quit again')

# --config uses another file instead of the home config; ./zel.toml is ignored
other = pathlib.Path(tempfile.mkdtemp(prefix='zelcheck-other-')) / 'another.toml'
other.write_text(CONFIG.replace('model = "fixture"', 'model = "from-another"'))
def run(*args, env_extra=None):
    return subprocess.run([BIN, *args], cwd=cwd, env=dict(base_env, ZEL_HOME=str(home), **(env_extra or {})), capture_output=True, text=True, timeout=30)
r = run('config', '--config', str(other)); check('from-another' in r.stdout and str(other) in r.stdout, 'zel config --config reads that file only', r.stdout)
r = run('config'); check('from-local-file' not in r.stdout and 'second-model' in r.stdout, 'a ./zel.toml is not read', r.stdout)
r = run('-p', 'stream-check', '--config', str(other)); check('Hello from the fake provider.' in r.stdout and requests[-1]['model'] == 'from-another', 'zel -p --config uses the model of that file', (r.stdout, r.stderr))
# the bash tool description: one text per mode, nothing the agent can guess, no old wording
main_tool = requests[0]['tools'][0]['description']
check('zel choice "Question"' in main_tool and 'Ask the user before launching subagents' in main_tool, 'the main description has zel choice and the subagent rule', main_tool)
check('cut to 8000 bytes' in main_tool and 'never truncated' not in main_tool and not re.search(r'\brg\b', main_tool), 'the output limit is stated, no tool hints', main_tool)
check('{timeout}' not in main_tool and '{output}' not in main_tool, 'no template placeholder is left', main_tool)
check(requests[0]['reasoning'].get('summary') == 'auto', 'the reasoning summary is always requested', requests[0].get('reasoning'))
r = run('-p', 'stream-check', '--config', str(other))
head_tool = requests[-1]['tools'][0]['description']
check('no user to ask' in head_tool and 'zel choice' not in head_tool and 'Ask the user before' not in head_tool, 'the headless description has no user and no zel choice', head_tool)

# removed settings are config errors now
old = pathlib.Path(tempfile.mkdtemp(prefix='zelcheck-old-')) / 'old.toml'
old.write_text(CONFIG + '\n[settings]\nslash_menu = true\n')
r = run('config', '--config', str(old)); check('unknown key "settings.slash_menu"' in r.stdout, 'settings.slash_menu is gone', r.stdout)
old.write_text(CONFIG + '\n[settings]\nreasoning_summary = true\n')
r = run('config', '--config', str(old)); check('unknown key "settings.reasoning_summary"' in r.stdout, 'settings.reasoning_summary is gone', r.stdout)
old.write_text(CONFIG + '\n[hooks]\nidle = [{ run = "true", match = "x" }]\n')
r = run('config', '--config', str(old)); check('only message, notification' in r.stdout, 'match on an event with nothing to match is an error', r.stdout)

# "/" always opens the menu; a bash_after hook with match fires and is in Tracing
home3 = pathlib.Path(tempfile.mkdtemp(prefix='zelcheck-hook-'))
(home3 / 'config.toml').write_text(CONFIG + '\n[hooks]\nbash_after = [{ match = "^echo hooked", run = "cat >/dev/null; echo HOOK-SAW-IT" }]\n')
t = Term(home3, cwd); t.pump(1.2)
t.send('/', .6); t.shot('13-slash-menu')
check('Resume' in t.view() and 'Models' in t.view(), '"/" opens the menu', t.view())
t.send('\x1b', .4)
t.send('hook-check\r'); t.wait('Ready for the next check', 'answer after the hooked command', 20)
outputs = [i.get('output', '') for i in requests[-1].get('input', []) if i.get('type') in ('function_call_output', 'custom_tool_call_output')]
check(any('hooked\nHOOK-SAW-IT' in o for o in outputs), 'bash_after with match added its line to the output', outputs)
t.send('\x0b'); t.send('tracing'); t.send('\r'); t.pump(.8); t.shot('14-hook-in-tracing')
check(re.search(r'hook\s+bash_after: ', t.view()) is not None, 'the hook run is an entry in Tracing', t.view())
t.send('\x1b', .4); t.send('\x1b', .4)
t.send('view-relative\r'); t.wait('Notification received: <notification>view: rel.png', 'zel view takes a path relative to the folder of the call', 25)
has_image = any(p.get('type') == 'input_image' for i in requests[-1].get('input', []) if isinstance(i.get('content'), list) for p in i['content'])
check(has_image, 'the image of a relative path reached the model', requests[-1].get('input', [])[-2:])
check(t.close() == 0, 'clean quit after hooks')

(OUT / 'terminal-checks.log').write_text('\n'.join(log) + '\n'); print('ALL PASS')
