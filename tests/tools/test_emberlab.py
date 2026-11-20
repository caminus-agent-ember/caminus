"""CPU test of tools/emberlab (the Ember lab): scripted brains on the local host, no model, no GPU, no SSH.

  python -I tests/tools/test_emberlab.py --scratch DIR

Checks:
  suite      the basic suite's scripted solver passes every check through the real resident and its gates (a harness
             change that breaks a solvable task shows here); the claiming script never passes a check, and each of its
             finished goals is counted as a false completion
  resume     a second `run` into the same folder runs nothing again
  compare    two arms over the same runs give "no difference"; the across-the-board rule on made-up cells
  session    a driven lab session: the request the model would see waits for us, our tool call reaches the resident,
             the timings a driven:caminus brain sends are the resident's engine speed, an interrupt closes the waiting call (the driven brain records the hang-up),
             close stops the daemon and the job workers it leaves behind (KILL after the grace, named), never a
             process a stale state file names
  tui        (with tmux) keys in, an exact screen and its SVG out; --read-only refuses a goal before anything runs
"""
import argparse
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import time

ROOT = Path(__file__).resolve().parents[2]
LAB = [sys.executable, '-I', str(ROOT / 'tools/emberlab/lab.py')]
TUI = [sys.executable, '-I', str(ROOT / 'tools/emberlab/tui.py')]
ap = argparse.ArgumentParser(); ap.add_argument('--scratch', required=True)
S = Path(ap.parse_args().scratch).resolve(); S.mkdir(parents=True, exist_ok=True)
FAILS = []
sys.path.insert(0, str(ROOT / 'tools/emberlab'))
import report  # noqa: E402


def check(ok, what):
    print(('PASS ' if ok else 'FAIL ') + what, flush=True)
    if not ok:
        FAILS.append(what)


def lab(*a, timeout=600):
    return subprocess.run(LAB + list(a), capture_output=True, text=True, timeout=timeout)


# ---- suite
out = S / 'run'
p = lab('run', '--suite', 'basic', '--brain', 'scripted:solve', '--brain', 'scripted:claim', '--out', str(out))
rows = report.load(out)
solve = [r for r in rows if r['brain'] == 'scripted:solve']
claim = [r for r in rows if r['brain'] == 'scripted:claim']
check(p.returncode == 0 and len(rows) == 10, f'suite: 10 runs recorded, exit 0 ({p.returncode}; {p.stderr[-300:]})')
check(len(solve) == 5 and all(r['check'] and r['finished'] and not r['false_completion'] for r in solve),
      'suite: the scripted solver passes all five checks through the gates: '
      + ', '.join(f"{r['task']}={r.get('state')}/{r.get('check')}" for r in solve))
check(len(claim) == 5 and not any(r['check'] for r in claim) and all(r['false_completion'] == r['finished'] for r in claim)
      and any(r['false_completion'] for r in claim),   # (vacuous if no claim goal ever finished)
      'suite: the claiming script passes no check; every goal it finished is a false completion')
check(all((out / 'runs' / r['id'] / f).exists() for r in rows for f in ('events.jsonl', 'check.out', 'run.json')),
      'suite: each run keeps its events, check output and record')
check(all(r.get('commit') and 'dirty' in r for r in rows), 'suite: each record names the runtime commit and dirty flag')

# ---- resume
p = lab('run', '--suite', 'basic', '--brain', 'scripted:solve', '--out', str(out))
check('5 runs (0 to do)' in p.stdout and len((out / 'results.jsonl').read_text().splitlines()) == 10,
      'resume: a second run into the same folder runs nothing again')
res = out / 'results.jsonl'
lines = res.read_text().splitlines()
first = json.loads(lines[0]); first.update(state='error', error='ssh dropped')
res.write_text('\n'.join([json.dumps(first)] + lines[1:]) + '\n')
p = lab('run', '--suite', 'basic', '--brain', 'scripted:' + first['brain'].split(':')[-1], '--out', str(out))
check('(1 to do)' in p.stdout and {r['id']: r for r in report.load(out)}[first['id']].get('state') != 'error',
      f'resume: a run that ended in error runs again ({p.stdout[-200:]})')

# ---- compare
arms = S / 'arms.json'
arms.write_text(json.dumps({'base': {}, 'b': {'config': {'max_tokens': 2048}}}))
out2 = S / 'ab'
p = lab('run', '--suite', 'basic', '--brain', 'scripted:solve', '--arms', str(arms), '--only', 'edit,read', '--out', str(out2))
c = report.compare(report.load(out2), 'base', 'b')
check(p.returncode == 0 and c['verdict'].startswith('no difference'), f'compare: same runs on two arms: {c["verdict"]}')
mk = lambda brain, arm, n, ok, false=0: [dict(id=f'{brain}{arm}{i}', brain=brain, arm=arm, repeat=i, task='t', check=i < ok,
                                             finished=i < ok + false, false_completion=ok <= i < ok + false) for i in range(n)]
v = lambda rows: report.compare(rows, 'A', 'B')['verdict']
check(v(mk('x', 'A', 3, 1) + mk('x', 'B', 3, 2) + mk('y', 'A', 3, 2) + mk('y', 'B', 3, 2)).startswith('better across the board'),
      'compare: one brain up, none down = better across the board')
check(v(mk('x', 'A', 3, 1) + mk('x', 'B', 3, 3) + mk('y', 'A', 3, 2) + mk('y', 'B', 3, 1)).startswith('worse for y'),
      'compare: a brain that drops makes the arm worse, whatever the others gain')
check(v(mk('x', 'A', 3, 2) + mk('x', 'B', 3, 2, false=1)).startswith('worse for x'),
      'compare: more false completions at the same pass rate is worse')
errored = mk('x', 'B', 3, 2)
errored[2].update(state='error')                # an infrastructure failure, not a failed check of the harness
c2 = report.compare(mk('x', 'A', 3, 2) + errored, 'A', 'B')
check(c2['brains'][0]['arm']['runs'] == 2 and c2['errored'] == ['xB2'] and 'left out' in report.compare_md(c2),
      f'compare: a run that ended in error is left out and named, not a failed check ({c2["verdict"]})')

# ---- session (driven: we are the model)
so = S / 'sessions'
p = lab('open', 'd', '--brain', 'driven:caminus', '--host', 'local', '--fixture', 'basic', '--out', str(so), '--ttl', '300')
check(p.returncode == 0, f'session: open ({p.stderr[-300:]})')
bad = lab('say', 'd', 'a steer for no goal', '--goal', 'zzzz', '--out', str(so))
check(bad.returncode != 0 and 'matches 0 goals' in (bad.stderr + bad.stdout) and 'sent (' not in bad.stdout,
      'session: say --goal with an unknown short id is refused, not reported as sent')
lab('say', 'd', 'List the files in your work folder.', '--out', str(so))
t = lab('turn', 'd', '--out', str(so), '--wait', '20')
check(t.returncode == 0 and 'terminal' in t.stdout and 'List the files' in t.stdout,
      'session: the waiting request shows the tools offered and our goal')
r = lab('reply', 'd', '--out', str(so), '--call', 'read', '--args', json.dumps({'path': 'work/inventory.py'}), '--settle', '3', '--width', '4000',
        '--timings', json.dumps({'prompt_n': 1488, 'prompt_per_second': 1240.2, 'predicted_n': 500, 'predicted_per_second': 62.4}))
check('read' in r.stdout and 'total_value' in r.stdout, 'session: our tool call ran in the resident and its result came back')
s = json.loads((so / 'sessions/d/session.json').read_text())
tp = Path(s['home']) / 'throughput.json'
tp = json.loads(tp.read_text()) if tp.exists() else {}
check((tp.get('source'), tp.get('tps'), tp.get('prompt_tps')) == ('engine', 62.4, 1240.2),
      f'session: a driven:caminus brain plays a caminus serve, and the resident takes its timings as the engine speed ({tp})')
# an Esc while the next request waits: the resident closes the call, and the driven brain records the hang-up
t = lab('turn', 'd', '--out', str(so), '--wait', '20')
BOOT = "import sys;sys.path.insert(0,sys.argv[1]);from caminus.__main__ import main;sys.argv=sys.argv[1:];sys.exit(main())"
agent = [sys.executable, '-I', '-c', BOOT, str(ROOT / 'python'), 'ember', 'agent', '--local']
st = subprocess.run(agent + ['status', 'lab', '--home', s['home'], '--json'], capture_output=True, text=True, timeout=60)
gid = json.loads(st.stdout)['goals'][-1]['id'] if st.returncode == 0 else ''
it = subprocess.run(agent + ['interrupt', 'lab', '--home', s['home'], gid, '--json'], capture_output=True, text=True, timeout=60)
gone = []
for _ in range(50):
    gone = sorted(Path(s['driven']).glob('*.gone.json'))
    if gone:
        break
    time.sleep(0.2)
t2 = lab('turn', 'd', '--out', str(so), '--wait', '1')
check(t.returncode == 0 and it.returncode == 0 and '"queued": true' in it.stdout and gone and 'hung up' in t2.stdout,
      f'session: an interrupt closes the waiting call and the driven brain records the hang-up ({it.stdout.strip()} {t2.stdout.strip()[:200]})')
# close: box and terminal workers outlive their daemon by design, so close ends them too. Fake jobs in the session's
# home: a worker that ignores TERM (close must KILL it), one that obeys, their commands (each its own group, as the
# resident starts them), and a stale state.json naming a process that is not the session's (a recycled pid: left alone).
procs = []


def spawn(code, *args, env=None):
    p = subprocess.Popen([sys.executable, '-I', '-c', code, *args], start_new_session=True, env=env)
    procs.append(p)
    return p.pid


def gone(pid):
    p = next((p for p in procs if p.pid == pid), None)
    if p is not None:
        return p.poll() is not None
    try:
        return open(f'/proc/{pid}/stat').read().rsplit(')', 1)[1].split()[0] in 'ZX'
    except OSError:
        return True


IGNORE_TERM = 'import signal,time;signal.signal(signal.SIGTERM,signal.SIG_IGN);time.sleep(120)'
SLEEP = 'import time;time.sleep(120)'
home = Path(s['home'])
stubborn, polite, stale = (home / 'jobs' / (c * 32) for c in 'abc')
command_env = {**os.environ, 'WORKSPACE': str(home)}
jobs = {stubborn: (spawn(IGNORE_TERM, str(stubborn)), spawn(SLEEP, env=command_env)),
        polite: (spawn(SLEEP, str(polite)), spawn(SLEEP, env=command_env)),
        stale: (spawn(SLEEP), None)}
for job, (worker, cmd) in jobs.items():
    job.mkdir(parents=True)
    (job / 'state.json').write_text(json.dumps(dict(phase='running', worker=worker, pid=cmd)))
# and one real terminal job, started the way the resident starts it (its own worker, command and paths)
sys.path.insert(0, str(ROOT / 'python'))
from caminus.ember.resident.terminal import Terminal  # noqa: E402
real = Terminal(home).call('d' * 32, dict(command='sleep 120', yield_ms=500))
daemon = json.loads((home / 'heartbeat.json').read_text())['pid']
time.sleep(0.5)
t0 = time.monotonic()
c = lab('close', 'd', '--out', str(so), timeout=120)
took = time.monotonic() - t0
time.sleep(0.5)
after = c.stdout.split('had to SIGKILL')[-1]
check(real.get('running') and c.returncode == 0
      and all(gone(p) for p in [daemon, *jobs[stubborn], *jobs[polite], real['worker'], real['pid']]),
      f'session: close ends the daemon, the job workers and their commands ({c.stdout.strip()} {c.stderr[-300:]})')
check('had to SIGKILL' in c.stdout and f'worker {"a" * 8} ({jobs[stubborn][0]})' in after
      and f'worker {"b" * 8}' not in after and took < 60,
      f'session: close says what it had to SIGKILL after the grace, and only that ({took:.0f} s)')
check(not gone(jobs[stale][0]), "session: close leaves alone a process a stale state.json names that is not the session's")
check((so / 'sessions/d/session.closed.json').exists(), 'session: close files the session as closed')
try:
    os.killpg(s['daemon_pid'], 0)
    alive = True
except ProcessLookupError:
    alive = False
check(not alive, 'session: close stops the daemon')
for p in procs:                 # the test's own fake processes: the decoy, and anything a failed close left
    if p.poll() is None:
        p.kill()
    p.wait()
for pid in (real.get('worker'), real.get('pid')):
    try:
        os.killpg(pid, 9) if pid and not gone(pid) else None
    except OSError:
        pass

# ---- propose
ph = S / 'simhome'
ph.mkdir(parents=True, exist_ok=True)
sys.path.insert(0, str(ROOT / 'python'))
sys.path.insert(0, str(ROOT / 'tools/emberlab'))
import proposal  # noqa: E402


def sim_record(home, **fields):
    """A `proposed` record in a scratch home: the reflection's output before anyone has measured it."""
    sim, store, rt = proposal._open(home)
    try:
        return sim.record(rt, 'g-propose', dict(dict(problem_group='propose test', change='turn sim off in the arm',
                                                     predicted='no difference on one task'), **fields),
                          cfg=dict(model='m', provider='p'))
    finally:
        store.close()


r1 = sim_record(ph)
po = S / 'propose'
p = lab('propose', '--sim-home', str(ph), '--record', r1['id'], '--brain', 'scripted:solve',
        '--config', '{"sim": false}', '--only', 'read', '--out', str(po), '--dry-run')
check(p.returncode == 0 and f"proposal {r1['id']}" in p.stdout and 'runs: 2' in p.stdout and not po.exists(),
      f'propose: --dry-run prints the plan and creates nothing ({p.returncode}; {p.stdout[-300:]})')
p = lab('propose', '--sim-home', str(ph), '--record', r1['id'], '--brain', 'anthropic:8091#claude-haiku-5-5',
        '--config', '{"sim": false}', '--out', str(po))
check(p.returncode != 0 and '--approve-paid' in (p.stderr + p.stdout) and not po.exists(),
      f'propose: a brain billed through the relay does not run without --approve-paid ({p.returncode}; '
      f'{(p.stderr + p.stdout)[-300:]})')
p = lab('propose', '--sim-home', str(ph), '--record', r1['id'], '--brain', 'scripted:solve', '--out', str(po))
check(p.returncode != 0 and 'measure nothing' in (p.stderr + p.stdout),
      'propose: two identical arms are refused (the proposal names no change)')
p = lab('propose', '--sim-home', str(ph), '--record', r1['id'], '--brain', 'scripted:solve',
        '--config', '{"sim": false}', '--only', 'read', '--out', str(po))
closed = proposal.load_record(ph, r1['id'])
arm_home = [json.loads((po / 'runs' / r['id'] / 'home' / 'config.json').read_text())
            for r in report.load(po) if r['arm'] == 'arm']
base_home = [json.loads((po / 'runs' / r['id'] / 'home' / 'config.json').read_text())
             for r in report.load(po) if r['arm'] == 'base']
check(p.returncode == 0 and closed['status'] == 'observed' and 'no difference' in closed['observed']
      and 'scripted:solve 1/1 -> 1/1' in closed['observed'],
      f'propose: the runs close the record with the per-brain numbers ({p.returncode}; '
      f'{closed["observed"][:200]}; {p.stderr[-200:]})')
check(closed['observed_by']['model'] == 'emberlab' and closed['brain']['model'] == 'm',
      'propose: the observation names emberlab, the predictor keeps the brain that made the prediction')
check(arm_home and base_home and all(c.get('sim') is False for c in arm_home) and all('sim' not in c for c in base_home),
      'propose: the arm run home has sim=false and the base run home does not (the proposal is the only difference)')
p = lab('propose', '--sim-home', str(ph), '--record', r1['id'], '--brain', 'scripted:solve',
        '--config', '{"sim": false}', '--only', 'read', '--out', str(po))
check(p.returncode != 0 and 'already observed' in (p.stderr + p.stdout),
      'propose: a closed record is not measured again')
p = lab('propose', '--sim-home', str(ph), '--record', 'nostore', '--brain', 'scripted:solve',
        '--config', '{"sim": false}', '--out', str(po))
check(p.returncode != 0 and 'no SIM record' in (p.stderr + p.stdout),
      'propose: a record the log does not hold is refused before anything runs')

# ---- tui
if shutil.which('tmux'):
    to = S / 'tui'
    p = subprocess.run(TUI + ['--out', str(to), '--size', '60x8', '--cmd', "printf 'ready\\n'; read x; echo got:$x; sleep 20",
                              'wait:ready', 'type:hi', 'key:Enter', 'wait:got:hi', 'shot:s'], capture_output=True, text=True, timeout=60)
    check(p.returncode == 0 and 'got:hi' in (to / 's.txt').read_text() and (to / 's.svg').read_text().startswith('<svg'),
          'tui: keys in, the exact screen and its SVG out')
else:
    print('SKIP tui: tmux is not installed')
p = subprocess.run(TUI + ['--out', str(S / 'ro'), '--cmd', 'true', '--read-only', 'key:BTab', 'type:fix the bug', 'key:Enter'],
                   capture_output=True, text=True, timeout=30)
check(p.returncode == 2 and 'type:fix the bug' in p.stdout and 'key:BTab' not in p.stdout.split('refused')[1],
      'tui: --read-only refuses a goal (and the Enter after it) before anything runs')

print('RESULT', 'FAIL' if FAILS else 'PASS')
sys.exit(1 if FAILS else 0)
