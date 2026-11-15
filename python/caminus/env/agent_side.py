"""The agent-side half of the environment (docs/specs/spec-agent-env.md sections 4 and 10): standalone stdlib-only
scripts that `caminus env setup --agent-side` installs into the agent user's ~/bin. They talk only to the bridge
directory. BRIDGE is baked in at install time.

  ask-root CMD...        ask the human to run CMD as root (exit 0/rc, 126 denied, 124 timeout)
  env-ask TEXT CHOICE... ask the human a question, optionally with choices (prints the answer)
  env-note               print and clear the steering notes left with `caminus env say`
"""
import base64
import os

_COMMON = '''
import json, os, sys, time, uuid

BRIDGE = {bridge!r}
TIMEOUT = float(os.environ.get('CAMINUS_ENV_TIMEOUT', '3600'))


def _write(path, obj):
    os.makedirs(os.path.dirname(path), exist_ok=True)
    tmp = path + '.tmp.' + str(os.getpid())
    with open(tmp, 'w') as f:
        json.dump(obj, f)
    os.replace(tmp, path)


def _ask(kind, extra):
    os.umask(0o007)
    rid = uuid.uuid4().hex[:16]
    req = {'id': rid, 'pid': os.getpid(), 'user': os.environ.get('USER', ''),
           'cwd': os.getcwd(), 'kind': kind, 'ts': time.time()}
    req.update(extra)
    _write(os.path.join(BRIDGE, 'req', rid + '.json'), req)
    deadline = time.time() + TIMEOUT
    while time.time() < deadline:
        p = os.path.join(BRIDGE, 'res', rid + '.json')
        if os.path.exists(p):
            with open(p) as f:
                return json.load(f)
        time.sleep(0.2)
    print(f'no answer within {TIMEOUT:g}s (request {rid})', file=sys.stderr)
    return None
'''

ASK_ROOT_SOURCE = '''#!/usr/bin/env python3
"""ask-root CMD... : ask the human to run CMD as root (caminus env ui / watch answers)."""
''' + _COMMON + '''

def main():
    cmd = ' '.join(sys.argv[1:])
    if not cmd:
        print('ask-root: no command given', file=sys.stderr)
        return 2
    res = _ask('root', {'cmd': cmd})
    if res is None:
        return 124
    if res['decision'] == 'denied':
        print(f"ask-root: denied by {res.get('approved_by', '?')}: {cmd}", file=sys.stderr)
        return 126
    sys.stdout.write(res.get('out') or '')
    sys.stderr.write(res.get('err') or '')
    return res.get('rc') if res.get('rc') is not None else 0


if __name__ == '__main__':
    sys.exit(main())
'''

ENV_ASK_SOURCE = '''#!/usr/bin/env python3
"""env-ask TEXT CHOICE... : ask the human a question; prints the answer (or the chosen choice)."""
''' + _COMMON + '''

def main():
    if len(sys.argv) < 2:
        print('env-ask: TEXT [CHOICE...]', file=sys.stderr)
        return 2
    text, choices = sys.argv[1], sys.argv[2:]
    res = _ask('question', {'text': text, 'choices': choices})
    if res is None:
        return 124
    if res['decision'] == 'denied':
        print(f"env-ask: denied by {res.get('approved_by', '?')}", file=sys.stderr)
        return 126
    ans = res.get('answer') or ''
    if choices and ans.strip().isdigit() and 1 <= int(ans) <= len(choices):
        ans = choices[int(ans) - 1]
    print(ans)
    return 0


if __name__ == '__main__':
    sys.exit(main())
'''

ENV_NOTE_SOURCE = '''#!/usr/bin/env python3
"""env-note : print and clear the steering notes left with `caminus env say` (call between steps)."""
import json, os, sys

BRIDGE = {bridge!r}


def main():
    nd = os.path.join(BRIDGE, 'notes')
    if not os.path.isdir(nd):
        return 0
    for name in sorted(os.listdir(nd)):
        if not name.endswith('.json'):
            continue
        p = os.path.join(nd, name)
        try:
            with open(p) as f:
                n = json.load(f)
            os.remove(p)
            print(f"[{n.get('from', '?')}] {n.get('text', '')}")
        except OSError:
            pass
    return 0


if __name__ == '__main__':
    sys.exit(main())
'''


def _baked(source, bridge):
    return source.replace('{bridge!r}', repr(bridge))


def ask_root_source(bridge):
    return _baked(ASK_ROOT_SOURCE, bridge)


def install_command(bridge, bin_path, robots_source, robots_path, memory_source, memory_path):
    """the shell string that installs the agent-side scripts, ROBOTS.md and MEMORY.md over SSH (base64: no quoting
    crosses the wire). bin_path is ~/bin/ask-root; env-ask and env-note land beside it."""
    d = os.path.dirname(bin_path)
    parts = [f'mkdir -p {d} {os.path.dirname(robots_path)}']
    for name, src in (('ask-root', ask_root_source(bridge)),
                      ('env-ask', _baked(ENV_ASK_SOURCE, bridge)),
                      ('env-note', _baked(ENV_NOTE_SOURCE, bridge))):
        payload = base64.b64encode(src.encode()).decode()
        target = os.path.join(d, name)
        parts.append(f"printf %s {payload} | base64 -d > {target} && chmod 700 {target}")
    robots = base64.b64encode(robots_source.encode()).decode()
    parts.append(f"printf %s {robots} | base64 -d > {robots_path} && chmod 600 {robots_path}")
    mem = base64.b64encode(memory_source.encode()).decode()
    parts.append(f"[ -e {memory_path} ] || printf %s {mem} | base64 -d > {memory_path}")
    return ' && '.join(parts)
