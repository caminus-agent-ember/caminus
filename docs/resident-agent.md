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
logout. Stopping the resident does not stop its inference server. Terminal workers
survive a resident restart, so long commands can be inspected rather than repeated.

The panel opens on **Sessions**. Select a named resident and press Enter for its
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
deleting a draft; Enter returns to typing. The footer shows the configured model,
context capacity, selected goal state/steps and pending request count. Context
capacity is not presented as a measured current token count. Updates arrive at
model/tool event boundaries, not as token-by-token private reasoning.

Most controls use slash commands:

| Command | Action |
|---|---|
| `/new [goal]` | Start or prepare a new goal |
| `/goals`, `/goal ID` | List goals or select an unambiguous ID/prefix to steer |
| `/newsession NAME`, `/session NAME`, `/sessions` | Create/open a named resident, or return home |
| `/cancel`, `/resume` | Cancel or resume the selected goal |
| `/start`, `/stop` | Control this session's resident loop; inference stays separate |
| `/requests` | Open human questions and root approvals |
| `/remember TEXT` | Append to the selected session's durable MEMORY.md |
| `/help`, `/quit` | Toggle help, or close the UI while the agent keeps running |

Requests retain their review controls: `a` answers/approves, `e` edits a root
command, `d` denies. Root execution hands the actual terminal to the human for
sudo. Steering does not approve root, answer a pending question or resume a
paused goal. Accepted in-flight steering is observed before completion; ordinary CLI steering to a
finished/cancelled goal is rejected. The conversation UI explicitly uses
`send --goal-id ID --continue` for completed-goal follow-ups; cancelled goals
cannot be reopened this way. `env run`, jobs,
sessions and all previous scriptable CLI flows remain available.

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
