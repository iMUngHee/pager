"""Native Codex loop smoke test using an isolated home and a local Responses fixture.

Requires a Codex host with experimental queue APIs, namespaced MCP calls and
--dangerously-bypass-hook-trust (tested on 0.165.0-alpha.1.2). No cloud model
credentials or real conversations are used. Retains the temporary state for inspection.
"""
import argparse
import shlex
import shutil
import sys
import hashlib
import re
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import os
from pathlib import Path
import queue
import sqlite3
import subprocess
import tempfile
import threading
import time

parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument('--pager', default=shutil.which('pager'), help='candidate pager executable')
parser.add_argument('--codex', default=shutil.which('codex'), help='compatible native Codex executable')
args = parser.parse_args()
if not args.pager or not args.codex:
    parser.error('provide --pager and --codex executable paths')
binary, codex = Path(args.pager).resolve(), Path(args.codex).resolve()
scratch = Path(tempfile.mkdtemp(prefix='pager-real-loop-'))
print('TEST_HOME', scratch, flush=True)
home = scratch / "codex home"
home.mkdir()
env = dict(os.environ, CODEX_HOME=str(home), PAGER_DB=str(scratch / "pager.db"), PAGER_WAKE="on")
env.pop("PAGER_SESSION", None)
if os.name == 'nt':
    # Windows command hooks need PowerShell to invoke paths containing spaces.
    hook_command = 'powershell.exe -NoProfile -Command ' + chr(34) + "& '" + binary.as_posix().replace("'", "''") + "' hook UserPromptSubmit" + chr(34)
else:
    hook_command = shlex.join([str(binary), 'hook', 'UserPromptSubmit'])
# Hooks are registered by the host at startup, rather than per-thread overrides.
(home / "config.toml").write_text("[hooks]\n[[hooks.UserPromptSubmit]]\n[[hooks.UserPromptSubmit.hooks]]\ntype = 'command'\ncommand = " + json.dumps(hook_command) + "\ncommand_windows = " + json.dumps(hook_command) + "\ntimeout = 10\n", encoding="utf-8")
request_count = 0
mode = 'setup'
turn_plans = {}
actor_aliases = {}
revision = 0
last_outputs = []
accepted_ids = set()
pause_ready = threading.Event()
critical_gate = threading.Event()
pause_injected = False
paused_actor = None
paused_turn = None
tool_names = []
artifact = scratch / 'shared-plan.txt'
resource = scratch / 'gpu-lease.txt'
resource.write_text('held',encoding='utf-8')
model_payloads = []


class Model(BaseHTTPRequestHandler):
    def log_message(self, *args): pass
    def do_POST(self):
        global request_count,revision,pause_injected,paused_actor,paused_turn,tool_names
        payload=json.loads(self.rfile.read(int(self.headers.get('Content-Length','0'))))
        model_payloads.append(json.dumps(payload));request_count+=1;number=request_count
        tool_names=[x.get('name','') for x in payload.get('tools',[])]
        meta=payload.get('client_metadata',{})
        thread=meta.get('thread_id');turn=meta.get('turn_id')
        latest=payload.get('input',[])[-1] if payload.get('input') else {}
        output=latest.get('output','') if latest.get('type')=='function_call_output' else ''
        if output:
            last_outputs.append(str(output))
            accepted_ids.update(int(x) for x in re.findall(r'sent #(\d+) to',str(output)))
            if '"isError":true' in str(output).replace(' ',''):
                print('TOOL ERROR',str(output)[:700],flush=True)
        if turn not in turn_plans:
            if mode=='normal' and revision<46:
                revision+=1
                turn_plans[turn]=[('work',revision),('send-normal',None)]
            elif mode=='idle':turn_plans[turn]=[('send-idle',None)]
            else:turn_plans[turn]=[]
        plan=turn_plans[turn]
        if mode=='idle' and 'automatic wake paused' in str(output) and not pause_injected:
            pause_injected=True;paused_actor=thread;paused_turn=turn
            plan[:]=[('send-idle',None)]*8+[('barrier',None),('release-work',None),('release',None),('release',None)]
        if plan and plan[0][0]=='barrier':
            plan.pop(0);pause_ready.set();critical_gate.wait(60)
        action,value=plan.pop(0) if plan else ('finish',None)
        self.send_response(200);self.send_header('Content-Type','text/event-stream');self.send_header('Connection','close');self.end_headers()
        def emit(x):self.wfile.write(('data: '+json.dumps(x)+'\n\n').encode());self.wfile.flush()
        emit({'type':'response.created','response':{'id':f'resp-{number}','status':'in_progress'}})
        if action in ('work','release-work'):
            path=artifact if action=='work' else resource
            content=f'revision {value}' if action=='work' else 'released'
            name='exec_command'
            if os.name == 'nt':
                # Native Windows exec_command uses PowerShell.
                quoted = str(path).replace("'", "''")
                command = f"Set-Content -LiteralPath '{quoted}' -Value '{content}' -Encoding utf8; (Get-FileHash -LiteralPath '{quoted}' -Algorithm SHA256).Hash"
            else:
                code = "from pathlib import Path; import sys,hashlib; p=Path(sys.argv[1]); p.write_text(sys.argv[2],encoding='utf-8'); print(hashlib.sha256(p.read_bytes()).hexdigest())"
                command = shlex.join([sys.executable, '-c', code, str(path), content])
            args={'cmd':command,'workdir':str(scratch),'max_output_tokens':100}
        elif action.startswith('send') or action=='release':
            name='msg_send'
            target='queue-b' if actor_aliases[thread]=='queue-a' else 'queue-a'
            text='review the same file' if action=='send-normal' else ('waiting for progress' if actor_aliases[thread]=='queue-a' else 'acknowledged')
            args={'target':target,'body':text,'task':'normal' if action=='send-normal' else 'idle','activity':'idle'}
            if action=='send-normal':args['progress']='artifact:'+hashlib.sha256(artifact.read_bytes()).hexdigest()
            if action=='release':args.update(body='GPU released after application exit',kind='release',event_id='gpu-lease-1')
        else:name=None
        if name:
            raw=json.dumps(args)
            item={'type':'function_call','id':f'fc-{number}','call_id':f'call-{number}','name':name,'arguments':raw}
            if name=='msg_send':item['namespace']='mcp__pager_test'
            emit({'type':'response.output_item.added','output_index':0,'item':dict(item,arguments='')})
            emit({'type':'response.function_call_arguments.delta','output_index':0,'item_id':item['id'],'delta':raw})
        else:item={'type':'message','id':f'msg-{number}','role':'assistant','content':[{'type':'output_text','text':'OK','annotations':[]}]}
        emit({'type':'response.output_item.done','output_index':0,'item':item})
        emit({'type':'response.completed','response':{'id':f'resp-{number}','status':'completed','output':[item],'usage':{'input_tokens':10,'output_tokens':1,'total_tokens':11}}})


model = ThreadingHTTPServer(("127.0.0.1", 0), Model)
threading.Thread(target=model.serve_forever, daemon=True).start()
stderr = (scratch / "app-server.stderr.log").open("w", encoding="utf-8")
# Only this isolated invocation trusts the known local pager hook. No persisted
# trust or real user's client configuration is changed.
process = subprocess.Popen([str(codex), "--dangerously-bypass-hook-trust", "app-server"],
                           env=env, stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                           stderr=stderr, text=True, encoding="utf-8")
responses = queue.Queue()
notifications = []


def read_responses():
    for line in process.stdout:
        try:
            responses.put(json.loads(line))
        except ValueError:
            pass


threading.Thread(target=read_responses, daemon=True).start()
request_id = 0


def call(method, params, allow_error=False):
    global request_id
    request_id += 1
    process.stdin.write(json.dumps({"jsonrpc": "2.0", "id": request_id, "method": method, "params": params}) + "\n")
    process.stdin.flush()
    while True:
        try:
            result = responses.get(timeout=25)
        except queue.Empty:
            raise AssertionError((method, process.poll(), (scratch / "app-server.stderr.log").read_text(encoding="utf-8")[-3000:]))
        if result.get("id") == request_id:
            if allow_error and "error" in result:
                return result
            if "error" in result:
                print("DIAGNOSTIC model requests:", request_count, flush=True)
                print("DIAGNOSTIC events:", json.dumps([e for e in notifications[-30:] if e.get("method", "").startswith(("turn/", "thread/queue"))]), flush=True)
                print("DIAGNOSTIC state:", scratch, flush=True)
                raise AssertionError(result)
            return result["result"]
        notifications.append(result)


def completed(thread_id, turn_id):
    deadline = time.monotonic() + 25
    while time.monotonic() < deadline:
        for event in notifications:
            turn = event.get("params", {}).get("turn", {})
            if event.get("method") == "turn/completed" and turn.get("id") == turn_id and event.get("params", {}).get("threadId") == thread_id:
                return turn
        try:
            notifications.append(responses.get(timeout=0.2))
        except queue.Empty:
            pass
    raise AssertionError(("turn completion timeout", notifications[-5:]))


def pager(*args):
    result = subprocess.run([str(binary), *args], env=env, text=True, encoding="utf-8", capture_output=True, timeout=15)
    assert result.returncode == 0, result.stderr
    return result.stdout


def queued(thread):
    return call("thread/queue/list", {"threadId": thread})["data"]


def native_idle():
    active=set()
    for event in notifications:
        params=event.get('params',{});turn=params.get('turn',{})
        if params.get('threadId') not in actor_aliases:continue
        if event.get('method')=='turn/started':active.add(turn.get('id'))
        elif event.get('method')=='turn/completed':active.discard(turn.get('id'))
    return not active

def drive_until(predicate,timeout=120):
    deadline=time.monotonic()+timeout
    while time.monotonic()<deadline:
        # The host may start a submission automatically after another turn.
        # Starting the same ID is atomic; only these explicit race errors are benign.
        for thread in actor_aliases:
            for entry in queued(thread):
                result=call('thread/queue/start',{'threadId':thread,'queuedSubmissionId':entry['id']},allow_error=True)
                if 'error' in result:
                    message=result['error']['message']
                    assert result['error']['code']==-32600 and any(x in message for x in ('active or pending turn','queued submission not found')),result
        if predicate():return
        time.sleep(0.03)
    raise AssertionError(('native drain timeout',scratch,last_outputs[-2:],notifications[-2:]))

def normal_finished():
    with sqlite3.connect(env['PAGER_DB']) as db:
        total,waiting=db.execute("SELECT count(*),sum(delivered_at IS NULL) FROM messages WHERE task='normal'").fetchone()
    return total==47 and waiting==0 and native_idle() and all(not queued(t) for t in actor_aliases)

def release_finished():
    with sqlite3.connect(env['PAGER_DB']) as db:
        row=db.execute("SELECT delivered_at IS NOT NULL FROM messages WHERE event_id='gpu-lease-1'").fetchone()
    return row and row[0] and native_idle() and all(not queued(t) for t in actor_aliases)

try:
    call('initialize',{'clientInfo':{'name':'pager_loop_live','version':'1'},'capabilities':{'experimentalApi':True}})
    hook=call('hooks/list',{'cwds':[str(scratch)]})['data'][0]['hooks'][0]
    call('config/value/write',{'keyPath':'hooks.state.'+json.dumps(hook['key'])+'.trusted_hash','value':hook['currentHash'],'mergeStrategy':'upsert'})
    for alias in ('queue-a','queue-b'):
        started=call('thread/start',{'cwd':str(scratch),'ephemeral':False,'approvalPolicy':'never','sandbox':'danger-full-access',
          'config':{'model_provider':'fixture','model':'fixture','model_providers.fixture':{'name':'fixture','base_url':f'http://127.0.0.1:{model.server_port}','wire_api':'responses','request_max_retries':0,'stream_max_retries':0},
          'mcp_servers.pager_test.command':str(binary),'mcp_servers.pager_test.args':['mcp'],
          'mcp_servers.pager_test.env':{'CODEX_HOME':str(home),'PAGER_DB':env['PAGER_DB'],'PAGER_WAKE':'on','PAGER_LOOP_IDLE':'2s','PAGER_LOOP_SIGNAL_TTL':'30s'}}})
        identity=started['thread']['id'];actor_aliases[identity]=alias
        pager('attach','--session',identity,'--tool','codex','--root',str(scratch));pager('alias',alias,'--session',identity)
        turn=call('turn/start',{'threadId':identity,'input':[{'type':'text','text':'Execute the deterministic pager fixture actions.'}]})['turn']
        assert completed(identity,turn['id'])['status']=='completed'
    a,b=actor_aliases
    print('PASS: two persisted native Codex actors and trusted hook initialized',flush=True)
    print('MODEL PAGER TOOLS:',[x for x in tool_names if 'pager' in x],flush=True)
    print('MODEL EXEC TOOLS:',[x for x in tool_names if 'exec' in x],flush=True)
    mode='normal'
    seed=pager('send','queue-b','review the same file','--session',a,'--task','normal','--activity','working')
    accepted_ids.update(int(x) for x in re.findall(r'sent #(\d+) to',seed))
    drive_until(normal_finished)
    mode='listen'
    with sqlite3.connect(env['PAGER_DB']) as db:
        count,maxhop=db.execute("SELECT count(*),max(hop) FROM messages WHERE task='normal'").fetchone()
        pauses=db.execute("SELECT count(*) FROM messages WHERE task='normal' AND wake_note!=''").fetchone()[0]
    assert count==47 and maxhop>=46 and pauses==0,(count,maxhop,pauses)
    print(f'PASS: 47 real-host messages, 46 causal hops, same body + actual artifact changes; no wake pause',flush=True)
    mode='idle'
    seed=pager('send','queue-b','waiting for progress','--session',a,'--task','idle','--activity','idle')
    accepted_ids.update(int(x) for x in re.findall(r'sent #(\d+) to',seed))
    drive_until(lambda:pause_ready.is_set(),timeout=30)
    current=paused_actor;turn=paused_turn
    with sqlite3.connect(env['PAGER_DB']) as db:
        held=db.execute("SELECT count(*) FROM messages WHERE task='idle' AND wake_note!='' AND delivered_at IS NULL AND listed_at IS NULL").fetchone()[0]
        epoch_before=db.execute('SELECT sum(causal_epoch) FROM sessions').fetchone()[0]
    target=a if current==b else b
    assert held==9 and not queued(target),(held,queued(target))
    print('PASS: actual idle ping-pong stops waking; 9 accepted repeated updates remain readable',flush=True)
    before_reads={json.loads(x).get('client_metadata',{}).get('turn_id') for x in model_payloads if json.loads(x).get('client_metadata',{}).get('thread_id')==target}
    mode='listen';critical_gate.set()
    drive_until(release_finished)
    assert resource.read_text(encoding='utf-8-sig').strip()=='released'
    after_reads={json.loads(x).get('client_metadata',{}).get('turn_id') for x in model_payloads if json.loads(x).get('client_metadata',{}).get('thread_id')==target}
    assert len(after_reads-before_reads)==1,(before_reads,after_reads)
    with sqlite3.connect(env['PAGER_DB']) as db:
        critical_count=db.execute("SELECT count(*) FROM messages WHERE event_id='gpu-lease-1'").fetchone()[0]
        assert critical_count==1
        assert db.execute('SELECT sum(causal_epoch) FROM sessions').fetchone()[0]==epoch_before
    assert any('GPU released after application exit' in x and 'event: release' in x for x in model_payloads if json.loads(x).get('client_metadata',{}).get('thread_id')==target)
    with sqlite3.connect(env['PAGER_DB']) as db:
        assert db.execute("SELECT delivered_at IS NOT NULL FROM messages WHERE event_id='gpu-lease-1'").fetchone()[0]==1
        total=db.execute('SELECT count(*) FROM messages').fetchone()[0]
    assert total==len(accepted_ids),(total,len(accepted_ids))
    retry=call('mcpServer/tool/call',{'threadId':current,'server':'pager_test','tool':'msg_send','arguments':{'target':actor_aliases[target],'body':'GPU released after application exit','task':'idle','activity':'idle','kind':'release','event_id':'gpu-lease-1'}})
    assert not retry.get('isError') and 'already recorded as read' in json.dumps(retry),retry
    assert not queued(target)
    print('PASS: retry after the important event was read produces no new notification',flush=True)
    print('PASS: genuine native tool-driven release bypasses pause without a human-epoch reset; duplicate event yields one record/wake',flush=True)
    print('PASS: important release reaches the next model request ahead of paused backlog',flush=True)
    print(f'PASS: all {total} accepted mail records preserved',flush=True)
    print('TEST_HOME',scratch,flush=True)
finally:
    critical_gate.set()
    if process.poll() is None:
        for thread in actor_aliases:
            try:
                call('thread/unsubscribe', {'threadId':thread}, allow_error=True)
            except (AssertionError, BrokenPipeError):
                break
        process.kill()
    process.wait();stderr.close();model.shutdown()
