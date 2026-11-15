"""OpenAI-compatible local-model transport; no engine, GPU or agent-CLI dependency."""
import json
import os
import urllib.error
import urllib.parse
import urllib.request


class ModelError(Exception):
    pass


class ModelContextError(ModelError):
    pass


class ModelTruncated(ModelError):
    def __init__(self, partial):
        super().__init__('model output budget exhausted; increase --max-tokens and resume this goal')
        self.partial = partial


class Cancelled(Exception):
    """the goal was cancelled while the model was generating; nothing of this reply is stored"""


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

    def complete(self, messages, tools=None, max_tokens=None, model=None, on_delta=None):
        """One reply. With on_delta(kind, value) the reply streams: kind is 'reasoning', 'content' or 'tool'
        (a tool call's name starts), value is the text or the name. on_delta returning False cancels: the
        stream is closed and Cancelled is raised, so a cancelled turn stores no reply and runs no tool."""
        remaining = self.context_size() - self.tokens(messages, tools or []) - 256
        if remaining < 1:
            raise ModelContextError('request exceeds the model context; compact or shorten the goal')
        body = dict(model=model or self.config['model'], messages=messages, stream=on_delta is not None,
                    max_tokens=min(max_tokens or int(self.config.get('max_tokens', 4096)), remaining),
                    temperature=float(self.config.get('temperature', .6)))
        for name in ('top_k','top_p','min_p','presence_penalty'):
            if name in self.config:
                body[name] = self.config[name]
        if tools:
            body['tools'] = tools
        if on_delta is not None:
            return self._stream(body, on_delta)
        result = self.request(self.base + '/chat/completions', body)
        self.last_usage = result.get('usage') or {}
        try:
            m = result['choices'][0]['message']
            if result['choices'][0].get('finish_reason') == 'length':
                raise ModelTruncated(m)
            return self._message(m)
        except (KeyError, IndexError, TypeError, ValueError) as e:
            raise ModelError('invalid model response: ' + str(e)) from e

    def _message(self, m):
        """the reply in the shape the store keeps: validated tool calls, no unknown keys"""
        out = dict(role='assistant', content=m.get('content') or '')
        if m.get('reasoning_content'):
            out['reasoning_content'] = m['reasoning_content']
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

    def _stream(self, body, on_delta):
        """read the SSE reply, hand each piece to on_delta, assemble the message complete() would return.
        A backend that answers with one whole JSON body instead of a stream is accepted and reported once."""
        headers = {'Content-Type': 'application/json', 'Accept': 'text/event-stream'}
        key = os.environ.get(self.config.get('api_key_env', 'CAMINUS_AGENT_API_KEY'), 'caminus-local')
        headers['Authorization'] = 'Bearer ' + key
        req = urllib.request.Request(self.base + '/chat/completions', json.dumps(body).encode(), headers)
        content, reasoning, calls, finish, usage = [], [], {}, None, {}
        try:
            with self.opener(req, timeout=float(self.config.get('timeout', 900))) as r:
                for raw in r:
                    line = raw.decode('utf-8', 'replace').strip()
                    if not line or line.startswith(':'):
                        continue
                    if not line.startswith('data:'):
                        if not line.startswith('{'):
                            continue
                        whole = json.loads(line)      # not SSE: the whole reply at once
                        m = whole['choices'][0]['message']
                        for kind, value in (('reasoning', m.get('reasoning_content')), ('content', m.get('content'))):
                            if value and on_delta(kind, value) is False:
                                raise Cancelled()
                        for call in m.get('tool_calls') or []:
                            if on_delta('tool', call['function']['name']) is False:
                                raise Cancelled()
                        self.last_usage = whole.get('usage') or {}
                        if whole['choices'][0].get('finish_reason') == 'length':
                            raise ModelTruncated(m)
                        return self._message(m)
                    data = line[5:].strip()
                    if data == '[DONE]':
                        break
                    chunk = json.loads(data)
                    if chunk.get('usage'):
                        usage = chunk['usage']
                    for choice in chunk.get('choices') or []:
                        if choice.get('finish_reason'):
                            finish = choice['finish_reason']
                        delta = choice.get('delta') or {}
                        if delta.get('reasoning_content'):
                            reasoning.append(delta['reasoning_content'])
                            if on_delta('reasoning', delta['reasoning_content']) is False:
                                raise Cancelled()
                        if delta.get('content'):
                            content.append(delta['content'])
                            if on_delta('content', delta['content']) is False:
                                raise Cancelled()
                        for call in delta.get('tool_calls') or []:
                            slot = calls.setdefault(call.get('index', 0), dict(id='', name='', arguments=''))
                            if call.get('id'):
                                slot['id'] = call['id']
                            fn = call.get('function') or {}
                            if fn.get('name'):
                                first = not slot['name']
                                slot['name'] += fn['name']
                                if first and on_delta('tool', slot['name']) is False:
                                    raise Cancelled()
                            if fn.get('arguments'):
                                slot['arguments'] += fn['arguments']
        except Cancelled:
            raise
        except (OSError, ValueError, KeyError, IndexError, TypeError, urllib.error.HTTPError) as e:
            raise ModelError(str(e)) from e
        self.last_usage = usage
        try:
            out = dict(role='assistant', content=''.join(content))
            if reasoning:
                out['reasoning_content'] = ''.join(reasoning)
            if finish == 'length':
                raise ModelTruncated(out)
            ordered = [calls[i] for i in sorted(calls)]
            if ordered:
                return self._message(dict(out, tool_calls=[
                    dict(id=c['id'], type='function', function=dict(name=c['name'], arguments=c['arguments']))
                    for c in ordered]))
            return self._message(out)
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
