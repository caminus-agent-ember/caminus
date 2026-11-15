// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 jdominick05

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

type residentSessionsMsg struct {
	agent, memory, err string
	names              []string
}

type residentCreatedMsg struct{ name, err string }

func envClip(text string, width int) string {
	if width <= 0 {
		width = 110
	}
	return ansi.Truncate(text, width, "")
}

func (m envModel) detailViewport() viewport.Model {
	w, h := m.width, m.height
	if w <= 0 {
		w = 110
	}
	if h <= 0 {
		h = 32
	}
	reserve := 8
	if m.mode != "" && m.mode != "normal" {
		reserve += 4
	}
	view := viewport.New(viewport.WithWidth(max(1, w-2)), viewport.WithHeight(max(1, h-reserve)))
	view.SoftWrap = true
	view.SetContent(m.pane())
	view.SetYOffset(m.pageScroll)
	return view
}

func nextEnvTab(current, delta int) int {
	for i, id := range envNavigation {
		if id == current {
			return envNavigation[(i+delta+len(envNavigation))%len(envNavigation)]
		}
	}
	return envNavigation[0]
}

func residentSessions(m envModel) tea.Cmd {
	return func() tea.Msg {
		if len(m.cli) == 0 {
			return residentSessionsMsg{agent: m.agentName, err: "resident sessions need the caminus CLI (--cli)"}
		}
		out, err := residentCommand(m, []string{"peers", m.agentName, "--json"}, "").CombinedOutput()
		if err != nil {
			return residentSessionsMsg{agent: m.agentName, err: strings.TrimSpace(string(out))}
		}
		var peers []struct {
			Name string `json:"name"`
		}
		if err = json.Unmarshal(out, &peers); err != nil {
			return residentSessionsMsg{agent: m.agentName, err: err.Error()}
		}
		result := residentSessionsMsg{agent: m.agentName}
		for _, p := range peers {
			result.names = append(result.names, p.Name)
		}
		out, err = residentCommand(m, []string{"memory", m.agentName, "--json"}, "").CombinedOutput()
		var memory struct {
			Memory string `json:"memory"`
		}
		if err == nil && json.Unmarshal(out, &memory) == nil {
			result.memory = memory.Memory
		}
		return result
	}
}

func createResidentSession(m envModel, name string) tea.Cmd {
	return func() tea.Msg {
		out, err := residentCommand(m, []string{"create", name, "--from", m.agentName, "--json"}, "").CombinedOutput()
		if err != nil {
			return residentCreatedMsg{err: strings.TrimSpace(string(out)) + ": " + err.Error()}
		}
		out, err = residentCommand(m, []string{"start", name, "--json"}, "").CombinedOutput()
		if err != nil {
			return residentCreatedMsg{err: "Session created; runtime start failed: " + strings.TrimSpace(string(out))}
		}
		return residentCreatedMsg{name: name}
	}
}

func (m *envModel) openResident(name string) {
	if name != m.agentName {
		m.residentEpoch++
		m.agentName = name
		m.resident = residentStatus{}
		m.residentEvents = nil
		m.chat = residentChat{}
		m.goalSel = 0
		m.sessionMemory = ""
	}
	m.pendingLast = ""
	m.tab = 6
	m.msg = ""
	m.initChat()
	m.syncChat()
	if validSessionName(m.agentName) {
		saveLastSession(m.agentName)
	}
}

// openLastSession reopens the last conversation at launch. The first session
// list message falls back to the homepage when the session no longer exists.
func openLastSession(m *envModel) {
	last := loadLastSession()
	if last == "" {
		return
	}
	m.openResident(last)
	m.pendingLast = last
}

// validSessionName accepts the session names the panel feeds openResident; it
// only rules out values that could never come from the bus or CLI.
func validSessionName(name string) bool {
	if name == "" || len(name) > 128 {
		return false
	}
	for _, r := range name {
		if r == '\n' || r == '\x00' || r == '\r' || r < 0x20 {
			return false
		}
	}
	return strings.TrimSpace(name) == name
}

// The last opened conversation persists in the user's state dir so the next
// launch opens it directly. Failures fall back to the Sessions homepage silently.
func lastSessionPath() string {
	state := os.Getenv("XDG_STATE_HOME")
	if state == "" {
		if h, err := os.UserHomeDir(); err == nil {
			state = filepath.Join(h, ".local", "state")
		} else {
			return ""
		}
	}
	return filepath.Join(state, "caminus", "envui-last-session")
}

func loadLastSession() string {
	b, err := os.ReadFile(lastSessionPath())
	if err != nil {
		return ""
	}
	name := strings.TrimRight(string(b), "\n")
	if !validSessionName(name) {
		return ""
	}
	return name
}

func saveLastSession(name string) {
	if demoMode { // the demo never replaces the real last session
		return
	}
	path := lastSessionPath()
	if path == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	_ = os.WriteFile(path, []byte(name+"\n"), 0o600)
}

func (m envModel) sessionKeys(k string) (tea.Model, tea.Cmd) {
	switch k {
	case "up", "k":
		m.sessionSel = max(0, m.sessionSel-1)
	case "down", "j":
		m.sessionSel = min(max(0, len(m.sessionNames)-1), m.sessionSel+1)
	case "n":
		m.mode, m.edit = "sessionnew", ""
	case "enter":
		name := m.agentName
		if m.sessionSel >= 0 && m.sessionSel < len(m.sessionNames) {
			name = m.sessionNames[m.sessionSel]
		}
		m.openResident(name)
		return m, tea.Batch(residentRefresh(m), residentSessions(m))
	}
	return m, nil
}

// pickerNames lists the sessions the Ctrl+P query still matches; an empty list
// keeps the current name so the switcher can always reopen it.
func (m envModel) pickerNames() []string {
	names := m.sessionNames
	if len(names) == 0 {
		names = []string{m.agentName}
	}
	query := strings.ToLower(m.chat.pickerQuery)
	if query == "" {
		return names
	}
	var out []string
	for _, n := range names {
		if strings.Contains(strings.ToLower(n), query) {
			out = append(out, n)
		}
	}
	return out
}

func (m envModel) sessionsPane() string {
	lines := []string{envHead.Render("SESSIONS"), "Enter opens a conversation · n creates a session · ↑/↓ select", ""}
	names := m.sessionNames
	if len(names) == 0 {
		names = []string{m.agentName}
	}
	for i, name := range names {
		mark := "  "
		if i == m.sessionSel {
			mark = "› "
		}
		line := mark + residentText(name)
		if name == m.agentName {
			line += fmt.Sprintf(" · %d goals", len(m.resident.Goals))
			if m.resident.DaemonAlive {
				line += " · listening"
			}
		}
		lines = append(lines, line)
	}
	return strings.Join(append(lines, "", "Each named session keeps its own history and memory. Sessions share the local model server."), "\n")
}

func (m envModel) residentSettingsPane() string {
	return strings.Join([]string{envHead.Render("SESSION SETTINGS · " + residentText(m.agentName)),
		"Model: " + residentText(m.resident.Model), fmt.Sprintf("Context capacity: %d tokens", m.resident.Context),
		fmt.Sprintf("Compaction: %.0f%% trigger → %.0f%% target", m.resident.CompactThreshold*100, m.resident.CompactTarget*100),
		"", "Settings shown here are read-only; new sessions inherit the selected session's model settings.",
		"Root approval happens in /requests; credentials and passwords are never displayed here.",
		"", "Conversation: Enter sends · Alt+Enter adds a line · Escape browses history · Ctrl+P switches session",
		"Scrollback: Page Up/Down or mouse wheel · Ctrl+L follows new activity",
		"Session commands: /new, /goals, /goal ID, /newsession NAME, /session NAME",
		"Controls: /start, /stop, /cancel, /resume, /requests, /remember TEXT, /help"}, "\n")
}
