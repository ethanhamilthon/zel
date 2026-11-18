"""0.11.0 real-PTY acceptance checks: the lua tool, init.lua/zel.lua/connect.json,
Lua hooks, tasks, prompts and show_prompt. Requires pyte and Pillow.
Usage: python scripts/verify-release.py BINARY OUTPUT_DIRECTORY
Uses temporary homes and a local fake provider. The PNG files are renderings
of the captured virtual terminal cells, not OS screenshots.
"""
import codecs, fcntl, json, os, pathlib, pty, select, struct, subprocess, sys
import tempfile, termios, threading, time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import pyte
from PIL import Image, ImageDraw, ImageFont

BIN = str(pathlib.Path(sys.argv[1]).resolve())
OUT = pathlib.Path(sys.argv[2]); OUT.mkdir(parents=True, exist_ok=True)
COLS, ROWS = 100, 30
requests = []

def answer(text): return {'type': 'message', 'role': 'assistant', 'content': [{'type': 'output_text', 'text': text}]}
def lua(code, ident): return {'type': 'function_call', 'name': 'lua', 'call_id': ident, 'arguments': json.dumps({'code': code})}

# The last user text picks the response; a call id already in the input means
# the call ran, so the model answers with what it got.
SCRIPT = {
    'lua-check': lambda: lua('print(add{a = 2, b = 3})\nreturn read("a.txt")', 'l1'),
    'block-check': lambda: lua('sh("rm -rf /tmp/nothing-here")', 'b1'),
    'task-check': lambda: lua('return task.spawn("bg check", function() sh("sleep 1") return "bg-done" end)', 't1'),
    'image-check': lambda: lua(f'return read("{OUT}/fixture.png")', 'i1'),
    'choice-check': lambda: lua('return choice("Pick a letter", {"A", "B"})', 'c1'),
    'timeout-check': lambda: lua('x = 41\nwhile true do end', 'to1'),
    'after-timeout': lambda: lua('return x', 'to2'),
    'edit-check': lambda: lua('write("notes.txt", "one\\ntwo\\n")\nedit("notes.txt", {{"one", "uno"}, {"two", "dos"}})\nreturn read("notes.txt"), #grep("dos", {path = "."})', 'e1'),
}

class Provider(BaseHTTPRequestHandler):
    def log_message(self, *args): pass
    def do_GET(self):
        self.send_response(200); self.end_headers()
        self.wfile.write(json.dumps({'data': [{'id': 'fixture'}, {'id': 'second-model'}]}).encode())
    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers['Content-Length']))); requests.append(body)
        items = body.get('input', [])
        users = []
        for item in items:
            if item.get('role') != 'user': continue
            c = item.get('content', '')
            users.append('\n'.join(p.get('text', '') for p in c) if isinstance(c, list) else c)
        last = users[-1] if users else ''
        outputs = [x for x in items if x.get('type') == 'function_call_output']
        step = SCRIPT.get(last.strip())
        if step and not (outputs and items[-1].get('type') == 'function_call_output'):
            output = [step()]
        elif items and items[-1].get('type') == 'function_call_output':
            output = [answer('Tool said: ' + items[-1]['output'])]
        elif last.startswith('<notification>'):
            output = [answer('Notification received: ' + last)]
        else:
            output = [answer('Ready.')]
        self.send_response(200); self.send_header('Content-Type', 'text/event-stream'); self.end_headers()
        def emit(data): self.wfile.write(('data: ' + json.dumps(data) + '\n\n').encode()); self.wfile.flush()
        try:
            for index, item in enumerate(output):
                emit({'type': 'response.output_item.added', 'output_index': index, 'item': item})
                if item['type'] == 'message':
                    emit({'type': 'response.output_text.delta', 'output_index': index, 'delta': item['content'][0]['text']})
            emit({'type': 'response.completed', 'response': {'output': output, 'usage': {'input_tokens': 200, 'output_tokens': 40}}})
        except (BrokenPipeError, ConnectionResetError): pass

server = ThreadingHTTPServer(('127.0.0.1', 0), Provider)
threading.Thread(target=server.serve_forever, daemon=True).start()
Image.new('RGB', (48, 32), (20, 80, 200)).save(OUT / 'fixture.png')
base_env = {k: v for k, v in os.environ.items() if not k.startswith('ZEL_')}
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


def to_chat(t):
    """Esc out of pages; Esc on the chat opens the main menu, which Esc closes."""
    for _ in range(4):
        menu = 'Open a past session' in t.view()
        t.send('\x1b', .5)
        if menu: return

home = pathlib.Path(tempfile.mkdtemp(prefix='zel-011-home-'))
cwd = pathlib.Path(tempfile.mkdtemp(prefix='zel-011-project-'))
(cwd / 'a.txt').write_text('file text from a.txt\n')
(home / 'connect.json').write_text(json.dumps({'provider': 'fixture', 'model': 'fixture', 'effort': 'low', 'providers': {
    'fixture': {'base_url': f'http://127.0.0.1:{server.server_port}/v1', 'api_key': '$FIXTURE_KEY'}}}, indent=2))
(home / 'lua').mkdir()
(home / 'lua' / 'extra.lua').write_text('return { systems = { extra = "LOADED-MODULE-RULE" } }\n')
idle_file = cwd / 'idle-hook.txt'
(home / 'init.lua').write_text(f'''local zel = require("zel")
return {{
  settings = {{ timeout = 3, colors = {{ bg = "#0A0A0A", accent = "#FF3B30" }} }},
  systems = {{ rules = "GLOBAL-RULE mode=" .. zel.session.mode, style = "global style" }},
  prompts = {{ plan = "Expanded test plan." }},
  functions = {{ add = {{ description = "adds a and b", fn = function(t) return t.a + t.b end }} }},
  hooks = {{
    {{ events = {{"lua_before"}}, fn = function(e, d) if d.code:find("rm %-rf") then return false, "no rm -rf" end end }},
    {{ events = {{"lua_after"}}, fn = function(e, d) return "AFTER-HOOK ok=" .. tostring(d.ok) end }},
    {{ events = {{"idle"}}, async = true, fn = function() zel.write("{idle_file}", "idle hook ran\\n") end }},
  }},
  loads = {{ "lua/extra.lua" }},
}}
''')
(cwd / 'zel.lua').write_text('return { systems = { style = "LOCAL-STYLE" } }\n')
env = dict(base_env, ZEL_HOME=str(home), FIXTURE_KEY='local-test')
base_env['FIXTURE_KEY'] = 'local-test'

# 1. zel config reads init.lua, its loads, zel.lua and connect.json.
r = subprocess.run([BIN, 'config'], env=env, cwd=cwd, capture_output=True, text=True)
(OUT / 'config.txt').write_text(r.stdout + r.stderr)
check(r.returncode == 0 and 'Errors:' not in r.stdout and 'GLOBAL-RULE mode=main' in r.stdout and 'LOADED-MODULE-RULE' in r.stdout
      and 'LOCAL-STYLE' in r.stdout and 'global style' not in r.stdout and '#plan' in r.stdout and 'add: adds a and b' in r.stdout
      and 'api_key=set' in r.stdout, 'zel config: init.lua + loads + zel.lua override + connect.json with $VAR', r.stdout)
check('replaces' in r.stdout or 'Warnings' in r.stdout, 'zel config warns that zel.lua replaces a system', r.stdout)

# 2. Headless: the lua tool, a config function, a file read and lua_after.
r = subprocess.run([BIN, '-p', 'lua-check'], env=env, cwd=cwd, capture_output=True, text=True, timeout=60)
(OUT / 'headless.txt').write_text(r.stdout + r.stderr)
check(r.returncode == 0 and '5\nfile text from a.txt' in r.stdout and 'AFTER-HOOK ok=true' in r.stdout, 'zel -p runs Lua with config functions and hooks', r.stdout + r.stderr)
check(requests[-1]['tools'][0]['name'] == 'lua' and 'add{...}' in requests[-1]['tools'][0]['description']
      and 'choice(' not in requests[-1]['tools'][0]['description'], 'headless tool text lists functions, no choice()', requests[-1]['tools'][0])
check('GLOBAL-RULE mode=headless' in json.dumps(requests[-1]), 'systems are built per session mode')

t = Term(home, cwd)
t.wait('Models', 'startup opens the model picker')
t.send('\x1b', .4); t.send('\x1b', .4); t.send('\x1b', .4)
t.send('lua-check\r')
t.wait('Tool said:', 'TUI: the lua call runs and the answer streams')
check('1 call' in t.view(), 'turn summary counts calls', t.view())
t.shot('01-lua-call')

# 3. lua_before blocks.
t.send('block-check\r')
t.wait('blocked by a hook: no rm -rf', 'lua_before hook blocks the call')

# 4. #prompt: the chat shows #plan, the model gets the block.
t.send('#pl', .8); t.send('\r', .4); t.send('go\r')
t.wait('Ready.', 'prompt message answered')
sent = json.dumps(requests[-1])
check('<prompt name=\\"plan\\">\\nExpanded test plan.\\n</prompt>' in sent, 'the model gets <prompt name="plan">', sent[-400:])
check('#plan go' in t.view() and 'Expanded test plan.' not in t.view(), 'show_prompt = false: the chat shows #plan', t.view())
t.shot('02-prompt-collapsed')

# 5. A task: the notification has its result.
t.send('task-check\r')
t.wait('Notification received: t1 finished: bg-done', 'task.spawn result arrives as a notification', 20)
check('<notification>t1 finished: bg-done</notification>' in json.dumps(requests[-1]), 'the model gets the task notification with its result')
t.shot('03-task-notification')

# 6. An image read attaches it to the next request.
t.send('image-check\r')
t.wait('fixture.png attached', 'read(png) reports the attached image')
check(any(p.get('type') == 'input_image' for item in requests[-1]['input'] for p in (item.get('content') if isinstance(item.get('content'), list) else [])),
      'read(png) attaches the image to the next request')

# 7. choice(): a picker, the answer as a notification.
t.send('choice-check\r')
t.wait('Pick a letter', 'choice() shows the picker')
t.shot('04-choice')
t.send('\r', 1.0)
t.wait('Notification received:', 'the choice answer comes back')
check('Pick a letter' in json.dumps(requests[-1]) and 'A' in requests[-1]['input'][-1]['content'] if isinstance(requests[-1]['input'][-1].get('content'), str) else True, 'answer reaches the model')

# 8. Timeout resets the VM.
t.send('timeout-check\r')
t.wait('timeout after 3s', 'timeout call returns', 20)
check('[timeout after 3s; VM reset]' in json.dumps(requests[-1]), 'the call times out and the VM is reset', [x for x in requests[-1]['input'] if x.get('type') == 'function_call_output'][-2:])
t.send('after-timeout\r')
t.wait('fresh VM', 'next call reports a fresh VM')
check('[lua: fresh VM]\\nnil' in json.dumps(requests[-1]), 'globals are gone after the reset')

# 9. edit() with several edits and grep().
t.send('edit-check\r')
t.wait('uno', 'edit and grep call')
check((cwd / 'notes.txt').read_text() == 'uno\ndos\n' and '\\n1\\n' in json.dumps(requests[-1]['input'][-1]['output']), 'edit() applies all edits, grep() finds the line', requests[-1]['input'][-1])

# 10. The async idle hook ran.
time.sleep(1)
check(idle_file.exists() and idle_file.read_text() == 'idle hook ran\n', 'async idle hook runs in its own VM with zel.* upvalue')

# 11. Tracing: Lua rows, the Lua view, highlighted code.
t.send('\x14', .8)
check('Tracing · All' in t.view() and 'lua ' in t.view(), 'Tracing lists lua entries', t.view())
t.send('\t', .5)
check('Tracing · Lua' in t.view(), 'Tab: the Lua view', t.view())
t.shot('05-tracing-lua')
t.send('\x1b[A', .3); t.send('\r', .6)
check('Tracing / lua' in t.view() and '── output' in t.view(), 'an entry shows code and output', t.view())
t.shot('06-tracing-entry')
to_chat(t)

# 12. Hot reload: show_prompt = true in zel.lua shows the full prompt text.
(cwd / 'zel.lua').write_text('return { systems = { style = "LOCAL-STYLE" }, settings = { show_prompt = true } }\n')
time.sleep(3)
t.send('#pl', .8); t.send('\r', .4); t.send('full\r')
t.wait('Expanded test plan.', 'hot reload of zel.lua: show_prompt = true shows the full text', 15)
t.shot('07-prompt-full')

# 13. A broken zel.lua keeps the previous config.
(cwd / 'zel.lua').write_text('return { systems = \n')
time.sleep(3)
t.send('\x14', 1.0)
for _ in range(4):  # Tracing keeps the Lua view of step 11; Tab back to All
    if 'Tracing · All' in t.view(): break
    t.send('\t', .5)
t.send('\r', .8)  # the latest entry: the error
t.wait('previous config stays', 'broken zel.lua: Tracing says the previous config stays', 10)
t.shot('08-broken-config')
to_chat(t)
t.send('block-check\r')
time.sleep(1.5)
check(requests[-1]['input'][-1].get('output', '').startswith('[blocked by a hook: no rm -rf]'), 'the previous config (its hooks) still works', requests[-1]['input'][-1])
(cwd / 'zel.lua').write_text('return {}\n')
time.sleep(2)

# 14. The model menu writes connect.json.
t.send('/', .8)  # "/" on an empty composer opens the menu
t.send('Models', .6); t.send('\r', 1.2)
t.wait('second-model', 'model menu lists models')
t.send('second', .6); t.send('\r', 1.0)
if 'Effort' in t.view() or 'low' in t.view(): t.send('\r', 1.0)
time.sleep(1)
conn = json.loads((home / 'connect.json').read_text())
check(conn['model'] == 'second-model' and conn['providers']['fixture']['api_key'] == '$FIXTURE_KEY', 'choosing a model writes connect.json and keeps the rest', conn)
t.shot('09-model-selected')
t.close()
(OUT / 'pty-checks.txt').write_text('\n'.join(log) + '\n')
print(f'{sum(l.startswith("PASS") for l in log)} checks passed')
