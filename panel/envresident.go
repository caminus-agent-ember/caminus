// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 jdominick05

package main

import (
	"bufio"
	"context"
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
	Name             string         `json:"name"`
	Model            string         `json:"model"`
	Context          int            `json:"context"`
	CompactThreshold float64        `json:"compact_threshold"`
	CompactTarget    float64        `json:"compact_target_ratio"`
	DaemonAlive      bool           `json:"daemon_alive"`
	Goals            []residentGoal `json:"goals"`
	LastEvent        int            `json:"last_event"`
}
type residentEvent struct {
	Seq  int            `json:"seq"`
	Goal string         `json:"goal"`
	Kind string         `json:"kind"`
	Data map[string]any `json:"data"`
}
type residentMsg struct {
	agent  string
	epoch  uint64
	status residentStatus
	events []residentEvent
	err    string
}
type residentActionMsg struct{ text string }

// residentLiveMsg is one NDJSON line from `agent stream`: the reply the runtime is generating right now.
// reset carries the whole text of a new turn; later lines carry only the suffix.
type residentLiveMsg struct {
	agent, turn, text, reasoning string
	tools                        []string
	reset, done                  bool
	err                          string
	epoch                        uint64
}

// residentStreamStartMsg hands the started reader process to the model.
type residentStreamStartMsg struct {
	agent string
	epoch uint64
	state *streamState
	err   string
}

// streamState is the one live reader for this conversation page: one long-lived process instead of a poll
// per keystroke, killed when the page, the session or the UI goes away.
type streamState struct {
	cancel context.CancelFunc
	sc     *bufio.Scanner
}

func residentCommand(m envModel, args []string, input string) *exec.Cmd {
	argv := append(append([]string{}, m.cli[1:]...), "env", "agent")
	argv = append(argv, args...)
	c := exec.Command(m.cli[0], argv...)
	c.Stdin = strings.NewReader(input)
	return c
}

// startStream opens `agent stream NAME`: the runtime rewrites a snapshot of the reply it is generating
// (at most ten times a second), and each line arrives here as one message.
func startStream(m envModel) tea.Cmd {
	if len(m.cli) == 0 || m.agentName == "" {
		return nil
	}
	name, epoch := m.agentName, m.residentEpoch
	cli := append([]string{}, m.cli...)
	return func() tea.Msg {
		ctx, cancel := context.WithCancel(context.Background())
		argv := append(append([]string{}, cli[1:]...), "env", "agent", "stream", name, "--interval", "0.05")
		cmd := exec.CommandContext(ctx, cli[0], argv...)
		out, err := cmd.StdoutPipe()
		if err == nil {
			err = cmd.Start()
		}
		if err != nil {
			cancel()
			return residentStreamStartMsg{agent: name, epoch: epoch, err: err.Error()}
		}
		go func() { _ = cmd.Wait() }() // reap when the page closes and the context kills it
		sc := bufio.NewScanner(out)
		sc.Buffer(make([]byte, 0, 64<<10), 1<<20) // a turn's whole text can arrive in one reset line
		return residentStreamStartMsg{agent: name, epoch: epoch, state: &streamState{cancel: cancel, sc: sc}}
	}
}

// streamLine reads one NDJSON line; the update loop asks for the next one after handling each.
func streamLine(agent string, epoch uint64, sc *bufio.Scanner) tea.Cmd {
	return func() tea.Msg {
		if sc == nil || !sc.Scan() {
			return residentLiveMsg{agent: agent, epoch: epoch, done: true}
		}
		var line struct {
			Turn      string   `json:"turn"`
			Content   string   `json:"content"`
			Reasoning string   `json:"reasoning"`
			Tools     []string `json:"tools"`
			Reset     bool     `json:"reset"`
			Done      bool     `json:"done"`
		}
		if err := json.Unmarshal(sc.Bytes(), &line); err != nil {
			return residentLiveMsg{agent: agent, epoch: epoch, err: err.Error(), done: true}
		}
		return residentLiveMsg{agent: agent, epoch: epoch, turn: line.Turn, text: line.Content,
			reasoning: line.Reasoning, tools: line.Tools, reset: line.Reset, done: line.Done}
	}
}

func residentRefresh(m envModel) tea.Cmd {
	name, after := m.agentName, m.chat.seq
	epoch := m.residentEpoch
	if name == "" {
		name = "resident"
	}
	return func() tea.Msg {
		if len(m.cli) == 0 {
			return residentMsg{agent: name, epoch: epoch, err: "resident controls need the caminus CLI (--cli)"}
		}
		out, err := residentCommand(m, []string{"status", name, "--json"}, "").CombinedOutput()
		if err != nil {
			return residentMsg{agent: name, epoch: epoch, err: strings.TrimSpace(string(out))}
		}
		var status residentStatus
		if err = json.Unmarshal(out, &status); err != nil {
			return residentMsg{agent: name, epoch: epoch, err: err.Error()}
		}
		var events []residentEvent
		// Catch up from the durable cursor, including history predating launch.
		// Bound each refresh; another tick continues a large backlog without
		// replacing older entries or skipping a page while the agent works.
		for page := 0; page < 4 && after < status.LastEvent; page++ {
			out, err = residentCommand(m, []string{"events", name, "--after", fmt.Sprint(after), "--json"}, "").CombinedOutput()
			if err != nil {
				return residentMsg{agent: name, epoch: epoch, err: strings.TrimSpace(string(out))}
			}
			var batch []residentEvent
			if err = json.Unmarshal(out, &batch); err != nil {
				return residentMsg{agent: name, epoch: epoch, err: err.Error()}
			}
			if len(batch) == 0 {
				break
			}
			events = append(events, batch...)
			after = batch[len(batch)-1].Seq
		}
		return residentMsg{agent: name, epoch: epoch, status: status, events: events}
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
		m.steerGoal = ""
	case "t":
		if m.goalSel < 0 || m.goalSel >= len(m.resident.Goals) {
			m.msg = "Select a goal to steer. Use n for a new goal."
			return m, nil
		}
		goal := m.resident.Goals[m.goalSel]
		if goal.ID == "" || goal.State == "completed" || goal.State == "cancelled" {
			m.msg = "That goal is finished. Use n for a new goal."
			return m, nil
		}
		m.mode, m.edit, m.steerGoal = "agentsteer", "", goal.ID
		m.msg = "Steering is read between model steps; human/root waits and paused goals still need their normal answer or resume."
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
	return terminalText(s, true)
}

func (m envModel) residentPane() string {
	state := "stopped"
	if m.resident.DaemonAlive {
		state = "listening"
	}
	lines := []string{envHead.Render(fmt.Sprintf("RESIDENT %s · %s", m.agentName, state)), "n: new goal · t: steer selected goal · s: start · x: stop runtime · c: cancel · m: agent"}
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
			if e.Kind == "user_message" {
				if s, ok := e.Data["text"].(string); ok {
					text = s
				}
			}
			if text != "" {
				activity = append(activity, e.Kind+": "+residentText(text))
			}
		}
		joined := strings.Join(activity, "\n")
		lines = append(lines, joined)
	}
	return strings.Join(lines, "\n")
}
