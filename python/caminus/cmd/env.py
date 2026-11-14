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
  caminus env ask-root CMD...        agent side: file a root request and wait for your answer
  caminus env watch [--once]         answer the queued root requests (approve / edit / deny)
  caminus env audit [--tail N]       the audit log
"""
import argparse
import getpass
import os
import pwd
import subprocess
import sys
import time

from ..env import agent_side, audit, bridge, jobs, runner, settings


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

    p = sub.add_parser('spawn', help='start a command detached in the agent user')
    p.add_argument('--name', '-n', required=True)
    p.add_argument('cmd', nargs=argparse.REMAINDER)

    sub.add_parser('jobs', help='list the detached jobs')

    p = sub.add_parser('stop', help='kill a detached job')
    p.add_argument('name')

    p = sub.add_parser('ask-root', help='agent side: ask for a root command and wait for the answer')
    p.add_argument('--timeout', type=float, default=3600.0)
    p.add_argument('cmd', nargs=argparse.REMAINDER)

    p = sub.add_parser('watch', help='answer queued root requests')
    p.add_argument('--once', action='store_true', help='drain the current queue and exit')

    p = sub.add_parser('audit', help='print the audit log')
    p.add_argument('--tail', type=int, default=20)
    return ap


def main(rest, transport=None):
    a = _parser().parse_args(rest)
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
        sys.stdout.write(r.stdout or '')
        return r.returncode
    if a.sub == 'stop':
        r = runner.run_shell(jobs.stop_script(a.name), transport=tr)
        sys.stdout.write(r.stdout or '')
        audit.log(settings.audit_path(), {'event': 'stop', 'job': a.name})
        return r.returncode
    if a.sub == 'ask-root':
        return _ask_root(a)
    if a.sub == 'watch':
        return _watch(a, tr)
    if a.sub == 'audit':
        for ev in audit.tail(settings.audit_path(), a.tail):
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
sudo mkdir -p {bridge}/req {bridge}/res
sudo chown root:{group} {bridge} {bridge}/req {bridge}/res
sudo chmod 770 {bridge} {bridge}/req {bridge}/res
sudo loginctl enable-linger {user}                   # its long-running servers survive logout
ssh-keygen -t ed25519 -f {key} -N '' -C caminus-env   # if {key} does not exist yet
# then add this line to {user_home}/.ssh/authorized_keys (dir 700, file 600, owned by {user}):
from="127.0.0.1,::1",no-agent-forwarding,no-X11-forwarding,no-port-forwarding {pubkey}
# finally: caminus env setup --agent-side   (installs ~/bin/ask-root and ~/ROBOTS.md as the agent user)
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
        r = runner.run_shell(agent_side.install_command(settings.bridge_dir(), settings.agent_bin(),
                                                        prompt.text(), settings.robots_path()), transport=tr)
        print(f'ask-root and ROBOTS.md installed as {settings.user()} ({settings.agent_bin()}, {settings.robots_path()}): exit {r.returncode}'
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
    os.execve(b, [b, 'env', '--user', settings.user(), '--bridge', settings.bridge_dir(),
                  '--audit', settings.audit_path(), '--key', settings.key_path()], dict(os.environ))


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
        res = bridge.submit(settings.bridge_dir(), cmd, timeout=a.timeout)
    except TimeoutError as e:
        print(f'caminus env ask-root: {e}', file=sys.stderr)
        return 124
    if res['decision'] == 'denied':
        print(f"caminus env ask-root: denied by {res.get('approved_by', '?')}: {cmd}", file=sys.stderr)
        return 126
    sys.stdout.write(res.get('out') or '')
    sys.stderr.write(res.get('err') or '')
    return res.get('rc') if res.get('rc') is not None else 0


def _watch(a, tr):
    os.umask(0o007)          # bridge files must stay readable by the agent user
    me = getpass.getuser()
    ap = settings.audit_path()
    seen = set()
    while True:
        reqs = [r for r in bridge.pending(settings.bridge_dir()) if r['id'] not in seen]
        for req in reqs:
            seen.add(req['id'])
            audit.log(ap, {'event': 'root_request', 'id': req['id'], 'from': req.get('user'), 'cmd': req['cmd']})
            print(f"\nroot request {req['id']} from {req.get('user')} (pid {req.get('pid')}, cwd {req.get('cwd')})")
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
