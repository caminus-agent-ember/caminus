"""The prompt cache's disk tier from the command line (caminus.cache.prompt; CPU only, nothing launched).

  caminus kv-cache list  [--dir DIR]                                  (from a checkout: python -I tools/kv_cache.py ...)
  caminus kv-cache prune [--dir DIR] [--older-than-days D] [--keep-per-root N] [--max-gb G] [--dry-run]
  caminus kv-cache purge [--dir DIR] [--older-than-days D]     (one store; no age: every entry)

DIR defaults to the cache folder (~/.cache/caminus/kv), which holds the root store and every model's store under it
(`caminus start NAME` uses kv/NAME). `list` shows every store and each entry's kind, tokens, size, age (since it was
written) and last use, beside a running server too (it reads headers only). `prune` removes entries by age, keeps at most
N parks per conversation root (the most recently used), and evicts the least recently used until the folder fits G; it skips
the store of a running server (stop it first, or let the server's own cap do it) and says so. `purge` is the old one-store
command: it takes the store's lock and refuses while a server runs on it."""
import argparse, os, sys, time


def _age(s):
    return f'{s / 86400:.1f} d' if s >= 86400 else f'{s / 3600:.1f} h'


def main(argv=None):
    ap = argparse.ArgumentParser(prog='caminus kv-cache', description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument('cmd', choices=('list', 'prune', 'purge')); ap.add_argument('--dir', default=None)
    ap.add_argument('--older-than-days', type=float, default=None)
    ap.add_argument('--keep-per-root', type=int, default=None)
    ap.add_argument('--max-gb', type=float, default=None)
    ap.add_argument('--dry-run', action='store_true', help='prune: say what would go, remove nothing')
    a = ap.parse_args(argv)
    try:
        from ..cache import prompt as pc          # (imports torch: the disk tier's staging buffers are torch tensors, CPU only)
    except ImportError as e:
        if e.name != 'torch' and not (e.name or '').startswith('torch.'):
            raise
        from ..device.torchcheck import status
        print(f'kv_cache: {a.cmd} refused: {status()[2]}', file=sys.stderr)
        return 2
    top = os.path.abspath(os.path.expanduser(a.dir or pc.DEFAULT_DIR))
    if a.cmd == 'purge':
        try:
            st = pc.DiskStore(top, staging=pc.Staging(piece=pc.PIECE, nbuf=1, pin=False))
        except pc.CacheError as e:
            print(f'kv_cache: purge refused: {e}. Stop the server first (one owner process at a time).', file=sys.stderr)
            return 2
        try:
            out = st.purge(all=a.older_than_days is None, older_than_s=None if a.older_than_days is None else a.older_than_days * 86400)
            print(f'purged {len(out)} entries from {st.root}')
        finally:
            st.close()
        return 0
    now = time.time()
    found = pc.stores(top)
    if a.cmd == 'list':
        total = 0
        for d, held, ents in found:
            print(f'{os.path.relpath(d, top) if d != top else "."}{"  (in use by a running server)" if held else ""}')
            for nm, h in sorted(ents, key=lambda x: -(x[1] or {}).get('_mtime', 0)):
                if h is None:
                    print(f'  {nm}  (unreadable: the next server start removes it)'); continue
                total += h['_bytes']
                made = h.get('created') or h['_mtime']
                print(f'  {nm}  {h.get("kind", "?"):5s} {h.get("n", 0):8d} tokens  {h["_bytes"] / 1e9:6.2f} GB  '
                      f'age {_age(now - made):>7s}  last used {_age(now - h["_mtime"]):>7s} ago')
        print(f'{sum(len(e) for _, _, e in found)} entries, {total / 1e9:.2f} GB in {top}')
        return 0
    # prune
    if a.older_than_days is None and a.keep_per_root is None and a.max_gb is None:
        print('kv_cache: prune needs --older-than-days, --keep-per-root and / or --max-gb', file=sys.stderr)
        return 2
    gone, skipped = [], []
    free = [(d, ents) for d, held, ents in found if not held]
    skipped = [d for d, held, _ in found if held]
    cands = [(h['_mtime'], h['_bytes'], d, nm, h) for d, ents in free for nm, h in ents if h is not None]
    drop = set()
    if a.older_than_days is not None:
        drop |= {(d, nm) for mt, _, d, nm, _ in cands if now - mt > a.older_than_days * 86400}
    if a.keep_per_root is not None:
        groups = {}
        for mt, b, d, nm, h in cands:
            # (a park without a root is never grouped: guessing would merge distinct chats)
            if h.get('kind') == 'park' and h.get('root_sha') and (d, nm) not in drop:
                groups.setdefault((h.get('key_sha'), h['root_sha']), []).append((mt, d, nm))
        for g in groups.values():
            drop |= {(d, nm) for _, d, nm in sorted(g, reverse=True)[max(0, a.keep_per_root):]}
    if a.max_gb is not None:
        held_bytes = sum(h['_bytes'] for dd, held, ents in found if held for _, h in ents if h)
        left = held_bytes + sum(b for _, b, d, nm, _ in cands if (d, nm) not in drop)
        for mt, b, d, nm, _ in sorted(cands):
            if left <= a.max_gb * 1e9: break
            if (d, nm) not in drop: drop.add((d, nm)); left -= b
    for mt, b, d, nm, _ in sorted(cands):
        if (d, nm) not in drop: continue
        gone.append((d, nm, b))
        if not a.dry_run:
            try: os.unlink(os.path.join(d, nm))
            except FileNotFoundError: pass
    for d, nm, b in gone:
        print(f'{"would remove" if a.dry_run else "removed"} {os.path.relpath(os.path.join(d, nm), top)} ({b / 1e9:.2f} GB)')
    for d in skipped:
        print(f'skipped {os.path.relpath(d, top) if d != top else "."}: a running server owns it (its own cap applies)')
    print(f'{"would free" if a.dry_run else "freed"} {sum(b for *_, b in gone) / 1e9:.2f} GB in {len(gone)} entries')
    return 0


if __name__ == '__main__':
    sys.exit(main())
