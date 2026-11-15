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

The panel reopens your last conversation directly; the Sessions page is one
Tab away. Select a named resident and press Enter for its
conversation; `n` creates a fresh named session with the same model settings,
separate memory/history, and its own persistent runtime. Existing sessions are
never overwritten. The five main pages are Sessions, Audit, Settings, Notes &
Memory, and Context. Tab/Shift-Tab or 1–5 navigates between them. Settings displays
current model/context/compaction settings; it is read-only.

Inside a conversation, a fixed multiline textbox stays below a scrollable
transcript. Ordinary typing goes into that textbox. Enter sends; Alt+Enter or
Shift+Enter inserts a newline; bracketed paste preserves Unicode and multiple
lines. Text steers the selected working goal. After completion, the next message
explicitly continues that same conversation with its previous context. A cancelled
goal starts fresh; `/new [goal]` explicitly starts another goal.
A refresh never redirects a draft. Failed delivery retains its text.

The transcript retains earlier goals, messages, commands and results. It loads
paged durable history rather than dropping all but a few recent entries, and
context compaction does not remove UI history. Markdown and fenced code render
with syntax highlighting; terminal commands and outputs have separate labels.
Page Up/Down and the mouse wheel scroll; incoming activity preserves the reading
position. Ctrl+L follows the latest activity. Escape switches to browsing without
deleting a draft; Enter returns to typing. Ctrl+P opens a session switcher that
filters by name without leaving the conversation. When the draft is a `/` token,
matching commands appear next to the composer and Tab cycles through them. The
footer shows the configured model,
context capacity, selected goal state/steps and pending request count. Context
capacity is not presented as a measured current token count.

While a reply generates, a bounded tail below the transcript shows its thinking,
each tool it starts and the text so far, redrawn a few times a second from the
runtime's live snapshot. The durable blocks replace it when the turn lands, so a
reply is never shown twice and the transcript is never rewritten mid-reply.
The live window takes about half the page while a reply is being written (the
transcript keeps the rest): Alt+Up/Down and the mouse wheel scroll it three lines at
a time, Page Up/Down by a screen, and Ctrl+T holds it where it is while text keeps
arriving; scrolling back to the bottom, or Ctrl+L, resumes following. Completed
thinking is stored with the reply and shown above it as its own block, scrollable
with the transcript; it is not sent back to the model or counted in its context budget.

Most controls use slash commands (type `/` in the composer and Tab completes
the highlighted name). A message that begins with `/` is read as a command; type
`//` first to send it as text, so a goal about `/tmp` or `/etc` still reaches the
agent:

| Command | Action |
|---|---|
| `/new [goal]` | Start or prepare a new goal |
| `/goals`, `/goal ID` | The goals overlay (Ctrl+G), or select an unambiguous ID/prefix to steer |
| `/newsession NAME`, `/session NAME`, `/sessions` | Create/open a named resident, or return home |
| `/cancel`, `/resume` | Cancel or resume the selected goal |
| `/compact` | Summarize the selected goal's history into working memory now, even below the automatic threshold |
| `/start`, `/stop` | Control this session's resident loop; inference stays separate |
| `/approvals`, `/requests` | The approvals overlay (Ctrl+A): pending root requests and questions |
| `/memory`, `/context`, `/audit`, `/settings` | Overlays with the session memory, the servers' context, the audit log, the session settings |
| `/deny TEXT` | Deny the waiting root request and tell the agent what to do instead |
| `/theme NAME`, `/glyphs [nerd\|unicode\|ascii]` | Colours and symbol set (saved per user; unicode by default) |
| `/remember TEXT` | Append to the selected session's durable MEMORY.md |
| `/help`, `/quit` | Toggle help, or close the UI while the agent keeps running |

Requests retain their review controls: `a` answers or approves (a root command
takes `a` twice: the first press names the command), `e` edits a root command,
`d` denies. Every screen is the chat or an overlay over it; Esc closes an overlay
and `q` never quits inside one. Tool calls and their results are one-line cards
(Ctrl+E opens them all); edits show as diff cards. Root execution hands the actual terminal to the human for
sudo. Steering does not approve root, answer a pending question or resume a
paused goal. Accepted in-flight steering is observed before completion; ordinary CLI steering to a
finished/cancelled goal is rejected, and steering a paused goal reports
`resume_required` so a queued message is not mistaken for delivered work.
The conversation UI explicitly uses
`send --goal-id ID --continue` for completed-goal follow-ups; cancelled goals
cannot be reopened this way. `env run`, jobs,
sessions and all previous scriptable CLI flows remain available.

When the selected goal calls `ask_user`, its question appears inline in place of
the steering textbox. Use Up/Down to select a choice, then Enter to answer;
number keys highlight a choice without submitting it. Select "Write your own
answer" or press `e` for a Unicode multiline answer. Questions without choices
open that text field directly. Escape returns from custom editing to the choices,
or defers the question and restores the untouched steering draft. A question
arriving over a draft keeps the textbox active and requires **F2** to take over
the question. Ordinary letters, numbers, paste, cursor movement and Enter cannot
become an answer in that state. Deferred questions remain available in `/requests`.
Refreshing never redirects a selected answer to another request; changed,
withdrawn or already-answered requests require another review. Root requests
always retain their explicit approval flow in `/requests`.

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
since the last steer. `todo` keeps the goal's checklist (pending, one item in
progress, done) and what blocks it, pinned into every turn so it survives
compaction and shown above the composer; `finish` wants every item done or what
is left named. `checkpoint` tars named paths before a destructive or
milestone-crossing action and `rollback` puts them back.

Before a graded goal completes, `finish` runs two checks. The **gate** is an
optional shell command given to `define_done` (`gate`), run in the agent's home;
a non-zero exit refuses the finish and its output becomes the next prompt. The
**judge** is a fresh model call with no tools that sees the goal, the done check,
the human's steers and the recorded evidence, not the agent's reasoning, and
answers met, not yet or impossible. Not yet refuses the finish with the reason
(three refusals pause the goal); impossible pauses it and reports to the human; an
unreadable reply is asked once more, then accepted unchecked. The judge runs on the
agent's own model in a fresh context by default (free on the local server); set
`judge_model` to `"advisor"` in the resident's config to use the advisor's model.

A reply cut off at the output limit runs nothing; the agent is told at its next
step and tries again in smaller pieces (three cut-offs in a row pause the goal).
Every turn's context states the reply limit, which model answers, and the measured
context use and distance to compaction.

The failure ledger keys a call by tool and arguments: a call that has failed
twice is refused with the reason and the model is told to change approach, and
the third failure pauses the goal. The watchdog pauses a goal that spends
without moving — no progress for `idle_timeout` seconds, a prompt over
`token_budget`, or the same call failing three times — and records why in the
goal's error, where the panel shows it.

`--no-loop-gates` records `loop_gates false` for fixture agents that predate the
loop; real residents get the gates by default.

## Tools

Besides `terminal`, `discover`, the loop's tools and the bridge (`ask_user`,
`ask_root`), a resident has:

| Tool | What it does |
|---|---|
| `read`, `edit`, `write` | Files without the terminal: numbered windows; exact, unique replacement (CRLF and mode kept, atomic); whole-file writes; edits return a diff |
| `todo` | The goal's checklist (see the loop) |
| `remember`, `history_search` | A one-fact note with its index line; full-text search over the agent's own past goals, results, journal and notes |
| `wait` | Block without model calls until a time, a file, a port, a process, a terminal session or a message; a message, cancellation or new work ends it early |
| `machine` | GPUs, memory, disk, load, the model servers, its sessions, peers and leases |
| `lease` | A visible, expiring claim on GPU cards and memory, checked against the human's GPU windows; a claim, not a lock |
| `report`, `budget`, `schedule` | fyi or need_you to the human; what the goal has used and has left; park the goal and wake it later |
| `web_search`, `web_fetch` | Search through [ddgs](https://pypi.org/project/ddgs/) (installed in the agent's account: `python3 -m venv ~/.venvs/ddgs && ~/.venvs/ddgs/bin/pip install ddgs`); a page as readable text |
| `advisor` | A stronger model's advice, within the owner's limits (below) |
| `mcp__SERVER__TOOL` | Tools from the agent's MCP servers (below) |

A tool result past about 8K tokens has its large text saved under `~/outputs/`;
the model gets the head, the tail and the path. Web pages, search results and MCP
results are marked as data, never instructions.

## Memory

`MEMORY.md` is a notes index: one line per note, capped at 4 KB, loaded into every
turn and fixed for the length of a goal. Notes (`notes/NAME.md`, one fact each) are
read on demand; `remember` writes a note and its index line together and is refused
when the index is full. `STATE.md` (doing, verified, assumed, next) loads first on
every turn and is asked for near compaction, so it is what the agent reads after a
compaction or a restart. An older, longer `MEMORY.md` is copied whole into
`notes/memory-before-index.md` the first time it is seen. Answers to questions stay
in the goal's transcript; nothing is written to memory automatically.

## Free time

With no work queued, a resident has free time: it settles in, reads its journal and
notes, and follows what interests it (research, writing, making things in
`~/studio`, tending up to three projects), writing a short intention as a todo and a
journal entry before it stops. It is never graded. Work preempts it at once; a
nightly dream consolidates notes. On the local server it works in the full context
window: before a stretch overwrites the human's conversation, the prompt cache
parks it to disk and the human's next request restores it. `caminus env agent
heartbeat NAME on|off|status|recap`; the recap is the "while you were away" line.
On a remote model free time spends its own allowance (`free_time.budget_usd`,
0.50 USD a day by default).

## When you are away

Nothing waits on you. A question you have not answered within `question_timeout`
(900 seconds by default; free time uses its own shorter window) is withdrawn by the
resident itself, never answered in your name, and the agent carries on: with the
advisor's pick when an advisor is configured, else with the option it recommended,
recording the choice in `STATE.md` and its finish. Silence is never approval: a
permission request (an irreversible change, an action in your name, spending money;
`ask_user` with `approval`) that times out is a no, and an unanswered root request
is not run. Each one is recorded (`question_unanswered`, `root_unanswered`), and you
can steer the goal when you are back.

## Brains: providers, models and effort

A resident's brain is any model its providers serve. A provider is an endpoint the agent can reach: the local
Caminus server, or a paid API through your key relay (the agent never holds the key):

```sh
caminus env relay --provider anthropic --cap-usd 3 --model claude-haiku-5-5 --ttl 86400   # http://127.0.0.1:8765/v1
caminus env relay --provider deepinfra --cap-usd 3 --ttl 86400                              # http://127.0.0.1:8766/v1/openai
caminus env agent provider NAME add anthropic --kind anthropic --endpoint http://127.0.0.1:8765/v1 --relay
caminus env agent provider NAME add deepinfra --kind deepinfra --endpoint http://127.0.0.1:8766/v1/openai --relay
caminus env agent models NAME --refresh        # what each provider serves: window, output cap, effort levels, prices
caminus env agent model NAME zai-org/GLM-5.2   # pick one (provider/id when two serve it)
caminus env agent effort NAME high             # among the levels that model offers
```

Each relay reads its key from a file only you can read (`~/.config/anthropic/api_key`,
`~/.config/caminus/deepinfra-key`; it refuses one others can read), keeps a daily dollar cap and exits after `--ttl`.
The Anthropic relay allows the models you name; the DeepInfra relay allows any chat model DeepInfra publishes a price
for, priced from its own list. The catalog is what the providers report: Anthropic's model list gives each model's
window, output cap, thinking and effort levels; DeepInfra's gives window, prices and whether it takes
`reasoning_effort`. In the panel, `/model` opens the list (Enter picks) and `/effort LEVEL` sets the depth; a running
resident takes either at its next turn.

A paid brain resends its whole context every turn, so it compacts early: at 38 % of its window, keeping the newest
7.6 % (on a 1M window about 400K and 80K). Set `paid_compact_threshold` and `paid_compact_target_ratio` (fractions of
the window) in the resident's config to change it; the local server keeps `compact_threshold` / `compact_target_ratio`
(85 % / 20 %).

## The advisor and two brains

`caminus env agent advisor NAME --model claude-opus-5-5 --endpoint URL` gives the
agent an `advisor` tool on a model the owner picks, through the owner's own key
relay, with a daily dollar cap and calls per goal. `caminus env agent fallback NAME
--model ... --endpoint ...` adds a second brain used only while the local server is
down, with its own daily allowance. Every failed or refused remote call is recorded
in the resident's `failures.jsonl`.

## MCP servers

`caminus env agent mcp NAME add SERVER [--env KEY=VALUE] -- COMMAND ARGS...` adds a
Model Context Protocol server (stdio); `mcp NAME` lists them (environment values are
never printed), `mcp NAME remove|off|on SERVER`. The client speaks the stateless
2026-07-28 revision and falls back to the `initialize` handshake for older servers.
A server starts on first use and stops with the agent; its log is
`~/mcp-logs/SERVER.log`. Example, a read-only GitHub server:

```sh
caminus env agent mcp NAME add github --env GITHUB_PERSONAL_ACCESS_TOKEN=... -- \
  github-mcp-server stdio --read-only --toolsets=repos,issues,pull_requests
```

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
