// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 jdominick05

package main

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"

	tea "charm.land/bubbletea/v2"
)

type residentGoal struct {
	ID      string `json:"id"`
	Prompt  string `json:"prompt"`
	State   string `json:"state"`
	Summary string `json:"summary"`
	Error   string `json:"error"`
	Steps   int    `json:"steps"`
}
type residentStatus struct {
	Name        string         `json:"name"`
	DaemonAlive bool           `json:"daemon_alive"`
	Goals       []residentGoal `json:"goals"`
	LastEvent   int            `json:"last_event"`
}
type residentEvent struct {
	Seq  int            `json:"seq"`
	Goal string         `json:"goal"`
	Kind string         `json:"kind"`
	Data map[string]any `json:"data"`
}
type residentMsg struct {
	status residentStatus
	events []residentEvent
	err    string
}
type residentActionMsg struct{ text string }

func residentCommand(m envModel, args []string, input string) *exec.Cmd {
	argv := append(append([]string{}, m.cli[1:]...), "env", "agent")
	argv = append(argv, args...)
	c := exec.Command(m.cli[0], argv...)
	c.Stdin = strings.NewReader(input)
	return c
}

func residentRefresh(m envModel) tea.Cmd {
	return func() tea.Msg {
		if len(m.cli) == 0 {
			return residentMsg{err: "resident controls need the caminus CLI (--cli)"}
		}
		name := m.agentName
		if name == "" {
			name = "resident"
		}
		out, err := residentCommand(m, []string{"status", name, "--json"}, "").CombinedOutput()
		if err != nil {
			return residentMsg{err: strings.TrimSpace(string(out))}
		}
		var status residentStatus
		if err = json.Unmarshal(out, &status); err != nil {
			return residentMsg{err: err.Error()}
		}
		out, err = residentCommand(m, []string{"events", name, "--after", fmt.Sprint(max(0, status.LastEvent-200)), "--json"}, "").CombinedOutput()
		if err != nil {
			return residentMsg{status: status, err: strings.TrimSpace(string(out))}
		}
		var events []residentEvent
		if err = json.Unmarshal(out, &events); err != nil {
			return residentMsg{err: err.Error()}
		}
		return residentMsg{status: status, events: events}
	}
}

func residentAction(m envModel, args []string, input string) tea.Cmd {
	return func() tea.Msg {
		if len(m.cli) == 0 {
			return residentActionMsg{"resident controls need the caminus CLI (--cli)"}
		}
		out, err := residentCommand(m, args, input).CombinedOutput()
		text := strings.TrimSpace(string(out))
		if err != nil {
			text = err.Error() + ": " + text
		}
		return residentActionMsg{text}
	}
}

func (m envModel) residentKeys(k string) (tea.Model, tea.Cmd) {
	if m.agentName == "" {
		m.agentName = "resident"
	}
	switch k {
	case "n", "enter":
		m.mode, m.edit = "agentgoal", ""
	case "m":
		m.mode, m.edit = "agentname", m.agentName
	case "s":
		return m, residentAction(m, []string{"start", m.agentName, "--json"}, "")
	case "x":
		return m, residentAction(m, []string{"stop", m.agentName, "--json"}, "")
	case "c":
		if m.goalSel < len(m.resident.Goals) {
			return m, residentAction(m, []string{"cancel", m.agentName, m.resident.Goals[m.goalSel].ID, "--json"}, "")
		}
	case "up", "k":
		if m.goalSel > 0 {
			m.goalSel--
		}
	case "down", "j":
		if m.goalSel+1 < len(m.resident.Goals) {
			m.goalSel++
		}
	}
	return m, nil
}

func residentText(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 32 && r != '\n' && r != '\t' {
			return -1
		}
		return r
	}, s)
}

func (m envModel) residentPane() string {
	state := "stopped"
	if m.resident.DaemonAlive {
		state = "listening"
	}
	lines := []string{envHead.Render(fmt.Sprintf("RESIDENT %s · %s", m.agentName, state)), "n: give a goal · s: start · x: stop runtime · c: cancel goal · m: choose agent"}
	if len(m.resident.Goals) == 0 {
		return strings.Join(append(lines, "\nWaiting for your first goal."), "\n")
	}
	for i, g := range m.resident.Goals {
		if i >= 8 {
			break
		}
		mark := "  "
		if i == m.goalSel {
			mark = "> "
		}
		prompt := strings.Split(residentText(g.Prompt), "\n")[0]
		if len([]rune(prompt)) > 100 {
			prompt = string([]rune(prompt)[:100]) + "…"
		}
		lines = append(lines, fmt.Sprintf("%s[%s] %s", mark, g.State, prompt))
	}
	if m.goalSel < len(m.resident.Goals) {
		g := m.resident.Goals[m.goalSel]
		lines = append(lines, "", fmt.Sprintf("%s · %d model steps", g.ID, g.Steps), residentText(g.Summary), residentText(g.Error))
		var activity []string
		for _, e := range m.residentEvents {
			if e.Goal != g.ID {
				continue
			}
			text := ""
			if s, ok := e.Data["content"].(string); ok {
				text = s
			}
			if result, ok := e.Data["result"].(map[string]any); ok {
				if s, ok := result["output"].(string); ok {
					text = s
				}
			}
			if s, ok := e.Data["question"].(string); ok {
				text = s
			}
			if text != "" {
				activity = append(activity, e.Kind+": "+residentText(text))
			}
		}
		if len(activity) > 6 {
			activity = activity[len(activity)-6:]
		}
		joined := strings.Join(activity, "\n")
		if len([]rune(joined)) > 6000 {
			joined = string([]rune(joined)[len([]rune(joined))-6000:])
		}
		lines = append(lines, joined)
	}
	return strings.Join(lines, "\n")
}
