// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 jdominick05

// caminus-panel env: the human side of the agent environment as a TUI (`caminus env ui`,
// docs/specs/spec-agent-env.md section 7). The Python CLI runs it with the paths as flags:
//
//	caminus-panel env --user NAME --bridge DIR --audit FILE --key FILE
//
// Tabs (tab / shift-tab or 1-6), all live:
//
//	1 requests  the bridge queue: ROOT (a approve — sudo runs attached to the terminal via
//	            tea.ExecProcess so the password prompt works, output teed to a temp file;
//	            e edit, d deny, s skip) and ASK (a answer; a number picks a choice)
//	2 jobs      detached processes in the agent user (k kill, l tail the log, n new job)
//	3 procs     the agent user's busiest processes (ps over ssh)
//	4 notes     steering inbox for the agent (n new note, d delete); the agent drains it with env-note
//	5 ctx       running Caminus servers and the context window each reports in /props
//	6 audit     the tail of the audit log
//
// Answer files and audit lines use the same JSON schema as python/caminus/env/{bridge,audit}.py,
// so `caminus env watch` / `audit` and this TUI are interchangeable.
package main

import (
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

type envReq struct {
	Digest  string   `json:"-"`
	ID      string   `json:"id"`
	Pid     int      `json:"pid"`
	User    string   `json:"user"`
	CWD     string   `json:"cwd"`
	Kind    string   `json:"kind"` // "root" (Cmd) or "question" (Text, Choices); empty = root
	Cmd     string   `json:"cmd"`
	Text    string   `json:"text"`
	Choices []string `json:"choices"`
	Reason  string   `json:"reason"`
	Ts      float64  `json:"ts"`
}

func (r envReq) kind() string {
	if r.Kind == "" {
		return "root"
	}
	return r.Kind
}

type job struct {
	Name, Pid, State, Log string
}

type note struct {
	ID, From, Text string
}

type server struct {
	Port  int
	Model string
	Nctx  string
}

var tabs = []string{"requests", "jobs", "procs", "notes", "ctx", "audit", "agent"}

type envModel struct {
	user           string
	bridge         string
	auditPath      string
	key            string
	cli            []string
	agentName      string
	resident       residentStatus
	residentEvents []residentEvent
	goalSel        int

	tab     int
	reqs    []envReq
	sel     int
	seen    map[string]bool
	jobs    []job
	jobSel  int
	procs   []string
	notes   []note
	noteSel int
	servers []server
	events  []string
	logName string
	logView string

	mode        string // "normal", "edit", "answer", "note", "newjob1", "newjob2"
	edit        string
	editRequest *envReq
	newJob      string

	msg    string
	sshOK  bool
	sshN   int
	tickN  int
	width  int
	height int
}

type envTickMsg time.Time
type probeMsg struct {
	ok  bool
	n   int
	err string
}
type jobsMsg []job
type procsMsg []string
type notesMsg []note
type serversMsg []server
type execDoneMsg struct {
	claim    string
	id       string
	decision string
	cmd      string
	rc       int
	out      string
}
type sshOutMsg struct {
	what string
	out  string
	err  error
}

func envMain(args []string) {
	fs := flag.NewFlagSet("env", flag.ExitOnError)
	user := fs.String("user", "", "the agent user")
	bridge := fs.String("bridge", "", "the bridge directory")
	audit := fs.String("audit", "", "the audit log path")
	key := fs.String("key", "", "the harness ssh key")
	cli := fs.String("cli", "", "JSON argv of the caminus CLI (for starting server jobs)")
	agent := fs.String("agent", "resident", "resident agent name")
	fs.Parse(args)
	if *user == "" || *bridge == "" || *audit == "" || *key == "" {
		fmt.Fprintln(os.Stderr, "caminus-panel env: --user --bridge --audit --key are all required")
		os.Exit(2)
	}
	var cliArgv []string
	if *cli != "" {
		if err := json.Unmarshal([]byte(*cli), &cliArgv); err != nil {
			fmt.Fprintln(os.Stderr, "caminus-panel env: bad --cli JSON:", err)
			os.Exit(2)
		}
	}
	m := envModel{user: *user, bridge: *bridge, auditPath: *audit, key: *key, cli: cliArgv,
		agentName: *agent, tab: 6, seen: map[string]bool{}, mode: "normal", width: 110, height: 32}
	p := tea.NewProgram(m)
	if _, err := p.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "caminus-panel env:", err)
		os.Exit(1)
	}
}

func (m envModel) Init() tea.Cmd {
	cmds := append([]tea.Cmd{tickCmd(), probeCmd(m)}, sshRefresh(m)...)
	cmds = append(cmds, residentRefresh(m))
	return tea.Batch(cmds...)
}

func tickCmd() tea.Cmd {
	return tea.Tick(500*time.Millisecond, func(t time.Time) tea.Msg { return envTickMsg(t) })
}

func sshCmd(m envModel, what, script string) *exec.Cmd {
	c := exec.Command("ssh", "-i", m.key, "-o", "BatchMode=yes", "-o", "ConnectTimeout=3",
		"-o", "ControlMaster=auto", "-o", "ControlPath="+filepath.Join(os.TempDir(), "caminus-envui-%r@%h:%p"),
		m.user+"@127.0.0.1", "bash", "-s")
	c.Stdin = strings.NewReader(script)
	return c
}

func sshRun(m envModel, what, script string) tea.Cmd {
	return func() tea.Msg {
		out, err := sshCmd(m, what, script).CombinedOutput()
		return sshOutMsg{what: what, out: string(out), err: err}
	}
}

func probeCmd(m envModel) tea.Cmd {
	return func() tea.Msg {
		out, err := sshCmd(m, "probe", "id -un").CombinedOutput()
		ok := err == nil && strings.TrimSpace(string(out)) == m.user
		return probeMsg{ok: ok, n: m.sshN + 1, err: strings.TrimSpace(string(out))}
	}
}

// the per-tab ssh scripts (run as the agent user; read-only except kill/new-job which act on its own jobs)
func jobsScript() string {
	return `for p in ~/env-jobs/*.pid; do [ -e "$p" ] || continue; n=$(basename "$p" .pid); pid=$(cat "$p"); if kill -0 "$pid" 2>/dev/null; then s=alive; else s=dead; fi; echo "$n|$pid|$s|~/env-logs/$n.log"; done`
}

func procsScript(user string) string {
	return fmt.Sprintf("ps -u %s -o pid=,etimes=,pcpu=,pmem=,args= --sort=-pcpu | head -12", shQuote(user))
}

func notesScript(bridge string) string {
	return fmt.Sprintf(`for f in %s/notes/*.json; do [ -e "$f" ] && cat "$f" && echo; done`, shQuote(bridge))
}

func serversScript() string {
	return `for p in $(seq 8092 8101); do printf 'PORT %d\n' "$p"; curl -s --max-time 1 "http://127.0.0.1:$p/props"; echo; done`
}

func sshRefresh(m envModel) []tea.Cmd {
	return []tea.Cmd{
		sshRun(m, "jobs", jobsScript()),
		sshRun(m, "procs", procsScript(m.user)),
		sshRun(m, "notes", notesScript(m.bridge)),
		sshRun(m, "servers", serversScript()),
	}
}

// ---------------------------------------------------------------- bridge and audit (pure, tested)

func loadRequests(bridge string) ([]envReq, error) {
	names, err := filepath.Glob(filepath.Join(bridge, "req", "*.json"))
	if err != nil {
		return nil, err
	}
	var out []envReq
	for _, n := range names {
		id := strings.TrimSuffix(filepath.Base(n), ".json")
		if _, err := os.Stat(filepath.Join(bridge, "res", id+".json")); err == nil {
			continue
		}
		data, err := os.ReadFile(n)
		if err != nil {
			continue
		}
		var r envReq
		if err := json.Unmarshal(data, &r); err != nil {
			continue
		}
		if r.ID == "" {
			r.ID = id
		}
		if !validBridgeID(id) || r.ID != id {
			continue
		}
		r.Digest = requestDigest(data)
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Ts < out[j].Ts })
	return out, nil
}

func writeAnswer(bridge, actor string, r envReq, decision, cmd string, rc *int, out, errText, answer string) error {
	if !validBridgeID(r.ID) {
		return fmt.Errorf("invalid bridge request id")
	}
	res := map[string]any{"id": r.ID, "decision": decision, "cmd": cmd, "approved_by": actor,
		"rc": rc, "out": out, "err": errText, "answer": answer, "ts": float64(time.Now().UnixMilli()) / 1000}
	data, _ := json.Marshal(res)
	dir := filepath.Join(bridge, "res")
	if err := os.MkdirAll(dir, 0o770); err != nil {
		return err
	}
	return withBridgeLock(bridge, r.ID, func() error {
		if _, err := checkPendingRequest(bridge, r); err != nil {
			return err
		}
		return writeBridgeJSON(filepath.Join(dir, r.ID+".json"), data)
	})
}

func validBridgeID(s string) bool {
	if len(s) < 1 || len(s) > 128 {
		return false
	}
	for i, r := range s {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			continue
		}
		if i > 0 && (r == '_' || r == '-') {
			continue
		}
		return false
	}
	return true
}

func writeBridgeJSON(path string, data []byte) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".bridge-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	info, err := os.Stat(dir)
	if err != nil {
		return err
	}
	if err = f.Chmod(0o660); err != nil {
		return err
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		if err = f.Chown(-1, int(stat.Gid)); err != nil {
			return err
		}
	}
	if _, err = f.Write(data); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

func writeNote(bridge, actor, text string) (string, error) {
	id := fmt.Sprintf("%d", time.Now().UnixNano())[:16]
	data, _ := json.Marshal(map[string]any{"id": id, "from": actor, "text": text, "ts": float64(time.Now().UnixMilli()) / 1000})
	dir := filepath.Join(bridge, "notes")
	if err := os.MkdirAll(dir, 0o770); err != nil {
		return "", err
	}
	if err := writeBridgeJSON(filepath.Join(dir, id+".json"), data); err != nil {
		return "", err
	}
	return id, nil
}

func appendAudit(path string, event map[string]any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	event["ts"] = float64(time.Now().UnixMilli()) / 1000
	data, _ := json.Marshal(event)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(data, '\n'))
	return err
}

func tailLines(path string, n int) []string {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if lines[0] == "" {
		lines = nil
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines
}

// shQuote quotes s for a POSIX shell word.
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// execRoot builds the command that runs cmd under sudo attached to the terminal, teeing the
// output to a temp file (the sudo password prompt goes to the tty, not the pipe).
func execRoot(cmd string) (*exec.Cmd, string, error) {
	f, err := os.CreateTemp("", "caminus-env-root-*.out")
	if err != nil {
		return nil, "", err
	}
	tmp := f.Name()
	f.Close()
	script := "set -o pipefail; sudo sh -c " + shQuote(cmd) + " 2>&1 | tee " + shQuote(tmp)
	return exec.Command("bash", "-c", script), tmp, nil
}

func parseJobs(out string) []job {
	var js []job
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		parts := strings.Split(line, "|")
		if len(parts) != 4 {
			continue
		}
		js = append(js, job{Name: parts[0], Pid: parts[1], State: parts[2], Log: parts[3]})
	}
	return js
}

func parseNotes(out string) []note {
	var ns []note
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || !strings.HasPrefix(line, "{") {
			continue
		}
		var n struct {
			ID   string `json:"id"`
			From string `json:"from"`
			Text string `json:"text"`
		}
		if json.Unmarshal([]byte(line), &n) == nil {
			ns = append(ns, note{ID: n.ID, From: n.From, Text: n.Text})
		}
	}
	return ns
}

func parseServers(out string) []server {
	var ss []server
	port := 0
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "PORT ") {
			port, _ = strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "PORT ")))
			continue
		}
		line = strings.TrimSpace(line)
		if port == 0 || !strings.HasPrefix(line, "{") {
			continue
		}
		var p struct {
			ModelAlias string `json:"model_alias"`
			Model      string `json:"model"`
			Nctx       any    `json:"n_ctx"`
			DGS        struct {
				Nctx any `json:"n_ctx"`
			} `json:"default_generation_settings"`
		}
		if json.Unmarshal([]byte(line), &p) == nil {
			nctx := p.Nctx
			if nctx == nil {
				nctx = p.DGS.Nctx
			}
			ss = append(ss, server{Port: port, Model: p.ModelAlias, Nctx: fmt.Sprintf("%v", nctx)})
		}
		port = 0
	}
	return ss
}

// ---------------------------------------------------------------- model

func (m envModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil
	case envTickMsg:
		m.tickN++
		m.refreshLocal()
		cmds := []tea.Cmd{tickCmd()}
		if m.tickN%2 == 0 {
			cmds = append(cmds, probeCmd(m))
		}
		if m.tickN%4 == 0 {
			cmds = append(cmds, sshRefresh(m)...)
			if m.tab == 6 {
				cmds = append(cmds, residentRefresh(m))
			}
		}
		return m, tea.Batch(cmds...)
	case probeMsg:
		m.sshOK, m.sshN, m.msg = msg.ok, msg.n, msg.err
		return m, nil
	case jobsMsg:
		m.jobs = msg
		if m.jobSel >= len(m.jobs) {
			m.jobSel = max(0, len(m.jobs)-1)
		}
		return m, nil
	case procsMsg:
		m.procs = msg
		return m, nil
	case notesMsg:
		m.notes = msg
		return m, nil
	case serversMsg:
		m.servers = msg
		return m, nil
	case residentMsg:
		if msg.err != "" {
			m.msg = msg.err
		} else {
			m.resident, m.residentEvents = msg.status, msg.events
			if m.goalSel >= len(m.resident.Goals) {
				m.goalSel = max(0, len(m.resident.Goals)-1)
			}
		}
		return m, nil
	case residentActionMsg:
		m.msg = msg.text
		return m, residentRefresh(m)
	case sshOutMsg:
		switch msg.what {
		case "jobs":
			m.jobs = parseJobs(msg.out)
		case "procs":
			m.procs = strings.Split(strings.TrimSpace(msg.out), "\n")
		case "notes":
			m.notes = parseNotes(msg.out)
		case "servers":
			m.servers = parseServers(msg.out)
		case "kill":
			m.msg = "kill: " + strings.TrimSpace(msg.out)
			_ = appendAudit(m.auditPath, map[string]any{"event": "kill_job", "out": strings.TrimSpace(msg.out)})
		case "newjob":
			m.msg = "spawned pid " + strings.TrimSpace(msg.out)
			_ = appendAudit(m.auditPath, map[string]any{"event": "spawn", "pid": strings.TrimSpace(msg.out), "cmd": m.newJob})
		case "log":
			m.logView = msg.out
		case "serve":
			m.msg = strings.TrimSpace(msg.out)
			_ = appendAudit(m.auditPath, map[string]any{"event": "serve_ui", "out": strings.TrimSpace(msg.out)})
		}
		return m, nil
	case execDoneMsg:
		err := finishRoot(m.bridge, msg.id, msg.claim, msg.rc, msg.out)
		_ = appendAudit(m.auditPath, map[string]any{"event": "root_result", "id": msg.id,
			"decision": msg.decision, "cmd": msg.cmd, "rc": msg.rc})
		m.seen[msg.id] = true
		m.msg = fmt.Sprintf("%s: exit %d", msg.decision, msg.rc)
		if err != nil {
			m.msg += "; " + err.Error()
		}
		m.refreshLocal()
		return m, tickCmd()
	case tea.KeyPressMsg:
		return m.onKey(msg.String())
	}
	return m, nil
}

func (m *envModel) refreshLocal() {
	reqs, err := loadRequests(m.bridge)
	if err == nil {
		var vis []envReq
		for _, r := range reqs {
			if !m.seen[r.ID] {
				vis = append(vis, r)
			}
		}
		m.reqs = vis
	}
	if m.sel >= len(m.reqs) {
		m.sel = max(0, len(m.reqs)-1)
	}
	m.events = tailLines(m.auditPath, 200)
}

func (m envModel) onKey(k string) (tea.Model, tea.Cmd) {
	// text input modes
	if m.mode != "" && m.mode != "normal" {
		switch k {
		case "enter":
			return m.submitInput()
		case "esc":
			m.mode, m.edit, m.newJob = "normal", "", ""
			m.editRequest = nil
			return m, nil
		case "backspace":
			if len(m.edit) > 0 {
				m.edit = m.edit[:len(m.edit)-1]
			}
			return m, nil
		default:
			if k == "space" {
				m.edit += " "
			} else if r := msgRunes(k); r != nil {
				m.edit += string(r)
			}
			return m, nil
		}
	}
	switch k {
	case "q", "ctrl+c":
		return m, tea.Quit
	case "tab":
		m.tab = (m.tab + 1) % len(tabs)
		return m, nil
	case "shift+tab":
		m.tab = (m.tab + len(tabs) - 1) % len(tabs)
		return m, nil
	case "1", "2", "3", "4", "5", "6", "7":
		m.tab = int(k[0] - '1')
		return m, nil
	case "r":
		m.refreshLocal()
		return m, tea.Batch(append(sshRefresh(m), residentRefresh(m))...)
	}
	switch m.tab {
	case 0:
		return m.reqKeys(k)
	case 1:
		return m.jobKeys(k)
	case 3:
		return m.noteKeys(k)
	case 6:
		return m.residentKeys(k)
	}
	return m, nil
}

func (m envModel) submitInput() (tea.Model, tea.Cmd) {
	switch m.mode {
	case "agentgoal":
		text := m.edit
		m.mode, m.edit = "normal", ""
		if strings.TrimSpace(text) == "" {
			return m, nil
		}
		return m, residentAction(m, []string{"send", m.agentName, "--stdin", "--json"}, text)
	case "agentname":
		m.agentName = strings.TrimSpace(m.edit)
		m.mode, m.edit, m.goalSel = "normal", "", 0
		return m, residentRefresh(m)
	case "answer":
		if m.editRequest == nil {
			m.mode = "normal"
			return m, nil
		}
		r := *m.editRequest
		m.editRequest = nil
		ans := m.edit
		if len(r.Choices) > 0 {
			if n, err := strconv.Atoi(strings.TrimSpace(ans)); err == nil && n >= 1 && n <= len(r.Choices) {
				ans = r.Choices[n-1]
			}
		}
		m.mode, m.edit = "normal", ""
		if err := writeAnswer(m.bridge, currentUser(), r, "answered", "", nil, "", "", ans); err != nil {
			m.msg = err.Error()
			m.refreshLocal()
			return m, nil
		}
		_ = appendAudit(m.auditPath, map[string]any{"event": "decision", "id": r.ID,
			"decision": "answered", "by": currentUser(), "answer": ans})
		m.seen[r.ID] = true
		m.msg = "answered " + r.ID
		m.refreshLocal()
		payload := base64.StdEncoding.EncodeToString([]byte("Q: " + r.Text + "\nA: " + ans + "\n---\n"))
		return m, sshRun(m, "mem", "printf %s "+payload+" | base64 -d >> ~/MEMORY.md")
	case "edit":
		cmd := m.edit
		m.mode, m.edit = "normal", ""
		if cmd == "" || m.editRequest == nil {
			return m, nil
		}
		r := *m.editRequest
		m.editRequest = nil
		c, tmp, err := execRoot(cmd)
		if err != nil {
			m.msg = err.Error()
			return m, nil
		}
		token, err := claimRoot(m.bridge, currentUser(), r, "edited", cmd)
		if err != nil {
			os.Remove(tmp)
			m.msg = err.Error()
			m.refreshLocal()
			return m, nil
		}
		m.msg = "running under sudo (approve with your password)..."
		return m, tea.ExecProcess(c, execCallback(r.ID, "edited", cmd, tmp, token))
	case "note":
		text := m.edit
		m.mode, m.edit = "normal", ""
		if text == "" {
			return m, nil
		}
		id, err := writeNote(m.bridge, currentUser(), text)
		if err != nil {
			m.msg = err.Error()
		} else {
			m.msg = "note " + id + " left for " + m.user
			_ = appendAudit(m.auditPath, map[string]any{"event": "note", "id": id, "text": text})
		}
		return m, sshRun(m, "notes", notesScript(m.bridge))
	case "newjob1":
		name := strings.TrimSpace(m.edit)
		m.mode, m.edit = "newjob2", ""
		m.newJob = name
		return m, nil
	case "newjob2":
		name := m.newJob
		cmd := m.edit
		m.mode, m.edit, m.newJob = "normal", "", ""
		if name == "" || cmd == "" {
			return m, nil
		}
		script := fmt.Sprintf("mkdir -p ~/env-jobs ~/env-logs; nohup setsid bash -c %s > ~/env-logs/%s.log 2>&1 & echo $! > ~/env-jobs/%s.pid; cat ~/env-jobs/%s.pid",
			shQuote(cmd), shQuote(name), shQuote(name), shQuote(name))
		return m, sshRun(m, "newjob", script)
	case "serve1":
		model := strings.TrimSpace(m.edit)
		m.mode, m.edit, m.newJob = "serve2", "", model
		return m, nil
	case "serve2":
		model := m.newJob
		placement := strings.TrimSpace(m.edit)
		m.mode, m.edit, m.newJob = "normal", "", ""
		if model == "" {
			return m, nil
		}
		if placement == "" {
			placement = "auto"
		}
		name := model
		args := append(append([]string{}, m.cli...), "env", "serve", name, model, "--placement", placement)
		m.msg = "caminus env serve " + model + " --placement " + placement + " ..."
		return m, func() tea.Msg {
			c := exec.Command(args[0], args[1:]...)
			out, err := c.CombinedOutput()
			return sshOutMsg{what: "serve", out: string(out), err: err}
		}
	}
	return m, nil
}

func execCallback(id, decision, cmd, tmp, claim string) func(error) tea.Msg {
	return func(err error) tea.Msg {
		rc := 0
		if ee, ok := err.(*exec.ExitError); ok {
			rc = ee.ExitCode()
		} else if err != nil {
			rc = 1
		}
		out, _ := os.ReadFile(tmp)
		os.Remove(tmp)
		return execDoneMsg{id: id, decision: decision, cmd: cmd, rc: rc, out: string(out), claim: claim}
	}
}

func (m envModel) reqKeys(k string) (tea.Model, tea.Cmd) {
	switch k {
	case "up", "k":
		if m.sel > 0 {
			m.sel--
		}
	case "down", "j":
		if m.sel < len(m.reqs)-1 {
			m.sel++
		}
	case "d":
		if m.sel < len(m.reqs) {
			r := m.reqs[m.sel]
			if err := writeAnswer(m.bridge, currentUser(), r, "denied", r.Cmd, nil, "", "", ""); err != nil {
				m.msg = err.Error()
				m.refreshLocal()
				return m, nil
			}
			_ = appendAudit(m.auditPath, map[string]any{"event": "decision", "id": r.ID,
				"decision": "denied", "by": currentUser()})
			m.seen[r.ID] = true
			m.msg = "denied " + r.ID
			m.refreshLocal()
		}
	case "s":
		if m.sel < len(m.reqs) {
			m.seen[m.reqs[m.sel].ID] = true
			m.refreshLocal()
		}
	case "a":
		if m.sel < len(m.reqs) {
			r := m.reqs[m.sel]
			if r.kind() == "question" {
				m.mode, m.edit = "answer", ""
				m.editRequest = &r
				return m, nil
			}
			c, tmp, err := execRoot(r.Cmd)
			if err != nil {
				m.msg = err.Error()
				return m, nil
			}
			token, err := claimRoot(m.bridge, currentUser(), r, "approved", r.Cmd)
			if err != nil {
				os.Remove(tmp)
				m.msg = err.Error()
				m.refreshLocal()
				return m, nil
			}
			m.msg = "running under sudo (approve with your password)..."
			return m, tea.ExecProcess(c, execCallback(r.ID, "approved", r.Cmd, tmp, token))
		}
	case "e":
		if m.sel < len(m.reqs) && m.reqs[m.sel].kind() == "root" {
			m.mode, m.edit = "edit", m.reqs[m.sel].Cmd
			r := m.reqs[m.sel]
			m.editRequest = &r
		}
	}
	return m, nil
}

func (m envModel) jobKeys(k string) (tea.Model, tea.Cmd) {
	switch k {
	case "up", "k":
		if m.jobSel > 0 {
			m.jobSel--
		}
	case "down", "j":
		if m.jobSel < len(m.jobs)-1 {
			m.jobSel++
		}
	case "K":
		if m.jobSel < len(m.jobs) {
			j := m.jobs[m.jobSel]
			return m, sshRun(m, "kill", fmt.Sprintf("pid=$(cat ~/env-jobs/%s.pid 2>/dev/null) && kill \"$pid\" 2>/dev/null; rm -f ~/env-jobs/%s.pid; echo killed %s", shQuote(j.Name), shQuote(j.Name), j.Name))
		}
	case "l":
		if m.jobSel < len(m.jobs) {
			m.logName = m.jobs[m.jobSel].Name
			return m, sshRun(m, "log", fmt.Sprintf("tail -c 4000 ~/env-logs/%s.log 2>/dev/null || echo '(no log yet)'", shQuote(m.jobs[m.jobSel].Name)))
		}
	case "n":
		m.mode, m.edit = "newjob1", ""
	case "S":
		if len(m.cli) == 0 {
			m.msg = "no --cli passed: start servers with `caminus env serve`"
		} else {
			m.mode, m.edit = "serve1", ""
		}
	}
	return m, nil
}

func (m envModel) noteKeys(k string) (tea.Model, tea.Cmd) {
	switch k {
	case "up", "k":
		if m.noteSel > 0 {
			m.noteSel--
		}
	case "down", "j":
		if m.noteSel < len(m.notes)-1 {
			m.noteSel++
		}
	case "n":
		m.mode, m.edit = "note", ""
	case "d":
		if m.noteSel < len(m.notes) {
			n := m.notes[m.noteSel]
			os.Remove(filepath.Join(m.bridge, "notes", n.ID+".json"))
			return m, sshRun(m, "notes", notesScript(m.bridge))
		}
	}
	return m, nil
}

// msgRunes returns the text of a key press (single runes), empty for special keys.
func msgRunes(k string) []rune {
	if len(k) == 1 {
		return []rune(k)
	}
	if strings.HasPrefix(k, "ctrl+") && len(k) == 6 {
		return []rune{rune(k[5] - 'a' + 1)}
	}
	return nil
}

func currentUser() string {
	if u := os.Getenv("USER"); u != "" {
		return u
	}
	return strconv.Itoa(os.Getuid())
}

// ---------------------------------------------------------------- view

var (
	envTitle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("12"))
	envSel    = lipgloss.NewStyle().Background(lipgloss.Color("240")).Foreground(lipgloss.Color("15"))
	envHead   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("10"))
	envDim    = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	envWarn   = lipgloss.NewStyle().Foreground(lipgloss.Color("11"))
	envTabOn  = lipgloss.NewStyle().Bold(true).Background(lipgloss.Color("240")).Foreground(lipgloss.Color("15"))
	envBorder = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("240"))
)

func (m envModel) render() string {
	var b strings.Builder
	b.WriteString(envTitle.Render("CAMINUS ENV") + "  " + envDim.Render("the agent's home: "+m.user+" on this machine") + "\n")
	for i, t := range tabs {
		if i == m.tab {
			b.WriteString(" " + envTabOn.Render(t) + " ")
		} else {
			b.WriteString(" " + envDim.Render(t) + " ")
		}
	}
	b.WriteString("\n\n")
	b.WriteString(envBorder.Render(m.pane()))
	switch m.mode {
	case "agentgoal":
		b.WriteString("\n\n" + envWarn.Render("goal for "+m.agentName+" (enter sends, esc cancels):") + "\n" + m.edit + "█")
	case "agentname":
		b.WriteString("\n\n" + envWarn.Render("resident agent name:") + "\n" + m.edit + "█")
	case "edit":
		b.WriteString("\n\n" + envWarn.Render("edit root command (enter runs it under sudo, esc cancels):") + "\n" + m.edit + "█")
	case "answer":
		b.WriteString("\n\n" + envWarn.Render("answer the question (enter sends, esc cancels; a number picks a choice):") + "\n" + m.edit + "█")
	case "note":
		b.WriteString("\n\n" + envWarn.Render("note for the agent (enter leaves it, esc cancels):") + "\n" + m.edit + "█")
	case "newjob1":
		b.WriteString("\n\n" + envWarn.Render("new detached job — name (enter, esc cancels):") + "\n" + m.edit + "█")
	case "serve1":
		b.WriteString("\n\n" + envWarn.Render("new server job — model name (enter, esc cancels):") + "\n" + m.edit + "█")
	case "serve2":
		b.WriteString("\n\n" + envWarn.Render("server "+m.newJob+" — placement auto|0|1|2 (enter=auto, esc cancels):") + "\n" + m.edit + "█")
	case "newjob2":
		b.WriteString("\n\n" + envWarn.Render("new job "+m.newJob+" — command (enter spawns it detached, esc cancels):") + "\n" + m.edit + "█")
	}
	status := fmt.Sprintf("ssh: %s  requests: %d  jobs: %d  msg: %s", map[bool]string{true: "OK", false: "FAIL"}[m.sshOK], len(m.reqs), len(m.jobs), m.msg)
	b.WriteString("\n\n" + envDim.Render(status))
	b.WriteString("\n" + envDim.Render("tab/1-7 panes · q quit · agent: n goal, s start, x stop, c cancel, m name · requests: a/e/d/s · r refresh"))
	return b.String()
}

func (m envModel) pane() string {
	switch m.tab {
	case 0:
		return m.reqPane()
	case 1:
		return m.jobsPane()
	case 2:
		return strings.Join(m.procs, "\n")
	case 3:
		return m.notesPane()
	case 4:
		return m.ctxPane()
	case 6:
		return m.residentPane()
	default:
		return m.auditPane()
	}
}

func (m envModel) reqPane() string {
	lines := []string{envHead.Render("REQUESTS (root: a approve, e edit, d deny, s skip · ask: a answer)")}
	if len(m.reqs) == 0 {
		lines = append(lines, envDim.Render("  none waiting"))
	}
	for i, r := range m.reqs {
		body, tag := r.Cmd, "ROOT"
		if r.kind() == "question" {
			tag, body = "ASK ", r.Text
			if len(r.Choices) > 0 {
				body += "  [" + strings.Join(r.Choices, " | ") + "]"
			}
		}
		line := fmt.Sprintf("  %s %s from %s (pid %d, %s)\n    %s", tag, r.ID, r.User, r.Pid, r.CWD, body)
		if i == m.sel {
			line = envSel.Render(line)
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

func (m envModel) jobsPane() string {
	lines := []string{envHead.Render("JOBS in the agent user (K kill, l tail log, n new job, S new model server)")}
	if len(m.jobs) == 0 {
		lines = append(lines, envDim.Render("  none"))
	}
	for i, j := range m.jobs {
		line := fmt.Sprintf("  %-16s pid=%-8s %s  log=%s", j.Name, j.Pid, j.State, j.Log)
		if i == m.jobSel {
			line = envSel.Render(line)
		}
		lines = append(lines, line)
	}
	if m.logName != "" {
		lines = append(lines, "", envHead.Render("log tail: "+m.logName), envDim.Render(m.logView))
	}
	return strings.Join(lines, "\n")
}

func (m envModel) notesPane() string {
	lines := []string{envHead.Render("STEERING NOTES for the agent (n new, d delete; its env-note drains them)")}
	if len(m.notes) == 0 {
		lines = append(lines, envDim.Render("  inbox empty"))
	}
	for i, n := range m.notes {
		line := fmt.Sprintf("  [%s] %s", n.From, n.Text)
		if i == m.noteSel {
			line = envSel.Render(line)
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

func (m envModel) ctxPane() string {
	lines := []string{envHead.Render("SERVERS on this machine and their context windows (/props n_ctx)")}
	if len(m.servers) == 0 {
		lines = append(lines, envDim.Render("  none running on 127.0.0.1:8092-8101"))
	}
	for _, s := range m.servers {
		lines = append(lines, fmt.Sprintf("  127.0.0.1:%d  %s  context %s", s.Port, s.Model, s.Nctx))
	}
	return strings.Join(lines, "\n")
}

func (m envModel) auditPane() string {
	lines := []string{envHead.Render("AUDIT (tail of " + m.auditPath + ")")}
	if len(m.events) == 0 {
		lines = append(lines, envDim.Render("  empty"))
	}
	for _, e := range m.events {
		if len(e) > m.width-6 {
			e = e[:m.width-6]
		}
		lines = append(lines, envDim.Render("  "+e))
	}
	return strings.Join(lines, "\n")
}

func (m envModel) View() tea.View {
	v := tea.NewView(m.render())
	v.AltScreen = true
	v.WindowTitle = "caminus env"
	return v
}
