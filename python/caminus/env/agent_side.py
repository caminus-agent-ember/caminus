"""The agent-side half of the bridge (docs/specs/spec-agent-env.md section 4): a standalone stdlib-only script that
`caminus env setup --agent-side` installs into the agent user's ~/bin/ask-root. It files a root request and waits for
the human's answer; the bridge path is baked in at install time."""
import base64
import os

# written verbatim into the agent user's home; talks only to BRIDGE (req/ and res/ subdirectories)
ASK_ROOT_SOURCE = '''#!/usr/bin/env python3
"""ask-root CMD... : ask the human to run CMD as root (caminus env watch answers). Prints the command's output and
exits with its exit code; 124 on timeout, 126 when denied."""
import json, os, sys, time, uuid

BRIDGE = {bridge!r}
REQ, RES = 'req', 'res'
TIMEOUT = float(os.environ.get('CAMINUS_ENV_ROOT_TIMEOUT', '3600'))


def _write(path, obj):
    os.makedirs(os.path.dirname(path), exist_ok=True)
    tmp = path + '.tmp.' + str(os.getpid())
    with open(tmp, 'w') as f:
        json.dump(obj, f)
    os.replace(tmp, path)


def main():
    os.umask(0o007)
    cmd = ' '.join(sys.argv[1:])
    if not cmd:
        print('ask-root: no command given', file=sys.stderr)
        return 2
    reqd = os.path.join(BRIDGE, REQ)
    rid = uuid.uuid4().hex[:16]
    _write(os.path.join(reqd, rid + '.json'),
           {'id': rid, 'pid': os.getpid(), 'user': os.environ.get('USER', ''), 'cwd': os.getcwd(), 'cmd': cmd, 'ts': time.time()})
    deadline = time.time() + TIMEOUT
    while time.time() < deadline:
        p = os.path.join(BRIDGE, RES, rid + '.json')
        if os.path.exists(p):
            with open(p) as f:
                res = json.load(f)
            break
        time.sleep(0.2)
    else:
        print(f'ask-root: no answer within {TIMEOUT:g}s (request {rid})', file=sys.stderr)
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


def ask_root_source(bridge):
    """the script text with the bridge path baked in"""
    return ASK_ROOT_SOURCE.replace('{bridge!r}', repr(bridge))


def install_command(bridge, bin_path):
    """the shell string that installs ask-root into the agent user's home over SSH (base64: no quoting crosses the wire)"""
    payload = base64.b64encode(ask_root_source(bridge).encode()).decode()
    d = os.path.dirname(bin_path)
    return f"mkdir -p {d} && printf %s {payload} | base64 -d > {bin_path} && chmod 700 {bin_path}"
