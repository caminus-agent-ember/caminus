// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 jdominick05

package main

import (
	"encoding/json"
	"fmt"
	"image/color"
	"slices"
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
	question                                *envReq
	questionChoice                          int
	questionCustom                          bool
	questionArmed                           bool
	questionInput                           textarea.Model
	questionDeferred, questionError         string
	picker                                  bool
	pickerQuery                             string
	pickerSel                               int
	slashIdx                                int
	slashToken, slashText                   string
	live                                    residentLive
	stream                                  *streamState
}

// residentLive is the reply being generated: shown under the durable transcript and replaced by it when the
// turn lands. Nothing here is durable; the transcript stays the record. hold keeps the window where the reader
// put it while more text arrives, so a long reply can be read at its own pace.
type residentLive struct {
	turn, text, reasoning string
	tools                 []string
	active, hold          bool
	top                   int
}

// highlightLive colors the inline code spans of a streaming line, so live text reads like the rendered
// transcript: `code` in the frost accent, everything else as written. Plain text otherwise - this runs on
// every snapshot, so it stays a single pass with no parsing.
func highlightLive(line string) string {
	var b strings.Builder
	for {
		open := strings.IndexByte(line, '`')
		if open < 0 {
			b.WriteString(line)
			return b.String()
		}
		shut := strings.IndexByte(line[open+1:], '`')
		if shut < 0 {
			b.WriteString(line)
			return b.String()
		}
		b.WriteString(line[:open])
		b.WriteString(envCode.Render(line[open+1 : open+1+shut]))
		line = line[open+1+shut+1:]
	}
}

// body is the live reply as wrapped lines: its reasoning, the tools it started, then the text.
func (l residentLive) body(width int) []string {
	var out []string
	if l.reasoning != "" {
		for _, ln := range strings.Split(ansi.Hardwrap(l.reasoning, width, true), "\n") {
			out = append(out, envThink.Render(ln))
		}
	}
	for _, name := range l.tools {
		out = append(out, envDim.Render("RUN · ")+envTool.Render(residentText(name))+envDim.Render(" …"))
	}
	if l.text != "" {
		for _, ln := range strings.Split(ansi.Hardwrap(l.text, width, true), "\n") {
			out = append(out, highlightLive(ln))
		}
	}
	if len(out) == 0 {
		out = []string{envDim.Render("generating…")}
	}
	return out
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
	// Wrap once when events or terminal width change. Viewport soft wrapping
	// recomputes ANSI widths across the entire history even for AtBottom/View,
	// turning every editor keystroke and cursor blink into a full transcript scan.
	c.transcript.SoftWrap = false
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
			if e.Kind == "assistant" {
				// The durable block replaces the live tail; keeping both would show the reply twice.
				m.chat.live = residentLive{}
			}
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
	m.syncQuestion()
	m.syncChat()
}

func (m *envModel) targetSelectedGoal() {
	g := m.selectedResidentGoal()
	m.chat.newGoal = g == nil || g.State == "cancelled"
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
	if m.chat.question != nil {
		m.chat.questionInput.SetWidth(w - 4)
		m.chat.questionInput.SetHeight(min(3, max(1, h-12)))
	}
	oldBottom := m.chat.transcript.AtBottom()
	// A viewport re-wraps its whole content on a size change, and this runs on every live snapshot, so a
	// 1 MB history would be re-read ten times a second while a reply streams. Only resize when it moved.
	if m.chat.transcript.Width() != w {
		m.chat.transcript.SetWidth(w)
	}
	// One header line and one footer line surround the transcript; the live tail sits between it and the
	// composer, so a generating reply never rewrites the durable content.
	// Half the body is the live window while a reply is written; the transcript keeps the other half.
	tail := 0
	if live := m.liveBlock(w); live != "" {
		tail = lipgloss.Height(live)
	}
	height := max(1, m.bodyHeight()-tail)
	if m.chat.help {
		height = max(1, height-3)
	}
	if m.chat.transcript.Height() != height {
		m.chat.transcript.SetHeight(height)
	}
	if m.chat.renderWidth != w || m.chat.markdown == nil {
		m.chat.markdown, _ = glamour.NewTermRenderer(glamour.WithStyles(envMarkdownStyle()), glamour.WithWordWrap(max(10, w-4)))
		m.chat.content, m.chat.rendered, m.chat.renderWidth = "", 0, w
	}
	changed := m.chat.rendered != len(m.residentEvents)
	for _, e := range m.residentEvents[m.chat.rendered:] {
		if block := m.chat.eventBlock(e); block != "" {
			m.chat.content += ansi.Hardwrap(eventCard(block), w, true) + "\n\n"
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

// bodyHeight is the space between the header and the footer, without the composer.
func (m envModel) bodyHeight() int {
	return max(4, max(12, m.height)-1-lipgloss.Height(m.chatBottom())-1)
}

// liveHeight is the live window's content lines (its label included): half the body less the border, so the
// transcript keeps the other half while a reply is being written.
func (m envModel) liveHeight() int { return max(3, m.bodyHeight()/2-3) }

// liveWindow is the number of body lines the live box shows: its content height less the label line.
func (m envModel) liveWindow() int { return max(1, m.liveHeight()-1) }

// liveTop is the first visible line: the tail while following, the held position otherwise.
func (m envModel) liveTop(body int) int {
	window := m.liveWindow()
	if !m.chat.live.hold {
		return max(0, body-window)
	}
	return max(0, min(m.chat.live.top, max(0, body-window)))
}

// scrollLive moves the live window and holds it, so text arriving mid-read cannot pull the view away.
// Scrolling back to the bottom resumes following.
func (m *envModel) scrollLive(delta int) {
	l := &m.chat.live
	if !l.active {
		return
	}
	body := len(l.body(max(10, m.width-4)))
	window := m.liveWindow()
	top := max(0, min(m.liveTop(body)+delta, max(0, body-window)))
	l.top = top
	l.hold = top < max(0, body-window)
}

// liveBlock renders the reply being generated in a fixed-height window: its reasoning, the tools it has
// started and the text so far. Plain text, not glamour: this is redrawn on every snapshot, and the durable
// block is styled once when the turn lands.
func (m envModel) liveBlock(w int) string {
	l := m.chat.live
	if !l.active {
		return ""
	}
	inner := max(10, w-4)
	body := l.body(inner)
	height := m.liveHeight()
	top := m.liveTop(len(body))
	label := "THINKING · live · Ctrl+T hold · Alt+↑/↓ scroll"
	if l.reasoning == "" {
		label = "AGENT · live · Ctrl+T hold · Alt+↑/↓ scroll"
	}
	if l.hold {
		label = fmt.Sprintf("LIVE · held · %d below · Ctrl+T follow", max(0, len(body)-(top+height)))
	}
	rows := []string{envDim.Render(label)}
	rows = append(rows, body[top:min(top+height-1, len(body))]...)
	return envBorder.Width(max(18, w-2)).Render(strings.Join(rows, "\n"))
}

// envTail keeps the last maxLines wrapped lines of s, so the live tail has a fixed ceiling.
func envTail(s string, width, maxLines int) string {
	lines := strings.Split(ansi.Hardwrap(s, max(8, width), true), "\n")
	if len(lines) > maxLines {
		lines = append([]string{envDim.Render("…")}, lines[len(lines)-maxLines:]...)
	}
	return strings.Join(lines, "\n")
}

// stopStream ends the reader process and drops the live tail; the transcript already holds what is durable.
func (c *residentChat) stopStream() {
	if c.stream != nil {
		c.stream.cancel()
		c.stream = nil
	}
	c.live = residentLive{}
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

// eventCard frames one event: its label, then its body behind a dim left rule, so the conversation reads as a
// stack of blocks instead of one wall of text. Blank lines stay blank, so a card can breathe.
func eventCard(block string) string {
	lines := strings.Split(strings.TrimRight(block, "\n"), "\n")
	if len(lines) < 2 {
		return block
	}
	var b strings.Builder
	b.WriteString(lines[0])
	for _, ln := range lines[1:] {
		if strings.TrimSpace(ansi.Strip(ln)) == "" {
			b.WriteString("\n")
			continue
		}
		b.WriteString("\n" + envDim.Render("│ ") + ln)
	}
	return b.String()
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
	heading := func(label string, c color.Color) string {
		return bold(c).Render(label)
	}
	switch e.Kind {
	case "goal_queued":
		return heading("YOU · goal "+shortGoal(e.Goal), cBlue) + "\n" + c.renderMarkdown(str("prompt"))
	case "user_message", "peer_message":
		text := str("text")
		if text == "" {
			return ""
		}
		return heading("YOU · steer "+shortGoal(e.Goal), cBlue) + "\n" + c.renderMarkdown(text)
	case "assistant":
		var parts []string
		// The reply's reasoning is shown once, above the answer, from the durable event: it is never
		// sent back to the model, so this block is display only.
		if text := str("reasoning"); text != "" {
			parts = append(parts, heading("THINKING", cMuted)+"\n"+c.renderMarkdown(text))
		}
		if text := str("content"); text != "" {
			parts = append(parts, heading("AGENT", cGreen)+"\n"+c.renderMarkdown(text))
		}
		calls, _ := e.Data["tool_calls"].([]any)
		for _, raw := range calls {
			call, _ := raw.(map[string]any)
			f, _ := call["function"].(map[string]any)
			name, _ := f["name"].(string)
			if name == "ask_user" {
				continue
			} // the durable question gets a native UI below
			rawArgs, _ := f["arguments"].(string)
			var args map[string]any
			_ = json.Unmarshal([]byte(rawArgs), &args)
			if command, ok := args["command"].(string); ok {
				parts = append(parts, heading("RUN · "+residentText(name), cOrange)+"\n"+c.renderMarkdown(fenced(command, "bash")))
			} else {
				var pretty any
				text := rawArgs
				if json.Unmarshal([]byte(rawArgs), &pretty) == nil {
					if b, err := json.MarshalIndent(pretty, "", "  "); err == nil {
						text = string(b)
					}
				}
				parts = append(parts, heading("TOOL · "+residentText(name), cOrange)+"\n"+c.renderMarkdown(fenced(text, "json")))
			}
		}
		return strings.Join(parts, "\n\n")
	case "tool_result":
		result, _ := e.Data["result"].(map[string]any)
		if str("name") == "ask_user" {
			answer, _ := result["answer"].(string)
			if answer == "" {
				answer, _ = result["decision"].(string)
			}
			return heading("YOU · answer", cBlue) + "\n" + c.renderMarkdown(answer)
		}
		if output, ok := result["output"].(string); ok && output != "" {
			return heading("OUTPUT · "+residentText(str("name")), cMuted) + "\n" + c.renderMarkdown(fenced(strings.ReplaceAll(output, "\r\n", "\n"), "text"))
		}
		if err, ok := result["error"].(string); ok {
			return heading("TOOL ERROR", cRed) + "\n" + c.renderMarkdown(fenced(err, "text"))
		}
		if b, err := json.MarshalIndent(result, "", "  "); err == nil {
			return heading("RESULT · "+residentText(str("name")), cMuted) + "\n" + c.renderMarkdown(fenced(string(b), "json"))
		}
	case "finished":
		return heading("FINISHED", cGreen) + "\n" + c.renderMarkdown(str("summary"))
	case "question", "approval_requested":
		text := str("text")
		label := "QUESTION"
		if e.Kind == "approval_requested" {
			text = str("reason") + "\n" + fenced(str("cmd"), "bash")
			label = "ROOT REVIEW · /requests"
		}
		return heading(label, cYellow) + "\n" + c.renderMarkdown(text)
	case "compacted":
		return heading("CONTEXT COMPACTED · conversation remains in this transcript", cYellow)
	case "cancelled":
		return heading("GOAL CANCELLED · "+shortGoal(e.Goal), cYellow)
	case "model_unavailable", "model_truncated", "context_refused":
		text := str("error")
		if partial, ok := e.Data["partial"].(map[string]any); ok {
			if content, _ := partial["content"].(string); content != "" {
				// Show what the model produced before it stopped, then why it stopped.
				text = c.renderMarkdown(content) + "\n" + envDim.Render(residentText(text))
			}
		}
		return heading(residentText(e.Kind), cRed) + "\n" + text
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
	if m.chat.question != nil && m.chat.questionArmed {
		return m.questionBox()
	}
	label := "New goal"
	if m.chat.question != nil && !m.chat.questionArmed {
		label = "Question waiting · F2 to answer · your draft is preserved"
	}
	if !m.chat.newGoal && m.chat.question == nil {
		label = "Steer goal " + shortGoal(m.chat.target)
		if g := m.selectedResidentGoal(); g != nil && g.ID == m.chat.target && g.State == "completed" {
			label = "Continue conversation · " + shortGoal(m.chat.target)
		}
	}
	if m.chat.sending {
		label = "Sending…"
	}
	if !m.chat.compose.Focused() {
		label += " · browsing history; Enter to type"
	}
	input := m.chat.compose.View()
	if (m.mode == "" || m.mode == "normal") && !m.chat.sending {
		value := strings.TrimRight(m.chat.compose.Value(), " ")
		if matches := slashMatches(value); !(len(matches) == 1 && matches[0] == value) && len(matches) > 0 {
			lines := matches
			if len(lines) > 4 {
				lines = lines[:4]
			}
			input = envDim.Render(envClip("Tab completes · "+strings.Join(lines, "  "), max(18, m.width-6))) + "\n" + input
		}
	}
	if m.mode == "agentname" {
		label = "Agent name · Enter selects · Esc cancels"
		input = terminalText(m.edit, false) + "█"
	}
	if m.mode == "agentgoal" || m.mode == "agentsteer" {
		input = terminalText(m.edit, true) + "█"
	}
	return envBorder.Width(max(18, m.width-2)).Render(envDim.Render(label) + "\n" + input)
}

// chatBottom renders whatever occupies the composer slot: the steering draft,
// the inline question or the Ctrl+P session switcher. syncChat sizes the
// transcript against its measured height.
func (m envModel) chatBottom() string {
	if m.chat.picker {
		return m.pickerBox()
	}
	return m.chatComposer()
}

// contextBarSlots is the fixed width of the usage bar: readable at a glance,
// small enough to keep the footer one line.
const contextBarSlots = 10

// humanTokens renders a token count at footer density: 482k, 1.0M.
func humanTokens(n int) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 1000:
		return fmt.Sprintf("%dk", (n+500)/1000)
	default:
		return fmt.Sprint(n)
	}
}

// compactTrigger normalizes the compaction threshold: a status without one
// behaves like the daemon's default, never like "compact 0%".
func compactTrigger(threshold float64) float64 {
	if threshold <= 0 || threshold >= 1 {
		return .85
	}
	return threshold
}

// contextBarLevel grades the measured footprint against the compaction
// trigger: 0 comfortable, 1 approaching, 2 at or past the trigger.
func contextBarLevel(used, capacity int, threshold float64) int {
	if used <= 0 || capacity <= 0 {
		return 0
	}
	switch share := float64(used) / float64(capacity); {
	case share >= compactTrigger(threshold):
		return 2
	case share >= compactTrigger(threshold)-0.15:
		return 1
	default:
		return 0
	}
}

// contextBar renders the measured context footprint for the footer as two
// pieces: an accent (label, bar, share) that takes the level's color, and a
// dim remainder (window size, trigger). A used count of 0 means the daemon
// has not measured anything yet; then only the window size shows — capacity
// is never presented as usage, and no percentage is invented from events.
func contextBar(used, capacity int, threshold float64) (accent, rest string) {
	if capacity <= 0 {
		return "", ""
	}
	if used <= 0 {
		return "", "ctx " + humanTokens(capacity) + " window"
	}
	share := float64(used) / float64(capacity)
	if share > 1 {
		share = 1
	}
	filled := int(share*contextBarSlots + 0.5)
	bar := strings.Repeat("█", filled) + strings.Repeat("░", contextBarSlots-filled)
	return fmt.Sprintf("ctx %s %d%%", bar, int(share*100+0.5)),
		fmt.Sprintf(" of %s · compact %.0f%%", humanTokens(capacity), compactTrigger(threshold)*100)
}

// contextStateStyle colors the header's status light by what it means: busy
// work in cyan, healthy listening in green, something-needs-you in amber,
// failure in red, anything else in the dim resting tone.
func contextStateStyle(state string) lipgloss.Style {
	switch state {
	case "working", "queued", "waiting_model":
		return envBusy
	case "listening":
		return envHead
	case "waiting", "waiting_user", "waiting_root", "paused":
		return envWarn
	case "error":
		return envCtxHot
	default:
		return envDim
	}
}

func (m envModel) renderChat() string {
	m.syncChat()
	w := max(20, m.width)
	clip := func(s string) string { return ansi.Truncate(s, w, "") }
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
	head := clip(envTitle.Render("CAMINUS") + " · " + residentText(m.agentName) +
		"  " + contextStateStyle(state).Render(indicator+" "+state) + " · " + fmt.Sprintf("%d steps", steps))
	accent, ctxRest := contextBar(m.resident.ContextUsed, m.resident.Context, m.resident.CompactThreshold)
	ctxStyle := envCtxOK
	switch contextBarLevel(m.resident.ContextUsed, m.resident.Context, m.resident.CompactThreshold) {
	case 1:
		ctxStyle = envWarn
	case 2:
		ctxStyle = envCtxHot
	}
	status := []string{envDim.Render(residentText(m.resident.Model))}
	if accent+ctxRest != "" {
		status = append(status, ctxStyle.Render(accent)+envDim.Render(ctxRest))
	}
	status = append(status,
		envDim.Render("SSH "+map[bool]string{true: "OK", false: "…"}[m.sshOK]),
		envDim.Render(fmt.Sprintf("%d requests", len(m.reqs))),
	)
	if !m.chat.transcript.AtBottom() {
		status = append(status, envDim.Render(fmt.Sprintf("history %.0f%%", m.chat.transcript.ScrollPercent()*100)))
	}
	if m.chat.live.active {
		hint := "live: Ctrl+T hold"
		if m.chat.live.hold {
			hint = "live: held · Ctrl+T follow"
		}
		status = append(status, envDim.Render(hint))
	}
	if m.chat.seq < m.resident.LastEvent {
		status = append(status, envDim.Render("loading history"))
	}
	if m.msg != "" {
		status = append(status, envDim.Render(terminalText(m.msg, false)))
	}
	status = append(status, envDim.Render("Enter send · Alt+Enter newline · PgUp/PgDn · Ctrl+P sessions · /help · Ctrl+C quit"))
	foot := clip(strings.Join(status, " · "))
	if m.chat.help {
		foot += "\n" + clip("/new [goal] · /goals · /goal ID · /cancel · /resume · /compact") + "\n" + clip("/newsession NAME · /session NAME · /sessions · /requests · /remember TEXT") + "\n" + clip("/start · /stop · /help · /quit · Esc browse · Ctrl+N new · Ctrl+L follow")
	}
	body := []string{head, m.chat.transcript.View()}
	if live := m.liveBlock(w); live != "" {
		body = append(body, live)
	}
	return strings.Join(append(body, m.chatBottom(), foot), "\n")
}

func (m envModel) chatUpdate(msg tea.Msg) (tea.Model, tea.Cmd) {
	m.initChat()
	var cmd tea.Cmd
	if wheel, ok := msg.(tea.MouseWheelMsg); ok {
		if m.chat.live.active {
			if wheel.Button == tea.MouseWheelUp {
				m.scrollLive(-3)
			} else {
				m.scrollLive(3)
			}
			return m, nil
		}
		m.chat.transcript, cmd = m.chat.transcript.Update(msg)
	} else if m.chat.question != nil && m.chat.questionArmed {
		if m.chat.questionCustom {
			m.chat.questionInput, cmd = m.chat.questionInput.Update(msg)
		}
	} else if !m.chat.sending {
		m.chat.compose, cmd = m.chat.compose.Update(msg)
	}
	return m, cmd
}

func (m envModel) chatKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	m.initChat()
	if m.chat.picker {
		return m.pickerKey(msg)
	}
	if m.chat.question != nil {
		return m.questionKey(msg)
	}
	return m.chatDraftKey(msg)
}

func (m envModel) chatDraftKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	k := msg.String()
	switch k {
	case "ctrl+c":
		return m, tea.Quit
	case "ctrl+p":
		m.chat.picker, m.chat.pickerQuery, m.chat.pickerSel = true, "", 0
		m.chat.compose.Blur()
		return m, residentSessions(m)
	case "tab", "shift+tab":
		if next, done := m.cycleSlash(); done {
			return next, nil
		}
		return m.onKey(k)
	case "pgup":
		if m.chat.live.active {
			m.scrollLive(-m.liveHeight())
			return m, nil
		}
		m.chat.transcript.PageUp()
		return m, nil
	case "pgdown":
		if m.chat.live.active {
			m.scrollLive(m.liveHeight())
			return m, nil
		}
		m.chat.transcript.PageDown()
		return m, nil
	case "ctrl+l":
		m.chat.transcript.GotoBottom()
		m.chat.live.hold = false // follow the live tail again too
		return m, nil
	case "ctrl+t":
		// Hold the live window where it is while the reply keeps arriving.
		m.chat.live.hold = !m.chat.live.hold
		if !m.chat.live.hold {
			m.chat.live.top = 0 // following recomputes the tail from the body length
		}
		return m, nil
	case "alt+up":
		m.scrollLive(-3)
		return m, nil
	case "alt+down":
		m.scrollLive(3)
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
	trimmed := strings.TrimSpace(text)
	if trimmed == "" || m.chat.sending {
		return m, nil
	}
	// A leading // sends the rest as a message, so a goal about /etc or /tmp is not taken for a command.
	if strings.HasPrefix(trimmed, "//") {
		return m.sendCompose(strings.TrimPrefix(trimmed, "/"))
	}
	if strings.HasPrefix(trimmed, "/") {
		return m.chatSlash(trimmed)
	}
	return m.sendCompose(text)
}

func (m envModel) sendCompose(text string) (tea.Model, tea.Cmd) {
	args := []string{"send", m.agentName, "--stdin", "--json"}
	if !m.chat.newGoal {
		args = append(args, "--goal-id", m.chat.target, "--continue")
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
		// A typo here used to clear the transcript and leave an empty conversation behind.
		if len(m.sessionNames) > 0 && !slices.Contains(m.sessionNames, arg) {
			m.msg = "no such session " + terminalText(arg, true) + " · Ctrl+P lists them"
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
	case "/compact":
		// Compaction needs the model and the single writer, so this asks the runtime to do it on its next
		// pass rather than writing history from here.
		g := m.selectedResidentGoal()
		args := []string{"compact", m.agentName}
		if g != nil {
			args = append(args, g.ID)
		}
		clear()
		return m, residentAction(m, args, "")
	case "/resume":
		g := m.selectedResidentGoal()
		if g == nil {
			m.msg = "No selected goal"
			return m, nil
		}
		clear()
		return m, residentAction(m, []string{"resume", m.agentName, g.ID, "--json"}, "")
	default:
		m.msg = "Unknown command. /help lists commands; //text sends a message; your draft is retained."
	}
	m.syncChat()
	return m, nil
}

// slashCommands backs the composer autocomplete; every name must be handled by
// chatSlash. The list stays in one place so the two never drift apart.
var slashCommands = []struct{ name, hint string }{
	{"/agent", "open another session NAME"},
	{"/cancel", "cancel the selected goal"},
	{"/compact", "summarize this goal's history into memory now"},
	{"/goal", "select a goal by ID prefix"},
	{"/goals", "list goals"},
	{"/help", "toggle the help footer"},
	{"/new", "start a new goal"},
	{"/newsession", "create a session NAME"},
	{"/quit", "close the UI; the daemon continues"},
	{"/remember", "append TEXT to this session's memory"},
	{"/requests", "open questions and root approvals"},
	{"/resume", "resume the selected goal"},
	{"/session", "open a session NAME"},
	{"/sessions", "open the Sessions homepage"},
	{"/start", "run this session's loop"},
	{"/stop", "stop this session's loop"},
}

// slashMatches returns the commands whose name the draft token still prefixes.
// An exact command stays listed so repeated Tab can cycle across the longer
// names sharing its prefix; arguments or a multiline draft get nothing.
func slashMatches(draft string) []string {
	if draft == "" || !strings.HasPrefix(draft, "/") {
		return nil
	}
	if strings.ContainsAny(draft, " \n") {
		return nil
	}
	var out []string
	for _, c := range slashCommands {
		if strings.HasPrefix(c.name, draft) {
			out = append(out, c.name)
		}
	}
	return out
}

// cycleSlash completes the draft to the next matching command on Tab without
// altering what the user has typed beyond the command token. Tab cycles across
// every command sharing the originally typed prefix (so /s can reach /start and
// /stop); an exact, unique token leaves Tab free for the page navigation.
func (m envModel) cycleSlash() (tea.Model, bool) {
	value := m.chat.compose.Value()
	trimmed := strings.TrimRight(value, " ")
	token := trimmed
	if trimmed == m.chat.slashText || trimmed == m.chat.slashToken {
		token = m.chat.slashToken // continue an in-progress cycle over the original prefix
	} else {
		m.chat.slashIdx = 0
	}
	matches := slashMatches(token)
	if len(matches) == 0 {
		return m, false
	}
	pick := matches[m.chat.slashIdx%len(matches)]
	m.chat.slashIdx++
	m.chat.slashToken = token
	m.chat.slashText = pick
	if pick == trimmed {
		return m, false
	}
	m.chat.compose.SetValue(pick + " ")
	return m, true
}

// The Ctrl+P switcher selects another session without leaving the conversation
// page. Enter opens the highlighted session; the section replaces the composer
// slot, so it reuses the composer's measured height.
func (m envModel) pickerKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	k := msg.String()
	switch k {
	case "ctrl+c":
		return m, tea.Quit
	case "pgup":
		if m.chat.live.active {
			m.scrollLive(-m.liveHeight())
			return m, nil
		}
		m.chat.transcript.PageUp()
		return m, nil
	case "pgdown":
		if m.chat.live.active {
			m.scrollLive(m.liveHeight())
			return m, nil
		}
		m.chat.transcript.PageDown()
		return m, nil
	case "tab", "shift+tab":
		return m.onKey(k)
	case "esc":
		m.chat.picker = false
		return m, m.chat.compose.Focus()
	case "f2":
		m.chat.picker = false
		if m.chat.question != nil {
			m.chat.questionArmed = true
			m.chat.compose.Blur()
			if m.chat.questionCustom {
				return m, m.chat.questionInput.Focus()
			}
			return m, nil
		}
		return m, m.chat.compose.Focus()
	case "up", "k":
		m.chat.pickerSel = max(0, m.chat.pickerSel-1)
	case "down", "j":
		m.chat.pickerSel++
	case "enter":
		names := m.pickerNames()
		if len(names) == 0 {
			return m, nil
		}
		name := names[min(m.chat.pickerSel, len(names)-1)]
		if name == m.agentName {
			m.chat.picker = false
			return m, m.chat.compose.Focus()
		}
		m.openResident(name)
		return m, tea.Batch(residentRefresh(m), residentSessions(m))
	case "backspace":
		if query := []rune(m.chat.pickerQuery); len(query) > 0 {
			m.chat.pickerQuery = string(query[:len(query)-1])
		}
	default:
		if r := msgRunes(k); r != nil {
			m.chat.pickerQuery += string(r)
		}
	}
	m.chat.pickerSel = max(0, min(m.chat.pickerSel, len(m.pickerNames())-1))
	return m, nil
}

func (m envModel) pickerBox() string {
	names := m.pickerNames()
	w := max(18, m.width-2)
	maxRows := max(1, m.height-8)
	first := 0
	if len(names) > maxRows {
		first = max(0, min(m.chat.pickerSel-maxRows+1, len(names)-maxRows))
	}
	rows := []string{envWarn.Render("OPEN SESSION"), "› " + residentText(m.chat.pickerQuery) + "█", ""}
	if len(names) == 0 {
		rows = append(rows, envDim.Render("no matching session"))
	}
	for idx := first; idx < min(first+maxRows, len(names)); idx++ {
		name := names[idx]
		mark := "  "
		if idx == m.chat.pickerSel {
			mark = "› "
		}
		row := mark + residentText(name)
		if idx == m.chat.pickerSel {
			row = lipgloss.NewStyle().Bold(true).Render(row)
		}
		rows = append(rows, envClip(row, w-4))
	}
	hint := "Enter opens · ↑/↓ choose · Esc cancels"
	return envBorder.Width(w).Render(strings.Join(rows, "\n") + "\n" + envDim.Render(envClip(hint, w-4)))
}
