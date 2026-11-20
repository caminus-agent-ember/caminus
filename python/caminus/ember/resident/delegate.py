"""/delegate (issue #342, full version): a child goal run through this harness's own goal loop.

The parent states a task and what done looks like; the child is an ordinary goal of the same store - its own
conversation, todo list, done check, gate, verify and spend row - with limits that make it safe to run from
inside another goal:

- permissions, chosen per call by the parent's brain: `read` (the default) may not change anything - write, edit,
  checkpoint, rollback are refused and terminal runs only a read-only command; `write` may change files, but only
  inside the worktree the delegate creates for it (a branch off HEAD), so the parent's tree is untouched until it
  merges or discards. A parent in plan mode may not grant write: a child never gets more than its parent has. The
  sandbox stays available to both: the folder it sees is a read-only bind at /work and /box dies with the box.
  Residual risk, stated plainly: a writing child is trusted the way the parent is - the worktree keeps its changes
  apart from the parent's tree, it is not a jail for a hostile process;
- depth 1: a child cannot delegate, so the tree is one level deep by construction. The human-facing and outward
  tools (ask_user, ask_root, github_handoff, send_message, report, remember, sim, schedule, lease, browser,
  propose_plan, advisor) and MCP tools belong to the parent: a child reports what it found and the parent acts;
- caps: a step limit, a time limit and a dollar cap per child. The cap is config `delegate_usd` (0.50 by default)
  and a call's `cap_usd` may lower it, never raise it; reaching it pauses the child and the parent gets a partial
  report. Spend is usd + the OpenCode plan's allowance, because inside a Go allowance a turn costs $0 of money and
  real allowance;
- a short structured report back: outcome (completed / capped / failed / incomplete / orphaned), summary, what it
  verified, what it skipped, steps, spend, allowance and its cap; a writing child's report also names its worktree,
  branch, base commit, the files it changed, a diffstat and the merge / discard commands;
- a SIM record per child: a `recorded` prediction at the start (task, role, permissions, caps, the done check it
  was given) and its `observed` outcome when the child closes, in the parent's goal.

Parallel children: children are ordinary queued goals, so the one loop runs them interleaved without threads. A
delegate call with `wait=true` (the default) runs the child through Runtime.run(target=child) inside that call and
replaces its result with the report; `wait=false` returns a handle, parks the parent (state waiting_children) once
its turn's other actions ran, and the loop's `settle` closes each child as it finishes and wakes the parent with
their reports. `collect` reads the reports of finished children and names the running ones. A card per child is
the panel's (`workers` in the status a UI reads).
"""
import json
import os
import shlex
import subprocess
import time

ROLES = ('worker', 'reviewer')
PERMISSIONS = ('read', 'write')
DEFAULTS = dict(cap_usd=0.50, max_steps=12, max_seconds=600, max_children=4)
# Every child, whatever its permissions: the outward and human-facing tools, and a nested delegate (depth 1).
ALWAYS_REFUSED = ('ask_user', 'ask_root', 'github_handoff', 'send_message', 'report', 'remember', 'sim',
                  'schedule', 'lease', 'browser', 'propose_plan', 'delegate', 'advisor')
# A read-only child may not change anything; a writing child may, inside its worktree.
WRITE_ONLY = ('write', 'edit', 'checkpoint', 'rollback')
REFUSED = ALWAYS_REFUSED + WRITE_ONLY   # (every child-refused name, for callers that do not know the permissions)
CHILD_PROMPT = '''[delegate child of goal {parent}] You are a {perms} child goal, run by that goal's delegate tool.

TASK
{task}

WHAT DONE LOOKS LIKE (call define_done with this, then verify and finish)
{done}

Rules that differ from a top-level goal: you cannot ask your human, message a peer, report, remember, write a SIM
record, schedule, hand off to GitHub, browse, or delegate further - those belong to your parent. {perms_text} Your
parent reads only your structured report (summary, verified, skipped, spend, and for a writing child the files it
changed), so the answer belongs in your finish summary: say plainly what you checked and what you could not. Name
files by path and line and quote the few lines that matter.'''
READ_TEXT = '''You cannot write or edit a file or run a command that changes anything: you read, search, run read-only
commands, use the sandbox, the web and machine, then define_done, verify and finish with what you found.'''
WRITE_TEXT = '''You may write and edit files and run commands, but only inside the worktree this delegate created for you
(call checkpoint before your first change to it, and name files relative to it): everything outside that worktree is
your parent's to change. A terminal command that changes something must start with `cd <your worktree> && `.'''


def read_only(cmd):
    """runtime.read_only_command without the circular import (runtime imports this module)."""
    from .runtime import read_only_command
    return bool(cmd) and read_only_command(cmd)


def _inside(wt, path):
    """True when `path` stays inside `wt` once the OS resolves it.

    A plain `path.startswith(wt + '/')` accepted `<wt>/../x`, which the OS then resolves outside the worktree:
    a writing child's `write(path='../x')` and `cd <wt> && echo x > ../x` both left it (#384 review).
    realpath() normalises `..`, `.`, doubled slashes and symlinks on both sides, so neither a `..` path nor a
    link inside the worktree can name a file outside it.
    """
    wt = os.path.realpath(wt)
    p = path if os.path.isabs(path) else os.path.join(wt, path)
    p = os.path.realpath(p)
    return p == wt or p.startswith(wt + os.sep)


# Programs that run another command from a string, or start a shell of their own: a text check cannot see what
# their argument does (a nested `sh -c "echo x > /tmp/evil"` hides its path inside one word), so a writing child
# may not call them (#384 review).
NESTED = frozenset('sh bash dash zsh ksh csh tcsh fish eval exec source xargs env sudo doas su chroot '
                   'unshare nsenter script'.split())


def _escaping(cmd, wt):
    """Why a writing child's command could change something outside its worktree, or ''.

    Best effort on the command text, not a sandbox (see the module docstring): a program that computes a path at
    run time (`python3 -c "open(...)"`) can still write anywhere, and no text check can see it; what the file
    tools do is exact (`_inside`). The command must start with `cd <worktree> &&`; no word may name a program
    that runs another command (`sh -c`, `eval`, `xargs`), an absolute path outside the worktree, a `..`
    component, a `$` expansion or a `~` home; and the value of `--opt=path` is checked as a path too. Words are
    split with punctuation, so a redirect glued to its target (`echo x >../out`) yields `../out` as a word.
    """
    try:
        lex = shlex.shlex(cmd, posix=True, punctuation_chars=';&|()<>`')
        lex.whitespace_split = True
        words = list(lex)
    except ValueError:
        return 'it cannot be parsed'
    if len(words) < 3 or words[0] != 'cd' or words[1] != wt or words[2] != '&&':
        return 'it must start with `cd %s && `' % wt
    for w in words[3:]:
        prog = w.rsplit('/', 1)[-1]
        if prog in NESTED or prog.lstrip('-') in NESTED:
            return '%s runs another command, which this check cannot see' % w
        if w.startswith('~') or '$' in w:
            return '%s could resolve outside the worktree' % w
        for p in ([w, w.split('=', 1)[1]] if '=' in w else [w]):
            if not _inside(wt, p):
                return '%s climbs out of the worktree' % w
    return ''


def refused(name, args, row=None):
    """Why a delegate child may not run this call, or None. Checked by the runtime before the tool runs.
    row: the child's delegates row - its permissions ('read' when absent) and its worktree when it may write."""
    row = row or {}
    write = (row.get('permissions') or 'read') == 'write'
    wt = (row.get('worktree') or '').rstrip('/')
    if name in ALWAYS_REFUSED or name.startswith('mcp__'):
        return ('a delegate child cannot %s: the human-facing and outward tools belong to the parent, a child is '
                'one level deep, and an MCP tool can change things this harness does not see. Report what you '
                'found; your parent acts.' % name)
    if name in WRITE_ONLY and not write:
        return ('a read-only delegate child cannot %s: report what you found; your parent makes the change' % name)
    if name == 'define_done' and args.get('gate') and not read_only(args['gate']):
        # a gate runs in a shell at finish (and after a change turn): `define_done(gate='touch x && ...')` created
        # the file and still reported completed (#382 review). A read-only gate proves as much and cannot run one.
        return ('a delegate child\'s gate must be a read-only command, because it runs in a shell when the goal '
                'finishes. Check with a command that only reads (grep, test -f, ls), or leave gate empty and report '
                'what you verified')
    if name == 'terminal':
        if args.get('stdin') is not None or args.get('signal'):
            # typing into a shell (or signalling one) reaches whatever that session is, possibly another goal's,
            # and bypasses both the read-only rule and a writing child's worktree rule: every child is refused
            # (#382 review kept this for a read-only child; the full version applies it to both)
            return ('a delegate child cannot type into a running shell or signal a session%s: that session may '
                    'belong to another goal, and typing bypasses the read-only and worktree checks. Run a command '
                    'of its own, or report what is missing'
                    % (' (a read-only child reads, it does not interact)' if not write else ''))
        cmd = str(args.get('command') or '')
        if not write and cmd and not read_only(cmd):
            return ('a read-only delegate child runs only a read-only command here. Report what you found; your '
                    'parent makes the change')
        if write and wt and cmd and not read_only(cmd):
            why = _escaping(cmd, wt)
            if why:
                return ('a writing child changes files only inside its worktree (%s): %s. Start the command with '
                        '`cd %s && ` and keep every path inside it' % (wt, why, wt))
        cwd = str(args.get('cwd') or '')
        if write and wt and cwd and not (os.path.isabs(cwd) and _inside(wt, cwd)):
            return ('a writing child starts a terminal command inside its worktree (%s), not %s'
                    % (wt, cwd))
    if write and wt and name in ('write', 'edit'):
        path = str(in_worktree(row, name, args).get('path') or '')
        if path.startswith('/') and not _inside(wt, path):
            return ('a writing child changes files inside its worktree (%s): name the file relative to it; the '
                    'files outside are your parent\'s to change' % wt)
    if write and wt and name in ('read', 'search'):
        # a writing child's world is its worktree: reading the parent's tree (or a home) from it leaks what the
        # parent did not hand it, and the PR body says such a read is refused (#384 review)
        path = str(in_worktree(row, name, args).get('path') or '')
        if path.startswith('/') and not _inside(wt, path):
            return ('a writing child reads inside its worktree (%s): name the file relative to it' % wt)
    if write and wt and name == 'checkpoint':
        outside = [str(p) for p in (in_worktree(row, name, args).get('paths') or [])
                   if str(p).startswith('/') and not _inside(wt, str(p))]
        if outside:
            return ('a writing child checkpoints paths inside its worktree (%s): %s is outside it'
                    % (wt, ', '.join(outside[:3])))
    return None


def in_worktree(row, name, args):
    """A writing child's view of its world: a relative path means inside its worktree, not the agent home.

    files.read / write / edit / search and checkpoint resolve a relative path against the home directory; for a
    child with a worktree the worktree is its home, so every path is made absolute here before the tool runs.
    Other calls (and other children) are left exactly as the model wrote them.
    """
    wt = (row or {}).get('worktree') or ''
    if not wt or (row.get('permissions') or 'read') != 'write':
        return args
    args = dict(args or {})
    if name in ('write', 'edit', 'read', 'search'):
        path = str(args.get('path') or '')
        if path and not os.path.isabs(path):
            args['path'] = os.path.join(wt, path)
    elif name == 'checkpoint':
        args['paths'] = [p if os.path.isabs(str(p)) else os.path.join(wt, str(p)) for p in (args.get('paths') or [])]
    return args


def _caps(rt, args):
    """(cap_usd, max_steps, max_seconds): the config's defaults, lowered by the call, never raised.

    A cap is a positive number; a missing or null config key takes the default. Zero or less is refused rather
    than read as "unlimited": `cap_usd=0` used to disable a child's cap altogether (#382 review), and a cap
    nobody can reach is not a cap."""
    cfg = rt.config
    cap = cfg.get('delegate_usd', DEFAULTS['cap_usd'])
    cap = DEFAULTS['cap_usd'] if cap is None else float(cap)
    if cap <= 0:
        raise ValueError('delegate_usd is %s: a cap of 0 does not mean unlimited, so no child can run under it. '
                         'Set a positive cap (the default is %s)' % (cfg.get('delegate_usd'), DEFAULTS['cap_usd']))
    if args.get('cap_usd') is not None:
        their = float(args['cap_usd'])
        if their <= 0:
            raise ValueError('cap_usd must be more than 0: it is what the child may spend, and 0 used to mean '
                             '"uncapped". Omit it to use delegate_usd ($%s)' % cap)
        cap = min(cap, their)
    steps = int(cfg.get('delegate_max_steps', DEFAULTS['max_steps']))
    if args.get('max_steps') is not None:
        their = int(args['max_steps'])
        if their <= 0:
            raise ValueError('max_steps must be at least 1: a child that may take no step cannot do anything')
        steps = min(steps, their)
    secs = int(cfg.get('delegate_max_seconds', DEFAULTS['max_seconds']))
    if args.get('max_seconds') is not None:
        their = int(args['max_seconds'])
        if their <= 0:
            raise ValueError('max_seconds must be at least 1: a child with no time cannot do anything')
        secs = min(secs, their)
    return cap, max(1, steps), max(1, secs)


def _brain(rt, args):
    """(Model, {model, provider}) the child runs: a call's model/provider from the catalog, the role's
    configured delegate.<role>_model, or the parent's own brain (the default)."""
    from .model import Model, remember_prices
    from . import catalog
    role = args.get('role') or 'worker'
    name = str(args.get('model') or '').strip()
    provider = str(args.get('provider') or '').strip() or None
    if not name:
        name = str((rt.config.get('delegate') or {}).get(role + '_model') or '').strip()
    if not name:
        return Model(dict(rt.config)), dict(model=rt.config.get('model'),
                                            provider=rt.config.get('provider') or rt.config.get('provider_kind'))
    cat = catalog.load(rt.home)
    entry = catalog.find(cat, name, provider)
    if entry is None:
        entry = catalog.find(catalog.refresh(rt.home, rt.config), name, provider)
    if entry is None:
        raise ValueError(f'no model {name} in the catalog: `models --refresh` lists what the providers serve')
    spec = catalog.providers(rt.config).get(entry['provider'])
    if not spec:
        raise ValueError('the provider of %s is no longer configured' % entry['id'])
    endpoint, api = catalog.transport(spec, entry)
    conf = dict(rt.config)
    conf.update(model=entry['id'], provider=entry['provider'], provider_kind=spec['kind'], endpoint=endpoint, api=api)
    for k in ('relay', 'prices', 'context', 'context_cap', 'thinking', 'model_variants', 'efforts', 'vision'):
        conf.pop(k, None)                 # (the parent's brain's facts, not this one's)
    if spec.get('relay'):
        conf['relay'] = True
    if entry.get('prices'):
        conf['prices'] = entry['prices']
        remember_prices(entry['id'], entry['prices'])
    if entry.get('context'):
        conf['context'] = int(entry['context'])
    if entry.get('vision'):
        conf['vision'] = True
    if entry.get('variants'):
        conf['model_variants'] = entry['variants']
    return Model(conf), dict(model=entry['id'], provider=entry['provider'])


def prepare(rt, args):
    """What start() needs that may wait or reach the network, resolved before the runtime opens its write
    transaction: the caps, the role, the permissions, the repository and the child's brain (a model from the
    catalog may need a refresh, which is a network call, and the store has one writer: #382 review)."""
    task = str(args.get('task') or '').strip()
    done = str(args.get('done_check') or '').strip()
    if not task:
        raise ValueError('delegate needs a task: what the child should find out or check')
    if not done:
        raise ValueError('delegate needs done_check: what done looks like for the child, as something it can '
                         'check (it becomes the child\'s define_done)')
    role = args.get('role') or 'worker'
    if role not in ROLES:
        raise ValueError('role must be one of ' + ', '.join(ROLES))
    perms = str(args.get('permissions') or 'read')
    if perms not in PERMISSIONS:
        raise ValueError('permissions must be one of ' + ', '.join(PERMISSIONS))
    wait = args.get('wait')
    wait = True if wait is None else bool(wait)
    repo = ''
    if perms == 'write':
        repo = str(args.get('repo') or '.').strip() or '.'
        if not os.path.isabs(repo):
            repo = str(rt.home / repo)
        repo = _git(['-C', repo, 'rev-parse', '--show-toplevel'])
    cap, steps, secs = _caps(rt, args)
    model, who = _brain(rt, args)
    return dict(task=task, done=done, role=role, permissions=perms, repo=repo, wait=wait,
                cap_usd=cap, max_steps=steps, max_seconds=secs, model=model, who=who)


def _git(args, cwd=None):
    """Run git (list argv, no shell) and return its stdout, or raise ValueError naming what it said."""
    p = subprocess.run(['git'] + args, cwd=cwd, capture_output=True, text=True, timeout=60)
    if p.returncode != 0:
        raise ValueError('git %s failed: %s' % (' '.join(args), (p.stderr or p.stdout).strip()[:300]))
    return p.stdout.strip()


def _worktree(rt, child, repo):
    """(repo top, base commit, worktree path, branch) for a writing child; creates the worktree and its branch.
    The parent's tree is never touched: the child works in its own checkout, on its own branch off HEAD."""
    top = _git(['-C', str(repo), 'rev-parse', '--show-toplevel'])
    base = _git(['-C', top, 'rev-parse', 'HEAD'])
    branch = 'delegate/' + child[:8]
    wt = rt.home / 'worktrees' / child[:8]
    wt.parent.mkdir(parents=True, exist_ok=True)
    _git(['-C', top, 'worktree', 'add', '-b', branch, str(wt), 'HEAD'])
    return top, base, str(wt), branch


def _remove_worktree(repo, worktree, branch):
    """Best effort: a writing child that failed validation must not leave a checkout or branch behind."""
    subprocess.run(['git', '-C', str(repo), 'worktree', 'remove', '--force', str(worktree)],
                   capture_output=True, timeout=60)
    subprocess.run(['git', '-C', str(repo), 'branch', '-D', branch], capture_output=True, timeout=60)


def _commit(row):
    """A writing child's changes belong on its branch: commit them in its worktree before the parent reads the
    report, so `git merge <branch>` merges them and the diffstat names the files. A child that only writes leaves
    them untracked, and merging a branch with no commit merges nothing (a test of the full version caught it).
    Best effort: a missing worktree or a failed commit still leaves the files and the commands in the report."""
    wt = row.get('worktree') or ''
    if not wt or not os.path.isdir(wt):
        return
    try:
        if not _git(['-C', wt, 'status', '--porcelain']).strip():
            return                    # nothing changed: the branch already points at the base commit
        _git(['-C', wt, 'add', '-A'])
        _git(['-C', wt, '-c', 'user.name=Caminus delegate', '-c', 'user.email=delegate@caminus.invalid',
              'commit', '-q', '-m', 'delegate %s: what its report describes' % (row.get('child') or '')[:8]])
    except ValueError:
        pass                          # never a reason to lose the child's outcome: the report still lists both


def _survey(row):
    """What a writing child changed: its worktree's status, its diffstat against the base commit, and the two
    commands the parent runs to keep or drop it. Best effort: a worktree already removed says so."""
    wt, repo, branch = row.get('worktree') or '', row.get('repo') or '.', row.get('branch') or ''
    out = dict(worktree=wt, branch=branch, base=row.get('base') or '')
    _commit(row)
    base = row.get('base') or 'HEAD'
    try:
        # the commit's files and whatever is still uncommitted; a rename is its new name
        changed = {ln.strip() for ln in _git(['-C', wt, 'diff', '--name-only', base]).splitlines() if ln.strip()}
        changed |= {ln[3:].split(' -> ')[-1] for ln in _git(['-C', wt, 'status', '--porcelain']).splitlines() if ln.strip()}
        out['files'] = sorted(changed)[:50]
        out['diffstat'] = _git(['-C', wt, 'diff', '--stat', base])[:2000]
    except ValueError as e:
        out['files'], out['diffstat'] = [], str(e)[:200]
    out['merge'] = 'git -C %s merge %s' % (repo, branch)
    out['discard'] = ('git -C %s worktree remove --force %s && git -C %s branch -D %s'
                      % (repo, wt, repo, branch))
    return out


def start(rt, parent, args, prepared=None):
    """Create the child goal and register its brain; the caller runs it after its transaction commits.
    (Creating the goal is part of the parent's action, atomically; running it is not - the child needs turns of
    its own, and a nested loop inside the write transaction would deadlock on BEGIN.) `prepared` is prepare()'s
    result, which the runtime resolves before BEGIN (#382 review)."""
    p = prepared or prepare(rt, args)
    if p['permissions'] == 'write' and rt.store.goal(parent).get('plan') == 1:
        raise ValueError('a plan-first goal has no approved plan yet: it may not grant a child write '
                         'permissions (a child never gets more than its parent has). Explore with a read-only '
                         'child, or finish the plan first')
    limit = int(rt.config.get('delegate_max_children', DEFAULTS['max_children']))
    used = rt.store.db.execute('SELECT count(*) FROM delegates WHERE parent=?', (parent,)).fetchone()[0]
    if used >= limit:
        raise ValueError('this goal has used its %d delegate children (delegate_max_children): finish one, or '
                         'do the rest yourself' % limit)
    child = rt.store._enqueue('', max_steps=p['max_steps'], max_seconds=p['max_seconds'], kind='delegate')
    top = base = worktree = branch = ''
    try:
        if p['permissions'] == 'write':
            top, base, worktree, branch = _worktree(rt, child, p['repo'])
        prompt = CHILD_PROMPT.format(parent=parent[:8], task=p['task'], done=p['done'],
                                     perms=p['permissions'],
                                     perms_text=WRITE_TEXT if p['permissions'] == 'write' else READ_TEXT)
        if worktree:
            prompt += '\nYour worktree: %s (branch %s, off %s). Name files relative to it.' % (worktree, branch, base[:12])
        # the goals.prompt column too (a card and a restart read it); store.update refuses it on purpose - the
        # generic API never rewrites a goal's prompt, which for a top-level goal is the human's own words
        rt.store.db.execute('UPDATE goals SET prompt=? WHERE id=?', (prompt, child))
        rt.store.delegate_add(child, parent, p['who'].get('model') or '', p['who'].get('provider') or '',
                              p['role'], p['cap_usd'], permissions=p['permissions'], worktree=worktree,
                              branch=branch, base=base, repo=top)
        rid = ''
        try:
            from . import sim
            if sim.enabled(rt):
                rec = sim.record(rt, parent, dict(
                    problem_group='delegate child',
                    change='delegate %s a %s child: %s' % (p['role'], p['permissions'], p['task']),
                    rationale='a child goal is bounded by its caps ($%.2f, %d steps, %ds) and reports back; %s'
                              % (p['cap_usd'], p['max_steps'], p['max_seconds'],
                                 'it writes only in its own worktree' if worktree else 'it is read-only'),
                    predicted='the child finishes within its caps and its report says what it verified: "%s"'
                              % p['done'][:400]),
                    eval_context='delegate %s' % p['role'])
                rid = rec['id']
                rt.store.db.execute('UPDATE delegates SET sim=? WHERE child=?', (rid, child))
        except Exception:
            rid = ''                      # a SIM record is a record, never a reason a child cannot run
    except BaseException:
        # nothing of a refused call may remain: the child goal, and (a writing child's) worktree and branch.
        # The caller (the runtime's dispatch) already holds the store's transaction, so this must not open its own
        # BEGIN: a nested BEGIN raises and would mask the real error with 'cannot start a transaction within a
        # transaction' (the failure that hid a bad field name in the first run of the delegate tests).
        if rt.store.db.in_transaction:
            rt.store.db.execute('DELETE FROM delegates WHERE child=?', (child,))
            rt.store.db.execute('DELETE FROM goals WHERE id=?', (child,))
        else:
            with rt.store.transaction():
                rt.store.db.execute('DELETE FROM delegates WHERE child=?', (child,))
                rt.store.db.execute('DELETE FROM goals WHERE id=?', (child,))
        if worktree:
            _remove_worktree(top, worktree, branch)
        raise
    rt.delegate_models[child] = p['model']
    rt.delegate_caps[child] = p['cap_usd']
    rt.store.event('delegate_started', dict(child=child, model=p['who'].get('model'),
                                           provider=p['who'].get('provider'), role=p['role'],
                                           permissions=p['permissions'], worktree=worktree, branch=branch,
                                           cap_usd=p['cap_usd'], max_steps=p['max_steps'],
                                           max_seconds=p['max_seconds'], wait=p['wait'],
                                           sim=rid), parent)
    out = dict(child=child, model=p['who'].get('model'), provider=p['who'].get('provider'), role=p['role'],
               permissions=p['permissions'], cap_usd=p['cap_usd'], max_steps=p['max_steps'],
               max_seconds=p['max_seconds'], outcome='running', sim=rid, wait=p['wait'],
               note=('the child runs now; this result is replaced by its report' if p['wait'] else
                     'the child runs in the background; this goal waits for it and collect reads its report'))
    if worktree:
        out.update(worktree=worktree, branch=branch, base=base)
    if p['wait']:
        out['_delegate_run'] = True
    else:
        out['_delegate_async'] = True
    return out


def _report(rt, row, g, outcome=None):
    """The short structured report a parent reads: the child's goal, what it verified, what it skipped, its spend
    and (a writing child) what it changed and the commands that keep or drop it."""
    child, state = row['child'], g['state']
    if outcome is None:
        err = g.get('error') or ''
        if state == 'completed':
            outcome = 'completed'
        elif state == 'paused' and ('delegate' in err or 'budget' in err):
            outcome = 'capped'       # the dollar cap, the step budget or the time budget ended it
        elif state == 'cancelled':
            outcome = 'incomplete'   # its parent was cancelled: it did not finish its own work
        else:
            outcome = 'failed'
    report = dict(child=child, role=row.get('role'), model=row.get('model'), provider=row.get('provider'),
                  outcome=outcome, state=state, permissions=row.get('permissions') or 'read',
                  summary=(g.get('summary') or g.get('error') or '')[:2000],
                  done_check=(g.get('done_check') or '')[:1000],
                  verified=state == 'completed' and rt.store.verified_since_steer(child),
                  skipped=[it['text'] for it in rt.store.todo(child)['items'] if it['state'] != 'done'][:20],
                  steps=int(g.get('steps') or 0), spend_usd=round(row.get('spend_usd') or 0.0, 6),
                  allowance_usd=round(row.get('allowance_usd') or 0.0, 6), cap_usd=row.get('cap_usd') or 0.0)
    if row.get('worktree'):
        report.update(_survey(row))
    if outcome != 'completed':
        report['note'] = ('the child did not complete: %s. Use what it found, or do the rest yourself; a new '
                          'delegate gets a fresh child.' % (report['summary'][:300] or state))
    return report


def _close(rt, parent, report):
    """File a child's report once: its delegates row, the event the parent and a UI read, the SIM observation,
    and the brain registration that must not outlive the run. The first close wins (settle and run_child may both
    arrive at the same child)."""
    child = report['child']
    row = rt.store.delegate(child) or {}
    if not row or row.get('outcome'):
        return report
    rt.store.delegate_done(child, report['outcome'], report)
    rt.store.event('delegate_done', {k: v for k, v in report.items() if k not in ('summary', 'skipped')}, parent)
    rid = row.get('sim') or ''
    if rid:
        try:
            from . import sim
            sim.observe(rt, parent, rid, observed='%s in %d steps, $%.4f of $%.2f%s%s' % (
                report['outcome'], report['steps'], report['spend_usd'] + report['allowance_usd'],
                report['cap_usd'] or 0.0,
                ', verified' if report['verified'] else '',
                (', changed ' + ', '.join(report['files'][:5])) if report.get('files') else ''),
                eval_context='delegate child (%s)' % (row.get('role') or 'worker'))
        except Exception:
            pass                          # the record is a record, never a gate on the report
    rt.delegate_models.pop(child, None)
    rt.delegate_caps.pop(child, None)
    return report


def park(rt, parent, handle):
    """After a delegate(wait=false) call: the goal has nothing to do until its children report, so it waits
    rather than spending turns asking whether they are done. The loop's settle wakes it."""
    with rt.store.transaction():
        if rt.store.goal(parent)['state'] not in ('cancelled',):
            rt.store.update(parent, state='waiting_children')
    rt.store.event('delegate_parked', dict(child=handle.get('child')), parent)


def waiting(rt, parent):
    """This goal's children that are still running, oldest first."""
    return [r['child'] for r in rt.store.delegates(parent=parent, open_only=True)]


def settle(rt):
    """Close the children that finished, cancel those of a stopped parent, and wake a parent whose children are
    all done. The loop calls this once a pass, whichever goal it just ticked: children are ordinary queued goals,
    so they run on their own. Returns whether anything moved."""
    moved = False
    for row in rt.store.delegates(open_only=True):
        parent = row['parent']
        pstate = rt.store.goal(parent)['state']
        g = rt.store.goal(row['child'])
        if pstate in ('cancelled', 'paused', 'error'):
            if g['state'] in ('queued', 'working', 'waiting_model'):
                with rt.store.transaction():
                    rt.store.update(g['id'], state='cancelled',
                                    error='its parent stopped (%s) while it ran' % pstate)
                g = rt.store.goal(g['id'])
            if g['state'] in ('completed', 'cancelled', 'paused', 'error'):
                _close(rt, parent, _report(rt, row, g, 'incomplete' if g['state'] != 'completed' else None))
                moved = True
            continue
        if g['state'] in ('completed', 'cancelled', 'paused', 'error'):
            _close(rt, parent, _report(rt, row, g))
            moved = True
    # A parent parked on its children wakes once none is running: the reports arrive as one nudge, the way peer
    # mail does, so its next turn has them in the conversation it reads.
    for parent in {r['parent'] for r in rt.store.delegates()}:
        if rt.store.goal(parent)['state'] != 'waiting_children':
            continue
        if rt.store.delegates(parent=parent, open_only=True):
            continue
        rows = rt.store.delegates(parent=parent)
        lines, ids = [], []
        for r in rows[-8:]:
            try:
                rep = json.loads(r.get('report') or '{}')
            except ValueError:
                rep = {}
            ids.append(r['child'])
            lines.append('- %s %s · %s steps · %s' % (r['child'][:8], r['outcome'] or '?', rep.get('steps', '?'),
                                                      ' '.join(str(rep.get('summary') or '').split())[:300]))
        text = ('[delegate] your children finished. Their reports are below; collect reads the structured ones. '
                'Verify what you act on.\n' + '\n'.join(lines))
        with rt.store.transaction():
            if rt.store.goal(parent)['state'] != 'waiting_children':
                continue
            rt.store.update(parent, state='working')
            rt.store.db.execute('INSERT INTO inbox(goal,text,kind) VALUES(?,?,?)', (parent, text, 'nudge'))
        rt.store.event('delegate_reports', dict(children=ids[-8:]), parent)
        moved = True
    return moved


def collect(rt, parent, args=None):
    """This goal's children: the reports of the finished ones (the same dict the report event carries) and the
    ids of the ones still running. `settle` first, so a child that finished a moment ago is not reported running."""
    settle(rt)
    done, running = [], []
    for r in rt.store.delegates(parent=parent):
        if r.get('outcome'):
            try:
                rep = json.loads(r.get('report') or '{}')
            except ValueError:
                rep = dict(child=r['child'])
            rep.setdefault('child', r['child'])
            rep.setdefault('outcome', r['outcome'])
            done.append(rep)
        else:
            running.append(dict(child=r['child'], role=r.get('role'), permissions=r.get('permissions') or 'read',
                                cap_usd=r.get('cap_usd'), started=r.get('started')))
    return dict(children=done, running=running,
                note=('no children yet' if not done and not running else
                      '%d finished, %d running' % (len(done), len(running))))


def run_child(rt, parent, job):
    """Run one child through the loop, in this call (wait=true), and turn its goal into the report the parent
    reads. The child's stop also watches the parent: cancelling, pausing or failing the parent ends the child at
    its next check (#382 review: _stop alone is only the daemon's stop, so a cancelled parent's child ran on)."""
    child = job['child']
    daemon_stop = getattr(rt, '_stop', None)

    def parent_gone():
        """True when the daemon is stopping, or the parent was cancelled, paused or failed while the child ran."""
        if daemon_stop and daemon_stop():
            return True
        return rt.store.goal(parent)['state'] in ('cancelled', 'paused', 'error')

    try:
        rt.run(target=child, stop=parent_gone)
        g = rt.store.goal(child)
        if rt.store.goal(parent)['state'] == 'cancelled' and g['state'] in ('working', 'queued', 'waiting_model'):
            with rt.store.transaction():
                rt.store.update(child, state='cancelled', error='its parent was cancelled while it ran')
            g = rt.store.goal(child)
        if g['state'] in ('working', 'queued', 'waiting_model'):
            # it must not be left runnable: the next turn would pick the parent's brain (its model registration
            # is this process's), and no other runtime can serve it either
            with rt.store.transaction():
                rt.store.update(child, state='paused', error='delegate child did not finish: its model was '
                                                            'unreachable or it ran out of its budget')
            g = rt.store.goal(child)
        return _close(rt, parent, _report(rt, rt.store.delegate(child) or {}, g))
    finally:
        rt._stop = daemon_stop


def capped(rt, gid):
    """Pause a child whose spend (money + plan allowance) reached its cap; True when it was paused."""
    row = rt.store.delegate(gid)
    if not row:
        return False
    cap = row.get('cap_usd')
    if cap is None:
        return False                      # only a missing cap is no cap: a stored 0 is a $0 cap, and it pauses
    cap = float(cap)
    spend, allowance = float(row.get('spend_usd') or 0.0), float(row.get('allowance_usd') or 0.0)
    if spend + allowance < cap:
        return False
    with rt.store.transaction():
        rt.store.update(gid, state='paused', error='delegate spend cap reached: $%.4f of $%.4f' % (spend + allowance, cap))
    rt.store.event('delegate_capped', dict(spend_usd=spend, allowance_usd=allowance, cap_usd=cap), gid)
    return True


def note_spend(rt, gid, usd, allowance):
    """A child's turn: the per-subagent ledger (delegates.spend_usd / allowance_usd), the metric the cap reads."""
    if gid not in (getattr(rt, 'delegate_caps', None) or ()):
        return
    rt.store.delegate_spend(gid, usd or 0.0, allowance or 0.0)


def sweep(rt):
    """At a runtime start, a child left runnable belongs to a run that ended (the store has one writer, and the
    runtime lock proved no other process holds it): pause it so it cannot run later on the wrong brain, and close
    its report as orphaned - its parent, if it is still waiting, wakes from `settle`."""
    for row in rt.store.delegates(open_only=True):
        g = rt.store.goal(row['child'])
        orphaned = False
        if g['state'] in ('queued', 'working', 'waiting_model'):
            with rt.store.transaction():
                rt.store.update(g['id'], state='paused', error='its runtime ended mid-delegation')
            g = rt.store.goal(g['id'])
            orphaned = True
        if g['state'] in ('completed', 'cancelled', 'paused', 'error'):
            _close(rt, row['parent'], _report(rt, row, g, 'orphaned' if orphaned else None))
