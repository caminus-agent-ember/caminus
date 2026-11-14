"""The scriptable resident entry point. All execution modes call Runtime.run."""
import argparse
import io
import json
import os
from pathlib import Path
import shlex
import signal
import subprocess
import sys
import time
import uuid

from .runtime import Runtime, EXIT, PAUSED, runtime_lock
from .store import Store, agent_home, atomic_json
from .terminal import alive
from .mail import Mail


def parser():
    ap=argparse.ArgumentParser(prog='caminus env agent')
    sub=ap.add_subparsers(dest='op',required=True)
    for op in ('init','run','once','send','status','events','resume','cancel','unit','start','stop','peers','message','mail'):
        p=sub.add_parser(op)
        p.add_argument('name')
        p.add_argument('--home',help='agent-side home; use an isolated directory for tests')
        p.add_argument('--json',action='store_true')
        if op=='init':
            p.add_argument('--model',required=True)
            p.add_argument('--endpoint',default='http://127.0.0.1:8092/v1')
            p.add_argument('--bridge',default=os.environ.get('CAMINUS_ENV_BRIDGE','/srv/caminus-env-bridge'))
            p.add_argument('--context',type=int)
            p.add_argument('--max-tokens',type=int,default=4096)
            p.add_argument('--api-key-env',default='CAMINUS_AGENT_API_KEY')
            p.add_argument('--bus',help='shared agent-side SQLite mail bus; defaults under ~/.local/share/caminus/agents')
        if op=='message':
            p.add_argument('to')
            p.add_argument('--text')
            p.add_argument('--stdin',action='store_true')
            p.add_argument('--recipient-goal')
            p.add_argument('--reply-to')
        if op in ('once','send'):
            p.add_argument('text',nargs='?')
            p.add_argument('--goal')
            p.add_argument('--stdin',action='store_true',help='read goal text from stdin')
            p.add_argument('--goal-id',help='resume an existing goal, or send it a steering message')
        if op in ('once','resume'):
            p.add_argument('--max-steps',type=int,help='optional test budget; production daemon has no step limit')
            p.add_argument('--max-seconds',type=float,help='optional test budget, retained in the goal checkpoint')
        if op=='once':
            p.add_argument('--jsonl',action='store_true',help='stream events followed by a result record')
        if op in ('events','mail'):
            p.add_argument('--after',type=int,default=0)
        if op in ('resume','cancel'):
            p.add_argument('goal_id')
    return ap


def summary(store,gid):
    g=store.goal(gid)
    return {**{k:g[k] for k in ('id','state','summary','error','steps')},
            'pending':[dict(id=a['id'],name=a['name'],request_id=a['bridge_id']) for a in store.actions(gid)]}


def unit_text(name,home,program=None):
    program=program or str(Path(sys.argv[0]).resolve())
    cmd=shlex.join([sys.executable,'-I',program,'run',name,'--home',str(home)]).replace('%','%%')
    return (f'[Unit]\nDescription=Caminus resident agent {name}\n\n[Service]\nType=simple\n'
            f'WorkingDirectory={str(home).replace("%","%%")}\nExecStart={cmd}\n'
            'Restart=on-failure\nRestartSec=3\nKillMode=process\nTimeoutStopSec=15\nUMask=0007\n'
            f'EnvironmentFile=-%h/.config/caminus/agents/{name}.env\n\n[Install]\nWantedBy=default.target\n')


def emit(value,json_mode=False):
    if json_mode:
        print(json.dumps(value,ensure_ascii=False),flush=True)
    elif isinstance(value,dict):
        if 'state' in value:
            print(f"{value.get('id','')}: {value['state']} {value.get('summary') or value.get('error') or ''}",flush=True)
            for p in value.get('pending',[]):
                if p.get('request_id'):print('request '+p['request_id']+' ('+p['name']+')')
        else:
            print(json.dumps(value,ensure_ascii=False,indent=2))
    else:
        print(value)


def main(argv=None,stdin=None):
    argv=list(sys.argv[1:] if argv is None else argv)
    if argv[:1]==['request']:
        payload=json.load(stdin or sys.stdin)
        return main(payload['args'],stdin=io.StringIO(payload.get('stdin','')))
    a=parser().parse_args(argv)
    try:
        if getattr(a,'max_steps',None) is not None and a.max_steps<1 or getattr(a,'max_seconds',None) is not None and a.max_seconds<=0:
            raise ValueError('test budgets must be positive')
        home=agent_home(a.name,a.home)
        if a.op=='init':
            if a.max_tokens<1 or a.context is not None and a.context<1024:
                raise ValueError('invalid model context or output budget')
            home.mkdir(parents=True,exist_ok=True,mode=0o700)
            cfg=dict(name=a.name,model=a.model,endpoint=a.endpoint.rstrip('/'),bridge=a.bridge,
                     context=a.context,max_tokens=a.max_tokens,api_key_env=a.api_key_env,bus=a.bus)
            if not cfg['endpoint'].endswith('/v1'):cfg['endpoint']+='/v1'
            from .model import Model
            Model(cfg)                     # validate before persisting
            mail=Mail(cfg)
            try:mail.register(home)
            finally:mail.close()
            atomic_json(home/'config.json',cfg)
            for name,text in (('MEMORY.md','# Persistent memory\n\nKeep durable facts and links to your work here.\n'),
                              ('IDENTITY.md',f'# {a.name}\n\nYou are the resident agent whose home is this directory.\n')):
                p=home/name
                if not p.exists():p.write_text(text)
            store=Store(home);store.close()
            emit(dict(name=a.name,home=str(home),configured=True),a.json)
            return 0
        if not (home/'config.json').is_file():
            raise ValueError('agent not initialized; run agent init first')
        if a.op in ('peers','message','mail'):
            mail=Mail(json.loads((home/'config.json').read_text()))
            try:
                if a.op=='peers':value=mail.peers()
                elif a.op=='mail':value=mail.messages(a.after)
                else:
                    text=(stdin or sys.stdin).read() if a.stdin else a.text
                    value=mail.send(a.to,text,recipient_goal=a.recipient_goal,reply_to=a.reply_to)
                emit(value,True)
            finally:mail.close()
            return 0
        if a.op in ('unit','start','stop'):
            unit='caminus-agent-'+a.name+'.service'
            if a.op=='unit':
                print(unit_text(a.name,home),end='');return 0
            env=dict(os.environ,XDG_RUNTIME_DIR='/run/user/'+str(os.getuid()))
            if a.op=='start':
                d=Path.home()/'.config/systemd/user';d.mkdir(parents=True,exist_ok=True)
                (d/unit).write_text(unit_text(a.name,home))
                subprocess.run(['systemctl','--user','daemon-reload'],env=env,check=True)
                subprocess.run(['systemctl','--user','enable','--now',unit],env=env,check=True)
            else:
                subprocess.run(['systemctl','--user','stop',unit],env=env,check=True)
            emit(dict(name=a.name,unit=unit,started=a.op=='start'),a.json)
            return 0
        store=Store(home)
        if a.op=='status':
            heartbeat=json.loads((home/'heartbeat.json').read_text()) if (home/'heartbeat.json').exists() else {}
            emit(dict(name=a.name,home=str(home),daemon_alive=alive(heartbeat.get('pid')),heartbeat=heartbeat,**store.status()),True)
            store.close();return 0
        if a.op=='events':
            emit(store.events(a.after),True);store.close();return 0
        if a.op in ('send','once'):
            text=(stdin or sys.stdin).read() if a.stdin else a.goal if a.goal is not None else a.text
            if a.op=='send' and a.goal_id:
                store.goal(a.goal_id)
                if not text:raise ValueError('steering text is required')
                store.db.execute('INSERT INTO inbox(goal,text) VALUES(?,?)',(a.goal_id,text))
                store.event('user_message',dict(text=text),a.goal_id)
                emit(dict(id=a.goal_id,queued=True),a.json);store.close();return 0
            if a.goal_id:
                gid=a.goal_id
                g=store.goal(gid)
                if g['state'] in ('completed','cancelled'):
                    raise ValueError('submit a new goal instead of replaying a finished one')
                store.update(gid,state='working',step_limit=g['steps']+a.max_steps if a.max_steps else None,
                             deadline=time.time()+a.max_seconds if a.max_seconds else None,error='')
            else:
                gid=store.enqueue(text or '',getattr(a,'max_steps',None),getattr(a,'max_seconds',None))
            if a.op=='send':
                emit(dict(id=gid,state='queued'),a.json);store.close();return 0
        elif a.op=='resume':
            gid=a.goal_id;g=store.goal(gid)
            if g['state'] in ('completed','cancelled'):
                raise ValueError('submit a new goal instead of replaying a finished one')
            store.update(gid,state='working',step_limit=g['steps']+a.max_steps if a.max_steps else None,
                         deadline=time.time()+a.max_seconds if a.max_seconds else None,error='')
            emit(summary(store,gid),a.json);store.close();return 0
        elif a.op=='cancel':
            gid=a.goal_id
            with store.transaction():
                store.goal(gid);store.update(gid,state='cancelled')
                for act in store.db.execute("SELECT id,args FROM actions WHERE goal=? AND name='terminal'",(gid,)):
                    try:args=json.loads(act['args'])
                    except ValueError:continue
                    if not isinstance(args,dict):continue
                    session=args.get('session') or act['id']
                    if not isinstance(session,str) or len(session)!=32 or any(c not in '0123456789abcdef' for c in session):continue
                    job=home/'jobs'/session
                    if job.is_dir():atomic_json(job/'control'/(uuid.uuid4().hex+'.json'),dict(signal='TERM'))
                store.event('cancelled',{},gid)
            emit(summary(store,gid),a.json);store.close();return 0
        else:
            gid=None
        store.close()
        stop=[False]
        last=0
        def observe(store):
            nonlocal last
            if a.op!='once' or not a.jsonl:return
            while True:
                events=store.events(last)
                for e in events:
                    last=e['seq']
                    if e['goal']==gid:print(json.dumps(dict(type='event',**e)),flush=True)
                if len(events)<200:break
        if a.op=='run':
            for sig in (signal.SIGINT,signal.SIGTERM):signal.signal(sig,lambda *args:stop.__setitem__(0,True))
        try:
            with runtime_lock(home):
                runtime=Runtime(home)
                try:
                    if a.op=='run':runtime.run(stop=lambda:stop[0])
                    else:runtime.run(target=gid,observe=observe)
                finally:runtime.mail.close();runtime.store.close()
        except BlockingIOError:
            if a.op=='run':raise ValueError('this agent runtime is already running')
            # A daemon already owns the loop. Oneshot submits and observes that same loop.
            watch=Store(home)
            try:
                while watch.goal(gid)['state'] not in PAUSED|{'waiting_model'}:
                    observe(watch)
                    time.sleep(.1)
                observe(watch)
            finally:watch.close()
        if a.op=='run':return 0
        store=Store(home);result=summary(store,gid);store.close()
        emit(dict(type='result',**result) if a.jsonl else result,a.json or a.jsonl)
        return EXIT.get(result['state'],1)
    except (OSError,ValueError,KeyError,subprocess.SubprocessError) as e:
        if getattr(a,'json',False):emit(dict(error=str(e)),True)
        else:print('caminus env agent: '+str(e),file=sys.stderr)
        return 1


if __name__=='__main__':
    raise SystemExit(main())
