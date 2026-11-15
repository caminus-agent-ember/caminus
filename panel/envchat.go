// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 jdominick05

package main

import (
	"encoding/json"
	"fmt"
	"strings"

	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/glamour/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// The chat owns editing, durable-history rendering and scroll position. Polls
// append events; they never replace the transcript or steal an unfinished draft.
type residentChat struct {
	ready, sending, newGoal, help, forceNew bool
	compose                                 textarea.Model
	transcript                              viewport.Model
	target, selectID                        string
	seq, rendered, renderWidth              int
	content                                 string
	markdown                                *glamour.TermRenderer
}

type residentComposeResult struct {
	agent, id, err string
	epoch          uint64
}

func (m *envModel) initChat() {
	if m.chat.ready {
		return
	}
	c := residentChat{ready: true, newGoal: true}
	c.compose = textarea.New()
	c.compose.Prompt = "› "
	c.compose.Placeholder = "Give the agent a goal, or type /help"
	c.compose.ShowLineNumbers = false
	c.compose.CharLimit = 0
	c.compose.MaxHeight = 0 // long pasted goals scroll inside the visible editor
	c.compose.SetHeight(3)
	c.compose.Focus()
	c.transcript = viewport.New()
	c.transcript.SoftWrap = true
	m.chat = c
}

func (m envModel) selectedResidentGoal() *residentGoal {
	if m.goalSel < 0 || m.goalSel >= len(m.resident.Goals) {
		return nil
	}
	g := m.resident.Goals[m.goalSel]
	return &g
}

func (m *envModel) receiveResident(msg residentMsg) {
	m.initChat()
	selected := ""
	if g := m.selectedResidentGoal(); g != nil {
		selected = g.ID
	}
	// A slower poll must not roll newer goal state backwards.
	if msg.status.LastEvent >= m.resident.LastEvent {
		m.resident = msg.status
	}
	for _, e := range msg.events {
		if e.Seq > m.chat.seq {
			m.residentEvents = append(m.residentEvents, e)
			m.chat.seq = e.Seq
		}
	}
	if m.chat.selectID != "" {
		selected = m.chat.selectID
	}
	m.goalSel = min(m.goalSel, max(0, len(m.resident.Goals)-1))
	for i, g := range m.resident.Goals {
		if g.ID == selected {
			m.goalSel = i
			m.chat.selectID = ""
			break
		}
	}
	if m.chat.compose.Value() == "" && !m.chat.sending && !m.chat.forceNew && m.chat.selectID == "" {
		m.targetSelectedGoal()
	}
	m.syncChat()
}

func (m *envModel) targetSelectedGoal() {
	g := m.selectedResidentGoal()
	m.chat.newGoal = g == nil || g.State == "completed" || g.State == "cancelled"
	m.chat.target = ""
	if !m.chat.newGoal {
		m.chat.target = g.ID
	}
}

func (m *envModel) syncChat() {
	m.initChat()
	w, h := max(20, m.width), max(12, m.height)
	m.chat.compose.SetWidth(w - 4)
	m.chat.compose.SetHeight(min(3, max(1, h-10)))
	oldBottom := m.chat.transcript.AtBottom()
	m.chat.transcript.SetWidth(w)
	m.chat.transcript.SetHeight(max(1, h-3-lipgloss.Height(m.chatComposer())-2))
	if m.chat.help {
		m.chat.transcript.SetHeight(max(1, m.chat.transcript.Height()-3))
	}
	if m.chat.renderWidth != w || m.chat.markdown == nil {
		m.chat.markdown, _ = glamour.NewTermRenderer(glamour.WithStandardStyle("dark"), glamour.WithWordWrap(max(10, w-4)))
		m.chat.content, m.chat.rendered, m.chat.renderWidth = "", 0, w
	}
	changed := m.chat.rendered != len(m.residentEvents)
	for _, e := range m.residentEvents[m.chat.rendered:] {
		if block := m.chat.eventBlock(e); block != "" {
			m.chat.content += block + "\n\n"
		}
	}
	m.chat.rendered = len(m.residentEvents)
	if changed || m.chat.transcript.TotalLineCount() == 0 {
		body := strings.TrimRight(m.chat.content, "\n")
		if body == "" {
			body = "Your agent's conversation and commands will stay here.\n\nType a goal below. /help shows the controls."
		}
		m.chat.transcript.SetContent(body)
	}
	if oldBottom {
		m.chat.transcript.GotoBottom()
	}
}

func (c *residentChat) renderMarkdown(text string) string {
	text = residentText(text)
	if c.markdown != nil {
		if rendered, err := c.markdown.Render(text); err == nil {
			return strings.Trim(rendered, "\n")
		}
	}
	return text
}

func fenced(text, language string) string {
	fence := "```"
	for strings.Contains(text, fence) {
		fence += "`"
	}
	return fence + language + "\n" + text + "\n" + fence
}

func (c *residentChat) eventBlock(e residentEvent) string {
	str := func(key string) string { s, _ := e.Data[key].(string); return s }
	heading := func(label, color string) string {
		return lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(color)).Render(label)
	}
	switch e.Kind {
	case "goal_queued":
		return heading("YOU · goal "+shortGoal(e.Goal), "12") + "\n" + c.renderMarkdown(str("prompt"))
	case "user_message", "peer_message":
		text := str("text")
		if text == "" {
			return ""
		}
		return heading("YOU · steer "+shortGoal(e.Goal), "12") + "\n" + c.renderMarkdown(text)
	case "assistant":
		var parts []string
		if text := str("content"); text != "" {
			parts = append(parts, heading("AGENT", "10")+"\n"+c.renderMarkdown(text))
		}
		calls, _ := e.Data["tool_calls"].([]any)
		for _, raw := range calls {
			call, _ := raw.(map[string]any)
			f, _ := call["function"].(map[string]any)
			name, _ := f["name"].(string)
			rawArgs, _ := f["arguments"].(string)
			var args map[string]any
			_ = json.Unmarshal([]byte(rawArgs), &args)
			if command, ok := args["command"].(string); ok {
				parts = append(parts, heading("RUN · "+residentText(name), "14")+"\n"+c.renderMarkdown(fenced(command, "bash")))
			} else {
				var pretty any
				text := rawArgs
				if json.Unmarshal([]byte(rawArgs), &pretty) == nil {
					if b, err := json.MarshalIndent(pretty, "", "  "); err == nil {
						text = string(b)
					}
				}
				parts = append(parts, heading("TOOL · "+residentText(name), "14")+"\n"+c.renderMarkdown(fenced(text, "json")))
			}
		}
		return strings.Join(parts, "\n\n")
	case "tool_result":
		result, _ := e.Data["result"].(map[string]any)
		if output, ok := result["output"].(string); ok && output != "" {
			return heading("OUTPUT · "+residentText(str("name")), "8") + "\n" + c.renderMarkdown(fenced(output, "text"))
		}
		if err, ok := result["error"].(string); ok {
			return heading("TOOL ERROR", "9") + "\n" + c.renderMarkdown(fenced(err, "text"))
		}
		if b, err := json.MarshalIndent(result, "", "  "); err == nil {
			return heading("RESULT · "+residentText(str("name")), "8") + "\n" + c.renderMarkdown(fenced(string(b), "json"))
		}
	case "finished":
		return heading("FINISHED", "10") + "\n" + c.renderMarkdown(str("summary"))
	case "question", "approval_requested":
		text := str("text")
		if e.Kind == "approval_requested" {
			text = str("reason") + "\n" + fenced(str("cmd"), "bash")
		}
		return heading("WAITING FOR YOU · /requests", "11") + "\n" + c.renderMarkdown(text)
	case "compacted":
		return heading("CONTEXT COMPACTED · conversation remains in this transcript", "11")
	case "cancelled":
		return heading("GOAL CANCELLED · "+shortGoal(e.Goal), "11")
	case "model_unavailable", "model_truncated", "context_refused":
		return heading(residentText(e.Kind), "9") + "\n" + c.renderMarkdown(str("error"))
	}
	return ""
}

func shortGoal(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func (m envModel) chatComposer() string {
	label := "New goal"
	if !m.chat.newGoal {
		label = "Steer goal " + shortGoal(m.chat.target)
	}
	if m.chat.sending {
		label = "Sending…"
	}
	if !m.chat.compose.Focused() {
		label += " · browsing history; Enter to type"
	}
	input := m.chat.compose.View()
	if m.mode == "agentname" {
		label = "Agent name · Enter selects · Esc cancels"
		input = terminalText(m.edit, false) + "█"
	}
	if m.mode == "agentgoal" || m.mode == "agentsteer" {
		input = terminalText(m.edit, true) + "█"
	}
	return envBorder.Width(max(18, m.width-2)).Render(envDim.Render(label) + "\n" + input)
}

func (m envModel) renderChat() string {
	m.syncChat()
	w := max(20, m.width)
	clip := func(s string) string { return ansi.Truncate(s, w, "") }
	var names []string
	for _, i := range envNavigation {
		names = append(names, envDim.Render(tabs[i]))
	}
	state := "stopped"
	if m.resident.DaemonAlive {
		state = "listening"
	}
	steps := 0
	if g := m.selectedResidentGoal(); g != nil {
		state = g.State
		steps = g.Steps
	}
	indicator := "●"
	if state == "working" || state == "queued" || state == "waiting_model" {
		frames := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
		indicator = frames[m.tickN%len(frames)]
	}
	head := clip(envTitle.Render("CAMINUS")+" · "+residentText(m.agentName)) + "\n" + clip(strings.Join(names, "  ")) + "\n" + clip(envHead.Render(indicator+" "+state)+fmt.Sprintf(" · %d steps", steps))
	status := fmt.Sprintf("%s · %s · ctx %d · SSH %s · %d requests", residentText(m.resident.Model), residentText(m.agentName), m.resident.Context, map[bool]string{true: "OK", false: "…"}[m.sshOK], len(m.reqs))
	if !m.chat.transcript.AtBottom() {
		status += fmt.Sprintf(" · history %.0f%%", m.chat.transcript.ScrollPercent()*100)
	}
	if m.chat.seq < m.resident.LastEvent {
		status += " · loading history"
	}
	if m.msg != "" {
		status += " · " + terminalText(m.msg, false)
	}
	foot := clip(envDim.Render(status)) + "\n" + clip(envDim.Render("Enter send · Alt+Enter newline · PgUp/PgDn scroll · /help · Tab panes · Ctrl+C quit"))
	if m.chat.help {
		foot += "\n" + clip("/new [goal] · /goals · /goal ID · /cancel · /resume") + "\n" + clip("/newsession NAME · /session NAME · /sessions · /requests · /remember TEXT") + "\n" + clip("/start · /stop · /help · /quit · Esc browse · Ctrl+N new · Ctrl+L follow")
	}
	return strings.Join([]string{head, m.chat.transcript.View(), m.chatComposer(), foot}, "\n")
}

func (m envModel) chatUpdate(msg tea.Msg) (tea.Model, tea.Cmd) {
	m.initChat()
	var cmd tea.Cmd
	if _, ok := msg.(tea.MouseWheelMsg); ok {
		m.chat.transcript, cmd = m.chat.transcript.Update(msg)
	} else if !m.chat.sending {
		m.chat.compose, cmd = m.chat.compose.Update(msg)
	}
	return m, cmd
}

func (m envModel) chatKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	m.initChat()
	k := msg.String()
	switch k {
	case "ctrl+c":
		return m, tea.Quit
	case "tab", "shift+tab":
		return m.onKey(k)
	case "pgup":
		m.chat.transcript.PageUp()
		return m, nil
	case "pgdown":
		m.chat.transcript.PageDown()
		return m, nil
	case "ctrl+l":
		m.chat.transcript.GotoBottom()
		return m, nil
	case "esc":
		m.chat.compose.Blur()
		return m, nil
	case "ctrl+n":
		if !m.chat.sending {
			m.chat.newGoal = true
			m.chat.forceNew = true
			m.chat.target = ""
			m.chat.compose.Reset()
			return m, m.chat.compose.Focus()
		}
		return m, nil
	}
	if !m.chat.compose.Focused() {
		switch k {
		case "enter", "t":
			return m, m.chat.compose.Focus()
		case "n":
			m.chat.newGoal = true
			m.chat.forceNew = true
			m.chat.target = ""
			return m, m.chat.compose.Focus()
		case "q":
			return m, tea.Quit
		case "up", "k":
			m.chat.transcript.ScrollUp(1)
			return m, nil
		case "down", "j":
			m.chat.transcript.ScrollDown(1)
			return m, nil
		case "home":
			m.chat.transcript.GotoTop()
			return m, nil
		case "end":
			m.chat.transcript.GotoBottom()
			return m, nil
		}
		return m, nil
	}
	if m.chat.sending {
		return m, nil
	}
	switch k {
	case "enter":
		return m.submitCompose()
	case "alt+enter", "shift+enter":
		m.chat.compose.InsertString("\n")
		return m, nil
	}
	return m.chatUpdate(msg)
}

func (m envModel) submitCompose() (tea.Model, tea.Cmd) {
	text := m.chat.compose.Value()
	if strings.TrimSpace(text) == "" || m.chat.sending {
		return m, nil
	}
	if strings.HasPrefix(strings.TrimSpace(text), "/") {
		return m.chatSlash(strings.TrimSpace(text))
	}
	return m.sendCompose(text)
}

func (m envModel) sendCompose(text string) (tea.Model, tea.Cmd) {
	args := []string{"send", m.agentName, "--stdin", "--json"}
	if !m.chat.newGoal {
		args = append(args, "--goal-id", m.chat.target)
	}
	name, epoch := m.agentName, m.residentEpoch
	m.chat.sending = true
	return m, func() tea.Msg {
		out, err := residentCommand(m, args, text).CombinedOutput()
		if err != nil {
			return residentComposeResult{agent: name, epoch: epoch, err: strings.TrimSpace(string(out)) + ": " + err.Error()}
		}
		var reply struct {
			ID string `json:"id"`
		}
		if err = json.Unmarshal(out, &reply); err != nil || reply.ID == "" {
			return residentComposeResult{agent: name, epoch: epoch, err: "Unrecognized delivery response; draft retained. Inspect /goals before retrying."}
		}
		return residentComposeResult{agent: name, epoch: epoch, id: reply.ID}
	}
}

func (m envModel) finishCompose(msg residentComposeResult) (tea.Model, tea.Cmd) {
	if msg.agent != m.agentName || msg.epoch != m.residentEpoch {
		return m, nil
	}
	m.chat.sending = false
	if msg.err != "" {
		m.msg = msg.err
		return m, nil
	}
	m.chat.compose.Reset()
	m.chat.forceNew = false
	m.chat.target, m.chat.selectID, m.chat.newGoal = msg.id, msg.id, false
	m.msg = "Delivered to goal " + shortGoal(msg.id)
	m.chat.transcript.GotoBottom()
	return m, residentRefresh(m)
}

func (m envModel) chatSlash(text string) (tea.Model, tea.Cmd) {
	command, arg, _ := strings.Cut(text, " ")
	arg = strings.TrimSpace(arg)
	clear := func() { m.chat.compose.Reset(); m.msg = "" }
	switch command {
	case "/help":
		clear()
		m.chat.help = !m.chat.help
	case "/quit":
		return m, tea.Quit
	case "/new":
		m.chat.newGoal, m.chat.target = true, ""
		m.chat.forceNew = true
		clear()
		if arg != "" {
			m.chat.compose.SetValue(arg)
			return m.sendCompose(arg)
		}
	case "/requests":
		clear()
		m.tab = 0
	case "/sessions":
		clear()
		m.tab = 7
		return m, residentSessions(m)
	case "/newsession":
		if arg == "" {
			m.msg = "Use /newsession NAME"
			return m, nil
		}
		clear()
		return m, createResidentSession(m, arg)
	case "/remember":
		if arg == "" {
			m.msg = "Use /remember TEXT"
			return m, nil
		}
		clear()
		return m, residentAction(m, []string{"memory", m.agentName, "--stdin", "--json"}, arg)
	case "/goals":
		clear()
		m.chat.help = true
		var goals []string
		for _, g := range m.resident.Goals {
			goals = append(goals, shortGoal(g.ID)+" "+g.State)
		}
		m.msg = strings.Join(goals, " · ")
	case "/goal":
		var found []int
		for i, g := range m.resident.Goals {
			if arg != "" && strings.HasPrefix(g.ID, arg) {
				found = append(found, i)
			}
		}
		if len(found) != 1 {
			m.msg = "Use /goal with one unambiguous goal ID from /goals"
			return m, nil
		}
		clear()
		m.goalSel = found[0]
		m.chat.forceNew = false
		m.targetSelectedGoal()
	case "/agent", "/session":
		if arg == "" {
			m.msg = "Use /agent NAME"
			return m, nil
		}
		clear()
		m.openResident(arg)
		return m, tea.Batch(residentRefresh(m), residentSessions(m))
	case "/start", "/stop":
		clear()
		return m, residentAction(m, []string{strings.TrimPrefix(command, "/"), m.agentName, "--json"}, "")
	case "/cancel":
		g := m.selectedResidentGoal()
		if g == nil {
			m.msg = "No selected goal to cancel"
			return m, nil
		}
		clear()
		return m, residentAction(m, []string{"cancel", m.agentName, g.ID, "--json"}, "")
	case "/resume":
		g := m.selectedResidentGoal()
		if g == nil {
			m.msg = "No selected goal"
			return m, nil
		}
		clear()
		return m, residentAction(m, []string{"resume", m.agentName, g.ID, "--json"}, "")
	default:
		m.msg = "Unknown command. /help lists commands; your draft is retained."
	}
	m.syncChat()
	return m, nil
}
