# Resident agents (experimental)

The resident is a native, durable loop around an OpenAI-compatible model. It runs
as the agent Unix user and uses that account's Ubuntu environment through real
PTY terminals. It can run installed programs, write programs, keep commands
running, send terminal input, communicate with peers and ask the human for help.
It does not require Qwen Code or another agent CLI to perform its work.

The inference server and the resident loop are separate processes. Installing
the resident does not move a running inference server into the agent account or
start a GPU workload. Provision the runtime and model files separately before
starting an agent-owned server, using the normal GPU guard and an agreed window.
Graphical desktop attachment and a built-in vision/computer-use interface are
not implemented; available Ubuntu automation programs can be used through the
terminal under the account's normal permissions.

## Start and use a resident

After `caminus env setup` and its human provisioning steps:

```sh
caminus env agent install
caminus env agent init resident --model MY_MODEL --endpoint http://127.0.0.1:8092/v1
caminus env agent start resident --json
caminus env agent send resident --stdin --json < goal.txt
caminus env agent status resident --json
caminus env ui
```

`install` uploads a stdlib-only, versioned zipapp over the existing SSH transport.
Goals and commands execute as the agent user. `start` enables an agent-owned
systemd user service; linger, provisioned by `env setup`, keeps it alive after
logout. `env setup` also creates the shared leases folder (`<bridge>/leases`,
setgid 2770) the `lease` tool and the human's GPU windows write. Stopping the
resident does not stop its inference server. Terminal workers
survive a resident restart, so long commands can be inspected rather than repeated.

### The panel: one chat, with overlays

`caminus env ui` opens the conversation; there are no pages to switch between.
Everything else opens over it in the same bordered box (↑↓ move, Enter acts, Esc
closes; `q` never quits inside an overlay). The header names the agent, its
state and the goal; the footer shows the brain, the context in use against the
window (a bar that warms toward the compaction point), this goal's spend and
today's on a paid brain, the last reply's speed and anything waiting for you.

The composer sits below the transcript. Enter sends, Alt+Enter or Shift+Enter
adds a line, paste keeps Unicode and newlines. A plain message is delivered at
the agent's next step as a **nudge**; `/steer TEXT` redirects (see "The loop").
A message that starts with `/` is a command; `//` sends it as text instead.

| Key | Action |
|---|---|
| Esc | browse the transcript (the draft stays); while a reply is being written, interrupt that turn instead (nothing of it is kept; the goal waits up to two minutes for your message) |
| ↑ on an empty composer | pull back your newest message the agent has not read yet |
| Ctrl+R | earlier prompts containing the draft, newest first |
| Ctrl+X Ctrl+E | edit the draft in `$VISUAL` / `$EDITOR` |
| Ctrl+E | open or fold every tool card; Ctrl+O shows every line of an open card (40 otherwise) |
| Ctrl+T, Alt+↑/↓ | hold or scroll the live reply while it is written |
| Ctrl+G, Ctrl+A, Ctrl+P | the goals, approvals and sessions overlays |
| `!cmd` | run `cmd` in the agent's account; the command and its output land in the draft |
| `@path` | in a message, attach that file from the agent's account (64 KB) |

While a reply is written, a bounded live box shows its thinking, the tools it
starts and the text so far, with Ember, the Caminus mascot, flickering beside
it; the durable blocks replace it when the turn lands.

| Command | Action |
|---|---|
| `/new [goal]`, `/goal ID`, `/goals` | start a goal, select one, or open the goals overlay (Shift+↑/↓ there reorders the queue) |
| `/goal --plan TEXT`, `/plan approve`, `/plan revise NOTE` | a plan-first goal: it explores read-only, proposes a plan and waits; the plan shows as a card above the composer |
| `/steer TEXT`, `/pause`, `/resume`, `/cancel` | steer, pause at the next step, resume, cancel |
| `/btw QUESTION` | a side question with no tools; nothing enters the goal |
| `/diff`, `/export` | the goal's changes in its git repositories (new files included); the goal as markdown in the agent's `~/exports` |
| `/rewind` | pick "undo from step N on": files the edit tools changed from that step go back and the transcript is cut there (two presses; terminal side effects are not undone) |
| `/approvals`, `/requests`, `/deny TEXT` | waiting questions and root requests; deny with what to do instead (it reaches the agent as a steer) |
| `/model`, `/effort` | the models overlay (every provider's models, windows, prices) and the reasoning level; a running resident takes them at its next turn |
| `/context`, `/memory`, `/audit`, `/settings` | what the agent's context holds and where compaction starts; memory; the audit log; settings |
| `/jobs`, `/servers`, `/agents` | the agent's jobs and processes; the model servers; every resident (advanced) |
| `/recap`, `/studio` | while you were away; what free time made (the shelf, the journal, the studio) |
| `/compact`, `/remember TEXT` | compact now; add to memory |
| `/update`, `/halt` | install this caminus's runtime between goals; the emergency stop (`/halt clear` lifts it) |
| `/glyphs`, `/theme` | symbol set (nerd, unicode, ascii) and colours |
| `/session NAME`, `/sessions`, `/start`, `/stop`, `/quit` | sessions, the resident's loop, close the UI (the agent keeps running) |

Custom commands: a markdown file `~/.config/caminus/commands/NAME.md` becomes
`/NAME ARGS` (front matter is dropped; `$ARGUMENTS` is replaced, else the
arguments are appended) and is sent as a message.

### Root requests and questions

A root request is an inline card. Esc, then `a a` approves (two presses: the
first only names the command), `g g` approves and grants that exact command for
the rest of its goal, `e` edits, `d` denies, `/deny TEXT` denies with what to do
instead. When several wait, one card lists them all and `[` `]` pick. A granted
repeat (the same command in the same goal, within 12 hours of your approval, as
your own audit log records it) runs with one `a`, still under your sudo; the
resident never runs root, and `caminus env watch` honours grants the same way.
`caminus env answer ID denied "WHAT INSTEAD"` and `answer ID approved --grant`
do the same from the command line.

Nothing waits on you. A pending request parks only the step that needs it: the
agent is told it is pending, works on what does not need the answer, and gets
the answer as a message (`wait` with `until message` sleeps until it arrives);
it cannot finish while a request is open. A question unanswered after 15 minutes
(`question_timeout`) takes the agent's recommended option (or the advisor's
pick); a permission question times out as no, and an unanswered root request is
never run. Desktop notifications announce what needs you (`CAMINUS_PANEL_NOTIFY=0`
turns them off).

Question tool arguments are rendered as the interactive question rather than raw
JSON. The chosen human answer remains in conversation history. Normal PTY CRLF
newlines render cleanly; control bytes in reviewed commands remain escaped.

The daemon polls without inference while idle or waiting for human answers.
There is no production step limit. Backend outages retain the goal and retry
with backoff. `ROBOTS.md`, the resident's `IDENTITY.md` and `MEMORY.md` provide
persistent instructions and memory; a SQLite journal records goals, tool calls,
results and messages. Completed actions are checkpointed. An interrupted terminal
with uncertain effects is reported as interrupted rather than blindly relaunched.
Context compaction summarizes bounded transcript fragments before continuing.

Root commands are requests, never executed by the resident. The human reviews the
exact command and approves, edits or denies it through the existing bridge. The
agent's open-ended shell does not grant root or access to another user's protected
files. This is a Unix account, not a security sandbox.

## The loop

A goal runs as ticks. The runtime owns the boundaries a model tends to skip
under pressure, and the model owns the inside of a tick.

A message to a working goal is a **nudge** by default: it adds to the work in
hand and keeps the plan. `--steer` (or `/steer TEXT` in the panel) is the heavy
form: it supersedes conflicting instructions, voids the plan and may move the
goal, so it has to be asked for. Both are filed verbatim in a `steers` table
with their kind, as well as tagged in the transcript, so a compaction and the
finish report can quote the human rather than a summary of them. After a compaction it restates the goal and every
steer word for word, bounded at 8 KB with the newest steers kept whole.

`define_done` states what done looks like as something checkable, and `verify`
records what the check actually showed. `finish` is refused while the goal has
no check, while a steer is waiting, or when the check has not been exercised
since the last steer. `ledger` keeps the milestones and blockers, pinned into
every turn so they survive compaction. `checkpoint` tars named paths before a
destructive or milestone-crossing action and `rollback` puts them back.

The goal's gate (the shell check `define_done` names) also runs after every turn
that changed something (an edit, a write, a terminal command that is not
read-only), before the next model call; the agent hears only a change (the gate
starting to fail, with its output, or passing again). A per-turn run gets two
minutes: a gate that takes longer is off per turn for that goal (it still runs at
finish), and one slower than 30 s runs per turn at most every ten minutes.
`gate_each_turn false` turns it off.

At `finish` the gate runs, then the judge: a fresh call that sees the goal, the
done check, your steers and the recorded evidence, not the agent's reasoning. It
answers met, not_yet (the finish is refused with what is missing), wait (the work
is done but its proof is still running: the agent is pointed at its `wait` tool)
or impossible (the goal pauses for you). Three not_yet in a row, or six waits,
pause the goal.

The failure ledger keys a call by tool and arguments: a call that has failed
twice is refused before it runs and the model is told to change approach, and two
refused repeats of it pause the goal. The watchdog pauses a goal that spends
without moving — no progress for `idle_timeout` seconds, a prompt over
`token_budget`, the same call failing three times, or three replies in a row cut
off at the output limit — and records why in the goal's error, where the panel
shows it.

Compaction has two tiers. When the context passes the threshold, old tool
outputs (outside the newest eight messages) are first replaced by a one-line stub
and the path of a file holding them, with no model call; a summary runs only if
that is not enough. A paid brain (remote, or through a key relay) compacts early,
at 38 % of its window down to 7.6 % (`paid_compact_threshold`,
`paid_compact_target_ratio`); a local one at 85 % down to 20 %.

A plan-first goal (`send --plan`, `/goal --plan`) explores before it changes
anything: `write`, `edit`, `checkpoint`, `rollback` and `ask_root` are refused,
and a terminal command runs only when every part of it is a read-only program
(`ls`, `grep`, `git status`, ...; anything unclear counts as a change). The agent
files its plan with `propose_plan` and waits (`waiting_plan`, no timeout) until
`agent plan NAME GOAL approve|reject [--note TEXT]`.

The emergency stop: `caminus env stop-all [--reason TEXT]` (`/halt`) writes a
STOP file in the bridge folder that only you can remove (`stop-all --clear`).
Every resident pauses its working goals after the current step, starts nothing
new, and ends the commands running in its terminals and any process its account
detached (TERM, then KILL ten seconds later); lifting the stop resumes nothing.

`--no-loop-gates` records `loop_gates false` for fixture agents that predate the
loop; real residents get the gates by default.

## Discover before building

The resident prompt authorizes routine reversible work with the account's shell,
web access, installed browsers, user-level software and scheduled jobs, subject
to the human's goal. It explains the root and question bridges, persistent PTYs,
memory and shared-resource rules. Read the authoritative prompt in
`python/caminus/env/resident/prompt.py`, or print it with the current account and
workspace values:

```sh
caminus env agent prompt resident
caminus env agent discover resident --json
caminus env agent discover resident --topic commands --query pdf --json
caminus env agent discover resident --topic packages --query tshark --json
caminus env agent discover resident --topic containers --json
```

These commands work before initialization and also accept `--local` for the
current account. They never call the model. Each new goal automatically receives
one discovery snapshot before its first model turn. That snapshot is saved in
the conversation and journal, so retries do not reinsert it and changing RAM or
disk readings do not continually alter the system-prompt prefix.

The model's `discover` tool provides focused hardware, command, web, image,
document, networking, development, Python, container and package information.
Overview inspects metadata without launching discovered programs, importing
third-party modules, reading user documents, contacting the internet or
initializing GPUs. Python results describe the current interpreter only.
Container queries explicitly use a local Docker socket or local podman; package
queries read installed status and cached apt policy. These queries have a
three-second timeout and a bounded returned output. No image is run or pulled,
and no package is installed or index updated by discovery.

The prompt requires checking these capabilities and a tool's documentation or a
small relevant smoke check before building or installing an alternative. Presence
is labeled as metadata, not proof of working functionality or an exhaustive scan
of every environment. The model may use apt; system installs and index updates
requiring root go through `ask_root` with exact commands and the effect and undo
plan. A task's explicit offline or installation restrictions still apply.

## Scriptable oneshot testing

Daemon and oneshot call the same runtime and tools. `once` submits a goal and
waits for its result. If a daemon already owns the loop, it queues and observes
that daemon. `--jsonl` streams goal events followed by a result record; `--json`
returns a result object. Goal text can be positional, `--goal TEXT` or stdin.

```sh
caminus env agent once resident --stdin --jsonl --max-steps 12 --max-seconds 120 < goal.txt
caminus env agent events resident --after 0 --json
caminus env agent once resident --goal-id GOAL_ID --json
caminus env agent cancel resident GOAL_ID --json
```

`caminus env agent stream resident` prints NDJSON for the reply being generated:
one line per change, the first line of a turn carrying the whole text (`reset`)
and later lines only the suffix, then a `done` line when the turn lands. It is
what the panel's live tail reads; `--once` prints the current snapshots and
exits, and `--seconds N` bounds a wait. Nothing it prints is durable.

Test budgets are optional, persist with the goal, and pause further actions;
they are not a hard process timeout for already running terminal jobs. `cancel`
sends TERM to the goal's terminal sessions. Root and clarification waits survive
process exit. Resuming uses the same goal and pending action, without submitting
the original command again. Finished or cancelled goals cannot be replayed.

Cancellation withdraws the goal's pending `ask_root` and `ask_user` requests. The CLI and panel recheck the
exact reviewed request under a shared lock before approving it, then save an
execution claim before starting sudo. A claim that wins before cancellation is
already committed; cancellation reports its ID and does not erase its result.
An interrupted claim requires human inspection and is never automatically rerun.
For scriptable root approvals, pass the reviewed request's `_digest` from
`env reqs --json` as `env answer ID approved --digest HASH`. Approving by ID alone
is rejected. An edited command still requires its reviewed request digest.
Interactive approvals display terminal controls and Unicode formatting characters
as visible escapes in commands, reasons and request metadata. The displayed
escaping does not change the raw command bound to the reviewed digest.

| Exit | State |
|---|---|
| 0 | completed |
| 20 | waiting for human clarification |
| 21 | waiting for human root approval |
| 22 | paused by a test budget |
| 23 | cancelled |
| 24 | waiting for an unavailable model |
| 1 | error |

For CPU fixtures or agentic tests in an isolated local home, every command also
accepts `--local`; this selects the same implementation without SSH. Initialize
each scratch agent with an isolated `--home` and `--bus`. Remote operation remains
the default. Credentials, when needed, are read from the configured environment
variable, not saved in config.json. The user service reads an optional
`~/.config/caminus/agents/NAME.env` owned by the agent account.

## Peer communication

Initialize each named resident, normally with the same local mail bus:

```sh
caminus env agent init reviewer --model MY_MODEL --endpoint http://127.0.0.1:8092/v1
caminus env agent start reviewer --json
caminus env agent peers resident --json
caminus env agent message resident reviewer --text 'Inspect the test results.' --json
caminus env agent mail reviewer --after 0 --json
```

The model has `peers` and `send_message` tools. Mail records the sender, recipient,
sender goal, optional recipient goal and reply linkage. Without a recipient goal,
delivery creates a new goal; with one, it queues steering input for that goal.
Offline mail persists. The recipient commits delivery before acknowledging it,
so recovery cannot create duplicate goals. Recovered sends reuse the action ID.
Messages to finished goals are retained with a delivery error; send a new goal
instead. Human waits stay paused when peer messages arrive.

`--reply-to MESSAGE_ID` links a reply. `--recipient-goal GOAL_ID` directs it to an
existing goal. Use `--stdin` instead of `--text` for scripts. `mail` exposes
delivery status and errors; `events` exposes received messages.

The default bus is `~/.local/share/caminus/agents/mail.sqlite3`; `init --bus PATH`
selects another. Names cannot silently register a different home on the same bus.
This is communication between residents sharing a Unix account. Sender names
provide attribution, not authentication against processes with the same UID.
Peer messages are data, cannot approve root, and do not override the human's goal.
Separate Unix accounts or remote-machine routing are not implemented.

## Compare Qwen Code and the resident

`benchmarks/resident_compare.py` gives both CLIs the exact same goal bytes and
copies the same seed files into a fresh workspace for every run. It alternates
order, records wall time for each agent invocation and runs an independent
verifier afterward. A zero exit code with wrong files fails. Timeouts, waiting
states and failed setup remain failures. Speedup is emitted only if every run
passes correctness; median times of passing runs and success counts are retained.
The default is three runs per harness.

The included `benchmarks/cases/resident-report` task checks a CSV-to-JSON program,
including exact decimal arithmetic, rounding, quoted Unicode fields, bad input
and preserving existing output on failure. The verifier runs outside the working
directory. Agents should not receive the verifier as part of their prompt.

Provide JSON argv arrays with `--qwen-argv` and `--resident-argv`. The goal travels
on stdin; the process working directory is the fresh workspace. `{workspace}`,
`{run_dir}` and `{goal_file}` are literal argv placeholders. Optional
`--qwen-prepare` and `--resident-prepare` perform setup outside the measured
interval. For the resident, initialize its home at `{workspace}` in preparation
and then run `once --stdin --json` against that home. Use a separate mail bus for
each run. Do not attach to a production daemon for this comparison.

`--profile FILE` records the model, endpoint, actual context, sampling, output
budget, reasoning mode, hardware and client versions/configuration used by both
CLIs. Configure both clients to match; the runner does not force provider flags
or certify a caller's profile. Disable unrelated advisors and fallback providers.
The harness system prompts and tool interfaces will differ by design. Report
those settings and cold/warm server-cache conditions with any timing claim.

```sh
python -I benchmarks/resident_compare.py --help
python -I tests/ci_cpu.py --scratch build/resident-cpu --only env,resident,resident_compare
```

Model-backed comparisons must run through the repository GPU guard in an agreed
window. The comparison runner never starts, stops or preempts the model server.
Its timeout terminates only the CLI's process group; durable resident terminal
workers may need an explicit goal cancellation afterward. CPU fixture tests
validate the comparison procedure, not live-model speed or correctness.

The model's output-limit stop reason pauses a goal instead of reporting it
completed or executing partial tool calls. Increase its output budget and resume
that goal. Malformed tool arguments become durable tool errors so the daemon can
continue working rather than restart on the same action.

Context management counts the full prompt, including the system and tool schemas.
`agent init --compact-threshold .85 --compact-target-ratio .2` starts compaction
at 85% of the configured context and requires the resulting prompt to fit within
20%. It may compact earlier to reserve the next response. Recent assistant/tool
groups are retained together when they fit; larger observations enter the summary.
The original goal remains pinned. An oversized goal or persistent memory pauses
the run instead of sending a request that cannot fit. Compaction events record
the before/after token counts and targets. Every generation also reserves output
room against the model window.
`--compaction-model` selects a summary alias on the same endpoint; a no-thinking
alias can summarize without using the summary budget for hidden reasoning.

## Paid brains, relays and updates

`agent provider add NAME --kind deepinfra|anthropic|openai|local --endpoint URL
[--relay]` registers a provider; `agent models --refresh` reads every provider's
models (window, output limit, reasoning levels, prices) into the agent's
catalog; `agent model ID` and `agent effort LEVEL` switch the brain (a running
resident takes it at its next turn). A paid provider goes through a key relay
the owner runs, so the agent never sees the key:

```sh
caminus env relay install --provider deepinfra --cap-usd 3   # a systemd user service, no expiry
caminus env relay status
```

The relay refuses models it cannot price and stops at its daily cap (HTTP 402);
the agent then waits for the next day or another brain. Every priced turn is
metered per goal and per day (`agent status` reports `spend_today` and
`spend_goals`).

`caminus env agent update NAME` (`/update`) installs this caminus's runtime: the
resident applies it between goals (`--now`: at the next step), restarts its own
service and reports the new version in its heartbeat (`--wait SECS` waits for it).

`tools/env_demo.py` runs all of this on a scripted model in a scratch home (no
agent account, GPU, server or key): the quickest way to see the chat.
