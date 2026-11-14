"""The root bridge (docs/specs/spec-agent-env.md section 4): the agent user files a request, only the invoking user's
side answers it and executes it under sudo. Files are written atomically (tmp + rename) with umask 007 so the other
user in BRIDGE_GROUP can read them; the harness never trusts a request's content beyond showing it to the human.

  req/<id>.json   {'id', 'pid', 'user', 'cwd', 'cmd', 'ts'}          (agent side writes)
  res/<id>.json   {'id', 'decision', 'cmd', 'approved_by', 'rc', 'out', 'err', 'ts'}   (harness side writes)
"""
import json
import os
import subprocess
import time
import uuid

REQ, RES = 'req', 'res'


def _dirs(bridge):
    return os.path.join(bridge, REQ), os.path.join(bridge, RES)


def _write_atomic(path, obj):
    os.makedirs(os.path.dirname(path), exist_ok=True)
    tmp = f'{path}.tmp.{os.getpid()}'
    with open(tmp, 'w') as f:
        json.dump(obj, f)
        f.flush()
        os.fsync(f.fileno())
    os.replace(tmp, path)


def submit(bridge, cmd, cwd=None, timeout=3600.0, poll=0.2):
    """agent side: file a root request and block until the human answers; returns the result dict
    (raises TimeoutError when nobody answers within timeout seconds)"""
    reqd, resd = _dirs(bridge)
    rid = uuid.uuid4().hex[:16]
    req = {'id': rid, 'pid': os.getpid(), 'user': _user(), 'cwd': cwd or os.getcwd(), 'cmd': cmd, 'ts': time.time()}
    _write_atomic(os.path.join(reqd, rid + '.json'), req)
    deadline = time.time() + timeout
    while time.time() < deadline:
        p = os.path.join(resd, rid + '.json')
        if os.path.exists(p):
            with open(p) as f:
                return json.load(f)
        time.sleep(poll)
    raise TimeoutError(f'no answer to root request {rid} within {timeout:g}s')


def pending(bridge):
    """harness side: the requests without an answer yet, oldest first"""
    reqd, resd = _dirs(bridge)
    out = []
    if not os.path.isdir(reqd):
        return out
    for name in sorted(os.listdir(reqd)):
        if not name.endswith('.json'):
            continue
        rid = name[:-len('.json')]
        if os.path.exists(os.path.join(resd, rid + '.json')):
            continue
        with open(os.path.join(reqd, name)) as f:
            out.append(json.load(f))
    return out


def answer(bridge, rid, decision, actor, cmd=None, rc=None, out='', err=''):
    """harness side: write the answer file; decision is 'approved', 'edited' or 'denied'"""
    _write_atomic(os.path.join(_dirs(bridge)[1], rid + '.json'),
                  {'id': rid, 'decision': decision, 'cmd': cmd, 'approved_by': actor,
                   'rc': rc, 'out': out, 'err': err, 'ts': time.time()})


def execute_root(cmd, runner=subprocess.run):
    """harness side: run exactly the string the human approved, as the invoking user under sudo (sudo asks for the
    human's password on the controlling terminal); returns the CompletedProcess"""
    return runner(['sudo', 'sh', '-c', cmd], capture_output=True, text=True)


def _user():
    import getpass
    return getpass.getuser()
