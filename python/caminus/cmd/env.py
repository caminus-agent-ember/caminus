"""`caminus env` : the agent environment (docs/specs/spec-agent-env.md). The harness runs here, as you; the agent
work runs as the second user over SSH; root waits for your approval.

  caminus env setup [--agent-side]   the sudo commands to provision the machine, then status; --agent-side also
                                     installs ~/bin/ask-root and ~/ROBOTS.md as the agent user
  caminus env status                 is it working? (user, key, ssh login, bridge, ask-root)
  caminus env run [--shell] CMD...   run CMD as the agent user (argv, or one shell string with --shell)
  caminus env shell                  an interactive session as the agent user (run any agent by hand)
  caminus env prompt                 print ROBOTS.md, the agent environment's own instructions
  caminus env ui                     the TUI: queued root requests + the live audit log (caminus-panel env)
  caminus env spawn --name N -- CMD...   start CMD detached in the agent user (log ~/env-logs/N.log, pid ~/env-jobs/N.pid)
  caminus env jobs                   the detached jobs (name, pid, alive, log)
  caminus env stop NAME              kill a detached job
  caminus env ask TEXT [CHOICE...]   agent side: ask you a question and wait for the answer
  caminus env reqs [--json]          the pending bridge requests (scriptable)
  caminus env answer ID DECISION [TEXT] [--json]   answer one: approved | edited TEXT | denied | answered TEXT
  caminus env say TEXT               leave a steering note for the agent (its env-note drains them)
  caminus env notes [--json]         the undrained notes
  caminus env ctx [--json]           the running servers and the context window each reports
  caminus env memory                 the agent's ~/MEMORY.md (its persistent memory)
  caminus env remember TEXT          append a line to the agent's ~/MEMORY.md
  caminus env serve NAME MODEL [--placement auto|0|1|2] [--ctx N] [--port P] [--dry-run] [-- serve flags...]
                                     a model server as a job: own home, port, tmux session, cards resolved
  caminus env new NAME -- CMD...     a named tmux session in the agent user running CMD (an agent CLI)
  caminus env sessions               the tmux sessions
  caminus env send NAME TEXT         type TEXT into the session (a live instruction)
  caminus env attach NAME            drop into the session's terminal
  caminus env jobs [--json]          same as jobs, machine-readable
  caminus env ask-root CMD...        agent side: file a root request and wait for your answer
  caminus env watch [--once]         answer the queued root requests (approve / edit / deny)
  caminus env audit [--tail N]       the audit log
"""
import argparse
import base64
import getpass
import json
import os
import pwd
import subprocess
import sys
import time

from ..env import agent_side, audit, bridge, jobs, runner, serve, sessions, settings


def _parser():
    ap = argparse.ArgumentParser(prog='caminus env', description=__doc__.splitlines()[0])
    sub = ap.add_subparsers(dest='sub', required=True)

    p = sub.add_parser('setup', help='print the provisioning commands, then check status')
    p.add_argument('--agent-side', action='store_true', help='also install ask-root into the agent user over SSH')

    sub.add_parser('status', help='check user, key, ssh login, bridge, ask-root')

    p = sub.add_parser('run', help='run a command as the agent user')
    p.add_argument('--shell', '-s', action='store_true', help='CMD is one shell string, not an argv')
    p.add_argument('cmd', nargs=argparse.REMAINDER)

    sub.add_parser('shell', help='interactive SSH session as the agent user (its instructions: ~/ROBOTS.md)')

    sub.add_parser('prompt', help="print ROBOTS.md, the agent environment's own instructions")

    sub.add_parser('ui', help='the TUI: queued root requests + the live audit log (caminus-panel env)')

    p = sub.add_parser('agent', add_help=False, help='resident agent: install/init/start/send/once/status/events/resume/cancel')
    p.add_argument('args', nargs=argparse.REMAINDER)

    p = sub.add_parser('spawn', help='start a command detached in the agent user')
    p.add_argument('--name', '-n', required=True)
    p.add_argument('cmd', nargs=argparse.REMAINDER)

    p = sub.add_parser('jobs', help='list the detached jobs')
    p.add_argument('--json', action='store_true')

    p = sub.add_parser('stop', help='kill a detached job')
    p.add_argument('name')

    p = sub.add_parser('ask', help='agent side: ask a question and wait for the answer')
    p.add_argument('--timeout', type=float, default=3600.0)
    p.add_argument('args', nargs=argparse.REMAINDER)

    p = sub.add_parser('say', help='leave a steering note for the agent')
    p.add_argument('text')

    p = sub.add_parser('notes', help='list the undrained steering notes')
    p.add_argument('--json', action='store_true')

    p = sub.add_parser('ctx', help='the running servers and their context windows')
    p.add_argument('--json', action='store_true')

    p = sub.add_parser('reqs', help='the pending bridge requests (scriptable)')
    p.add_argument('--json', action='store_true')

    p = sub.add_parser('answer', help='answer one request: approved | edited TEXT | denied | answered TEXT')
    p.add_argument('--json', action='store_true')
    p.add_argument('id')
    p.add_argument('decision', choices=['approved', 'edited', 'denied', 'answered'])
    p.add_argument('text', nargs='?')

    sub.add_parser('memory', help="print the agent's ~/MEMORY.md")

    p = sub.add_parser('remember', help="append a line to the agent's ~/MEMORY.md")
    p.add_argument('text')

    p = sub.add_parser('serve', help='start a model server as an agent-user job (own home/port/session)')
    p.add_argument('name')
    p.add_argument('model')
    p.add_argument('--placement', default='auto', choices=serve.PLACEMENTS,
                   help="auto: the model's own card count; 0/1: that one card; 2: split across both")
    p.add_argument('--ctx')
    p.add_argument('--port', type=int)
    p.add_argument('--dry-run', action='store_true', help='print the launcher and session command, start nothing')
    p.add_argument('extra', nargs='*')   # (everything after -- is passed to serve; see main)

    p = sub.add_parser('new', help='a named tmux session running CMD in the agent user')
    p.add_argument('name')
    p.add_argument('cmd', nargs=argparse.REMAINDER)

    sub.add_parser('sessions', help='the tmux sessions in the agent user')

    p = sub.add_parser('send', help='type TEXT + Enter into session NAME (a live instruction)')
    p.add_argument('name')
    p.add_argument('text')

    p = sub.add_parser('attach', help="drop into session NAME's terminal (watch and type)")
    p.add_argument('name')

    p = sub.add_parser('ask-root', help='agent side: ask for a root command and wait for the answer')
    p.add_argument('--timeout', type=float, default=3600.0)
    p.add_argument('cmd', nargs=argparse.REMAINDER)

    p = sub.add_parser('watch', help='answer queued root requests')
    p.add_argument('--once', action='store_true', help='drain the current queue and exit')

    p = sub.add_parser('audit', help='print the audit log')
    p.add_argument('--tail', type=int, default=20)
    p.add_argument('--json', action='store_true')
    return ap


def main(rest, transport=None):
    if rest[:1] == ['agent']:
        from ..env import agent_host
        return agent_host.main(rest[1:], transport or subprocess.run)
    extra_serve = None
    if rest[:1] == ['serve'] and '--' in rest:
        i = rest.index('--')
        extra_serve = rest[i + 1:]
        rest = rest[:i]
    a = _parser().parse_args(rest)
    if extra_serve is not None:
        a.extra = extra_serve
    tr = transport or subprocess.run
    if a.sub == 'setup':
        return _setup(a, tr)
    if a.sub == 'status':
        return _status(tr)
    if a.sub == 'run':
        return _run(a, tr)
    if a.sub == 'shell':
        print(f'agent environment: you are {settings.user()} on this machine; your instructions are in '
              f'~/ROBOTS.md (`caminus env prompt` prints them from the harness side); root: ~/bin/ask-root CMD...',
              file=sys.stderr)
        runner.shell()          # never returns
    if a.sub == 'prompt':
        from ..env import prompt
        sys.stdout.write(prompt.text())
        return 0
    if a.sub == 'ui':
        return _ui()
    if a.sub == 'spawn':
        return _spawn(a, tr)
    if a.sub == 'jobs':
        r = runner.run_shell(jobs.jobs_script(), transport=tr)
        if getattr(a, 'json', False):
            out = []
            for line in (r.stdout or '').splitlines():
                parts = line.split()
                if len(parts) >= 4:
                    out.append({'name': parts[0], 'pid': parts[1].split('=')[1], 'state': parts[2],
                                'log': parts[3].split('=', 1)[1]})
            print(json.dumps(out))
        else:
            sys.stdout.write(r.stdout or '')
        return r.returncode
    if a.sub == 'stop':
        script = jobs.stop_script(a.name) + f'; tmux kill-session -t {a.name} 2>/dev/null; true'
        r = runner.run_shell(script, transport=tr)
        sys.stdout.write(r.stdout or '')
        audit.log(settings.audit_path(), {'event': 'stop', 'job': a.name})
        return 0
    if a.sub == 'ask':
        return _ask(a)
    if a.sub == 'say':
        nid = bridge.say(settings.bridge_dir(), a.text, getpass.getuser())
        audit.log(settings.audit_path(), {'event': 'note', 'id': nid, 'text': a.text})
        print(f'note {nid} left for {settings.user()}')
        return 0
    if a.sub == 'notes':
        ns = bridge.notes(settings.bridge_dir())
        if a.json:
            print(json.dumps(ns))
        else:
            for n in ns:
                print(f"[{n.get('from', '?')}] {n.get('text', '')}")
        return 0
    if a.sub == 'reqs':
        reqs = bridge.pending(settings.bridge_dir())
        if a.json:
            print(json.dumps(reqs))
        else:
            for r in reqs:
                body = r.get('cmd') if r.get('kind', 'root') == 'root' else r.get('text', '')
                print(f"{r['id']} [{r.get('kind', 'root')}] from {r.get('user')}: {body}")
        return 0
    if a.sub == 'answer':
        return _answer(a, tr)
    if a.sub == 'memory':
        r = runner.run_shell(f'cat {settings.memory_path()} 2>/dev/null || echo "(no MEMORY.md yet)"', transport=tr)
        sys.stdout.write(r.stdout or '')
        return 0
    if a.sub == 'remember':
        return _remember(a, tr)
    if a.sub == 'serve':
        return _serve(a, tr)
    if a.sub == 'new':
        cmd = a.cmd
        if cmd and cmd[0] == '--':
            cmd = cmd[1:]
        if not cmd:
            print('caminus env new: no command', file=sys.stderr)
            return 2
        audit.log(settings.audit_path(), {'event': 'new_session', 'name': a.name, 'cmd': ' '.join(cmd)})
        r = runner.run_shell(sessions.new_script(a.name, cmd), transport=tr)
        sys.stderr.write(r.stderr or '')
        print(f'session {a.name} started' if r.returncode == 0 else f'failed: {(r.stderr or "").strip()}')
        return r.returncode
    if a.sub == 'sessions':
        r = runner.run_shell(sessions.sessions_script(), transport=tr)
        sys.stdout.write(r.stdout or '')
        return 0
    if a.sub == 'send':
        audit.log(settings.audit_path(), {'event': 'send', 'session': a.name, 'text': a.text})
        r = runner.run_shell(sessions.send_script(a.name, a.text), transport=tr)
        if r.returncode != 0:
            sys.stderr.write(r.stderr or '')
        return r.returncode
    if a.sub == 'attach':
        import os as _os
        _os.execvp('ssh', sessions.attach_argv(a.name, runner.ssh_base()))
    if a.sub == 'ctx':
        from ..env import ctx
        rows = ctx.servers()
        if a.json:
            print(json.dumps(rows))
            return 0
        if not rows:
            print('no server running on 127.0.0.1:8092-8101')
            return 0
        for s in rows:
            print(f"127.0.0.1:{s['port']}  {s['model']}  context {s['n_ctx']}")
        return 0
    if a.sub == 'ask-root':
        return _ask_root(a)
    if a.sub == 'watch':
        return _watch(a, tr)
    if a.sub == 'audit':
        evs = audit.tail(settings.audit_path(), a.tail)
        if a.json:
            print(json.dumps(evs))
        else:
            for ev in evs:
                print(ev)
        return 0
    return 2


PROVISION = """\
# run these as your own user (sudo asks for your password); they create the agent user, SSH, the bridge and the key:
sudo apt-get install -y openssh-server && sudo systemctl enable --now ssh
sudo useradd -m -s /bin/bash {user}
sudo passwd -l {user}
sudo groupadd -f {group}
sudo usermod -aG render,video,{group} {user}          # GPU access for the agent work; {you} joins the bridge group
sudo usermod -aG {group} {you}
sudo mkdir -p {bridge}/req {bridge}/res {bridge}/notes
sudo chown root:{group} {bridge} {bridge}/req {bridge}/res {bridge}/notes
sudo chmod 770 {bridge} {bridge}/req {bridge}/res {bridge}/notes
sudo loginctl enable-linger {user}                   # its long-running servers survive logout
# optional, for `caminus env serve` (the server runs as the agent user and must read python+torch+caminus):
sudo mkdir -p /opt/caminus-runtime && sudo mount --bind "$HOME/miniforge3" /opt/caminus-runtime/conda 2>/dev/null || true
sudo mount --bind "$HOME/src" /opt/caminus-runtime/src 2>/dev/null || true
sudo mount --bind "$HOME/.local/share/caminus/models" /opt/caminus-runtime/models 2>/dev/null || true
# then: export CAMINUS_ENV_PYTHON='/opt/caminus-runtime/conda/envs/drafter/bin/python -I'
#       export CAMINUS_ENV_PKG=/opt/caminus-runtime/src/caminus/public/python
#       export CAMINUS_ENV_MODELS=/opt/caminus-runtime/models
ssh-keygen -t ed25519 -f {key} -N '' -C caminus-env   # if {key} does not exist yet
# then add this line to {user_home}/.ssh/authorized_keys (dir 700, file 600, owned by {user}):
from="127.0.0.1,::1",no-agent-forwarding,no-X11-forwarding,no-port-forwarding {pubkey}
# finally: caminus env setup --agent-side   (installs ~/bin/ask-root + env-ask + env-note, ~/ROBOTS.md, ~/MEMORY.md)
"""


def _setup(a, tr):
    key = settings.key_path()
    pub = key + '.pub'
    print(PROVISION.format(user=settings.user(), group=settings.BRIDGE_GROUP, you=getpass.getuser(),
                           bridge=settings.bridge_dir(), key=key,
                           pubkey=(open(pub).read().strip() if os.path.exists(pub) else '<contents of ' + pub + '>'),
                           user_home=f"/home/{settings.user()}"))
    if a.agent_side and os.path.exists(key):
        from ..env import prompt
        r = runner.run_shell(agent_side.install_command(settings.bridge_dir(), settings.agent_bin(), prompt.text(),
                                                        settings.robots_path(), prompt.MEMORY_TEMPLATE,
                                                        settings.memory_path()), transport=tr)
        print(f'agent-side files installed as {settings.user()} ({settings.agent_bin()}, {settings.robots_path()}, '
              f'{settings.memory_path()}): exit {r.returncode}'
              + ('' if r.returncode == 0 else f' ({(r.stderr or "").strip()})'), file=sys.stderr)
    return _status(tr)


def _status(tr):
    checks = []

    def check(name, ok, detail=''):
        checks.append((name, ok, detail))
        print(f"{'PASS' if ok else 'FAIL'} {name}{(': ' + detail) if detail else ''}")

    try:
        pwd.getpwnam(settings.user())
        check(f'user {settings.user()}', True)
    except KeyError:
        check(f'user {settings.user()}', False, 'no such user (caminus env setup)')
    check(f'key {settings.key_path()}', os.path.exists(settings.key_path()))
    ok, detail = runner.probe(transport=tr)
    check('ssh login as the agent user', ok, detail)
    b = settings.bridge_dir()
    check(f'bridge {b}', os.path.isdir(b) and os.access(b, os.W_OK),
          '' if os.path.isdir(b) else 'missing (caminus env setup)')
    r = runner.run_shell(f'test -x {settings.agent_bin()}', transport=tr)   # shell: ~ must expand remotely
    check('ask-root installed', r.returncode == 0, 'run: caminus env setup --agent-side' if r.returncode else '')
    return 0 if all(ok for _, ok, _ in checks) else 1


def _ui():
    from .manage import panel_binary
    b = panel_binary()
    if b is None:
        print('caminus env ui: caminus-panel is not installed (build it: cd panel && go build -o caminus-panel .; '
              'or set CAMINUS_PANEL)', file=sys.stderr)
        return 1
    from .manage import cli_argv
    os.execve(b, [b, 'env', '--user', settings.user(), '--bridge', settings.bridge_dir(),
                  '--audit', settings.audit_path(), '--key', settings.key_path(),
                  '--cli', json.dumps(cli_argv())], dict(os.environ))


def _spawn(a, tr):
    cmd = a.cmd
    if cmd and cmd[0] == '--':
        cmd = cmd[1:]
    if not cmd:
        print('caminus env spawn: no command', file=sys.stderr)
        return 2
    try:
        script = jobs.spawn_script(a.name, cmd)
    except ValueError as e:
        print(f'caminus env spawn: {e}', file=sys.stderr)
        return 2
    audit.log(settings.audit_path(), {'event': 'spawn', 'name': a.name, 'cmd': ' '.join(cmd)})
    r = runner.run_shell(script, transport=tr)
    sys.stdout.write(r.stdout or '')
    sys.stderr.write(r.stderr or '')
    return r.returncode


def _run(a, tr):
    cmd = a.cmd
    if cmd and cmd[0] == '--':
        cmd = cmd[1:]
    if not cmd:
        print('caminus env run: no command', file=sys.stderr)
        return 2
    audit.log(settings.audit_path(), {'event': 'run', 'user': settings.user(), 'cmd': ' '.join(cmd)})
    r = runner.run_shell(' '.join(cmd), transport=tr) if a.shell else runner.run_argv(cmd, transport=tr)
    sys.stdout.write(r.stdout or '')
    sys.stderr.write(r.stderr or '')
    return r.returncode


def _ask_root(a):
    cmd = ' '.join(a.cmd)
    if cmd.startswith('--'):
        cmd = cmd[2:].strip()
    if not cmd:
        print('caminus env ask-root: no command', file=sys.stderr)
        return 2
    try:
        res = bridge.submit(settings.bridge_dir(), 'root', cmd=cmd, timeout=a.timeout)
    except TimeoutError as e:
        print(f'caminus env ask-root: {e}', file=sys.stderr)
        return 124
    if res['decision'] == 'denied':
        print(f"caminus env ask-root: denied by {res.get('approved_by', '?')}: {cmd}", file=sys.stderr)
        return 126
    sys.stdout.write(res.get('out') or '')
    sys.stderr.write(res.get('err') or '')
    return res.get('rc') if res.get('rc') is not None else 0


def _ask(a):
    args = list(a.args)
    if args and args[0] == '--':
        args = args[1:]
    if not args:
        print('caminus env ask: TEXT [CHOICE...]', file=sys.stderr)
        return 2
    text, choices = args[0], args[1:]
    try:
        res = bridge.submit(settings.bridge_dir(), 'question', text=text, choices=choices or None, timeout=a.timeout)
    except TimeoutError as e:
        print(f'caminus env ask: {e}', file=sys.stderr)
        return 124
    if res['decision'] == 'denied':
        print(f"caminus env ask: denied by {res.get('approved_by', '?')}", file=sys.stderr)
        return 126
    print(res.get('answer') or '')
    return 0


def _watch(a, tr):
    os.umask(0o007)          # bridge files must stay readable by the agent user
    me = getpass.getuser()
    ap = settings.audit_path()
    seen = set()
    while True:
        reqs = [r for r in bridge.pending(settings.bridge_dir()) if r['id'] not in seen]
        for req in reqs:
            seen.add(req['id'])
            kind = req.get('kind', 'root')
            audit.log(ap, {'event': 'request', 'id': req['id'], 'kind': kind, 'from': req.get('user')})
            print(f"\n{kind} request {req['id']} from {req.get('user')} (pid {req.get('pid')}, cwd {req.get('cwd')})")
            if kind == 'question':
                print(f"  {req.get('text', '')}")
                if req.get('choices'):
                    print('  choices: ' + ' | '.join(f'{i + 1}) {c}' for i, c in enumerate(req['choices'])))
                ans = input('answer (text, or choice number; empty denies): ')
                if not ans.strip():
                    bridge.answer(settings.bridge_dir(), req['id'], 'denied', me)
                    audit.log(ap, {'event': 'decision', 'id': req['id'], 'decision': 'denied', 'by': me})
                else:
                    if req.get('choices') and ans.strip().isdigit() and 1 <= int(ans) <= len(req['choices']):
                        ans = req['choices'][int(ans) - 1]
                    bridge.answer(settings.bridge_dir(), req['id'], 'answered', me, answer_text=ans)
                    audit.log(ap, {'event': 'decision', 'id': req['id'], 'decision': 'answered', 'by': me, 'answer': ans})
                    _remember_qa(req.get('text', ''), ans, tr)
                continue
            print(f"  {req['cmd']}")
            choice = input('approve [a] / edit [e] / deny [d] / skip [s]: ').strip().lower()
            if choice == 's':
                continue
            if choice == 'd':
                bridge.answer(settings.bridge_dir(), req['id'], 'denied', me)
                audit.log(ap, {'event': 'root_decision', 'id': req['id'], 'decision': 'denied', 'by': me})
                print('denied.')
                continue
            cmd = req['cmd']
            if choice == 'e':
                cmd = input('root command: ').strip()
                if not cmd:
                    continue
            r = bridge.execute_root(cmd, runner=tr)
            bridge.answer(settings.bridge_dir(), req['id'],
                          'edited' if cmd != req['cmd'] else 'approved', me, cmd=cmd,
                          rc=r.returncode, out=r.stdout or '', err=r.stderr or '')
            audit.log(ap, {'event': 'root_result', 'id': req['id'], 'decision': 'approved' if cmd == req['cmd'] else 'edited',
                           'by': me, 'cmd': cmd, 'rc': r.returncode})
            sys.stdout.write(r.stdout or '')
            sys.stderr.write(r.stderr or '')
        if a.once:
            return 0
        time.sleep(0.5)


def _answer(a, tr):
    """answer one pending request non-interactively (for scripts and tests); approved/edited run sudo"""
    me = getpass.getuser()
    reqs = {r['id']: r for r in bridge.pending(settings.bridge_dir())}
    if a.id not in reqs:
        print(f'caminus env answer: no pending request {a.id} (caminus env reqs)', file=sys.stderr)
        return 2
    req = reqs[a.id]
    ap = settings.audit_path()
    if a.decision == 'denied':
        bridge.answer(settings.bridge_dir(), a.id, 'denied', me)
        audit.log(ap, {'event': 'decision', 'id': a.id, 'decision': 'denied', 'by': me})
        res = {'id': a.id, 'decision': 'denied'}
    elif a.decision == 'answered':
        if not a.text:
            print('caminus env answer: answered needs TEXT', file=sys.stderr)
            return 2
        bridge.answer(settings.bridge_dir(), a.id, 'answered', me, answer_text=a.text)
        audit.log(ap, {'event': 'decision', 'id': a.id, 'decision': 'answered', 'by': me, 'answer': a.text})
        _remember_qa(req.get('text', ''), a.text, tr)
        res = {'id': a.id, 'decision': 'answered', 'answer': a.text}
    else:
        cmd = a.text if a.decision == 'edited' else req.get('cmd', '')
        if not cmd:
            print('caminus env answer: no command to run', file=sys.stderr)
            return 2
        r = bridge.execute_root(cmd, runner=tr)
        bridge.answer(settings.bridge_dir(), a.id, a.decision, me, cmd=cmd, rc=r.returncode,
                      out=r.stdout or '', err=r.stderr or '')
        audit.log(ap, {'event': 'root_result', 'id': a.id, 'decision': a.decision, 'by': me,
                       'cmd': cmd, 'rc': r.returncode})
        sys.stdout.write(r.stdout or '')
        sys.stderr.write(r.stderr or '')
        res = {'id': a.id, 'decision': a.decision, 'cmd': cmd, 'rc': r.returncode}
    if a.json:
        print(json.dumps(res))
    return 0


def _remember_qa(question, answer, tr):
    """append a Q/A block to the agent's MEMORY.md (the learn loop), over ssh, base64-safe"""
    payload = base64.b64encode(f'Q: {question}\nA: {answer}\n---\n'.encode()).decode()
    runner.run_shell(f"printf %s {payload} | base64 -d >> {settings.memory_path()}", transport=tr)


def _remember(a, tr):
    payload = base64.b64encode((a.text.rstrip('\n') + '\n').encode()).decode()
    r = runner.run_shell(f"printf %s {payload} | base64 -d >> {settings.memory_path()}", transport=tr)
    audit.log(settings.audit_path(), {'event': 'remember', 'text': a.text})
    if r.returncode != 0:
        sys.stderr.write(r.stderr or '')
        return r.returncode
    print(f'appended to {settings.user()}:{settings.memory_path()}')
    return 0


def _serve(a, tr):
    """caminus env serve: resolve placement, build the launcher, start the tmux session (or just print with --dry-run)"""
    import base64 as _b64
    from ..cmd import manage
    extra = list(getattr(a, 'extra', []) or [])
    try:
        m, e, devs, nc = serve.resolve(a.model, a.placement, ctx=a.ctx)
    except serve.ServeError as ex:
        print(f'caminus env serve: {ex}', file=sys.stderr)
        return 2
    running = [s for s in manage.all_servers() if manage.alive(s.get('guard_pid'))]
    port = a.port or serve.alloc_port(taken={s['port'] for s in running})
    from .. import config
    serve_args = manage.serve_argv(m, e, a.name, port, a.ctx, extra)
    serve_args = serve.remap_paths(serve_args, config.model_dir(), os.environ.get('CAMINUS_ENV_MODELS', ''))
    home = f'~/homes/{a.name}'
    serve_args = _repoint(serve_args, home)          # --cache-dir/--flight-dir into the server's own home
    if nc != m.cards:
        serve_args += ['--cards', str(nc)]
    script = serve.launcher(a.name, serve_args, devs)
    cards = '+'.join(str(d) for d in devs)
    summary = (f'{a.name}: {m.name} on card(s) {cards} at http://127.0.0.1:{port} '
               f'(home ~/homes/{a.name}, session {a.name})')
    if a.dry_run:
        print(summary)
        print('--- run.sh ---')
        print(script, end='')
        print('--- session ---')
        print('caminus env new', a.name, '-- bash ~/homes/%s/run.sh' % a.name)
        return 0
    payload = _b64.b64encode(script.encode()).decode()
    prep = (f'mkdir -p {home} && printf %s {payload} | base64 -d > {home}/run.sh && chmod 700 {home}/run.sh')
    r = runner.run_shell(prep, transport=tr)
    if r.returncode != 0:
        print(f'caminus env serve: writing the launcher failed: {(r.stderr or "").strip()}', file=sys.stderr)
        return 1
    r = runner.run_shell(sessions.new_script(a.name, ['bash', f'{home}/run.sh']), transport=tr)
    if r.returncode != 0:
        print(f'caminus env serve: starting the session failed: {(r.stderr or "").strip()}', file=sys.stderr)
        return 1
    audit.log(settings.audit_path(), {'event': 'serve', 'name': a.name, 'model': m.name,
                                       'cards': devs, 'port': port})
    print(summary + '  (watch: caminus env sessions / attach ' + a.name + '; logs ~/homes/%s/serve.log)' % a.name)
    return 0


def _repoint(serve_args, home):
    """--cache-dir/--flight-dir values into the server's own home (the launcher runs as the agent user)"""
    out = list(serve_args)
    for i, a in enumerate(out[:-1]):
        if a == '--cache-dir':
            out[i + 1] = f'{home}/kv'
        elif a == '--flight-dir':
            out[i + 1] = f'{home}/flight'
    return out
