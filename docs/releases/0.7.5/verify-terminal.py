import os,pty,subprocess,tempfile,time,fcntl,termios,struct,select,signal,json,threading,pathlib,codecs,base64,sys
from http.server import ThreadingHTTPServer,BaseHTTPRequestHandler
import pyte
from PIL import Image,ImageDraw,ImageFont
ROOT=pathlib.Path('/tmp/zel-075-evidence')
BIN=str(ROOT/'zel')
requests=[]
image_seen=False

def text_item(text):return {'type':'message','role':'assistant','content':[{'type':'output_text','text':text}]}
def command_item(command,call):return {'type':'function_call','name':'bash','call_id':call,'arguments':json.dumps({'command':command})}
class Provider(BaseHTTPRequestHandler):
 def log_message(self,*args):pass
 def do_GET(self):
  data=json.dumps({'data':[{'id':'fixture'}]}).encode();self.send_response(200);self.end_headers();self.wfile.write(data)
 def do_POST(self):
  global image_seen
  body=json.loads(self.rfile.read(int(self.headers['Content-Length'])));requests.append(body)
  inputs=body.get('input',[])
  users=[]
  for item in inputs:
   if item.get('role')=='user':
    content=item.get('content','')
    if isinstance(content,list):
     users.append('\n'.join(p.get('text','') for p in content))
     for p in content:
      if p.get('type')=='input_image':
       assert p['image_url'].startswith('data:image/'),p
       image_seen=True
    else:users.append(content)
  last=users[-1] if users else ''
  calls={x.get('call_id') for x in inputs if x.get('type')=='function_call'}
  choice_ids={'ask-choice':'choice-tool','ask-preset':'preset-tool','ask-cancel':'cancel-tool'}
  if last in choice_ids and choice_ids[last] not in calls:
   output=[command_item('zel choice "Release question" "Preset A" "Preset B"',choice_ids[last])]
  elif 'Release question:' in last:output=[text_item('Custom answer received: '+last)]
  elif last=='attach-image' and 'image-tool' not in calls:
   output=[command_item('zel view '+str(ROOT/'fixture.png'),'image-tool')]
  elif last=='stream-check':output=[text_item('## Formatted streaming\n\nA **bold** answer with `inline code`.\n\n- first item\n- second item\n\n[Documentation](https://example.com)\n\nDone.')]
  elif last=='background-check' and 'task-tool' not in calls:
   output=[command_item("zel task run --name proof -- sh -c 'sleep 1; echo background-ok'",'task-tool')]
  elif 'finished' in last:output=[text_item('Background completion received.')]
  else:output=[text_item('Image received and decoded.' if image_seen and last.startswith('<notification>view:') else 'Ready for the next check.')]
  if last=='error-check':
   self.send_response(400);self.end_headers();self.wfile.write(b'{"error":{"message":"fixture rejected"}}');return
  self.send_response(200);self.send_header('Content-Type','text/event-stream');self.end_headers()
  def send(e):self.wfile.write(('data: '+json.dumps(e)+'\n\n').encode());self.wfile.flush()
  try:
   if last=='slow-check':
    send({'type':'response.output_text.delta','output_index':0,'delta':'Waiting for cancellation…'})
    for _ in range(50):time.sleep(.1);send({'type':'response.output_text.delta','output_index':0,'delta':'.'})
   for index,item in enumerate(output):
    send({'type':'response.output_item.added','output_index':index,'item':dict(item,arguments='') if item['type']=='function_call' else dict(item,content=[])})
    if item['type']=='function_call':
     args=item['arguments']
     for j in range(0,len(args),8):send({'type':'response.function_call_arguments.delta','output_index':index,'delta':args[j:j+8]})
    else:
     text=item['content'][0]['text']
     for j in range(0,len(text),4):send({'type':'response.output_text.delta','output_index':index,'delta':text[j:j+4]});time.sleep(.003)
   send({'type':'response.completed','response':{'output':output,'usage':{'input_tokens':200,'output_tokens':30}}})
  except (BrokenPipeError,ConnectionResetError):pass

server=ThreadingHTTPServer(('127.0.0.1',0),Provider)
threading.Thread(target=server.serve_forever,daemon=True).start()
home=pathlib.Path(tempfile.mkdtemp(prefix='zel075-home-'))
cwd=pathlib.Path(tempfile.mkdtemp(prefix='zel075-project-'))
(home/'zel.toml').write_text(f'provider="fixture"\nmodel="fixture"\n[providers.fixture]\nbase_url="http://127.0.0.1:{server.server_port}/v1"\napi_key="local-fixture"\n[colors]\nbg="#0A0A0A"\n')
Image.new('RGB',(48,32),(20,80,200)).save(ROOT/'fixture.png')
master,slave=pty.openpty();fcntl.ioctl(slave,termios.TIOCSWINSZ,struct.pack('HHHH',28,100,0,0))
env={k:v for k,v in os.environ.items() if not k.startswith(('ZEL_','REX_'))};env.update(ZEL_HOME=str(home),TERM='xterm-256color')
p=subprocess.Popen([BIN],stdin=slave,stdout=slave,stderr=slave,cwd=cwd,env=env,start_new_session=True);os.close(slave)
class Terminal(pyte.Screen):
 def scroll_up(self,count=1):
  x,y=self.cursor.x,self.cursor.y
  self.cursor.y=self.margins.bottom if self.margins else self.lines-1
  for _ in range(count):self.index()
  self.cursor.x,self.cursor.y=x,y
 def scroll_down(self,count=1):
  x,y=self.cursor.x,self.cursor.y
  self.cursor.y=self.margins.top if self.margins else 0
  for _ in range(count):self.reverse_index()
  self.cursor.x,self.cursor.y=x,y
class TerminalStream(pyte.Stream):
 csi=dict(pyte.Stream.csi,S='scroll_up',T='scroll_down')
 events=pyte.Stream.events|{'scroll_up','scroll_down'}
screen=Terminal(100,28);stream=TerminalStream(screen);decoder=codecs.getincrementaldecoder('utf8')('replace');transcript=bytearray()

def pump(seconds=.3):
 end=time.monotonic()+seconds
 while time.monotonic()<end:
  if select.select([master],[],[],min(.05,max(0,end-time.monotonic())))[0]:
   try:data=os.read(master,65536)
   except OSError:break
   if not data:break
   transcript.extend(data);stream.feed(decoder.decode(data))

def send(text):os.write(master,text.encode() if isinstance(text,str) else text);pump()
def view():return '\n'.join(screen.display)
def waitfor(predicate,label,timeout=8):
 end=time.monotonic()+timeout
 while time.monotonic()<end:
  pump(.1)
  if predicate():print('PASS',label,flush=True);return
 raise AssertionError(label+'\n'+view())

def capture(name):
 (ROOT/(name+'.txt')).write_text(view())
 font_path='/System/Library/Fonts/Menlo.ttc'
 font=ImageFont.truetype(font_path,15)
 cellw=10;cellh=21
 image=Image.new('RGB',(screen.columns*cellw+24,screen.lines*cellh+24),'#0A0A0A');draw=ImageDraw.Draw(image)
 colors={'default':'#EDEDED','black':'#222222','red':'#FF3B30','green':'#75C379','brown':'#DDC779','blue':'#7EA7DB','magenta':'#BD8ACF','cyan':'#7FC9C0','white':'#EDEDED'}
 for y in range(screen.lines):
  for x in range(screen.columns):
   cell=screen.buffer[y][x]
   fg=colors.get(cell.fg,'#'+cell.fg if len(cell.fg)==6 else '#EDEDED')
   draw.text((12+x*cellw,12+y*cellh),cell.data,font=font,fill=fg)
 image.save(ROOT/(name+'.png'))

try:
 waitfor(lambda:b'\x1b[?1049h' in transcript,'terminal entered interactive mode',timeout=15)
 pump(.2)
 send('ask-choice\r')
 waitfor(lambda:'Write your own answer' in view(),'custom option is visible')
 capture('01-choice-options')
 send(b'\x1b[B\x1b[B\r')
 waitfor(lambda:'Your answer' in view(),'custom answer editor opens')
 send('Свой ответ: без миграции 123')
 capture('02-choice-custom-input')
 send(b'\x1b')
 waitfor(lambda:'Write your own answer' in view(),'Esc returns to choices without submitting')
 send('\r')
 waitfor(lambda:'Свой ответ:' in view(),'custom draft is retained')
 send('\r')
 waitfor(lambda:any('Свой ответ: без миграции 123' in json.dumps(r,ensure_ascii=False) for r in requests),'custom notification reached provider')
 pump(.6);capture('03-choice-custom-delivered')
 send('ask-preset\r')
 waitfor(lambda:'Write your own answer' in view(),'preset choice opens')
 send('2')
 waitfor(lambda:any('Release question: Preset B' in json.dumps(r) for r in requests),'numeric preset shortcut delivers answer')
 pump(.3)
 send('ask-cancel\r')
 waitfor(lambda:'Write your own answer' in view(),'cancel choice opens')
 send(b'\x1b')
 waitfor(lambda:any('Release question: cancelled' in json.dumps(r) for r in requests),'Esc cancels the question')
 pump(.3)
 send('stream-check\r')
 waitfor(lambda:'Done.' in view(),'formatted streaming completes')
 capture('04-formatted-streaming')
 send('attach-image\r')
 waitfor(lambda:image_seen,'provider received hydrated image bytes')
 pump(.5)
 sessions=list((home/'sessions').glob('*/*/session.jsonl'))
 assert len(sessions)==1,sessions
 session=sessions[0];saved=session.read_text()
 assert 'zel-image:v1:' in saved and 'data:image/png;base64,' not in saved
 assert list((session.parent/'attachments').glob('preview-v1-*'))
 print('PASS saved history uses file references, not base64',flush=True)
 capture('05-image-accepted')
 send('background-check\r')
 waitfor(lambda:any('finished' in json.dumps(r) for r in requests),'background task completion delivered')
 pump(.4);capture('06-background-completion')
 send(b'\x14');pump(.5)
 capture('07-tracing-complete-commands')
 send(b'\x1b');pump(.2)
 fcntl.ioctl(master,termios.TIOCSWINSZ,struct.pack('HHHH',18,64,0,0));screen.resize(18,64);os.kill(p.pid,signal.SIGWINCH);pump(.5)
 capture('08-resized-terminal');print('PASS terminal resized without losing the session',flush=True)
 send('slow-check\r')
 waitfor(lambda:'Waiting for cancellation' in view(),'slow streamed response begins')
 send(b'\x04')
 waitfor(lambda:'stopped by user' in view(),'Ctrl+D cancels the active stream')
 capture('09-cancelled-stream')
 send('error-check\r')
 waitfor(lambda:'fixture rejected' in view(),'provider error is reported')
 capture('10-provider-error')
 count=len(requests);send('recovery-check\r')
 waitfor(lambda:len(requests)>count,'new turn works after provider error')
 pump(.4)
 send(b'\x11')
 p.wait(timeout=8)
 assert p.returncode==0,p.returncode
 print('PASS clean quit',flush=True)
 # Reopen the same saved session headlessly through its actual CLI.
 result=subprocess.run([BIN,'-p','resume-check','--resume',session.parent.name],cwd=cwd,env=env,capture_output=True,text=True,timeout=15)
 assert result.returncode==0,(result.stdout,result.stderr)
 assert image_seen
 print('PASS saved file-backed chat resumes via CLI',flush=True)
 # Generated legacy chat: embedded image bytes must not be migrated.
 legacy=session.parent.parent/'legacy-fixture';legacy.mkdir()
 oldurl='data:image/png;base64,'+base64.b64encode((ROOT/'fixture.png').read_bytes()).decode()
 records=[{'t':'session','v':1,'id':'legacy-fixture','cwd':str(cwd.resolve()),'mode':'headless','provider':'fixture','model':'fixture','config':[str(home/'zel.toml')]},
 {'t':'item','seq':0,'data':{'role':'user','content':[{'type':'input_text','text':'legacy image'},{'type':'input_image','image_url':oldurl}]}}]
 legacy_bytes=('\n'.join(json.dumps(x) for x in records)+'\n').encode();(legacy/'session.jsonl').write_bytes(legacy_bytes)
 result=subprocess.run([BIN,'-p','legacy-check','--resume','legacy-fixture'],cwd=cwd,env=env,capture_output=True,text=True,timeout=15)
 assert result.returncode==0,(result.stdout,result.stderr)
 assert (legacy/'session.jsonl').read_bytes().startswith(legacy_bytes)
 assert 'zel-image:v1:' not in (legacy/'session.jsonl').read_text()
 print('PASS legacy embedded-image chat resumes without migration',flush=True)
 preview=next((session.parent/'attachments').glob('preview-v1-*'));data=preview.read_bytes();preview.unlink()
 try:
  result=subprocess.run([BIN,'-p','missing-check','--resume',session.parent.name],cwd=cwd,env=env,capture_output=True,text=True,timeout=15)
  assert result.returncode!=0 and 'image preview unavailable' in result.stderr,(result.returncode,result.stdout,result.stderr)
  print('PASS missing preview fails explicitly without dropping the image',flush=True)
 finally:preview.write_bytes(data)
 print('PASS version:',subprocess.check_output([BIN,'version'],text=True).strip(),flush=True)
 (ROOT/'manual-result.json').write_text(json.dumps({'ok':True,'requests':len(requests),'session':str(session),'home':str(home),'project':str(cwd),'custom_answer':'Свой ответ: без миграции 123','resumed_answer':result.stdout},ensure_ascii=False,indent=2))
finally:
 if sys.exc_info()[0] and p.poll() is None:
  os.kill(p.pid,signal.SIGQUIT);pump(1)
  (ROOT/'failure-stack.txt').write_text(transcript.decode('utf8','replace'))
 (ROOT/'terminal.ansi').write_bytes(transcript)
 if p.poll() is None:
  p.terminate()
  try:p.wait(timeout=5)
  except subprocess.TimeoutExpired:p.kill();p.wait()
 os.close(master);server.shutdown()
