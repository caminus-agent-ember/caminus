"""Model transports: OpenAI-compatible chat completions (the local server) and Anthropic's Messages API.
Standard library only; no engine, GPU or agent-CLI dependency."""
import json
import os
import time
import urllib.error
import urllib.parse
import urllib.request


class ModelError(Exception):
    pass


class ModelContextError(ModelError):
    pass


class ModelTruncated(ModelError):
    def __init__(self, partial, reason='model output budget exhausted; increase --max-tokens and resume this goal',
                 retryable=False):
        super().__init__(reason)
        self.partial = partial
        # A budget truncation needs a human decision; a stream cut is the server going away mid-reply and
        # the same turn succeeds once it is back, so the runtime retries it instead of pausing the goal.
        self.retryable = retryable


class ModelRefused(ModelError):
    """the provider declined the request (Messages API stop_reason 'refusal'): retrying the same turn cannot help"""
    status = 400


ANTHROPIC_VERSION = '2026-10-21'

# USD per million tokens (input, output, cache read, 5-minute cache write), below and above a prompt-length line.
# From the Claude API reference as cached 2026-11-03: check live pricing before budgeting on these numbers.
PRICES = {'claude-haiku-5-5': ((0.10, 0.50, 0.01, 0.125), (0.50, 2.50, 0.05, 0.625), 100_000),
          'claude-sonnet-5-5': ((2.00, 10.00, 0.10, 2.50),) * 2 + (10**9,),
          'claude-opus-5-5': ((4.00, 20.00, 0.20, 5.00),) * 2 + (10**9,)}


def price(model, usage):
    """the USD cost of one call's usage on a priced model, or None when the model's price is not known"""
    row = PRICES.get(model)
    if row is None:
        return None
    low, high, line = row
    usage = usage or {}
    read = usage.get('cache_read_input_tokens') or 0
    wrote = usage.get('cache_creation_input_tokens') or 0
    out = usage.get('completion_tokens', usage.get('output_tokens')) or 0
    # last_usage reports prompt_tokens as everything sent; the uncached part is what remains after the cache.
    total = usage.get('prompt_tokens')
    fresh = (total - read - wrote) if isinstance(total, int) else (usage.get('input_tokens') or 0)
    rate = high if fresh + read + wrote > line else low
    return (max(0, fresh) * rate[0] + out * rate[1] + read * rate[2] + wrote * rate[3]) / 1e6
LOOPBACK = ('127.0.0.1', 'localhost', '::1')


def http_error(e):
    """a ModelError carrying the status, the provider's reason (the body's first 2 KB) and any retry-after"""
    try:
        detail = e.read(2048).decode('utf-8', 'replace').strip()
    except (OSError, ValueError):
        detail = ''
    finally:
        e.close()
    err = ModelError(str(e) + (': ' + detail if detail else ''))
    err.status = e.code      # a refusal must not be retried like an outage
    try:
        err.retry_after = float((e.headers or {}).get('retry-after') or 0) or None
    except (TypeError, ValueError):
        err.retry_after = None
    return err


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
        self.api = config.get('api', 'openai')
        if self.api not in ('openai', 'anthropic'):
            raise ValueError('api must be openai or anthropic')
        # A remote provider bills each call: the runtime bounds its retries (a local server is waited for).
        self.remote = parsed.hostname not in LOOPBACK
        self.context = config.get('context')
        if self.api == 'anthropic' and self.context is None:
            raise ValueError('the anthropic api needs an explicit context window (--context)')
        self.counter = True
        self.last_usage = {}
        self.last_speed = None

    def headers(self, stream=False):
        name = self.config.get('api_key_env', 'CAMINUS_AGENT_API_KEY')
        key = os.environ.get(name)
        headers = {'Content-Type': 'application/json'}
        if stream:
            headers['Accept'] = 'text/event-stream'
        if self.api == 'anthropic':
            # No key is refused before anything is sent, unless the endpoint is a local relay that adds it.
            if key:
                headers['x-api-key'] = key
            elif self.remote:
                err = ModelError('the API key variable %s is not set in the agent environment' % name)
                err.status = 401
                raise err
            headers['anthropic-version'] = ANTHROPIC_VERSION
        else:
            headers['Authorization'] = 'Bearer ' + (key or 'caminus-local')
        return headers

    def request(self, url, body=None):
        req = urllib.request.Request(url, None if body is None else json.dumps(body).encode(), self.headers())
        try:
            with self.opener(req, timeout=float(self.config.get('timeout', 900))) as r:
                raw = r.read(16 * 1024 * 1024 + 1)
                if len(raw) > 16 * 1024 * 1024:
                    raise ModelError('model response exceeds16MiB')
                return json.loads(raw)
        except urllib.error.HTTPError as e:
            raise http_error(e) from e
        except (OSError, ValueError) as e:
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
        if self.api == 'anthropic':
            return self._anthropic(messages, tools, min(max_tokens or int(self.config.get('max_tokens', 4096)),
                                                        remaining), model, on_delta)
        body = dict(model=model or self.config['model'], messages=messages, stream=on_delta is not None,
                    max_tokens=min(max_tokens or int(self.config.get('max_tokens', 4096)), remaining),
                    temperature=float(self.config.get('temperature', .6)))
        for name in ('top_k','top_p','min_p','presence_penalty'):
            if name in self.config:
                body[name] = self.config[name]
        if tools:
            body['tools'] = tools
        if on_delta is not None:
            # usage in the final chunk gives the generated token count for a client-measured decode speed
            body['stream_options'] = dict(include_usage=True)
            return self._stream(body, on_delta)
        result = self.request(self.base + '/chat/completions', body)
        self.last_usage = result.get('usage') or {}
        self.last_speed = None     # no decode timing without streamed pieces: nothing is reported rather than a wrong number
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
        req = urllib.request.Request(self.base + '/chat/completions', json.dumps(body).encode(),
                                     self.headers(stream=True))
        content, reasoning, calls, finish, usage = [], [], {}, None, {}
        self.last_speed = None
        t_first = t_last = None    # decode window: first choice-chunk to the last chunk read, prefill excluded
        done_seen = False
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
                        done_seen = True
                        break
                    chunk = json.loads(data)
                    now = time.monotonic()
                    if chunk.get('choices') and t_first is None:
                        t_first = now
                    t_last = now
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
        except urllib.error.HTTPError as e:
            raise http_error(e) from e
        except (OSError, ValueError, KeyError, IndexError, TypeError) as e:
            raise ModelError(str(e)) from e
        self.last_usage = usage
        tokens = usage.get('completion_tokens') if isinstance(usage, dict) else None
        if isinstance(tokens, int) and tokens > 0 and t_first and t_last > t_first:
            self.last_speed = dict(tokens=tokens, seconds=round(t_last - t_first, 3),
                                   tps=round(tokens / (t_last - t_first), 1))
        try:
            out = dict(role='assistant', content=''.join(content))
            if reasoning:
                out['reasoning_content'] = ''.join(reasoning)
            if finish is None and not done_seen:
                # The connection closed before the turn ended: a proxy or backend cut the stream. The text
                # so far is shown with the error, and nothing of this reply is stored or executed.
                raise ModelTruncated(out, 'the reply ended before it was complete; nothing was executed',
                                     retryable=True)
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
        if self.counter and self.api == 'anthropic':
            try:
                body = self._to_anthropic(messages, tools)
                body.pop('max_tokens', None)
                value = self.request(self.base + '/messages/count_tokens', body)['input_tokens']
                if isinstance(value, bool) or not isinstance(value, int) or value < 0:
                    raise ValueError('invalid token count')
                return value
            except (ModelError, KeyError, ValueError, TypeError) as e:
                if getattr(e, 'status', 0) in (401, 402, 403):
                    raise              # a missing or refused key is not a counting problem
                self.counter = False
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

    # ---- Anthropic Messages API ---------------------------------------------------------------------------

    def _to_anthropic(self, messages, tools, model=None):
        """the resident's OpenAI-shaped request as a Messages API body.

        Prompt caching: tools and the leading system prompt are the stable prefix (breakpoint 1 on the system
        block); the newest stored message carries breakpoint 2, so each turn reads the whole history from the
        cache and writes only what it added. The trailing system message (memory, loop state) is rebuilt every
        turn and therefore sits after both breakpoints, as a mid-conversation system message."""
        system, rest = [], list(messages)
        while rest and rest[0]['role'] == 'system':
            system.append(rest.pop(0)['content'])
        tail = rest.pop()['content'] if rest and rest[-1]['role'] == 'system' else None
        out = []

        def add(role, blocks):
            if not blocks:
                return
            if out and out[-1]['role'] == role:      # steers and tool results arrive as separate messages
                out[-1]['content'].extend(blocks)
            else:
                out.append(dict(role=role, content=list(blocks)))

        for m in rest:
            role = m['role']
            if role == 'system':                     # a mid-history system message: kept as an operator note
                add('user', [dict(type='text', text='[system] ' + m['content'])])
            elif role == 'tool':
                add('user', [dict(type='tool_result', tool_use_id=m['tool_call_id'], content=m['content'] or '')])
            elif role == 'assistant':
                blocks = [dict(type='text', text=m['content'])] if m.get('content') else []
                for call in m.get('tool_calls') or []:
                    f = call['function']
                    try:
                        args = json.loads(f['arguments']) if f['arguments'].strip() else {}
                    except ValueError:
                        args = dict(raw_arguments=f['arguments'])
                    if not isinstance(args, dict):
                        args = dict(value=args)
                    blocks.append(dict(type='tool_use', id=call['id'], name=f['name'], input=args))
                add('assistant', blocks)
            else:
                add('user', [dict(type='text', text=m['content'] or ' ')])
        if out and out[0]['role'] != 'user':
            out.insert(0, dict(role='user', content=[dict(type='text', text='(conversation resumed)')]))
        if out:
            out[-1]['content'][-1]['cache_control'] = dict(type='ephemeral')
        if tail:
            if out and out[-1]['role'] == 'user':
                out.append(dict(role='system', content=tail))
            else:
                # a system message must follow a user turn, and a final assistant turn would be a prefill
                add('user', [dict(type='text', text='[system] ' + tail)])
        elif out and out[-1]['role'] == 'assistant':
            add('user', [dict(type='text', text='Continue.')])
        body = dict(model=model or self.config['model'], messages=out)
        if system:
            body['system'] = [dict(type='text', text='\n\n'.join(system), cache_control=dict(type='ephemeral'))]
        if tools:
            body['tools'] = [dict(name=t['function']['name'], description=t['function'].get('description', ''),
                                  input_schema=t['function']['parameters']) for t in tools]
        # Thinking stays off: the store keeps no thinking blocks, and replaying a turn without them is a
        # history edit. 'disabled' is accepted up to effort high, so the effort is set rather than defaulted.
        # thinking 'adaptive' (the advisor: current Opus and Sonnet refuse thinking off) leaves it to the model;
        # thinking blocks in the reply are skipped, since nothing replays them.
        if self.config.get('thinking', 'disabled') == 'disabled':
            body['thinking'] = dict(type='disabled')
        body['output_config'] = dict(effort=self.config.get('effort', 'medium'))
        return body

    def _anthropic(self, messages, tools, max_tokens, model, on_delta):
        body = self._to_anthropic(messages, tools, model)
        body['max_tokens'] = max_tokens
        body['stream'] = True
        allowed = ('low', 'medium', 'high') if self.config.get('thinking', 'disabled') == 'disabled' else (
            'low', 'medium', 'high', 'xhigh', 'max')
        if self.config.get('effort', 'medium') not in allowed:
            raise ValueError('effort must be one of %s here' % ', '.join(allowed))
        return self._anthropic_stream(body, on_delta or (lambda kind, value: None))

    def _anthropic_stream(self, body, on_delta):
        """read a Messages API event stream into the reply shape complete() returns"""
        req = urllib.request.Request(self.base + '/messages', json.dumps(body).encode(), self.headers(stream=True))
        text, blocks, stop, usage, details = [], {}, None, {}, None
        self.last_speed = None
        t_first = t_last = None
        done = False
        try:
            with self.opener(req, timeout=float(self.config.get('timeout', 900))) as r:
                for raw in r:
                    line = raw.decode('utf-8', 'replace').strip()
                    if not line.startswith('data:'):
                        continue          # 'event:' names repeat the data's type; blank lines end an event
                    ev = json.loads(line[5:].strip())
                    kind = ev.get('type')
                    now = time.monotonic()
                    if kind == 'message_start':
                        usage.update((ev.get('message') or {}).get('usage') or {})
                    elif kind == 'content_block_start':
                        block = ev['content_block']
                        blocks[ev['index']] = slot = dict(type=block['type'], id=block.get('id', ''),
                                                          name=block.get('name', ''), json='')
                        if slot['type'] == 'tool_use' and on_delta('tool', slot['name']) is False:
                            raise Cancelled()
                    elif kind == 'content_block_delta':
                        if t_first is None:
                            t_first = now
                        t_last = now
                        delta = ev['delta']
                        if delta.get('type') == 'text_delta':
                            text.append(delta['text'])
                            if on_delta('content', delta['text']) is False:
                                raise Cancelled()
                        elif delta.get('type') == 'input_json_delta':
                            blocks[ev['index']]['json'] += delta.get('partial_json', '')
                    elif kind == 'message_delta':
                        stop = (ev.get('delta') or {}).get('stop_reason') or stop
                        details = (ev.get('delta') or {}).get('stop_details') or details
                        usage.update(ev.get('usage') or {})
                    elif kind == 'message_stop':
                        done = True
                        break
                    elif kind == 'error':
                        err = ev.get('error') or {}
                        raise ModelError('%s: %s' % (err.get('type', 'error'), err.get('message', '')))
        except (Cancelled, ModelError):
            raise
        except urllib.error.HTTPError as e:
            raise http_error(e) from e
        except (OSError, ValueError, KeyError, IndexError, TypeError) as e:
            raise ModelError(str(e)) from e
        read = usage.get('cache_read_input_tokens') or 0
        wrote = usage.get('cache_creation_input_tokens') or 0
        out_tokens = usage.get('output_tokens') or 0
        self.last_usage = dict(prompt_tokens=(usage.get('input_tokens') or 0) + read + wrote,
                               completion_tokens=out_tokens, cache_read_input_tokens=read,
                               cache_creation_input_tokens=wrote)
        if out_tokens > 0 and t_first and t_last > t_first:
            self.last_speed = dict(tokens=out_tokens, seconds=round(t_last - t_first, 3),
                                   tps=round(out_tokens / (t_last - t_first), 1))
        out = dict(role='assistant', content=''.join(text))
        if not done:
            raise ModelTruncated(out, 'the reply ended before it was complete; nothing was executed', retryable=True)
        if stop == 'refusal':
            category = (details or {}).get('category')
            raise ModelRefused('the provider declined this request' + (' (%s)' % category if category else '')
                               + '; rephrase the goal or use another model')
        if stop == 'max_tokens':
            raise ModelTruncated(out)
        calls = []
        for i in sorted(blocks):
            b = blocks[i]
            if b['type'] == 'tool_use':
                calls.append(dict(id=b['id'], type='function',
                                  function=dict(name=b['name'], arguments=b['json'] or '{}')))
        try:
            return self._message(dict(out, tool_calls=calls) if calls else out)
        except (KeyError, IndexError, TypeError, ValueError) as e:
            raise ModelError('invalid model response: ' + str(e)) from e
