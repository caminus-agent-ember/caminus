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
import re
import tempfile
from .resident import approval

REQ, RES = 'req', 'res'


def _dirs(bridge):
    return os.path.join(bridge, REQ), os.path.join(bridge, RES)


def _write_atomic(path, obj):
    os.makedirs(os.path.dirname(path), exist_ok=True)
    tmp = None
    try:
        with tempfile.NamedTemporaryFile(mode='w', dir=os.path.dirname(path), prefix='.bridge-', delete=False) as f:
            tmp=f.name
            os.fchmod(f.fileno(),0o660)
            os.fchown(f.fileno(),-1,os.stat(os.path.dirname(path)).st_gid)
            json.dump(obj, f)
            f.flush()
            os.fsync(f.fileno())
        os.replace(tmp, path)
    finally:
        if tmp and os.path.exists(tmp):os.unlink(tmp)


def check_id(rid):
    if not isinstance(rid,str) or not re.fullmatch(r'[A-Za-z0-9][A-Za-z0-9_-]{0,127}',rid):
        raise ValueError('invalid bridge request id')


def submit(bridge, kind, cmd=None, text=None, choices=None, cwd=None, timeout=3600.0, poll=0.2):
    """agent side: file a request and block until the human answers; returns the result dict.
    kind 'root' carries cmd, kind 'question' carries text and optional choices.
    (raises TimeoutError when nobody answers within timeout seconds)"""
    reqd, resd = _dirs(bridge)
    rid = uuid.uuid4().hex[:16]
    req = {'id': rid, 'pid': os.getpid(), 'user': _user(), 'cwd': cwd or os.getcwd(), 'kind': kind, 'ts': time.time()}
    if kind == 'root':
        req['cmd'] = cmd
    if kind == 'question':
        req['text'] = text
        if choices:
            req['choices'] = list(choices)
    _write_atomic(os.path.join(reqd, rid + '.json'), req)
    deadline = time.time() + timeout
    while time.time() < deadline:
        p = os.path.join(resd, rid + '.json')
        if os.path.exists(p):
            with open(p) as f:
                result=json.load(f)
            if result.get('decision')!='executing':return result
        time.sleep(poll)
    raise TimeoutError(f'no answer to {kind} request {rid} within {timeout:g}s')


def pending(bridge):
    """harness side: the requests without an answer yet, oldest first (raises when the bridge dir exists but
    cannot be read — e.g. this session predates the bridge group)"""
    reqd, resd = _dirs(bridge)
    out = []
    if not os.path.isdir(reqd):
        if os.path.exists(reqd):
            raise PermissionError(f'{reqd} exists but is not readable (re-login to pick up the bridge group)')
        return out
    for name in sorted(os.listdir(reqd)):
        if not name.endswith('.json'):
            continue
        rid = name[:-len('.json')]
        try:check_id(rid)
        except ValueError:continue
        if os.path.exists(os.path.join(resd, rid + '.json')):
            continue
        try:out.append(approval.snapshot(bridge,rid))
        except ValueError:continue
    return out


def answer(bridge, rid, decision, actor, cmd=None, rc=None, out='', err='', answer_text='',request=None):
    """harness side: write the answer file; decision is 'approved', 'edited', 'denied' or 'answered'"""
    check_id(rid)
    return approval.answer(bridge,request or approval.snapshot(bridge,rid),decision,actor,
                           cmd=cmd,rc=rc,out=out,err=err,answer=answer_text)


claim=approval.claim
finish=approval.finish


def say(bridge, text, actor):
    """harness side: leave a steering note for the agent (notes/<id>.json; the agent's env-note drains them)"""
    nid = uuid.uuid4().hex[:16]
    _write_atomic(os.path.join(bridge, 'notes', nid + '.json'),
                  {'id': nid, 'from': actor, 'text': text, 'ts': time.time()})
    return nid


def notes(bridge):
    """the undrained steering notes, oldest first"""
    nd = os.path.join(bridge, 'notes')
    out = []
    if not os.path.isdir(nd):
        return out
    for name in sorted(os.listdir(nd)):
        if name.endswith('.json'):
            with open(os.path.join(nd, name)) as f:
                out.append(json.load(f))
    return out


def drain_notes(bridge):
    """agent side: read and remove all pending notes; returns them oldest first"""
    nd = os.path.join(bridge, 'notes')
    out = notes(bridge)
    for n in out:
        try:
            os.remove(os.path.join(nd, n['id'] + '.json'))
        except OSError:
            pass
    return out


def execute_root(cmd, runner=subprocess.run):
    """harness side: run exactly the string the human approved, as the invoking user under sudo (sudo asks for the
    human's password on the controlling terminal); returns the CompletedProcess"""
    return runner(['sudo', 'sh', '-c', cmd], capture_output=True, text=True)


def _user():
    import getpass
    return getpass.getuser()
