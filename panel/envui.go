// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 jdominick05

// caminus-panel env: the human side of the agent environment as a TUI (`caminus env ui`,
// docs/specs/spec-agent-env.md section 8). The Python CLI runs it with the paths as flags:
//
//	caminus-panel env --user NAME --bridge DIR --audit FILE --key FILE
//
// Left pane: the root requests waiting in the bridge (req/ without res/). Keys: up/down select,
// a approve (sudo runs attached to the terminal, so the password prompt works; output is teed
// to a temp file for the answer), e edit the command first, d deny, s skip for this session,
// q quit. Right pane: the live audit tail. The answer files and audit lines use the same JSON
// schema as python/caminus/env/bridge.py and audit.py, so `caminus env watch` and `caminus env
// audit` read what this TUI writes.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

type envReq struct {
	ID   string  `json:"id"`
	Pid  int     `json:"pid"`
	User string  `json:"user"`
	CWD  string  `json:"cwd"`
	Cmd  string  `json:"cmd"`
	Ts   float64 `json:"ts"`
}

type envModel struct {
	user      string
	bridge    string
	auditPath string
	key       string

	reqs   []envReq
	sel    int
	seen   map[string]bool
	events []string
	mode   string // "normal" or "edit"
	edit   string
	msg    string
	sshOK  bool
	sshN   int
	width  int
	height int
}

type envTickMsg time.Time
type probeMsg struct {
	ok  bool
	n   int
	err string
}
type execDoneMsg struct {
	id       string
	decision string
	cmd      string
	rc       int
	out      string
}

func envMain(args []string) {
	fs := flag.NewFlagSet("env", flag.ExitOnError)
	user := fs.String("user", "", "the agent user")
	bridge := fs.String("bridge", "", "the bridge directory")
	audit := fs.String("audit", "", "the audit log path")
	key := fs.String("key", "", "the harness ssh key")
	fs.Parse(args)
	if *user == "" || *bridge == "" || *audit == "" || *key == "" {
		fmt.Fprintln(os.Stderr, "caminus-panel env: --user --bridge --audit --key are all required")
		os.Exit(2)
	}
	m := envModel{user: *user, bridge: *bridge, auditPath: *audit, key: *key,
		seen: map[string]bool{}, mode: "normal", width: 100, height: 30}
	p := tea.NewProgram(m)
	if _, err := p.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "caminus-panel env:", err)
		os.Exit(1)
	}
}

func (m envModel) Init() tea.Cmd {
	return tea.Batch(tickCmd(), probeCmd(m))
}

func tickCmd() tea.Cmd {
	return tea.Tick(500*time.Millisecond, func(t time.Time) tea.Msg { return envTickMsg(t) })
}

func probeCmd(m envModel) tea.Cmd {
	return func() tea.Msg {
		c := exec.Command("ssh", "-i", m.key, "-o", "BatchMode=yes", "-o", "ConnectTimeout=3",
			"-o", "ControlMaster=auto", "-o", "ControlPath="+filepath.Join(os.TempDir(), "caminus-envui-%p"),
			m.user+"@127.0.0.1", "id -un")
		out, err := c.CombinedOutput()
		ok := err == nil && strings.TrimSpace(string(out)) == m.user
		return probeMsg{ok: ok, n: m.sshN + 1, err: strings.TrimSpace(string(out))}
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
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Ts < out[j].Ts })
	return out, nil
}

func writeAnswer(bridge, actor string, r envReq, decision, cmd string, rc *int, out, errText string) error {
	res := map[string]any{"id": r.ID, "decision": decision, "cmd": cmd, "approved_by": actor,
		"rc": rc, "out": out, "err": errText, "ts": float64(time.Now().UnixMilli()) / 1000}
	data, _ := json.Marshal(res)
	dir := filepath.Join(bridge, "res")
	if err := os.MkdirAll(dir, 0o770); err != nil {
		return err
	}
	tmp := filepath.Join(dir, r.ID+".json.tmp")
	if err := os.WriteFile(tmp, data, 0o660); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, r.ID+".json"))
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
// output to a temp file (the sudo password prompt goes to the tty, not the pipe). Returns the
// command and the temp file to read back.
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

// ---------------------------------------------------------------- model

func (m envModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil
	case envTickMsg:
		m.refresh()
		return m, tea.Batch(tickCmd(), probeCmd(m))
	case probeMsg:
		m.sshOK, m.sshN, m.msg = msg.ok, msg.n, msg.err
		return m, nil
	case execDoneMsg:
		rc := msg.rc
		_ = writeAnswer(m.bridge, currentUser(), envReq{ID: msg.id}, msg.decision, msg.cmd, &rc, msg.out, "")
		_ = appendAudit(m.auditPath, map[string]any{"event": "root_result", "id": msg.id,
			"decision": msg.decision, "cmd": msg.cmd, "rc": msg.rc})
		m.seen[msg.id] = true
		m.msg = fmt.Sprintf("%s: exit %d", msg.decision, msg.rc)
		m.refresh()
		return m, tickCmd()
	case tea.KeyPressMsg:
		return m.onKey(msg.String())
	}
	return m, nil
}

func (m *envModel) refresh() {
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
	if m.mode == "edit" {
		switch k {
		case "enter":
			cmd := m.edit
			m.mode = "normal"
			if cmd == "" || m.sel >= len(m.reqs) {
				return m, nil
			}
			r := m.reqs[m.sel]
			c, tmp, err := execRoot(cmd)
			if err != nil {
				m.msg = err.Error()
				return m, nil
			}
			id, decision := r.ID, "edited"
			m.msg = "running under sudo (approve with your password)..."
			return m, tea.Batch(tea.ExecProcess(c, func(err error) tea.Msg {
				rc := 0
				if ee, ok := err.(*exec.ExitError); ok {
					rc = ee.ExitCode()
				} else if err != nil {
					rc = 1
				}
				out, _ := os.ReadFile(tmp)
				os.Remove(tmp)
				return execDoneMsg{id: id, decision: decision, cmd: cmd, rc: rc, out: string(out)}
			}))
		case "esc":
			m.mode = "normal"
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
	case "up", "k":
		if m.sel > 0 {
			m.sel--
		}
	case "down", "j":
		if m.sel < len(m.reqs)-1 {
			m.sel++
		}
	case "r":
		m.refresh()
	case "d":
		if m.sel < len(m.reqs) {
			r := m.reqs[m.sel]
			_ = writeAnswer(m.bridge, currentUser(), r, "denied", r.Cmd, nil, "", "")
			_ = appendAudit(m.auditPath, map[string]any{"event": "root_decision", "id": r.ID,
				"decision": "denied", "by": currentUser()})
			m.seen[r.ID] = true
			m.msg = "denied " + r.ID
			m.refresh()
		}
	case "s":
		if m.sel < len(m.reqs) {
			m.seen[m.reqs[m.sel].ID] = true
			m.refresh()
		}
	case "a":
		if m.sel < len(m.reqs) {
			r := m.reqs[m.sel]
			c, tmp, err := execRoot(r.Cmd)
			if err != nil {
				m.msg = err.Error()
				return m, nil
			}
			m.msg = "running under sudo (approve with your password)..."
			return m, tea.ExecProcess(c, func(err error) tea.Msg {
				rc := 0
				if ee, ok := err.(*exec.ExitError); ok {
					rc = ee.ExitCode()
				} else if err != nil {
					rc = 1
				}
				out, _ := os.ReadFile(tmp)
				os.Remove(tmp)
				return execDoneMsg{id: r.ID, decision: "approved", cmd: r.Cmd, rc: rc, out: string(out)}
			})
		}
	case "e":
		if m.sel < len(m.reqs) {
			m.mode = "edit"
			m.edit = m.reqs[m.sel].Cmd
		}
	}
	return m, nil
}

// msgRunes returns the text of a key press (single runes), empty for special keys.
func msgRunes(k string) []rune {
	if len(k) == 1 {
		return []rune(k)
	}
	if strings.HasPrefix(k, "ctrl+") {
		if len(k) == 6 { // ctrl+a..z
			return []rune{rune(k[5] - 'a' + 1)}
		}
		return nil
	}
	if k == "backspace" {
		return []rune("\b")
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
	envBorder = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("240"))
)

func (m envModel) render() string {
	var b strings.Builder
	b.WriteString(envTitle.Render("CAMINUS ENV") + "  " + envDim.Render("the agent environment: "+m.user+" on this machine") + "\n\n")

	queue := []string{envHead.Render("ROOT REQUESTS (a approve, e edit, d deny, s skip)")}
	if len(m.reqs) == 0 {
		queue = append(queue, envDim.Render("  none waiting"))
	}
	for i, r := range m.reqs {
		line := fmt.Sprintf("  %s from %s (pid %d, %s)\n    %s", r.ID, r.User, r.Pid, r.CWD, r.Cmd)
		if i == m.sel {
			line = envSel.Render(line)
		}
		queue = append(queue, line)
	}

	audit := []string{envHead.Render("AUDIT (tail of " + m.auditPath + ")")}
	if len(m.events) == 0 {
		audit = append(audit, envDim.Render("  empty"))
	}
	for _, e := range m.events {
		if len(e) > m.width-6 {
			e = e[:m.width-6]
		}
		audit = append(audit, envDim.Render("  "+e))
	}

	left := envBorder.Render(strings.Join(queue, "\n"))
	right := envBorder.Render(strings.Join(audit, "\n"))
	panes := lipgloss.JoinHorizontal(lipgloss.Top, left, " ", right)
	b.WriteString(panes)

	if m.mode == "edit" {
		b.WriteString("\n\n" + envWarn.Render("edit root command (enter runs it under sudo, esc cancels):") + "\n" + m.edit + "█")
	}
	status := fmt.Sprintf("ssh: %s  requests: %d  msg: %s", map[bool]string{true: "OK", false: "FAIL"}[m.sshOK], len(m.reqs), m.msg)
	b.WriteString("\n\n" + envDim.Render(status))
	b.WriteString("\n" + envDim.Render("keys: up/down select · a approve · e edit · d deny · s skip · r refresh · q quit"))
	return b.String()
}

func (m envModel) View() tea.View {
	v := tea.NewView(m.render())
	v.AltScreen = true
	v.WindowTitle = "caminus env"
	return v
}
