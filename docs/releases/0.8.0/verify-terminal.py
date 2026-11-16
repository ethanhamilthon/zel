"""Real PTY smoke test. Usage: python verify-terminal.py BINARY OUTPUT_DIRECTORY.
Requires pyte and Pillow. Uses only a temporary home and a local fake provider.
PNG files are renderings of the captured virtual terminal, not OS screenshots.
"""
import base64, codecs, fcntl, json, os, pathlib, pty, select, signal, struct
import subprocess, sys, tempfile, termios, threading, time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import pyte
from PIL import Image, ImageDraw, ImageFont

BIN = str(pathlib.Path(sys.argv[1]).resolve())
OUT = pathlib.Path(sys.argv[2]); OUT.mkdir(parents=True, exist_ok=True)
requests = []
image_seen = False

def answer(text):
    return {'type':'message','role':'assistant','content':[{'type':'output_text','text':text}]}
def command(text, ident):
    return {'type':'function_call','name':'bash','call_id':ident,'arguments':json.dumps({'command':text})}

class Provider(BaseHTTPRequestHandler):
    def log_message(self, *args): pass
    def do_GET(self):
        self.send_response(200); self.end_headers()
        self.wfile.write(json.dumps({'data':[{'id':'fixture'},{'id':'second-model'}]}).encode())
    def do_POST(self):
        global image_seen
        body = json.loads(self.rfile.read(int(self.headers['Content-Length']))); requests.append(body)
        users = []
        for item in body.get('input', []):
            if item.get('role') != 'user': continue
            content = item.get('content', '')
            if isinstance(content, list):
                users.append('\n'.join(p.get('text','') for p in content))
                for p in content:
                    if p.get('type') == 'input_image':
                        assert p['image_url'].startswith('data:image/'); image_seen = True
            else: users.append(content)
        last = users[-1] if users else ''
        calls = {x.get('call_id') for x in body.get('input', [])}
        choice_ids = {'ask-choice':'custom','ask-preset':'preset','ask-cancel':'cancel'}
        if last in choice_ids and choice_ids[last] not in calls:
            output = [command(f'{BIN} choice "Release question: detail" "Preset A" "Preset B"', choice_ids[last])]
        elif last == 'background-check' and 'task' not in calls:
            output = [command(f'{BIN} task run --name proof -- sh -c "sleep 1; echo background-ok"', 'task')]
        elif last == 'child-check' and 'child' not in calls:
            output = [command(f'{BIN} -p child-answer', 'child')]
        elif last == 'attach-image' and 'image' not in calls:
            output = [command(f'{BIN} view {OUT}/fixture.png', 'image')]
        elif last == 'stream-check':
            output = [answer('## Formatted streaming\n\nHello — привет 👩🏽‍💻!\n\nA **bold** answer and `code`.\n\n- First\n- Second\n\n[Documentation](https://example.com)\n\nDone.')]
        elif last == 'slow-check': output = [answer('Waiting for cancellation…')]
        elif last.startswith('<notification>'): output = [answer('Notification received: '+last)]
        else: output = [answer('Ready for the next check.')]
        if last == 'error-check':
            self.send_response(400); self.end_headers(); self.wfile.write(b'{"error":{"message":"fixture rejected"}}'); return
        self.send_response(200); self.send_header('Content-Type','text/event-stream'); self.end_headers()
        def emit(data):
            self.wfile.write(('data: '+json.dumps(data)+'\n\n').encode()); self.wfile.flush()
        try:
            if last == 'slow-check':
                emit({'type':'response.output_text.delta','output_index':0,'delta':'Waiting for cancellation…'})
                for _ in range(60): time.sleep(.1); emit({'type':'response.output_text.delta','output_index':0,'delta':'.'})
            for index, item in enumerate(output):
                initial = dict(item, arguments='') if item['type']=='function_call' else dict(item, content=[])
                emit({'type':'response.output_item.added','output_index':index,'item':initial})
                if item['type']=='function_call':
                    emit({'type':'response.function_call_arguments.delta','output_index':index,'delta':item['arguments']})
                else:
                    text = item['content'][0]['text']
                    for i in range(0,len(text),3):
                        emit({'type':'response.output_text.delta','output_index':index,'delta':text[i:i+3]}); time.sleep(.015)
            emit({'type':'response.completed','response':{'output':output,'usage':{'input_tokens':200,'output_tokens':40}}})
        except (BrokenPipeError, ConnectionResetError): pass

server = ThreadingHTTPServer(('127.0.0.1',0), Provider)
threading.Thread(target=server.serve_forever, daemon=True).start()
home = pathlib.Path(tempfile.mkdtemp(prefix='zel080-home-'))
cwd = pathlib.Path(tempfile.mkdtemp(prefix='zel080-project-'))
(home/'zel.toml').write_text(f'''provider="fixture"
model="fixture"
[providers.fixture]
base_url="http://127.0.0.1:{server.server_port}/v1"
api_key="local-test"
[completions]
plan="Expanded test plan."
[colors]
bg="#0A0A0A"
accent="#FF3B30"
''')
(cwd/'my file.go').write_text('sample\n')
Image.new('RGB',(48,32),(20,80,200)).save(OUT/'fixture.png')
env = {k:v for k,v in os.environ.items() if not k.startswith(('ZEL_','REX_'))}
env.update(ZEL_HOME=str(home), TERM='xterm-256color', COLORTERM='truecolor')
master, slave = pty.openpty()
fcntl.ioctl(slave,termios.TIOCSWINSZ,struct.pack('HHHH',28,100,0,0))
def controlling_tty():
    os.setsid()
    fcntl.ioctl(0,termios.TIOCSCTTY,0)
p = subprocess.Popen([BIN], stdin=slave, stdout=slave, stderr=slave, cwd=cwd, env=env, preexec_fn=controlling_tty)
os.close(slave)
screen = pyte.Screen(100,28); stream = pyte.Stream(screen)
decoder = codecs.getincrementaldecoder('utf8')('replace'); transcript = bytearray()

def pump(seconds=.15):
    end = time.monotonic()+seconds
    while time.monotonic()<end:
        if select.select([master],[],[],min(.04,max(0,end-time.monotonic())))[0]:
            try: data = os.read(master,65536)
            except OSError: break
            if not data: break
            transcript.extend(data); stream.feed(decoder.decode(data))
def send(text): os.write(master,text.encode() if isinstance(text,str) else text); pump()
def view():
    # pyte does not understand joined emoji clusters. Read its cells directly
    # instead of its display property, which can fail on a wide-cell suffix.
    return '\n'.join(''.join(screen.buffer[y][x].data or ' ' for x in range(screen.columns)) for y in range(screen.lines))
def wait(predicate, label, timeout=12):
    end = time.monotonic()+timeout
    while time.monotonic()<end:
        pump(.1)
        if predicate(): print('PASS',label,flush=True); return
    raise AssertionError(label+'\n'+view())
def idle():
    pump(.6)
def capture(name):
    (OUT/(name+'.txt')).write_text(view())
    font = ImageFont.truetype('/System/Library/Fonts/Menlo.ttc',15)
    image = Image.new('RGB',(screen.columns*10+24,screen.lines*21+24),'#0A0A0A')
    draw = ImageDraw.Draw(image)
    colors = {'default':'#EDEDED','black':'#222222','red':'#FF3B30','green':'#75C379','brown':'#DDC779','blue':'#7EA7DB','magenta':'#BD8ACF','cyan':'#7FC9C0','white':'#EDEDED'}
    for y in range(screen.lines):
        for x in range(screen.columns):
            cell = screen.buffer[y][x]
            fg = colors.get(cell.fg,'#'+cell.fg if len(cell.fg)==6 else '#EDEDED')
            draw.text((12+x*10,12+y*21),cell.data,font=font,fill=fg)
    image.save(OUT/(name+'.png'))
def string_values(value):
    if isinstance(value,str): yield value
    elif isinstance(value,dict):
        for child in value.values(): yield from string_values(child)
    elif isinstance(value,list):
        for child in value: yield from string_values(child)
def request_contains(text): return any(text in s for r in requests for s in string_values(r))
def close_menu(): send(b'\x1b'); send(b'\x1b'); send(b'\x1b')

try:
    wait(lambda:b'\x1b[?1049h' in transcript,'tcell enters the alternate screen',15)
    send('\x1b[200~Русский 👩🏽‍💻\nsecond line\x1b[201~')
    assert len(requests)==0, 'bracketed paste submitted a message'
    wait(lambda:'second line' in view(),'bracketed multiline paste is editable')
    capture('01-multiline-composer')
    send('\r'); wait(lambda:request_contains('Русский 👩🏽‍💻'),'Unicode message reaches provider'); idle()
    send(b'\x0b'); send('Models'); send('\r')
    wait(lambda:'second-model' in view(),'model list is searchable')
    capture('02-models')
    send('second'); send('\r'); wait(lambda:'medium' in view(),'effort picker opens')
    send('\r'); wait(lambda:'fixture/second-model' in view(),'model selection is applied')
    send('#pl'); wait(lambda:'#plan' in view(),'prompt suggestions open'); send('\t')
    capture('03-completed-token'); send(b'\x1b[D\x1b[D'); send(b'\x1b[3~')
    assert '#plan' not in view(), 'completed token was split instead of deleted'
    send(b'\x15'); send('#pl\t\r')
    wait(lambda:request_contains('Expanded test plan.'),'prompt completion expands before sending'); idle()
    send('@my'); wait(lambda:'my file.go' in view(),'quoted file suggestion opens'); send('\t')
    assert '@"my file.go"' in view(),view()
    print('PASS paths with spaces complete as one token',flush=True)
    send(b'\x15'); send('ask-choice\r')
    wait(lambda:'Write your own answer' in view(),'choice card opens'); capture('04-choice')
    send(b'\x1b[B\x1b[B\r'); wait(lambda:'Your answer' in view(),'custom answer uses the shared editor')
    send('Свой ответ'); send(b'\x0a'); send('second line')
    capture('05-custom-multiline')
    # Click after the first character in the first custom-answer row.
    row = next(i for i,line in enumerate(view().splitlines()) if 'Свой ответ' in line)
    send(f'\x1b[<0;3;{row+1}M\x1b[<0;3;{row+1}m'); send('X')
    assert 'СXвой ответ' in view(),view()
    print('PASS mouse places the custom-answer cursor',flush=True)
    send('\r'); wait(lambda:request_contains('СXвой ответ\nsecond line'),'custom answer notification reaches provider'); idle()
    wait(lambda:'Release question: detail:' in view(),'answered question remains in chat')
    capture('06-answer-record')
    send('ask-preset\r'); wait(lambda:'Write your own answer' in view(),'preset question opens'); send('2')
    wait(lambda:request_contains('Release question: detail: Preset B'),'numeric preset answer works'); idle()
    send('ask-cancel\r'); wait(lambda:'Write your own answer' in view(),'cancel question opens'); send(b'\x1b')
    wait(lambda:request_contains('Release question: detail: cancelled'),'Esc cancels a question'); idle()
    send('stream-check\r'); wait(lambda:'Formatted streaming' in view(),'formatted stream starts')
    capture('07-streaming'); wait(lambda:'Done.' in view(),'formatted stream completes'); idle()
    send('attach-image\r'); wait(lambda:image_seen,'provider receives the decoded image'); idle()
    send('background-check\r'); wait(lambda:'proof' in view(),'running background task is visible')
    capture('08-background-task')
    wait(lambda:request_contains('finished'),'background completion notification arrives'); idle()
    send('child-check\r'); wait(lambda:request_contains('child-answer'),'headless child agent runs'); idle()
    send(b'\x14'); wait(lambda:'Menu / trace' in view(),'tracing opens'); capture('09-tracing')
    send('bash'); send('\r'); wait(lambda:'Tracing / bash' in view(),'trace entry opens'); capture('10-trace-command'); close_menu()
    fcntl.ioctl(master,termios.TIOCSWINSZ,struct.pack('HHHH',18,64,0,0)); screen.resize(18,64); os.kill(p.pid,signal.SIGWINCH); pump(.5)
    capture('11-resized'); print('PASS terminal resize retains the session',flush=True)
    send('slow-check\r'); wait(lambda:'Waiting for cancellation' in view(),'slow stream begins')
    send('queued-draft\r'); wait(lambda:'queued' in view(),'sending during a turn queues the draft')
    send('X'); assert 'queued-draftX' in view(),view()
    print('PASS editing a queued draft withdraws it',flush=True)
    send(b'\x15'); send(b'\x04')
    wait(lambda:'stopped by user' in view(),'Ctrl+D cancels and displays the result'); capture('12-cancelled'); idle()
    send('error-check\r'); wait(lambda:'fixture rejected' in view(),'provider error is visible'); idle()
    count = len(requests); send('recovery-check\r'); wait(lambda:len(requests)>count,'new turn works after an error'); idle()
    send(b'\x0b'); send('Settings'); send('\r')
    wait(lambda:'Bash timeout' in view(),'settings menu opens')
    send('timeout'); send('\r'); wait(lambda:'Bash timeout in seconds' in view(),'setting editor opens')
    send(b'\x01\x0b'); send('17\r')
    wait(lambda:'17 s' in view(),'setting persists through the shared form')
    assert 'bash_timeout = 17' in (home/'zel.toml').read_text()
    close_menu()
    send(b'\x11'); p.wait(timeout=8); assert p.returncode==0,p.returncode
    print('PASS clean quit restores terminal modes',flush=True)
    sessions = list((home/'sessions').glob('*/*/session.jsonl')); assert len(sessions)==2,sessions
    session = next(s for s in sessions if (s.parent/'choices.jsonl').exists())
    assert 'zel-image:v1:' in session.read_text()
    result = subprocess.run([BIN,'-p','resume-check','--resume',session.parent.name],cwd=cwd,env=env,capture_output=True,text=True,timeout=15)
    assert result.returncode==0,(result.stdout,result.stderr)
    assert 'СXвой ответ' in (session.parent/'choices.jsonl').read_text()
    print('PASS saved image and answers survive resume',flush=True)
    (OUT/'result.json').write_text(json.dumps({'ok':True,'requests':len(requests),'version':subprocess.check_output([BIN,'version'],text=True).strip(),'home':str(home),'project':str(cwd)},indent=2))
finally:
    if sys.exc_info()[0]:
        try: capture('failure')
        except Exception as error: print('Capture failed:',error,flush=True)
    (OUT/'terminal.ansi').write_bytes(transcript)
    if p.poll() is None:
        try: send(b'\x11'); pump(.4)
        except OSError: pass
    os.close(master)
    if p.poll() is None:
        p.terminate()
        try: p.wait(timeout=3)
        except subprocess.TimeoutExpired:
            p.kill()
            try: p.wait(timeout=3)
            except subprocess.TimeoutExpired: print('Child did not reap after PTY closed',flush=True)
    server.shutdown()
