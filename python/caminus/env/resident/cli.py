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
from . import approval
from .discovery import TOPICS, discover
from .prompt import render as render_prompt


def parser():
    ap=argparse.ArgumentParser(prog='caminus env agent')
    sub=ap.add_subparsers(dest='op',required=True)
    for op in ('init','create','memory','run','once','send','status','events','stream','resume','cancel','compact','unit','start','stop','peers','message','mail','discover','prompt'):
        p=sub.add_parser(op)
        p.add_argument('name')
        p.add_argument('--home',help='agent-side home; use an isolated directory for tests')
        p.add_argument('--json',action='store_true')
        if op=='create':
            p.add_argument('--from',dest='template',required=True,help='copy model settings from a sibling resident; memory/history start fresh')
        if op=='memory':
            p.add_argument('--text',help='append a note to this resident\'s MEMORY.md')
            p.add_argument('--stdin',action='store_true')
        if op=='discover':
            p.add_argument('--topic',choices=TOPICS,default='overview')
            p.add_argument('--query',default='')
        if op=='init':
            p.add_argument('--model',required=True)
            p.add_argument('--endpoint',default='http://127.0.0.1:8092/v1')
            p.add_argument('--bridge',default=os.environ.get('CAMINUS_ENV_BRIDGE','/srv/caminus-env-bridge'))
            p.add_argument('--context',type=int)
            p.add_argument('--max-tokens',type=int,default=4096)
            p.add_argument('--compact-threshold',type=float,default=.85)
            p.add_argument('--compact-target-ratio',type=float,default=.2)
            p.add_argument('--compaction-model',help='optional model alias for summaries on the same endpoint')
            p.add_argument('--api-key-env',default='CAMINUS_AGENT_API_KEY')
            p.add_argument('--bus',help='shared agent-side SQLite mail bus; defaults under ~/.local/share/caminus/agents')
            p.add_argument('--idle-timeout',type=float,default=0,
                           help='seconds without progress before the watchdog pauses the goal (0: off)')
            p.add_argument('--token-budget',type=int,default=0,
                           help='prompt tokens a goal may reach before the watchdog pauses it (0: off)')
            p.add_argument('--no-loop-gates',action='store_true',
                           help='record loop_gates false: a fixture agent that predates the done check and skips it')
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
        if op=='send':
            p.add_argument('--continue',dest='continue_goal',action='store_true',help='allow an explicit follow-up to a completed goal, preserving its conversation')
            p.add_argument('--steer',action='store_true',
                           help='redirect the goal: the message supersedes conflicting instructions and voids the '
                                'plan. Without it a message is a nudge, which keeps the plan.')
        if op in ('once','resume'):
            p.add_argument('--max-steps',type=int,help='optional test budget; production daemon has no step limit')
            p.add_argument('--max-seconds',type=float,help='optional test budget, retained in the goal checkpoint')
        if op=='once':
            p.add_argument('--jsonl',action='store_true',help='stream events followed by a result record')
        if op in ('events','mail'):
            p.add_argument('--after',type=int,default=0)
        if op=='stream':
            p.add_argument('--interval',type=float,default=.05)
            p.add_argument('--seconds',type=float,help='stop after this long (default: until the reader goes away)')
            p.add_argument('--once',action='store_true',help='print the live snapshots once and exit')
        if op in ('resume','cancel'):
            p.add_argument('goal_id')
        if op=='compact':
            p.add_argument('goal_id',nargs='?',help='goal to compact; defaults to the most recent one')
    return ap


def summary(store,gid):
    g=store.goal(gid)
    return {**{k:g[k] for k in ('id','state','summary','error','steps')},
            'pending':[dict(id=a['id'],name=a['name'],request_id=a['bridge_id']) for a in store.actions(gid)]}


def resume_goal(store,gid,max_steps=None,max_seconds=None):
    # Cancellation/completion and this check-transition must share the same
    # SQLite write lock; a stale read must never resurrect a finished goal.
    with store.transaction():
        g=store.goal(gid)
        if g['state'] in ('completed','cancelled'):
            raise ValueError('submit a new goal instead of replaying a finished one')
        store.update(gid,state='working',step_limit=g['steps']+max_steps if max_steps else None,
                     deadline=time.time()+max_seconds if max_seconds else None,error='')


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
        if a.op=='create':
            agent_home(a.template)  # validate the sibling name before joining it
            source=home.parent/a.template
            cfg=json.loads((source/'config.json').read_text())
            cfg['name']=a.name
            from .model import Model
            Model(cfg)
            home.mkdir(mode=0o700)  # refuse existing sessions, including partial ones
            atomic_json(home/'config.json',cfg)
            (home/'MEMORY.md').write_text('# Persistent memory\n\nKeep durable facts and links to your work here.\n')
            (home/'IDENTITY.md').write_text(f'# {a.name}\n\nYou are the resident agent whose home is this directory.\n')
            store=Store(home);store.close()
            mail=Mail(cfg)
            try:mail.register(home)
            finally:mail.close()
            emit(dict(name=a.name,home=str(home),configured=True),True)
            return 0
        if a.op=='discover':
            emit(discover(home,a.topic,a.query),True)
            return 0
        if a.op=='prompt':
            value=render_prompt(home)
            emit(dict(prompt=value),True) if a.json else print(value)
            return 0
        if a.op=='init':
            if not 0 < a.compact_target_ratio < a.compact_threshold < 1:
                raise ValueError('compaction ratios must satisfy 0 < target < threshold < 1')
            if a.max_tokens<1 or a.context is not None and a.context<1024:
                raise ValueError('invalid model context or output budget')
            home.mkdir(parents=True,exist_ok=True,mode=0o700)
            cfg=dict(name=a.name,model=a.model,endpoint=a.endpoint.rstrip('/'),bridge=a.bridge,
                     context=a.context,max_tokens=a.max_tokens,api_key_env=a.api_key_env,bus=a.bus,
                     compact_threshold=a.compact_threshold,compact_target_ratio=a.compact_target_ratio,
                     compaction_model=a.compaction_model,loop_gates=not a.no_loop_gates,
                     idle_timeout=a.idle_timeout,token_budget=a.token_budget)
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
        if a.op=='memory':
            p=home/'MEMORY.md'
            text=(stdin or sys.stdin).read() if a.stdin else a.text
            if text is not None:
                with p.open('a') as f:f.write('\n'+text.rstrip()+'\n')
            emit(dict(name=a.name,memory=p.read_text() if p.exists() else ''),True)
            return 0
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
            cfg=json.loads((home/'config.json').read_text())
            # Measured prompt footprint from the last turn's compaction probe; absent when the
            # runtime has never measured this agent, so a UI shows capacity only, no estimate.
            measured=json.loads((home/'context.json').read_text()) if (home/'context.json').exists() else {}
            # Measured decode speed of the last streamed reply; absent until a reply reports usage.
            speed=json.loads((home/'throughput.json').read_text()) if (home/'throughput.json').exists() else {}
            emit(dict(name=a.name,home=str(home),model=cfg['model'],context=cfg.get('context'),
                      context_used=measured.get('tokens') if isinstance(measured,dict) else None,
                      tok_per_s=speed.get('tps') if isinstance(speed,dict) else None,
                      compact_threshold=cfg.get('compact_threshold',.85),compact_target_ratio=cfg.get('compact_target_ratio',.2),
                      daemon_alive=alive(heartbeat.get('pid')),heartbeat=heartbeat,**store.status()),True)
            store.close();return 0
        if a.op=='events':
            emit(store.events(a.after),True);store.close();return 0
        if a.op=='compact':
            # Compaction needs the model and the single writer, so a live runtime performs it on its next
            # pass; with no daemon the CLI takes the lock and does it here.
            gid=store.resolve(a.goal_id) if a.goal_id else store.latest_goal()
            beat=json.loads((home/'heartbeat.json').read_text()) if (home/'heartbeat.json').exists() else {}
            if alive(beat.get('pid')):
                rid=store.request(gid,'compact')
                store.event('compact_requested',dict(request=rid),gid)
                if a.json:emit(dict(goal=gid,queued=True,request=rid),True)
                else:print(f"compaction queued for {gid[:8]}; the runtime performs it on its next pass",flush=True)
                store.close();return 0
            with runtime_lock(home):
                runtime=Runtime(home)
                try:
                    rid=runtime.store.request(gid,'compact')
                    runtime.compact_request(gid)
                    done=runtime.store.db.execute('SELECT state,error FROM requests WHERE id=?',(rid,)).fetchone()
                finally:runtime.mail.close();runtime.store.close()
            if done['state']=='failed':
                if a.json:emit(dict(goal=gid,failed=True,error=done['error']),True)
                else:print(f"{gid[:8]}: not compacted — {done['error']}",flush=True)
                store.close();return 1
            row=store.db.execute("SELECT data FROM events WHERE goal=? AND kind='compacted' ORDER BY seq DESC LIMIT 1",(gid,)).fetchone()
            data=json.loads(row['data']) if row else {}
            if a.json:emit(dict(goal=gid,**data),True)
            else:print(f"{gid[:8]}: compacted {data.get('before')} → {data.get('after')} messages "
                       f"({data.get('before_tokens')} → {data.get('after_tokens')} tokens)",flush=True)
            store.close();return 0
        if a.op=='stream':
            # NDJSON, one line per change of a live snapshot: a UI holds one process instead of polling the
            # agent's home over SSH. The first line of a turn carries the whole text (reset), later lines
            # carry only the suffix; a vanished snapshot is reported once as done.
            live=home/'live'
            seen={}
            deadline=time.time()+a.seconds if a.seconds else None
            store.close()
            while True:
                for p in sorted(live.glob('*.json')) if live.is_dir() else []:
                    try:data=json.loads(p.read_text())
                    except (OSError,ValueError):continue
                    if not isinstance(data,dict):continue
                    gid=str(data.get('goal') or p.stem)
                    prev=seen.get(gid) or {}
                    content=str(data.get('content') or '');reasoning=str(data.get('reasoning') or '')
                    line=dict(goal=gid,turn=str(data.get('turn') or ''),tools=data.get('tools') or [])
                    if line['turn']!=prev.get('turn'):
                        line.update(reset=True,content=content,reasoning=reasoning)
                    else:
                        line.update(content=content[len(str(prev.get('content') or '')):],
                                    reasoning=reasoning[len(str(prev.get('reasoning') or '')):])
                    if line.get('reset') or line['content'] or line['reasoning'] or line['tools']!=prev.get('tools'):
                        emit(line,True)
                    seen[gid]=data
                for gid in list(seen):
                    if not (live/(gid+'.json')).exists():
                        emit(dict(goal=gid,turn=str(seen[gid].get('turn') or ''),done=True),True)
                        seen.pop(gid)
                if a.once or (deadline and time.time()>=deadline):return 0
                time.sleep(max(.02,a.interval))
        if a.op in ('send','once'):
            text=(stdin or sys.stdin).read() if a.stdin else a.goal if a.goal is not None else a.text
            if a.op=='send' and a.goal_id:
                if not text:raise ValueError('steering text is required')
                with store.transaction():
                    state=store.goal(a.goal_id)['state']
                    if state=='cancelled' or state=='completed' and not a.continue_goal:
                        raise ValueError('goal finished; submit a new goal instead of steering it')
                    if state=='completed':
                        store.update(a.goal_id,state='working',summary='',error='',deadline=None,step_limit=None)
                        store.event('goal_continued',{},a.goal_id)
                    store.db.execute('INSERT INTO inbox(goal,text,kind) VALUES(?,?,?)',
                                     (a.goal_id,text,'steer' if a.steer else 'nudge'))
                    store.event('user_message',dict(text=text,kind='steer' if a.steer else 'nudge'),a.goal_id)
                    # A paused goal keeps the text but will not read it until someone resumes: say so
                    # rather than reporting a delivery the agent will never act on.
                    resume_required=state in ('paused','error','waiting_user','waiting_root')
                emit(dict(id=a.goal_id,queued=True,resume_required=resume_required),a.json);store.close();return 0
            if a.goal_id:
                gid=a.goal_id
                resume_goal(store,gid,a.max_steps,a.max_seconds)
            else:
                gid=store.enqueue(text or '',getattr(a,'max_steps',None),getattr(a,'max_seconds',None))
            if a.op=='send':
                emit(dict(id=gid,state='queued'),a.json);store.close();return 0
        elif a.op=='resume':
            gid=a.goal_id
            resume_goal(store,gid,a.max_steps,a.max_seconds)
            emit(summary(store,gid),a.json);store.close();return 0
        elif a.op=='cancel':
            gid=a.goal_id
            in_flight=[]
            with store.transaction():
                store.goal(gid)
                bridge=Path(json.loads((home/'config.json').read_text())['bridge']).expanduser()
                for act in store.db.execute("SELECT id,bridge_id FROM actions WHERE goal=? AND name IN ('ask_root','ask_user') AND state!='done'",(gid,)):
                    rid=act['bridge_id'] or act['id'][:16]
                    if (bridge/'req'/(rid+'.json')).exists():
                        result=approval.withdraw(bridge,rid,gid)
                        if result.get('decision')=='executing':in_flight.append(rid)
                store.update(gid,state='cancelled')
                for act in store.db.execute("SELECT id,args FROM actions WHERE goal=? AND name='terminal'",(gid,)):
                    try:args=json.loads(act['args'])
                    except ValueError:continue
                    if not isinstance(args,dict):continue
                    session=args.get('session') or act['id']
                    if not isinstance(session,str) or len(session)!=32 or any(c not in '0123456789abcdef' for c in session):continue
                    job=home/'jobs'/session
                    if job.is_dir():atomic_json(job/'control'/(uuid.uuid4().hex+'.json'),dict(signal='TERM'))
                store.event('cancelled',{},gid)
            emit({**summary(store,gid),'root_already_claimed':in_flight},a.json);store.close();return 0
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
                ticks=0
                while watch.goal(gid)['state'] not in PAUSED|{'waiting_model'}:
                    observe(watch)
                    ticks+=1
                    if ticks%20==0:
                        beat=json.loads((home/'heartbeat.json').read_text()) if (home/'heartbeat.json').exists() else {}
                        if beat.get('pid') and not alive(beat['pid']):
                            # The daemon that held the loop is gone; report it instead of waiting forever.
                            failed=dict(summary(watch,gid),error='the daemon holding this goal exited; run resume')
                            emit(failed if a.json or a.jsonl else dict(error=failed['error']),a.json or a.jsonl)
                            return EXIT['error']
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
