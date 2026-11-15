"""Big tool outputs go to a file; the model gets a preview and the path.

A build log, a test run or a large read can be hundreds of kilobytes. Put into the conversation whole, one result
fills the window and every later turn re-reads it. Past LIMIT_CHARS (about 8K tokens) the large text fields of a
result are saved under ~/outputs/ and replaced by their head and tail with the file's path, so the agent can read or
search the rest when it needs it (read with offset, grep in the terminal, history_search for the preview).
"""
import json
import time
from pathlib import Path

LIMIT_CHARS = 32_000      # a result's JSON past this is spilled (about 8K tokens)
FIELD_MIN = 4_000         # only text fields longer than this are worth moving out
HEAD, TAIL = 6_000, 2_000 # characters of a spilled field kept in the conversation
KEEP_FILES = 200          # newest spilled files kept in ~/outputs


def _size(result):
    return len(json.dumps(result, ensure_ascii=False))


def _prune(folder):
    files = sorted((p for p in folder.glob('*.txt') if p.is_file()), key=lambda p: p.stat().st_mtime)
    for p in files[:-KEEP_FILES]:
        try:
            p.unlink()
        except OSError:
            pass


def spill(home, name, action_id, result):
    """Return the result to record: unchanged when small, with large fields moved to files when not."""
    if not isinstance(result, dict) or _size(result) <= LIMIT_CHARS:
        return result
    folder = Path(home) / 'outputs'
    try:
        folder.mkdir(exist_ok=True)
    except OSError:
        return result                                   # nowhere to write: keep the result, never lose it
    stamp = time.strftime('%Y%m%d-%H%M%S')
    out, saved = dict(result), []
    fields = sorted((k for k, v in out.items() if isinstance(v, str) and len(v) > FIELD_MIN),
                    key=lambda k: len(out[k]), reverse=True)
    for k in fields:
        if _size(out) <= LIMIT_CHARS:
            break
        text = out[k]
        rel = f'outputs/{stamp}-{name}-{action_id[:8]}-{k}.txt'
        try:
            (Path(home) / rel).write_text(text)
        except OSError:
            continue
        lines = text.count('\n') + 1
        out[k] = (text[:HEAD] + f'\n… [{len(text) - HEAD - TAIL:,} characters omitted; the whole {k} ({lines:,} '
                  f'lines) is in {rel}] …\n' + text[-TAIL:])
        saved.append(dict(field=k, path=rel, chars=len(text), lines=lines))
    if _size(out) > LIMIT_CHARS:
        # Still too big (many fields, a long list): the whole result goes to one file.
        rel = f'outputs/{stamp}-{name}-{action_id[:8]}-result.txt'
        try:
            whole = json.dumps(result, ensure_ascii=False, indent=1)
            (Path(home) / rel).write_text(whole)
        except OSError:
            return result
        out = dict(preview=whole[:HEAD], saved=[dict(field='(whole result)', path=rel, chars=len(whole))])
        saved = out['saved']
    else:
        out['saved'] = saved
    out['note'] = ('large output saved to a file and shortened here; read the file (with offset) or grep it in the '
                   'terminal for the part you need')
    _prune(folder)
    return out
