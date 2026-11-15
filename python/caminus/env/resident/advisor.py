"""The advisor: a stronger model the resident may consult, on its human's key and within its human's limits.

The resident calls the `advisor` tool with a question when it is stuck, before a risky or irreversible step, or
before declaring a hard goal done. The runtime sends the advisor a focused brief (the goal, its done check, the
ledger, the newest part of the transcript) and the question, with no tools; the advice is the tool result. The
resident knows the advisor exists and what it has left: the tool's description says when to use it, and the
per-turn context carries the calls and dollars remaining.

Configuration, in config.json under "advisor" (absent: no advisor; the tool answers that none is configured):
    model          the advisor model, e.g. claude-opus-5-5 or claude-sonnet-5-5
    endpoint       the Messages API root; point it at the owner's own key relay (its own key file, model allowlist
                   and daily cap) so the advisor's key and budget stay separate from the resident's
    api_key_env    the key variable, when the endpoint is not a local relay that adds the key itself
    budget_usd     1.00   dollars per day the runtime lets the advisor spend (priced per call)
    calls_per_goal 5      advice calls one goal may make
    effort         medium Messages API effort for the advisor (its thinking stays adaptive: the current Opus and
                          Sonnet do not accept thinking off)
    max_tokens     4096   the advice length budget, thinking included
"""
import datetime
import json
import urllib.request

from .model import Model, ModelError, price
from .store import atomic_json

DEFAULTS = dict(budget_usd=1.0, calls_per_goal=5, effort='medium', max_tokens=4096)

BRIEF = ('You are the advisor to an autonomous agent that works in its own Unix account on its human\'s Linux '
         'machine. It consults you when it is stuck, before a risky step, or before declaring a hard goal done. You '
         'see a brief of its goal and recent work, and its question. Give concrete, prioritized advice: what to do '
         'next, what to check, what to avoid, and why. Point out an assumption it has not verified. Be brief; it pays '
         'for every token. You cannot run anything yourself.')


class Advisor:
    def __init__(self, runtime):
        self.rt = runtime
        self.path = runtime.home / 'advisor.json'

    @property
    def cfg(self):
        raw = self.rt.config.get('advisor')
        if not raw or not raw.get('model') or not raw.get('endpoint'):
            return None
        return {**DEFAULTS, **raw}

    def _state(self):
        try:
            return json.loads(self.path.read_text())
        except (OSError, ValueError):
            return {}

    def spent_today(self):
        return float(self._state().get(datetime.date.today().isoformat(), {}).get('usd', 0.0))

    def calls(self, gid):
        return sum(1 for e in self.rt.store.events() if e['goal'] == gid and e['kind'] == 'advice')

    def status_line(self, gid):
        """the line the per-turn context carries, so the resident knows what it can still ask"""
        cfg = self.cfg
        if cfg is None:
            return ''
        left_calls = max(0, int(cfg['calls_per_goal']) - self.calls(gid)) if gid else int(cfg['calls_per_goal'])
        left_usd = max(0.0, float(cfg['budget_usd']) - self.spent_today())
        return ('ADVISOR: %s is available through the advisor tool: %d call%s left for this goal, $%.2f left today.'
                % (cfg['model'], left_calls, '' if left_calls == 1 else 's', left_usd))

    def ask(self, gid, question, context=''):
        cfg = self.cfg
        if cfg is None:
            raise ValueError('no advisor is configured for this resident; your human can add one '
                             '(caminus env agent advisor NAME --model ... --endpoint ...)')
        question = (question or '').strip()
        if not question:
            raise ValueError('ask the advisor a specific question')
        if self.calls(gid) >= int(cfg['calls_per_goal']):
            raise ValueError('this goal has used its %d advisor calls; decide with what you have' % int(cfg['calls_per_goal']))
        if self.spent_today() >= float(cfg['budget_usd']):
            raise ValueError("today's advisor budget is spent; decide with what you have")
        if price(cfg['model'], {}) is None:
            raise ValueError('the advisor model %s has no known price, so its spend cannot be limited' % cfg['model'])
        model = Model(dict(model=cfg['model'], endpoint=cfg['endpoint'], api='anthropic',
                           context=int(cfg.get('context', 200000)), max_tokens=int(cfg['max_tokens']),
                           api_key_env=cfg.get('api_key_env', 'CAMINUS_ADVISOR_API_KEY'), effort=cfg['effort'],
                           thinking='adaptive', timeout=float(cfg.get('timeout', 300))),
                      opener=getattr(self.rt, 'advisor_opener', None) or urllib.request.urlopen)
        model.counter = False                     # the brief is small; no count_tokens round trip per question
        messages = [dict(role='system', content=BRIEF),
                    dict(role='user', content=self._brief(gid, context) + '\n\nQUESTION FROM THE AGENT:\n' + question)]
        try:
            reply = model.complete(messages, max_tokens=int(cfg['max_tokens']))
        except ModelError as e:
            raise ValueError('the advisor did not answer: %s' % e)
        usd = price(cfg['model'], model.last_usage) or 0.0
        state = self._state()
        day = state.setdefault(datetime.date.today().isoformat(), dict(usd=0.0, calls=0))
        day['usd'] = round(day['usd'] + usd, 6)
        day['calls'] += 1
        atomic_json(self.path, state)
        self.rt.store.event('advice', dict(question=question[:500], model=cfg['model'], usd=round(usd, 6)), gid)
        return dict(advice=reply['content'], model=cfg['model'], usd=round(usd, 5),
                    calls_left=max(0, int(cfg['calls_per_goal']) - self.calls(gid)))

    def _brief(self, gid, context):
        g = self.rt.store.goal(gid)
        parts = ['GOAL:\n' + g['prompt'][:4000]]
        if g.get('done_check'):
            parts.append('DONE CHECK:\n' + g['done_check'][:1000])
        if g.get('ledger'):
            parts.append('LEDGER:\n' + str(g['ledger'])[:2000])
        recent = []
        for m in g['messages'][-14:]:
            text = m.get('content') or ''
            for c in m.get('tool_calls') or []:
                text += '\n[call %s %s]' % (c['function']['name'], c['function']['arguments'][:400])
            recent.append('%s: %s' % (m['role'], text[:1500]))
        parts.append('RECENT WORK (newest last):\n' + '\n---\n'.join(recent))
        if context:
            parts.append('AGENT NOTE:\n' + context[:2000])
        return '\n\n'.join(parts)
