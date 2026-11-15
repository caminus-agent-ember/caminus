"""The scriptable resident entry point. All execution modes call Runtime.run."""
import argparse
import io
import json
import os
import urllib.parse
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


def _current(cfg):
    keys=('model','provider','endpoint','api','context','max_tokens','effort','thinking','relay','prices')
    return {k:cfg.get(k) for k in keys if cfg.get(k) is not None}


def model_ops(a,home):
    """provider / models / model / effort: the brains this resident can use, found by asking the providers."""
    import re as _re
    from . import catalog
    cfg=json.loads((home/'config.json').read_text())
    if a.op=='provider':
        provs=dict(cfg.get('providers') or {})
        if a.action!='list':
            if not a.provider or not _re.fullmatch(r'[A-Za-z0-9_-]{1,32}',a.provider):
                raise ValueError('name the provider: letters, digits, - and _')
            if a.action=='add':
                if not a.kind or not a.endpoint:raise ValueError('provider add NAME --kind KIND --endpoint URL')
                provs[a.provider]=dict(kind=a.kind,endpoint=a.endpoint.rstrip('/'),**(dict(relay=True) if a.relay else {}))
            elif provs.pop(a.provider,None) is None:
                raise ValueError('no provider named '+a.provider)
            if provs:cfg['providers']=provs
            else:cfg.pop('providers',None)
            atomic_json(home/'config.json',cfg)
        emit(dict(providers=provs),a.json);return 0
    if a.op=='models':
        cat=catalog.load(home)
        if a.refresh or not cat.get('models'):
            cat=catalog.refresh(home,cfg,only=a.provider)
        rows=[m for m in cat.get('models',[]) if not a.provider or m['provider']==a.provider]
        emit(dict(models=rows,providers=cat.get('providers'),current=_current(cfg)),a.json);return 0
    if a.op=='model':
        if not a.id:
            emit(dict(current=_current(cfg)),a.json);return 0
        cat=catalog.load(home)
        entry=catalog.find(cat,a.id,a.provider)
        if entry is None:
            cat=catalog.refresh(home,cfg)
            entry=catalog.find(cat,a.id,a.provider)
        if entry is None:
            raise ValueError(f'no model {a.id} in the catalog: `models --refresh` lists what the providers serve')
        spec=catalog.providers(cfg).get(entry['provider'])
        if not spec:raise ValueError('the provider of %s is no longer configured' % entry['id'])
        cfg.update(model=entry['id'],provider=entry['provider'],provider_kind=spec['kind'],endpoint=spec['endpoint'],
                   api='anthropic' if spec['kind']=='anthropic' else 'openai')
        for k in ('relay','prices','context','thinking'):
            cfg.pop(k,None)
        if spec.get('relay'):cfg['relay']=True
        if entry.get('prices'):cfg['prices']=entry['prices']
        if entry.get('context'):cfg['context']=int(entry['context'])
        if entry.get('max_output'):
            cfg['max_tokens']=min(int(entry['max_output']),32768)
        if spec['kind']=='anthropic' and entry.get('thinking')=='adaptive':
            cfg['thinking']='adaptive'        # current Opus and Sonnet refuse thinking off; the store keeps none anyway
        levels=entry.get('efforts') or []
        if cfg.get('effort') not in levels:
            cfg.pop('effort',None)
            if 'medium' in levels:cfg['effort']='medium'
        if cfg.get('api')=='openai':
            cfg.pop('api',None)               # openai is the default transport
        atomic_json(home/'config.json',cfg)
        emit(dict(current=_current(cfg),entry=entry,note='a running resident takes it at its next turn'),a.json);return 0
    if a.op=='effort':
        cat=catalog.load(home)
        try:entry=catalog.find(cat,cfg.get('model',''),cfg.get('provider'))
        except ValueError:entry=None
        levels=(entry or {}).get('efforts') or []
        if a.level:
            if a.level=='default':
                cfg.pop('effort',None)
            elif entry is not None and a.level not in levels:
                raise ValueError('%s offers %s' % (cfg.get('model'), ', '.join(levels) if levels else 'no effort control'))
            else:
                cfg['effort']=a.level
            atomic_json(home/'config.json',cfg)
        emit(dict(effort=cfg.get('effort'),levels=levels,model=cfg.get('model')),a.json);return 0


def parser():
    ap=argparse.ArgumentParser(prog='caminus env agent')
    sub=ap.add_subparsers(dest='op',required=True)
    for op in ('init','create','memory','run','once','send','status','events','stream','resume','cancel','compact','unit','start','stop','peers','message','mail','discover','prompt','heartbeat','advisor','fallback','mcp','provider','models','model','effort'):
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
            p.add_argument('--api',choices=('openai','anthropic'),default='openai',
                           help="wire format: OpenAI chat completions (the local server) or Anthropic's Messages API")
            p.add_argument('--effort',choices=('low','medium','high'),
                           help='Messages API effort (thinking stays off; default medium)')
            p.add_argument('--max-model-retries',type=int,
                           help='failed model calls in a row before the goal stops (default: unbounded for a '
                                'local endpoint, 8 for a remote one)')
            p.add_argument('--bus',help='shared agent-side SQLite mail bus; defaults under ~/.local/share/caminus/agents')
            p.add_argument('--idle-timeout',type=float,default=0,
                           help='seconds without progress before the watchdog pauses the goal (0: off)')
            p.add_argument('--token-budget',type=int,default=0,
                           help='prompt tokens a goal may reach before the watchdog pauses it (0: off)')
            p.add_argument('--no-loop-gates',action='store_true',
                           help='record loop_gates false: a fixture agent that predates the done check and skips it')
        if op=='fallback':
            p.add_argument('--model',help='the fallback model while the primary is unreachable, e.g. claude-haiku-5-5')
            p.add_argument('--endpoint',help="its API root, e.g. the owner's key relay")
            p.add_argument('--api',choices=('openai','anthropic'))
            p.add_argument('--context',type=int)
            p.add_argument('--effort',choices=('low','medium','high'))
            p.add_argument('--api-key-env')
            p.add_argument('--budget-usd',type=float,help='dollars per day the fallback may spend on work (default 1.00)')
            p.add_argument('--down-seconds',type=float,help='how long a failed primary is left alone (default 60)')
            p.add_argument('--off',action='store_true')
        if op=='provider':
            p.add_argument('action',nargs='?',default='list',choices=('list','add','remove'))
            p.add_argument('provider',nargs='?',help='a short name: local, anthropic, deepinfra, ...')
            p.add_argument('--kind',choices=('local','anthropic','deepinfra','openai'))
            p.add_argument('--endpoint',help="its API root; the owner's key relay for a paid one (it adds the key)")
            p.add_argument('--relay',action='store_true',help='the endpoint is the key relay: billed, retries bounded')
        if op=='models':
            p.add_argument('--refresh',action='store_true',help='ask the providers again (otherwise the cached list)')
            p.add_argument('--provider',help='only this provider')
        if op=='model':
            p.add_argument('id',nargs='?',help='a model id from `models` (provider/id when two serve it); none shows the current one')
            p.add_argument('--provider')
        if op=='effort':
            p.add_argument('level',nargs='?',help="a level the current model offers (see `models`), or 'default'")
        if op=='mcp':
            p.add_argument('action',nargs='?',default='list',choices=('list','add','remove','off','on'))
            p.add_argument('server',nargs='?',help='the server name (letters, digits, - and _)')
            p.add_argument('--env',action='append',default=[],help='KEY=VALUE for its environment (repeat)')
            # (after --: the command that starts the server, e.g. -- npx -y @modelcontextprotocol/server-memory;
            #  main() splits it off before parsing)
        if op=='advisor':
            p.add_argument('--model',help='the advisor model, e.g. claude-opus-5-5 or claude-sonnet-5-5')
            p.add_argument('--endpoint',help="Messages API root; the owner's own key relay keeps the advisor's key and cap separate")
            p.add_argument('--api-key-env',help='key variable, when the endpoint is not a local relay that adds the key')
            p.add_argument('--budget-usd',type=float,help='dollars per day the advisor may spend (default 1.00)')
            p.add_argument('--calls-per-goal',type=int,help='advice calls one goal may make (default 5)')
            p.add_argument('--effort',choices=('low','medium','high','xhigh','max'))
            p.add_argument('--off',action='store_true',help='remove the advisor')
        if op=='heartbeat':
            p.add_argument('state',nargs='?',choices=('on','off','status','recap'),default='status',
                           help='free time: the resident lives in its account when no work is queued')
            p.add_argument('--rest-minutes',type=float)
            p.add_argument('--budget-usd',type=float,help='daily free-time allowance on a priced remote model')
            p.add_argument('--afk-minutes',type=float,help='how long a free-time question waits for an answer')
            p.add_argument('--messages-per-day',type=int)
            p.add_argument('--stretch-minutes',type=float)
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
    cmdline=[]
    if argv[:1]==['mcp'] and '--' in argv:
        # an MCP server's own command line follows --; split it off before parsing, so flags like npx -y stay its own
        cut=argv.index('--');argv,cmdline=argv[:cut],argv[cut+1:]
    a=parser().parse_args(argv)
    if a.op=='mcp':a.cmdline=cmdline
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
            if a.api!='openai':cfg['api']=a.api
            if a.effort:cfg['effort']=a.effort
            if a.max_model_retries is not None:
                if a.max_model_retries<0:raise ValueError('--max-model-retries must be 0 or more')
                cfg['max_model_retries']=a.max_model_retries
            # A bare host gets the /v1 root; a provider URL with its own path (…/v1/openai) is kept as given.
            if not urllib.parse.urlsplit(cfg['endpoint']).path.strip('/'):cfg['endpoint']+='/v1'
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
        if a.op=='fallback':
            from .brain import DEFAULTS as FB
            from .model import Model, price
            cfg=json.loads((home/'config.json').read_text())
            fb={} if a.off else dict(cfg.get('fallback') or {})
            if not a.off:
                for flag in ('model','endpoint','api','context','effort','api_key_env','budget_usd','down_seconds'):
                    v=getattr(a,flag)
                    if v is not None:fb[flag]=v.rstrip('/') if flag=='endpoint' else v
            if fb:
                if not (fb.get('model') and fb.get('endpoint')):raise ValueError('a fallback needs --model and --endpoint')
                m=Model({k:v for k,v in fb.items() if k not in FB})          # validates the endpoint and api
                if m.remote and price(fb['model'],{}) is None:
                    raise ValueError('no known price for %s, so its spend cannot be limited' % fb['model'])
                cfg['fallback']=fb
            else:
                cfg.pop('fallback',None)
            atomic_json(home/'config.json',cfg)
            state=json.loads((home/'fallback.json').read_text()) if (home/'fallback.json').exists() else {}
            import datetime
            emit(dict(fallback={**FB,**fb} if fb else None,
                      today=state.get(datetime.date.today().isoformat(),dict(usd=0.0,turns=0))),a.json)
            return 0
        if a.op in ('provider','models','model','effort'):
            return model_ops(a,home)
        if a.op=='mcp':
            import re as _re
            cfg=json.loads((home/'config.json').read_text())
            servers=dict(cfg.get('mcp') or {})
            if a.action!='list':
                if not a.server or not _re.fullmatch(r'[A-Za-z0-9_-]{1,40}',a.server):
                    raise ValueError('name the server: letters, digits, - and _ (at most 40)')
                if a.action=='add':
                    cmdline=a.cmdline
                    if not cmdline:raise ValueError('mcp add SERVER -- COMMAND [ARGS...]: give the command after --')
                    env={}
                    for kv in a.env:
                        k,sep,v=kv.partition('=')
                        if not sep or not k:raise ValueError('--env takes KEY=VALUE')
                        env[k]=v
                    servers[a.server]=dict(command=cmdline[0],args=cmdline[1:],env=env,enabled=True)
                elif a.server not in servers:
                    raise ValueError('no MCP server named '+a.server)
                elif a.action=='remove':
                    servers.pop(a.server)
                else:
                    servers[a.server]['enabled']=a.action=='on'
                if servers:cfg['mcp']=servers
                else:cfg.pop('mcp',None)
                atomic_json(home/'config.json',cfg)
            # an env value can be a secret: show only its keys
            emit(dict(mcp={n:dict(command=v['command'],args=v.get('args',[]),env=sorted(v.get('env') or {}),
                                  enabled=v.get('enabled',True)) for n,v in servers.items()},
                      note='restart the agent (stop, start) for a change to reach a running daemon'),a.json)
            return 0
        if a.op=='advisor':
            from .advisor import Advisor, DEFAULTS
            from .model import price
            cfg=json.loads((home/'config.json').read_text())
            adv=dict(cfg.get('advisor') or {})
            if a.off:
                adv={}
            for flag in ('model','endpoint','api_key_env','budget_usd','calls_per_goal','effort'):
                v=getattr(a,flag)
                if v is not None:
                    if isinstance(v,(int,float)) and v<0:raise ValueError('--'+flag.replace('_','-')+' must be 0 or more')
                    adv[flag]=v.rstrip('/') if flag=='endpoint' else v
            if adv.get('model') and price(adv['model'],{}) is None:
                raise ValueError('no known price for %s, so its spend cannot be limited' % adv['model'])
            if adv and not (adv.get('model') and adv.get('endpoint')):
                raise ValueError('an advisor needs --model and --endpoint')
            if adv!=(cfg.get('advisor') or {}):
                if adv:cfg['advisor']=adv
                else:cfg.pop('advisor',None)
                atomic_json(home/'config.json',cfg)
            state=json.loads((home/'advisor.json').read_text()) if (home/'advisor.json').exists() else {}
            import datetime
            emit(dict(advisor={**DEFAULTS,**adv} if adv else None,
                      today=state.get(datetime.date.today().isoformat(),dict(usd=0.0,calls=0))),a.json)
            return 0
        if a.op=='heartbeat':
            from .freetime import settings
            cfg=json.loads((home/'config.json').read_text())
            ft=dict(cfg.get('free_time') or {}) if not isinstance(cfg.get('free_time'),bool) else dict(enabled=cfg['free_time'])
            if a.state=='recap':
                from .freetime import recap
                st=Store(home)
                try:emit(dict(recap=recap(st)),a.json)
                finally:st.close()
                return 0
            if a.state in ('on','off'):ft['enabled']=a.state=='on'
            for flag,key,scale in (('rest_minutes','rest_seconds',60),('afk_minutes','afk_seconds',60),
                                   ('stretch_minutes','stretch_seconds',60),('budget_usd','budget_usd',1),
                                   ('messages_per_day','messages_per_day',1)):
                v=getattr(a,flag)
                if v is not None:
                    if v<0:raise ValueError('--'+flag.replace('_','-')+' must be 0 or more')
                    ft[key]=v*scale
            if ft!=(cfg.get('free_time') or {}):
                cfg['free_time']=ft;atomic_json(home/'config.json',cfg)
            state=json.loads((home/'free_time.json').read_text()) if (home/'free_time.json').exists() else {}
            import datetime
            today=state.get('days',{}).get(datetime.date.today().isoformat(),{})
            emit(dict(free_time=settings(cfg),spent_today_usd=today.get('usd',0.0),messages_today=today.get('messages',0),
                      last_start=state.get('last_start'),last_end=state.get('last_end'),last_dream=state.get('last_dream')),a.json)
            return 0
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
