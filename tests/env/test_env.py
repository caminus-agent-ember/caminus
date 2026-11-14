"""CPU tests of the agent environment (caminus.env, caminus.cmd.env): nothing touches ssh, sudo, a GPU or the network;
the transports are fakes.

  python -I tests/env/test_env.py --scratch DIR     RESULT PASS

Everything runs under scratch: the state dir (audit log, ssh control socket path), the bridge directory and the key
path come from the environment."""
import argparse
import json
import os
import subprocess
import sys
import threading
import time

sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), '..', '..', 'python'))

FAILS = []


def check(ok, what):
    print(('PASS ' if ok else 'FAIL ') + what, flush=True)
    if not ok:
        FAILS.append(what)


class FakeTransport:
    """stands in for subprocess.run: records argv and stdin, answers canned output"""

    def __init__(self, rc=0, out='agent\n', err=''):
        self.calls = []
        self.rc, self.out, self.err = rc, out, err

    def __call__(self, argv, **kw):
        self.calls.append((argv, kw))
        return subprocess.CompletedProcess(argv, self.rc, self.out, self.err)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument('--scratch', required=True)
    a = ap.parse_args()
    S = os.path.abspath(a.scratch)
    for bad in (os.path.expanduser('~/.config'), os.path.expanduser('~/.local'), os.path.expanduser('~/.ssh')):
        if S.startswith(bad):
            raise SystemExit(f'--scratch must not be under {bad}')
    os.makedirs(S, exist_ok=True)
    state = os.path.join(S, 'state')
    bridge_dir = os.path.join(S, 'bridge')
    os.environ.update(CAMINUS_STATE_DIR=state, CAMINUS_ENV_BRIDGE=bridge_dir,
                      CAMINUS_ENV_AUDIT=os.path.join(state, 'env-audit.jsonl'),
                      CAMINUS_ENV_USER='test-agent', CAMINUS_ENV_KEY=os.path.join(S, 'key'))
    from caminus.env import agent_side, audit, bridge, jobs, prompt, runner, settings
    from caminus.cmd import env as cmd_env

    # settings: environment first
    check(settings.user() == 'test-agent', 'settings.user from the environment')
    check(settings.bridge_dir() == bridge_dir, 'settings.bridge_dir from the environment')
    check(settings.key_path().endswith('key'), 'settings.key_path from the environment')

    # runner: the ssh argv and the stdin-carried script
    tr = FakeTransport(out='test-agent\n')
    r = runner.run_argv(['echo', 'a b', '--flag'], transport=tr)
    argv, kw = tr.calls[0]
    check(argv[0] == 'ssh' and 'test-agent@127.0.0.1' in argv, 'runner: ssh to the agent user on 127.0.0.1')
    check(any(x == f'-i' and argv[i + 1] == settings.key_path() for i, x in enumerate(argv)), 'runner: the harness key')
    check(any(x == 'BatchMode=yes' for x in argv) and any(x == 'ControlMaster=auto' for x in argv)
          and any(x.startswith('ControlPath=') for x in argv), 'runner: batch mode and the ControlMaster socket')
    check(argv[-2:] == ['bash', '-s'], 'runner: the command runs as `bash -s`')
    check(kw['input'] == "echo 'a b' --flag", 'runner: argv is shlex-quoted into the stdin script')
    r = runner.run_shell('make -j4 && echo done', transport=tr)
    check(tr.calls[1][1]['input'] == 'make -j4 && echo done', 'runner: a shell string travels verbatim on stdin')
    ok, detail = runner.probe(transport=tr)
    check(ok, f'runner.probe accepts the agent user ({detail.strip()})')
    ok, _ = runner.probe(transport=FakeTransport(out='someone-else\n'))
    check(not ok, 'runner.probe rejects a wrong user')

    # bridge: submit -> pending -> answer, atomic and complete
    os.makedirs(os.path.join(bridge_dir, 'req'), exist_ok=True)
    os.makedirs(os.path.join(bridge_dir, 'res'), exist_ok=True)
    got = {}

    def agent():
        try:
            got['res'] = bridge.submit(bridge_dir, 'root', cmd='systemctl restart foo', timeout=10)
        except TimeoutError as e:
            got['timeout'] = str(e)

    t = threading.Thread(target=agent)
    t.start()
    deadline = time.time() + 5
    while not bridge.pending(bridge_dir) and time.time() < deadline:
        time.sleep(0.05)
    reqs = bridge.pending(bridge_dir)
    check(len(reqs) == 1 and reqs[0]['cmd'] == 'systemctl restart foo' and reqs[0]['user'], 'bridge: the request is pending with its cmd and user')
    bridge.answer(bridge_dir, reqs[0]['id'], 'approved', 'human', cmd=reqs[0]['cmd'], rc=0, out='ok\n', err='')
    t.join(5)
    check(not t.is_alive() and got.get('res', {}).get('out') == 'ok\n' and got['res']['decision'] == 'approved',
          'bridge: the agent side receives the complete answer')
    check(bridge.pending(bridge_dir) == [], 'bridge: an answered request leaves the queue')
    t = threading.Thread(target=agent)
    t.start()
    time.sleep(0.5)
    bridge.answer(bridge_dir, bridge.pending(bridge_dir)[0]['id'], 'denied', 'human')
    t.join(5)
    check(got.get('res', {}).get('decision') == 'denied', 'bridge: a denial reaches the agent side')
    try:
        bridge.submit(bridge_dir, 'root', cmd='never answered', timeout=0.4, poll=0.05)
        check(False, 'bridge: submit times out without an answer')
    except TimeoutError:
        check(True, 'bridge: submit times out without an answer')

    # execute_root: exactly the approved string, under sudo, argv-safe
    tr2 = FakeTransport(rc=0, out='root says hi\n')
    r = bridge.execute_root('rm -rf /x', runner=tr2)
    check(tr2.calls[0][0] == ['sudo', 'sh', '-c', 'rm -rf /x'], 'bridge: root runs as `sudo sh -c <approved string>`')

    # audit
    audit.log(settings.audit_path(), {'event': 'run', 'cmd': 'id'})
    audit.log(settings.audit_path(), {'event': 'root_decision', 'decision': 'denied'})
    ev = audit.tail(settings.audit_path(), 10)
    check([e['event'] for e in ev] == ['run', 'root_decision'], 'audit: appends and tails in order')
    check(audit.tail(os.path.join(S, 'missing.jsonl')) == [], 'audit: a missing log is empty')

    # agent side: the installed script is valid python with the bridge baked in
    src = agent_side.ask_root_source(bridge_dir)
    check(bridge_dir in src, 'agent_side: the bridge path is baked into ask-root')
    compile(src, 'ask-root', 'exec')
    check(True, 'agent_side: ask-root compiles')
    cmd = agent_side.install_command(bridge_dir, settings.agent_bin(), prompt.text(), settings.robots_path(),
                                     prompt.MEMORY_TEMPLATE, settings.memory_path())
    check('base64 -d' in cmd and settings.agent_bin() in cmd and settings.robots_path() in cmd
          and settings.memory_path() in cmd,
          'agent_side: install command carries ask-root, ROBOTS.md and MEMORY.md base64')
    for name, src in (('env-ask', agent_side.ENV_ASK_SOURCE), ('env-note', agent_side.ENV_NOTE_SOURCE)):
        baked = src.replace('{bridge!r}', repr(bridge_dir))
        compile(baked, name, 'exec')
        check(f'/{name}' in cmd, f'agent_side: install command carries {name}')

    # the agent's own instructions
    text = prompt.text()
    check(os.path.exists(prompt.ROBOTS), 'prompt: ROBOTS.md ships with the package')
    check('ask-root' in text and 'whoami' in text and 'sudo' in text,
          'prompt: ROBOTS.md tells the agent what it is, that it cannot sudo, and how to ask for root')
    import io
    from contextlib import redirect_stdout
    buf = io.StringIO()
    with redirect_stdout(buf):
        rc = cmd_env.main(['prompt'])
    check(rc == 0 and buf.getvalue() == text, 'cli: env prompt prints ROBOTS.md')

    # CLI wiring
    from caminus import __main__
    check('env' in __main__.COMMANDS, 'cli: env is a command')
    tr3 = FakeTransport(rc=0, out='hi\n')
    rc = cmd_env.main(['run', '--', 'echo', 'hi'], transport=tr3)
    check(rc == 0 and tr3.calls[0][1]['input'] == 'echo hi', 'cli: env run executes as the agent user and returns its code')
    check(audit.tail(settings.audit_path(), 1)[0]['event'] == 'run', 'cli: env run writes the audit log')
    rc = cmd_env.main(['audit', '--tail', '5'])
    check(rc == 0, 'cli: env audit prints the log')
    rc = cmd_env.main(['run', '--shell', 'echo hi'], transport=tr3)
    check(tr3.calls[-1][1]['input'] == 'echo hi', 'cli: env run --shell takes one shell string')
    check(cmd_env._parser().parse_args(['ui']).sub == 'ui', 'cli: env ui is a subcommand (the TUI)')

    # bridge: typed requests, steering notes
    got2 = {}
    before = {r['id'] for r in bridge.pending(bridge_dir)}
    t = threading.Thread(target=lambda: got2.update(res=bridge.submit(bridge_dir, 'question',
                                                                       text='which model?', choices=['coder', 'swift'], timeout=10)))
    t.start()
    deadline = time.time() + 5
    q = None
    while time.time() < deadline:
        fresh = [r for r in bridge.pending(bridge_dir) if r['id'] not in before]
        if fresh:
            q = fresh[0]
            break
        time.sleep(0.05)
    check(q is not None, 'bridge: the question request appeared')
    check(q['kind'] == 'question' and q['text'] == 'which model?' and q['choices'] == ['coder', 'swift'],
          'bridge: question requests carry text and choices')
    bridge.answer(bridge_dir, q['id'], 'answered', 'human', answer_text='swift')
    t.join(5)
    check(got2.get('res', {}).get('answer') == 'swift' and got2['res']['decision'] == 'answered',
          'bridge: the answer reaches the asker')
    nid = bridge.say(bridge_dir, 'focus on TP', 'human')
    check([n['text'] for n in bridge.notes(bridge_dir)] == ['focus on TP'], 'bridge: say leaves a note')
    drained = bridge.drain_notes(bridge_dir)
    check(len(drained) == 1 and drained[0]['id'] == nid and bridge.notes(bridge_dir) == [],
          'bridge: drain_notes reads and clears the inbox')

    # ctx: parses /props with a fake opener
    from caminus.env import ctx
    import io
    class FakeResp(io.BytesIO):
        def __enter__(self): return self
        def __exit__(self, *a): return False
    def fake_opener(url, timeout=0):
        port = int(url.rsplit(':', 1)[1].split('/')[0])
        if port != 8092:
            raise OSError('no server')
        return FakeResp(json.dumps({'model_alias': 'coder', 'n_ctx': 262144}).encode())
    rows = ctx.servers(opener=fake_opener)
    check(rows == [{'port': 8092, 'model': 'coder', 'n_ctx': 262144}], 'ctx: reads model and n_ctx from /props')

    for sub in (['ask', 'q?'], ['say', 'note'], ['notes'], ['ctx'], ['reqs'], ['answer', 'x', 'denied'], ['memory'], ['remember', 'm']):
        check(cmd_env._parser().parse_args(sub).sub == sub[0], f'cli: env {sub[0]} is a subcommand')

    # jobs: detached-process script builders
    s = jobs.spawn_script('build', ['make', '-j4', 'a b'])
    check('nohup setsid' in s and "bash -c 'make -j4 '\"'\"'a b'\"'\"''" in s and '~/env-logs/build.log' in s
          and '~/env-jobs/build.pid' in s, 'jobs: spawn script detaches with nohup+setsid, log and pid files')
    check('kill -0' in jobs.jobs_script() and 'alive' in jobs.jobs_script(), 'jobs: jobs script probes liveness')
    check('kill "$pid"' in jobs.stop_script('build'), 'jobs: stop script kills the pid')
    for bad in ('', 'a/b', 'x;rm -rf /', 'a b'):
        try:
            jobs.check_name(bad)
            check(False, f'jobs: name {bad!r} accepted')
        except ValueError:
            pass
    check(True, 'jobs: bad names rejected')
    check(cmd_env._parser().parse_args(['spawn', '-n', 'x', '--', 'make']).sub == 'spawn'
          and cmd_env._parser().parse_args(['jobs']).sub == 'jobs'
          and cmd_env._parser().parse_args(['stop', 'x']).sub == 'stop', 'cli: spawn/jobs/stop subcommands')

    # pending() distinguishes a missing bridge from an unreadable one
    unreadable = os.path.join(S, 'noperm')
    os.makedirs(os.path.join(unreadable, 'req'))
    os.chmod(os.path.join(unreadable, 'req'), 0)
    try:
        bridge.pending(unreadable)
        check(False, 'bridge: pending raises on an unreadable bridge dir')
    except PermissionError:
        check(True, 'bridge: pending raises on an unreadable bridge dir')
    finally:
        os.chmod(os.path.join(unreadable, 'req'), 0o700)

    print(f'RESULT {"PASS" if not FAILS else "FAIL"} ({len(FAILS)} failed)')
    return 1 if FAILS else 0


if __name__ == '__main__':
    sys.exit(main())
