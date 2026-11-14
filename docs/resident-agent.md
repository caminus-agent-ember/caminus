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

The seventh panel tab, **agent**, shows goals, waiting states and recent activity.
Use `n` to send a goal, `s` to start, `x` to stop the loop, `c` to cancel the selected
goal and `m` to choose a named resident. The existing requests tab handles human
questions and root requests. `env run`, sessions, jobs and all previous CLI flows
remain available.

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
