# The agent environment (`caminus env`, experimental)

An agent gets an environment instead of a fixed tool list: a second user on this machine, reached over SSH, where it
can do any IO that user can do. The boundary is Unix, not a sandbox: your home, your keys and your processes are
protected by file ownership and permissions, and root is not in the agent's reach at all — a root command waits in a
bridge until you approve it, and only your side executes it.

Status: experimental, CPU-tested only (tests/env/test_env.py; no live SSH in CI). Not wired to the panel yet.

**Code:**
- `python/caminus/env/settings.py`: the names and paths (`CAMINUS_ENV_USER`, `CAMINUS_ENV_KEY`, `CAMINUS_ENV_BRIDGE`, `CAMINUS_ENV_AUDIT`, then defaults).
- `python/caminus/env/runner.py`: the SSH transport.
- `python/caminus/env/bridge.py`: the root request/result protocol.
- `python/caminus/env/audit.py`: the audit log.
- `python/caminus/env/agent_side.py`: the `ask-root` script installed into the agent user.
- `python/caminus/cmd/env.py`: the `caminus env` command.

**Tests:** tests/env/test_env.py (suite `env` in tests/ci_cpu.py).

## 1. The model

Two principals on one machine:

- **You** run the harness (`caminus env ...`). It holds the SSH key, the bridge and the audit log.
- **The agent user** (default `caminus-agent`) runs all agent work: `caminus env run CMD...` executes as that user,
  `caminus env shell` opens an interactive session for any agent CLI, and anything started there keeps running after
  logout (`loginctl enable-linger`).

What the boundary gives you, concretely: the agent user cannot read your home (750), your `~/.ssh`, your shell
environment or your API keys; it cannot signal your processes; it has no sudo rights, so it cannot become root. It
does still have the network, every world-readable file, the GPU (it is in the `render`/`video` groups so Caminus and
benchmarks run there), and write access to anything group- or world-writable — this is an environment, not a jail.
Scope credentials deliberately: give the agent user only short-lived, low-scope tokens in its own environment.

## 2. Provisioning

`caminus env setup` prints the exact sudo commands (they are run by you, never by an unattended agent): install and
start `openssh-server`, create the user with a locked password, create the `caminus-env` bridge group and put both
users in it, the `render`/`video` groups for the agent user, the bridge directory `/srv/caminus-env-bridge` (root:
group, 770, with `req/` and `res/`), enable linger, generate the harness key, and add one line to the agent user's
`authorized_keys`:

```
from="127.0.0.1,::1",no-agent-forwarding,no-X11-forwarding,no-port-forwarding <harness pubkey>
```

`caminus env setup --agent-side` installs `~/bin/ask-root` as the agent user over SSH. `caminus env status` checks
each piece: the user exists, the key exists, `ssh` login works (`id -un` answers the agent user), the bridge is
writable, `ask-root` is installed.

## 3. The SSH runner

One persistent ControlMaster connection to `127.0.0.1` with `BatchMode=yes`. Commands travel **on stdin**
(`ssh ... bash -s`), so agent-supplied text is never re-quoted across the wire: `run_argv` shlex-quotes an argv into the
script, `run_shell` sends one shell string verbatim. The ControlMaster socket lives in the state dir.

## 4. The root bridge

The agent user files a request; only the harness side answers and executes. Files are JSON, written atomically
(tmp + rename, umask 007 so the other user can read them):

- `req/<id>.json`: `{'id', 'pid', 'user', 'cwd', 'cmd', 'ts'}` — written by the agent side (`ask-root CMD...`, or
  `caminus env ask-root CMD...` when the package is installed in that user).
- `res/<id>.json`: `{'id', 'decision', 'cmd', 'approved_by', 'rc', 'out', 'err', 'ts'}` — written by the harness side.

`caminus env watch` drains `req/`, shows you each command with its origin, and asks: **a**pprove runs exactly that
string as `sudo sh -c <cmd>` (sudo asks for your password on your terminal), **e**dit lets you change it first,
**d**eny answers 126, **s**kip leaves it queued. The result (exit code, stdout, stderr) goes back to the waiting
agent process. A request nobody answers times out (agent side: exit 124). The harness never executes anything from a
request file other than the string it displayed and you approved.

## 5. The audit log

One JSON line per event in the state dir (`env-audit.jsonl`, writable only by you): every `env run`, every root
request, decision and result. `caminus env audit --tail N` prints the last N. sudo's own log in
`/var/log/auth.log` is the second record of every root execution.

## 6. Security properties, honestly

- Protects: your files, credentials and processes from the agent user and from anything that injects into it; root
  entirely (the agent user has no sudo; elevation needs your password on a command you saw).
- Does not confine: network egress, world-readable files, GPU, disk, and any shared writable tree remain open. An
  agent with full network IO can exfiltrate whatever it can read — which is why it can only read its own scoped
  environment.
- The bridge directory is writable by the agent user; treat request files as untrusted input (they are display-only).
