"""/btw: a side question about a goal, answered by one model call that stores nothing in the goal.

The brief is the goal's prompt, its done check and its todo, plus the newest messages as context and the
question itself. No tools are offered, so the answer is talk, not work: the goal's transcript, steps and
state are untouched (an event btw keeps the question and the answer's first 300 characters for the
journal). The runtime's own model answers inside its own window: older context is trimmed until the
request and a full answer fit.
"""
KEEP = 20          # newest goal messages that ride along as context
HEADROOM = 512     # tokens kept for the answer and the request frame
PROMPT_LIMIT = 8000   # the goal's own words, bounded: a huge goal must not make the brief itself overflow


def brief(rt, gid):
    """The system part of a side question: what the goal is and where it stands."""
    g = rt.store.goal(gid)
    system = ['GOAL: ' + (g['prompt'] or '')[:PROMPT_LIMIT]]
    if g.get('done_check'):
        system.append('DONE CHECK: ' + g['done_check'])
    todo = rt.store.todo(gid)
    if todo['items']:
        system.append('TODO: ' + ' | '.join(f"[{it['state']}] {it['text']}" for it in todo['items']))
    if todo['blockers']:
        system.append('BLOCKED BY: ' + ' | '.join(todo['blockers']))
    return dict(role='system', content='\n'.join(system))


def ask(rt, gid, question):
    """One model call about the goal; returns the answer as text."""
    question_msg = dict(role='user', content='BTW, a side question about this goal (your answer is shown to '
                                             'your human and kept nowhere else; you have no tools): '
                                             + question)
    max_tokens = min(int(rt.config.get('max_tokens', 4096)), rt.model.context_size() // 4)
    messages = rt.store.goal(gid)['messages'][-KEEP:]
    while True:
        request = [brief(rt, gid), *messages, question_msg]
        if rt.model.tokens(request, []) + max_tokens + HEADROOM <= rt.model.context_size():
            break
        if not messages:
            raise ValueError('the goal does not fit the model window for a side question: compact it first')
        messages = messages[1:]           # trim older context until the request and its answer fit
    answer = rt.model.complete(request, None, max_tokens=max_tokens)['content']
    rt.store.event('btw', dict(question=question, answer=answer[:300]), gid)
    return answer
