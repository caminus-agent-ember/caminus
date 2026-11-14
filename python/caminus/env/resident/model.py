"""OpenAI-compatible local-model transport; no engine, GPU or agent-CLI dependency."""
import json
import os
import urllib.error
import urllib.parse
import urllib.request


class ModelError(Exception):
    pass


class Model:
    def __init__(self, config, opener=urllib.request.urlopen):
        self.config, self.opener = config, opener
        self.base = config['endpoint'].rstrip('/')
        parsed = urllib.parse.urlsplit(self.base)
        if parsed.scheme not in ('http', 'https') or not parsed.hostname or parsed.username or parsed.password:
            raise ValueError('endpoint must be an HTTP(S) URL without embedded credentials')
        self.root = self.base[:-3] if self.base.endswith('/v1') else self.base
        self.context = config.get('context')
        self.counter = True
        self.last_usage = {}

    def request(self, url, body=None):
        headers = {'Content-Type': 'application/json'}
        key = os.environ.get(self.config.get('api_key_env', 'CAMINUS_AGENT_API_KEY'), 'caminus-local')
        headers['Authorization'] = 'Bearer ' + key
        req = urllib.request.Request(url, None if body is None else json.dumps(body).encode(), headers)
        try:
            with self.opener(req, timeout=float(self.config.get('timeout', 900))) as r:
                raw = r.read(16 * 1024 * 1024 + 1)
                if len(raw) > 16 * 1024 * 1024:
                    raise ModelError('model response exceeds16MiB')
                return json.loads(raw)
        except (OSError, ValueError, urllib.error.HTTPError) as e:
            raise ModelError(str(e)) from e

    def context_size(self):
        if self.context is None:
            p = self.request(self.root + '/props')
            self.context = p.get('n_ctx')
        if isinstance(self.context, bool) or not isinstance(self.context, int) or self.context < 1024:
            raise ModelError('backend must report a positive context window, or set --context explicitly')
        return self.context

    def complete(self, messages, tools=None, max_tokens=None):
        body = dict(model=self.config['model'], messages=messages, stream=False,
                    max_tokens=max_tokens or int(self.config.get('max_tokens', 4096)),
                    temperature=float(self.config.get('temperature', .6)))
        if tools:
            body['tools'] = tools
        result = self.request(self.base + '/chat/completions', body)
        self.last_usage = result.get('usage') or {}
        try:
            m = result['choices'][0]['message']
            out = dict(role='assistant', content=m.get('content') or '')
            calls = m.get('tool_calls') or []
            if calls:
                ids = [c['id'] for c in calls]
                if len(ids) != len(set(ids)) or any(not isinstance(i, str) or not i for i in ids):
                    raise ValueError('tool call ids must be nonempty and unique')
                for c in calls:
                    if not isinstance(c['function']['name'], str) or not isinstance(c['function']['arguments'], str):
                        raise ValueError('invalid tool call')
                out['tool_calls'] = [dict(id=c['id'], type='function', function=c['function']) for c in calls]
            if not out['content'] and not calls:
                raise ValueError('empty assistant response')
            return out
        except (KeyError, IndexError, TypeError, ValueError) as e:
            raise ModelError('invalid model response: ' + str(e)) from e

    def tokens(self, messages, tools):
        if self.counter:
            try:
                system, converted = [], []
                for m in messages:
                    if m['role'] == 'system':
                        system.append(m['content'])
                        continue
                    if m['role'] == 'tool':
                        converted.append(dict(role='user', content=[dict(type='tool_result', tool_use_id=m['tool_call_id'], content=m['content'])]))
                        continue
                    blocks = [dict(type='text', text=m['content'])] if m.get('content') else []
                    for call in m.get('tool_calls') or []:
                        f = call['function']
                        try:
                            args = json.loads(f['arguments'])
                        except ValueError:
                            args = dict(raw_arguments=f['arguments'])
                        blocks.append(dict(type='tool_use', id=call['id'], name=f['name'], input=args))
                    converted.append(dict(role=m['role'], content=blocks))
                ts = [dict(name=t['function']['name'], description=t['function']['description'], input_schema=t['function']['parameters']) for t in tools]
                result = self.request(self.base + '/messages/count_tokens', dict(model=self.config['model'], system='\n'.join(system), messages=converted, tools=ts))
                value = result['input_tokens']
                if isinstance(value, bool) or not isinstance(value, int) or value < 0:
                    raise ValueError('invalid token count')
                return value
            except (ModelError, KeyError, ValueError):
                # Other compatible providers need not implement Caminus's counter.
                # UTF8 bytes plus framing is conservative for byte-based tokenizers.
                self.counter = False
        return len(json.dumps(dict(messages=messages, tools=tools), ensure_ascii=False).encode()) + 256 * len(messages)
