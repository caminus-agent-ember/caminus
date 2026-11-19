"""/reflect: the post-goal reflection (issue #341).

When a goal completes, one cheap model call asks what harness change (a prompt section, a tool, a gate, a cap)
would have made this goal faster or more reliable, and writes the answer as a SIM record with status
'proposed'. That record is a queue a person reads, never a change: the harness is edited only by a human and a
pull request. The proposal carries the goal it came from and the evidence the call saw (the done check, the
judge's verdict, the verification, the failed calls), so anyone can check it against the goal that produced it.

The call runs after the goal's completion transaction, outside it (a model call inside BEGIN would block every
other writer), and never raises: a reflection that failed is an event on a completed goal, not a failed goal.
It is skipped when the SIM log is off (an A/B arm records nothing), when config 'reflect' is false, in free
time (not graded work), and when the runtime was asked to stop. A goal re-finished after its human reopened it
reflects again: that is a new episode with new evidence.

The system message begins with REFLECT so a log reader (and the test fixture) can name the call, as the
judge's begins with JUDGE.
"""
import json

from . import sim

MAX_TOKENS = 700              # well under model.SHORT_CALL: this call runs with thinking off
SYSTEM = '''REFLECT. You review one finished agent goal and propose at most one change to the harness (the system
prompt, a tool, a gate, a cap) that would have made this goal faster or more reliable. You see the goal, its
done check, how it ended, its tool calls and the failures along the way. Propose only a change the evidence
supports: a recurring problem this goal hit, not a style preference, and not a restatement of the goal's own
work. Nothing you say is applied; a person reads the proposal.

Answer with only a JSON object:
{"problem_group": "the recurring problem, short", "change": "what would change in the harness",
 "rationale": "why the evidence supports it", "predicted": "the outcome, as a test, CI or a reviewer can confirm or contradict"}

Or, when this goal shows no recurring problem worth a change:
{"skip": true, "reason": "why"}'''
STRICT = 'Answer with only the JSON object (or {"skip": true, "reason": "..."}), nothing else.'


def maybe(rt, gid, summary='', open_work=''):
    """One cheap call after a goal completed: a 'proposed' SIM record, or nothing. Never raises."""
    try:
        from .freetime import KINDS as FREE_KINDS
        g = rt.store.goal(gid)
        if not g or g.get('kind', 'work') in FREE_KINDS:
            return None
        if not sim.enabled(rt) or not rt.config.get('reflect', True):
            return None
        if rt.stopping():
            return None
        if not callable(getattr(getattr(rt, 'model', None), 'complete', None)):
            return None           # a stubbed runtime (some tests drop a plain namespace in for the model)
        msgs = [dict(role='system', content=SYSTEM),
                dict(role='user', content=brief(rt, gid, summary, open_work))]
        text = _ask(rt, gid, msgs)
        rec = parse(text)
        if rec is None:
            text = _ask(rt, gid, msgs + [dict(role='assistant', content=text[:2000]),
                                         dict(role='user', content=STRICT)])
            rec = parse(text)
        if rec is None:
            rt.store.event('reflection_failed', dict(reason='the reply could not be read, twice', reply=text[:500]), gid)
            return None
        if rec.get('skip'):
            rt.store.event('reflection_skipped', dict(reason=rec['reason'][:300]), gid)
            return None
        goal = rt.store.goal(gid) or {}
        rec.update(status='proposed',
                   eval_context='goal %s · %s steps · judge %s' % (
                       gid[:8], goal.get('steps') or 0, _judge(rt, gid) or 'not run'))
        return sim.record(rt, gid, rec)
    except Exception as e:
        try:
            rt.store.event('reflection_failed', dict(reason='%s: %s' % (type(e).__name__, str(e)[:300])), gid)
        except Exception:
            pass                  # the reflection is a proposal, never a reason to lose a finished goal
        return None


def _ask(rt, gid, msgs):
    """One short call, priced into the goal's spend like any other turn (inside the brain's allowance)."""
    reply = rt.model.complete(msgs, tools=None, max_tokens=MAX_TOKENS)
    try:
        rt.brains.account(gid)
    except Exception:
        pass
    try:
        rt._meter(gid)
    except Exception:
        pass
    return (reply.get('content') if isinstance(reply, dict) else '') or ''


def parse(text):
    """The JSON object in a reply, or None. A fenced block or a sentence around it is tolerated; a record needs
    a change and a prediction (sim.record refuses one without them), and {"skip": true} proposes nothing."""
    import re
    m = re.search(r'\{.*\}', text or '', re.S)
    if not m:
        return None
    try:
        d = json.loads(m.group(0))
    except ValueError:
        return None
    if not isinstance(d, dict):
        return None
    if d.get('skip'):
        return dict(skip=True, reason=str(d.get('reason') or 'no reason given')[:500])
    change, predicted = str(d.get('change') or '').strip(), str(d.get('predicted') or '').strip()
    if not (change and predicted):
        return None
    return dict(problem_group=str(d.get('problem_group') or '').strip(),
                change=change, rationale=str(d.get('rationale') or '').strip(), predicted=predicted)


def _judge(rt, gid):
    row = rt.store.db.execute("SELECT data FROM events WHERE goal=? AND kind='judged' ORDER BY seq DESC LIMIT 1",
                              (gid,)).fetchone()
    try:
        return json.loads(row['data']).get('verdict') if row else None
    except (ValueError, KeyError, TypeError):
        return None


def brief(rt, gid, summary, open_work):
    """The evidence the call sees: the goal, the done check, its own plan, how it ended, and where it struggled."""
    g = rt.store.goal(gid) or {}
    parts = ['GOAL:\n' + (g.get('prompt') or '')[:3000],
             'DONE CHECK:\n' + (g.get('done_check') or '(none)')]
    todo = rt.store.todo(gid)
    if todo.get('items'):
        mark = {'done': '[x]', 'in_progress': '[>]', 'pending': '[ ]'}
        parts.append("THE AGENT'S PLAN (its todo list):\n" + '\n'.join(
            '- ' + mark.get(it['state'], '[ ]') + ' ' + it['text'][:200] for it in todo['items'][:30]))
        if todo.get('blockers'):
            parts.append('WHAT IT SAID BLOCKED IT:\n' + '\n'.join('- ' + str(b)[:200] for b in todo['blockers'][:10]))
    parts.append('HOW IT ENDED:\n' + ((summary or '(no summary)')[:2000]))
    if open_work:
        parts.append('LEFT OPEN (the agent says):\n' + open_work[:1000])
    parts.append('THE GOAL TOOK %s MODEL TURNS, the agent changed approach %s times, and %d steer(s) arrived.' % (
        g.get('steps') or 0, g.get('approach_changes') or 0, len(rt.store.steers(gid))))
    verdict = _judge(rt, gid)
    if verdict:
        row = rt.store.db.execute("SELECT data FROM events WHERE goal=? AND kind='judged' ORDER BY seq DESC LIMIT 1",
                                  (gid,)).fetchone()
        try:
            reason = str(json.loads(row['data']).get('reason') or '')[:600]
        except (ValueError, KeyError, TypeError):
            reason = ''
        parts.append('THE FINISH JUDGE: %s — %s' % (verdict, reason))
    row = rt.store.db.execute("SELECT data FROM events WHERE goal=? AND kind='verified' ORDER BY seq DESC LIMIT 1",
                              (gid,)).fetchone()
    if row:
        try:
            parts.append('WHAT THE AGENT VERIFIED:\n' + str(json.loads(row['data']).get('evidence') or '')[:1000])
        except (ValueError, KeyError, TypeError):
            pass
    rows = rt.store.db.execute(
        "SELECT data FROM events WHERE goal=? AND kind='tool_result' ORDER BY seq DESC LIMIT 200", (gid,)).fetchall()
    names, failed, errors = {}, 0, []
    for r in rows:
        try:
            d = json.loads(r['data'])
        except ValueError:
            continue
        name = d.get('name') or '?'
        names[name] = names.get(name, 0) + 1
        res = d.get('result')
        if isinstance(res, dict) and res.get('error'):
            failed += 1
            errors.append('%s: %s' % (name, str(res['error'])[:200]))
    parts.append('ITS TOOL CALLS (newest first, at most 200): ' +
                 (', '.join('%s×%d' % (k, v) for k, v in names.items()) or '(none)'))
    if failed:
        parts.append('%d of those calls failed; the newest:\n' % failed + '\n'.join(errors[:5]))
    return '\n\n'.join(parts)
