"""The loop judge: a finish is checked by evidence, not by the agent's word.

When a graded goal calls finish (after define_done and verify), two things run before the goal completes:

1. The gate: the shell command the agent gave define_done as `gate`, if any, run in its home. A non-zero exit refuses
   the finish and its output becomes the agent's next prompt.
2. The judge: a fresh model call with no tools that sees the goal, the done check, the human's steers, the recorded
   evidence and the recent tool results - not the agent's reasoning or its plan. It applies the evidence rules and
   answers met, not_yet or impossible. not_yet refuses the finish with the reason; impossible pauses the goal and tells
   the human. After JUDGE_LIMIT refusals the goal pauses instead of looping (agent and judge disagree: a person decides).

The judge runs on the agent's own model in a fresh context by default, so it costs nothing on a local brain. With config
`judge_model: "advisor"` it runs on the advisor's model instead (an independent reviewer, paid from the advisor's daily
budget; faster than a busy local model), falling back to the agent's own model when that call cannot be made. A reply that is not the JSON asked
for is asked once more, strictly; a second unreadable reply lets the finish through, marked unchecked, so a broken
judge cannot trap a goal. Every verdict records the model that gave it, and unread ones are counted (judged events
with verdict "unread"), so a model that answers loosely shows up in the evals instead of quietly turning the judge off.
"""
import json
import re
import subprocess

GATE_TIMEOUT = 600
JUDGE_LIMIT = 3
EVIDENCE_RESULTS = 12

SYSTEM = '''JUDGE. You check whether an agent's goal is done. You see the goal, the done check the agent wrote, its human's
steers, and evidence from what actually ran. You do not see the agent's reasoning; do not guess at it.

Evidence rules:
- Printed text proves only that text was printed. A claim in a summary is not evidence.
- A command's exit code and output, a file's content, a test result: these are evidence.
- An approval or decision by the human needs a real human message in the steers, not the agent's say-so.
- The done check and every steer must be satisfied, not most of them.
- If the evidence shows the goal cannot be done as asked (a missing permission, a contradiction), say impossible.

Answer with one JSON object and nothing else:
{"verdict": "met" | "not_yet" | "impossible", "reason": "one or two sentences naming the evidence or what is missing"}'''


def run_gate(home, gate):
    try:
        p = subprocess.run(['bash', '-lc', gate], cwd=home, capture_output=True, text=True, timeout=GATE_TIMEOUT)
    except subprocess.TimeoutExpired:
        return False, f'the gate did not finish in {GATE_TIMEOUT} s'
    out = (p.stdout + p.stderr)[-4000:]
    return p.returncode == 0, f'exit {p.returncode}\n{out}'.strip()


def evidence(rt, gid, summary, open_work):
    g = rt.store.goal(gid)
    parts = ['GOAL:\n' + g['prompt'][:4000], 'DONE CHECK:\n' + (g.get('done_check') or '(none)')]
    steers = [s['text'] for s in rt.store.steers(gid)]
    if steers:
        parts.append('HUMAN STEERS (newest last):\n' + '\n'.join('- ' + s[:600] for s in steers[-10:]))
    events = rt.store.db.execute(
        "SELECT kind,data FROM events WHERE goal=? AND kind IN ('verified','tool_result') ORDER BY seq DESC LIMIT ?",
        (gid, EVIDENCE_RESULTS * 2)).fetchall()
    lines = []
    for r in reversed(events):
        d = json.loads(r['data'])
        if r['kind'] == 'verified':
            lines.append('verify (the agent\'s account of its check): ' + str(d.get('evidence', ''))[:600])
        else:
            res = json.dumps(d.get('result'), ensure_ascii=False)
            lines.append(f"{d.get('name')} -> {res[:800]}")
    parts.append('EVIDENCE (oldest first):\n' + ('\n'.join(lines[-EVIDENCE_RESULTS:]) or '(none recorded)'))
    parts.append('THE AGENT SAYS IT IS DONE:\n' + summary[:2000] + (('\nLEFT OPEN: ' + open_work[:1000]) if open_work else ''))
    return '\n\n'.join(parts)


def verdict(text):
    m = re.search(r'\{.*\}', text or '', re.S)
    if not m:
        return None
    try:
        v = json.loads(m.group(0))
    except ValueError:
        return None
    if v.get('verdict') not in ('met', 'not_yet', 'impossible'):
        return None
    return dict(verdict=v['verdict'], reason=str(v.get('reason', ''))[:1000])


def review(rt, gid, result):
    """Gate, then judge. Returns the finish result to record (an error to refuse it; `pause` to pause the goal)."""
    g = rt.store.goal(gid)
    gate = g.get('gate')
    if gate:
        ok, out = run_gate(rt.home, gate)
        rt.store.event('gate', dict(command=gate, passed=ok, output=out[-2000:]), gid)
        if not ok:
            return dict(error='the gate failed, so the goal is not done; fix what it shows, then finish again:\n' + out)
    refusals = rt.store.db.execute(
        "SELECT count(*) FROM events WHERE goal=? AND kind='judged' AND data LIKE '%\"not_yet\"%'", (gid,)).fetchone()[0]
    messages = [dict(role='system', content=SYSTEM),
                dict(role='user', content=evidence(rt, gid, result.get('summary', ''), result.get('open', '')))]
    def ask(msgs):
        if rt.config.get('judge_model') == 'advisor':
            try:
                return rt.advisor.raw(gid, msgs, 600)   # the owner chose an independent, paid reviewer
            except Exception:
                pass
        reply = rt.model.complete(msgs, tools=None, max_tokens=600)
        try:
            rt.brains.account(gid)
        except Exception:
            pass
        name = (getattr(rt.model, 'config', None) or {}).get('model') or rt.config.get('model', '')
        return (reply.get('content') if isinstance(reply, dict) else '') or '', name
    try:
        text, who = ask(messages)
        v = verdict(text)
        if v is None:                                    # once more, strictly
            text, who = ask(messages + [dict(role='assistant', content=text[:2000]),
                                        dict(role='user', content='Answer with only the JSON object: '
                                             '{"verdict": "met" | "not_yet" | "impossible", "reason": "..."}')])
            v = verdict(text)
    except Exception as e:                               # the brain is down or refused: do not trap the goal
        rt.store.event('judged', dict(verdict='unread', reason=f'the judge call failed: {type(e).__name__}'), gid)
        return dict(result, judge='the judge could not run; finish accepted unchecked')
    if v is None:
        rt.store.event('judged', dict(verdict='unread', reason='the judge reply could not be read, twice',
                                      model=who, reply=text[:500]), gid)
        return dict(result, judge='the judge reply could not be read; finish accepted unchecked')
    v['model'] = who
    rt.store.event('judged', v, gid)
    if v['verdict'] == 'met':
        return dict(result, judge='met: ' + v['reason'])
    if v['verdict'] == 'impossible':
        return dict(error='the judge found the goal cannot be done as asked: ' + v['reason'] +
                    ' The goal is paused for your human.', pause='judge: impossible: ' + v['reason'])
    if refusals + 1 >= JUDGE_LIMIT:
        return dict(error=f'the judge refused this finish {refusals + 1} times (latest: {v["reason"]}). The goal is '
                    'paused for your human to decide.', pause='judge refused the finish %d times: %s'
                    % (refusals + 1, v['reason']))
    return dict(error='not done yet, by the evidence: ' + v['reason'] + ' Do what is missing, run the check, verify, '
                'then finish again.')
