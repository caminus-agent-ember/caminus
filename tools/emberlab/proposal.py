# SPDX-License-Identifier: AGPL-3.0-or-later
# Copyright (C) 2026 jdominick05
"""Measure a proposal: a `proposed` SIM record becomes two arms, and the runs close it.

Issue #341, step 3. The post-goal reflection writes what tool, rule or prompt change would have made a goal faster or
more reliable, as a SIM record with status `proposed`. This turns one into an experiment and appends what the lab
showed, so the record's prediction is judged by runs instead of by argument:

  base  the tree as it runs now
  arm   the same tree with the proposal applied: `--config` / `--env` overrides, or another `--arm-checkout`

Both arms run through `lab.py run` itself, so there is one run path, one results file and one verdict
(`report.compare`: an arm is better only if it is better across the board). The record then gets the outcome with
`sim.observe`, carrying the per-brain numbers the verdict rests on; `observed_by` names `emberlab`, the predictor's
own brain stays named in `brain`.

What runs is the owner's decision, not this command's default. The plan (suite, arms, brains, number of runs, which
brains bill) is printed first, and nothing starts until `--approve-paid` for a brain billed through the owner's relay
(a `anthropic:` or `openai+relay:` brain; the tool A/B brain is Haiku there), and `--unsafe-local` or `--unsafe-free`
for a real model on the local host. `--dry-run` stops after the plan.

  python -I tools/emberlab/lab.py propose --sim-home HOME --record ID --suite basic \\
      --brain anthropic:8091#claude-haiku-5-5 --repeats 3 --config '{"sim": false}'

The record says *what* to change and the command line says *how*, because a proposal in words is not a switch. A
proposal about code names `--arm-checkout DIR`: both arms then run on that checkout, and the diff between them is
still only the switch.
"""
from pathlib import Path
import json
import sys

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE))
import brains as brains_mod     # noqa: E402
import report as report_mod     # noqa: E402
import runner                   # noqa: E402

RESIDENT = Path(runner.REPO) / 'python'


def _rt(store):
    """A runtime-shaped object for the SIM module: it needs a store, a home and a config, not a live model."""
    return type('RT', (), {'store': store, 'home': store.home, 'config': {}})()


def _open(home):
    """The session's store, and the resident's SIM module from this lab's own tree (one log format, one writer)."""
    if str(RESIDENT) not in sys.path:
        sys.path.insert(0, str(RESIDENT))
    from caminus.ember.resident import sim
    from caminus.ember.resident.store import Store
    store = Store(Path(home).expanduser().resolve())
    return sim, store, _rt(store)


def load_record(home, rid):
    """One record's newest version from a session's home, or None."""
    sim, store, rt = _open(home)
    try:
        return sim._get(rt, rid)
    finally:
        store.close()


def write_observation(home, rid, observed, eval_context='', prediction_error='', goal=None):
    """Append the outcome to the record the arms measured: one new version, status 'observed'."""
    sim, store, rt = _open(home)
    try:
        return sim.observe(rt, goal, rid, observed, eval_context, prediction_error,
                           cfg=dict(model='emberlab', provider='', effort=''))
    finally:
        store.close()


def arms_from_proposal(prop, base_checkout=None, arm_checkout=None):
    """Two arms from one record: `base` runs now, `arm` has the proposal applied."""
    base_path = base_checkout or runner.REPO
    if arm_checkout and Path(arm_checkout).expanduser().resolve() != Path(base_path).expanduser().resolve():
        return [runner.Arm('base', base_path, prop.get('base_config'), prop.get('base_env')),
                runner.Arm('arm', arm_checkout, prop.get('config'), prop.get('env'))]
    checkout = arm_checkout or base_path
    base = runner.Arm('base', checkout, prop.get('base_config'), prop.get('base_env'))
    arm = runner.Arm('arm', checkout, prop.get('config'), prop.get('env'))
    if base.config == arm.config and base.env == arm.env:
        raise ValueError('the proposal changes neither config nor env, and no --arm-checkout names the change: two '
                         'identical arms measure nothing')
    return [base, arm]


def host_kind(a, brains):
    """The host `cmd_run` will pick, computed the same way: an explicit --host, else `agent` for a real brain."""
    return a.host or ('agent' if any(getattr(b, 'real', False) for b in brains) else 'local')


def approval_missing(a, brains):
    """What the owner still has to allow before this run starts: the plan alone never starts one."""
    paid = [b.label() for b in brains if getattr(b, 'paid', False)]
    real = [b.label() for b in brains if getattr(b, 'real', False)]
    missing = []
    if paid and not a.approve_paid:
        missing.append("--approve-paid (billed through the owner's relay: " + ', '.join(paid) + ')')
    # the same rule as cmd_run, paid brains included: on the local host a real model gets a shell as you
    if real and host_kind(a, brains) == 'local' and not (a.unsafe_local or a.unsafe_free):
        missing.append('--unsafe-local (a real model runs as you on the local host: ' + ', '.join(real) + ')')
    return missing


def plan_md(prop, suite, brains, arms, repeats, out, tasks):
    paid = [b.label() for b in brains if getattr(b, 'paid', False)]
    runs = len(arms) * len(brains) * tasks * max(1, repeats)
    lines = [f"proposal {prop['id']} · {prop.get('problem_group') or 'unclassified'}", '',
             f"- change: {(prop.get('change') or '').strip()[:400]}",
             f"- predicted: {(prop.get('predicted') or '').strip()[:400]}",
             f"- suite: {suite['name']} ({tasks} tasks), {repeats} repeat(s)",
             f"- brains: {', '.join(b.label() for b in brains)}",
             f"- arms: {', '.join(a.name + ' @ ' + str(a.checkout) + ' ' + a.commit for a in arms)}",
             f"- runs: {runs} in {out}",
             '- verdict: an arm is better only if it is better across the board (report.compare)']
    if paid:
        lines.append(f"- **billed through the owner's relay: {', '.join(paid)}** (the relay's daily cap is the guard)")
    return '\n'.join(lines)


def observed_text(verdict):
    """The record's `observed` field: the verdict and the per-brain numbers it rests on."""
    cells = [o for o in verdict['brains'] if 'missing' not in o]
    parts = [verdict['verdict']]
    if cells:
        parts.append('passed base -> arm: ' + ', '.join(
            f"{o['brain']} {o['base']['passed']}/{o['base']['runs']} -> {o['arm']['passed']}/{o['arm']['runs']}"
            for o in cells))
        parts.append('false completions: ' + ', '.join(
            f"{o['brain']} {o['base']['false_completions']} -> {o['arm']['false_completions']}" for o in cells))
        parts.append('median steps: ' + ', '.join(
            f"{o['brain']} {o['base']['median_steps']:g} -> {o['arm']['median_steps']:g}" for o in cells))
    if verdict.get('errored'):
        parts.append(f"{len(verdict['errored'])} run(s) ended in error and are left out: "
                     + ', '.join(verdict['errored'][:5]))
    return '; '.join(parts)


def _json_arg(text, what):
    if not text:
        return None
    try:
        value = json.loads(text)
    except ValueError as e:
        raise ValueError(f'--{what} is not JSON: {e}')
    if not isinstance(value, dict):
        raise ValueError(f'--{what} must be a JSON object')
    return value


def cmd_propose(a, run):
    """`lab.py propose ...`: the plan first, the runs only with the owner's flags, and the record closed by numbers."""
    home = Path(a.sim_home).expanduser().resolve()
    rec = load_record(home, a.record)
    if rec is None:
        raise SystemExit(f'no SIM record {a.record} in {home}')
    if rec.get('status') == 'observed':
        raise SystemExit(f'SIM record {a.record} is already observed: a closed record is not measured again')
    try:
        prop = dict(rec, config=_json_arg(a.config, 'config'), env=_json_arg(a.env, 'env'),
                    base_config=_json_arg(a.base_config, 'base-config'), base_env=_json_arg(a.base_env, 'base-env'))
    except ValueError as e:
        raise SystemExit(str(e))
    suite = runner.load_suite(a.suite)
    brains = [brains_mod.parse(s, a.parallel) for s in a.brain]
    try:
        arms = arms_from_proposal(prop, a.base_checkout, a.arm_checkout)
    except ValueError as e:
        raise SystemExit(str(e))
    out = Path(a.out).expanduser().resolve() if a.out else home / 'emberlab' / 'sim-ab' / a.record
    only = [k for k in (a.only or '').split(',') if k]
    tasks = [t for t in suite['tasks'] if not only or t['key'] in only]
    if only and len(tasks) != len(only):
        raise SystemExit(f'unknown task in --only: {sorted(set(only) - {t["key"] for t in tasks})}')
    print(plan_md(prop, suite, brains, arms, a.repeats, out, len(tasks)), flush=True)
    missing = approval_missing(a, brains)
    if missing:
        raise SystemExit('not started: ' + '; '.join(missing))
    if a.dry_run:
        return 0
    out.mkdir(parents=True, exist_ok=True)
    arms_file = out / 'arms.json'
    arms_file.write_text(json.dumps({x.name: dict(checkout=str(x.checkout), config=x.config, env=x.env)
                                     for x in arms}, indent=1))
    print(f'lab.py run --suite {a.suite} --arms {arms_file} --out {out}', flush=True)
    runs = type('Run', (), dict(suite=a.suite, brain=list(a.brain), arm=None, arms=str(arms_file), out=str(out),
                                repeats=a.repeats, only=a.only, host=a.host,
                                unsafe_local=a.unsafe_local or a.unsafe_free,
                                jobs=a.jobs, parallel=a.parallel, timeout=None, rerun=False))()
    if run(runs):
        raise SystemExit('a run ended in error: the record stays open (rerun the same command into the same folder)')
    verdict = report_mod.compare(report_mod.load(out), 'base', 'arm')
    print(report_mod.compare_md(verdict), flush=True)
    write_observation(home, a.record, observed_text(verdict),
                      eval_context=f"tools/emberlab propose: suite {suite['name']}, brains "
                                   f"{', '.join(b.label() for b in brains)}, repeats {a.repeats}, {out}",
                      prediction_error=a.prediction_error or '', goal=a.goal)
    print(f"SIM record {a.record}: observed", flush=True)
    return 0 if verdict['verdict'].startswith(('better', 'no difference')) else 1
