// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 jdominick05

package main

import (
	"encoding/json"
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
)

type residentSessionsMsg struct {
	agent, memory, err string
	names              []string
}

type residentCreatedMsg struct{ name, err string }

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
	m.tab = 6
	m.msg = ""
	m.initChat()
	m.syncChat()
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
		"", "Conversation: Enter sends · Alt+Enter adds a line · Escape browses history",
		"Scrollback: Page Up/Down or mouse wheel · Ctrl+L follows new activity",
		"Session commands: /new, /goals, /goal ID, /newsession NAME, /session NAME",
		"Controls: /start, /stop, /cancel, /resume, /requests, /remember TEXT, /help"}, "\n")
}
