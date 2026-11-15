"""The resident's durable goals, conversation, actions and observable event stream."""
from contextlib import contextmanager
import json
from pathlib import Path
import re
import sqlite3
import time
import uuid


def agent_home(name, home=None):
    if not re.fullmatch(r'[A-Za-z0-9][A-Za-z0-9_.-]{0,63}', name):
        raise ValueError('agent name: 1..64 letters, digits, dots, underscores or hyphens')
    return Path(home).expanduser().resolve() if home else Path.home() / 'homes' / name


def atomic_json(path, value, mode=0o600, group=None):
    path = Path(path)
    path.parent.mkdir(parents=True, exist_ok=True)
    tmp = path.with_name(path.name + '.' + uuid.uuid4().hex + '.tmp')
    try:
        with tmp.open('x') as f:
            tmp.chmod(mode)
            if group is not None:
                import os
                os.fchown(f.fileno(), -1, group)
            json.dump(value, f, ensure_ascii=False)
            f.flush()
            import os
            os.fsync(f.fileno())
        tmp.replace(path)
    finally:
        tmp.unlink(missing_ok=True)


class Store:
    def __init__(self, home):
        self.home = Path(home)
        self.home.mkdir(parents=True, exist_ok=True, mode=0o700)
        self.db = sqlite3.connect(self.home / 'agent.sqlite3', timeout=10, isolation_level=None)
        self.db.row_factory = sqlite3.Row
        self.db.execute('PRAGMA journal_mode=WAL')
        self.db.execute('PRAGMA busy_timeout=10000')
        self.db.executescript('''
          CREATE TABLE IF NOT EXISTS goals(
            id TEXT PRIMARY KEY, prompt TEXT NOT NULL, state TEXT NOT NULL,
            messages TEXT NOT NULL DEFAULT '[]', summary TEXT NOT NULL DEFAULT '',
            error TEXT NOT NULL DEFAULT '', steps INTEGER NOT NULL DEFAULT 0,
            step_limit INTEGER, deadline REAL, created REAL NOT NULL, updated REAL NOT NULL);
          CREATE TABLE IF NOT EXISTS actions(
            id TEXT PRIMARY KEY, goal TEXT NOT NULL, call_id TEXT NOT NULL,
            name TEXT NOT NULL, args TEXT NOT NULL, state TEXT NOT NULL,
            result TEXT, bridge_id TEXT, UNIQUE(goal,call_id));
          CREATE TABLE IF NOT EXISTS events(
            seq INTEGER PRIMARY KEY AUTOINCREMENT, ts REAL NOT NULL,
            goal TEXT, kind TEXT NOT NULL, data TEXT NOT NULL);
          CREATE TABLE IF NOT EXISTS inbox(
            id INTEGER PRIMARY KEY AUTOINCREMENT, goal TEXT NOT NULL,
            text TEXT NOT NULL, consumed INTEGER NOT NULL DEFAULT 0);
          CREATE TABLE IF NOT EXISTS peer_deliveries(id TEXT PRIMARY KEY, goal TEXT, error TEXT);
        ''')

    def close(self):
        self.db.close()

    @contextmanager
    def transaction(self):
        self.db.execute('BEGIN IMMEDIATE')
        try:
            yield
            self.db.execute('COMMIT')
        except BaseException:
            self.db.execute('ROLLBACK')
            raise

    def event(self, kind, data, goal=None):
        self.db.execute('INSERT INTO events(ts,goal,kind,data) VALUES(?,?,?,?)',
                        (time.time(), goal, kind, json.dumps(data, ensure_ascii=False)))

    def events(self, after=0, limit=200):
        return [dict(seq=r['seq'], ts=r['ts'], goal=r['goal'], kind=r['kind'], data=json.loads(r['data']))
                for r in self.db.execute('SELECT * FROM events WHERE seq>? ORDER BY seq LIMIT ?', (after, limit))]

    def enqueue(self, prompt, max_steps=None, max_seconds=None):
        if not prompt.strip():
            raise ValueError('goal must not be empty')
        if max_steps is not None and max_steps < 1 or max_seconds is not None and max_seconds <= 0:
            raise ValueError('test budgets must be positive')
        with self.transaction():
            gid = self._enqueue(prompt, max_steps, max_seconds)
        return gid

    def _enqueue(self, prompt, max_steps=None, max_seconds=None):
        """Caller owns the transaction, including any delivery checkpoint."""
        gid, now = uuid.uuid4().hex, time.time()
        self.db.execute('INSERT INTO goals(id,prompt,state,messages,step_limit,deadline,created,updated) VALUES(?,?,?,?,?,?,?,?)',
            (gid, prompt, 'queued', json.dumps([dict(role='user', content=prompt)]), max_steps,
             now + max_seconds if max_seconds else None, now, now))
        self.event('goal_queued', dict(prompt=prompt), gid)
        return gid

    def goal(self, gid):
        r = self.db.execute('SELECT * FROM goals WHERE id=?', (gid,)).fetchone()
        if r is None:
            raise ValueError('unknown goal ' + gid)
        out = dict(r)
        out['messages'] = json.loads(out['messages'])
        return out

    def update(self, gid, **fields):
        allowed = {'state', 'messages', 'summary', 'error', 'steps', 'step_limit', 'deadline'}
        if not fields or not set(fields) <= allowed:
            raise ValueError('invalid goal fields')
        if 'messages' in fields:
            fields['messages'] = json.dumps(fields['messages'], ensure_ascii=False)
        fields['updated'] = time.time()
        sql = ','.join(k + '=?' for k in fields)
        self.db.execute('UPDATE goals SET ' + sql + ' WHERE id=?', [*fields.values(), gid])

    def next_goal(self):
        rows = self.db.execute("SELECT id FROM goals WHERE state IN ('queued','working','waiting_model','waiting_root','waiting_user') ORDER BY created")
        return [r['id'] for r in rows]

    def actions(self, gid):
        return [dict(r) for r in self.db.execute("SELECT * FROM actions WHERE goal=? AND state!='done' ORDER BY rowid", (gid,))]

    def model_reply(self, gid, message, reasoning=''):
        """Store one reply. reasoning is displayed with the reply but never sent back to the model:
        it is not part of messages, so replay and the token budget are unchanged."""
        with self.transaction():
            g = self.goal(gid)
            if g['state'] == 'cancelled':
                return False
            self.update(gid, messages=[*g['messages'], message], steps=g['steps'] + 1, state='working', error='')
            for call in message.get('tool_calls') or []:
                f = call['function']
                self.db.execute('INSERT INTO actions(id,goal,call_id,name,args,state) VALUES(?,?,?,?,?,?)',
                    (uuid.uuid4().hex, gid, call['id'], f['name'], f['arguments'], 'pending'))
            self.event('assistant', dict(message, reasoning=reasoning) if reasoning else message, gid)
        return True

    def action_result(self, action, result):
        with self.transaction():
            g = self.goal(action['goal'])
            # Only the runtime lock's owner calls this. Completed actions are never replayed.
            if self.db.execute('SELECT state FROM actions WHERE id=?', (action['id'],)).fetchone()[0] == 'done':
                return
            self.db.execute("UPDATE actions SET state='done',result=? WHERE id=?", (json.dumps(result), action['id']))
            msg = dict(role='tool', tool_call_id=action['call_id'], content=json.dumps(result, ensure_ascii=False))
            self.update(g['id'], messages=[*g['messages'], msg], state='cancelled' if g['state']=='cancelled' else 'working')
            self.event('tool_result', dict(name=action['name'], call_id=action['call_id'], result=result), g['id'])

    def status(self):
        goals = [dict(r) for r in self.db.execute('SELECT id,prompt,state,summary,error,steps,created,updated FROM goals ORDER BY created DESC LIMIT 50')]
        waiting = [dict(r) for r in self.db.execute("SELECT goal,name,bridge_id FROM actions WHERE state='waiting'")]
        return dict(goals=goals, waiting=waiting, last_event=self.db.execute('SELECT coalesce(max(seq),0) FROM events').fetchone()[0])
