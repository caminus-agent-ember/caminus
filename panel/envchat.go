// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 jdominick05

package main

import (
	"encoding/json"
	"fmt"
	"image/color"
	"os"
	"slices"
	"strings"
	"time"

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
	expanded                                bool      // Ctrl+E: every card open (tool calls, output, thinking)
	armed                                   string    // the root request a first "a" armed; a second approves
	armedAt                                 time.Time // when it was armed (10 s to confirm)
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
	approvals                               bool // the Approvals overlay is over the conversation
	approvalSel                             int  // the row it has selected
	approvalDetail                          bool // Enter opened the selected request in full
	goals, goalsDetail                      bool // the Goals overlay, and its selected goal opened in full
	goalsSel                                int
	models                                  modelsState // the Models overlay (/model)
	page, pageTop                           int         // an old page shown as an overlay (-1: none), and its scroll
	slashIdx                                int
	slashToken, slashText                   string
	pathIdx                                 int
	pathPrefix, pathPick                    string
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
	wrapText, wrapThink   *wrapCache // finished lines wrapped once per turn (body)
}

// wrapCache keeps the wrapped, styled lines of a growing text's finished part (everything up to its last newline),
// so a snapshot wraps only what arrived since the last one and the partial line at the end. A streamed reply only
// grows within a turn; anything else (a new width, a shorter text) starts over.
type wrapCache struct {
	width, done int
	lines       []string
}

func (c *wrapCache) wrap(s string, width int, style func(string) string) []string {
	cut := strings.LastIndexByte(s, '\n') + 1
	if c.width != width || cut < c.done {
		c.width, c.done, c.lines = width, 0, nil
	}
	if cut > c.done {
		for _, ln := range strings.Split(ansi.Hardwrap(strings.TrimSuffix(s[c.done:cut], "\n"), width, true), "\n") {
			c.lines = append(c.lines, style(ln))
		}
		c.done = cut
	}
	out := slices.Clip(c.lines)
	for _, ln := range strings.Split(ansi.Hardwrap(s[cut:], width, true), "\n") {
		out = append(out, style(ln))
	}
	return out
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

// appendText adds a streamed delta to the reply (the text only ever grows within a turn).
func (l residentLive) appendText(delta string) residentLive { l.text += delta; return l }

// body is the live reply as wrapped lines: its reasoning, the tools it started, then the text.
func (l residentLive) body(width int) []string {
	var out []string
	if l.reasoning != "" {
		if l.wrapThink == nil {
			l.wrapThink = &wrapCache{} // (a fixture without one; the live handler gives each turn its own)
		}
		out = append(out, l.wrapThink.wrap(l.reasoning, width, func(s string) string { return envThink.Render(s) })...)
	}
	for _, name := range l.tools {
		out = append(out, envDim.Render(g.collapsed+" ")+envTool.Render(g.tool+" "+residentText(name))+envDim.Render(" …"))
	}
	if l.text != "" {
		if l.wrapText == nil {
			l.wrapText = &wrapCache{}
		}
		out = append(out, l.wrapText.wrap(l.text, width, highlightLive)...)
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
	c := residentChat{ready: true, newGoal: true, page: -1}
	c.compose = textarea.New()
	// One prompt mark on the first line, an equal indent under it: the editor asks per display line, so a wrapped
	// draft keeps the column its text starts in.
	c.compose.SetPromptFunc(2, promptMark)
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
	m.chatStyles()
}

// promptMark is the composer's prompt: the glyph on the first line, nothing on the lines under it (the editor pads
// them to the same width, so the text stays in one column).
func promptMark(info textarea.PromptInfo) string {
	if info.LineNumber == 0 {
		return g.user + " "
	}
	return ""
}

// chatStyles gives the composer the palette's prompt and placeholder styles. A style keeps the colour it was built
// with, so a theme switch calls this again.
func (m *envModel) chatStyles() {
	s := m.chat.compose.Styles()
	s.Focused.Prompt, s.Blurred.Prompt = envBusy, envBusy
	s.Focused.Placeholder, s.Blurred.Placeholder = envDim, envDim
	m.chat.compose.SetStyles(s)
}

func (m envModel) selectedResidentGoal() *residentGoal {
	if m.goalSel < 0 || m.goalSel >= len(m.resident.Goals) {
		return nil
	}
	g := m.resident.Goals[m.goalSel]
	return &g
}

// maxResidentEvents bounds the events the panel keeps in memory for one conversation.
const maxResidentEvents = 2000

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
			if len(m.residentEvents) >= maxResidentEvents {
				// Bounded: a resident that runs for days must not grow the panel without limit. The oldest
				// events leave memory; the journal on the agent side keeps them.
				m.residentEvents = append(m.residentEvents[:0:0], m.residentEvents[len(m.residentEvents)-maxResidentEvents/2:]...)
			}
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
	// The composer's box is w-2 wide: four of those cells are its border and padding, so the editor gets w-6 and a
	// full line never eats the frame.
	m.chat.compose.SetWidth(w - 6)
	m.chat.compose.Placeholder = m.composerHint()
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
			m.chat.content += ansi.Hardwrap(block, w, true) + "\n\n"
		}
	}
	m.chat.rendered = len(m.residentEvents)
	if changed || m.chat.transcript.TotalLineCount() == 0 {
		body := strings.TrimRight(m.chat.content, "\n")
		if body == "" {
			body = mascotBeside(0, "", envTitle.Render("CAMINUS")+envDim.Render(" env"),
				"Your agent's conversation and commands will stay here.",
				envDim.Render("Type a goal below. /help shows the controls."))
		}
		m.chat.transcript.SetContent(body)
	}
	if oldBottom {
		m.chat.transcript.GotoBottom()
	}
}

// bodyHeight is the space between the header and the footer, without the composer.
func (m envModel) bodyHeight() int {
	// the header is two lines (title, rule); the footer one or two (chatFoot)
	return max(4, max(12, m.height)-2-lipgloss.Height(m.chatBottom())-lipgloss.Height(m.chatFoot(max(20, m.width))))
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
	spin := g.spinner[m.tickN%len(g.spinner)]
	who := g.agent + " agent"
	if l.reasoning != "" && l.text == "" {
		who = g.agent + " thinking"
	}
	hint := "  live · Ctrl+T hold · Alt+↑/↓ scroll"
	if l.hold {
		hint = fmt.Sprintf("  held · %d below · Ctrl+T follow", max(0, len(body)-(top+height)))
	}
	rows := []string{bold(cGreen).Render(who) + " " + envBusy.Render(spin) + envDim.Render(hint)}
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

// eventBlock draws one durable event in the shell's style (shell.go): the human's words and the agent's replies as
// labelled turns on a thin rule, every tool call and its result as a one-line card (Ctrl+E expands them all), root
// requests as the inline approval card. The result is final: syncChat caches it and never draws it again.
func (c *residentChat) eventBlock(e residentEvent) string {
	str := func(key string) string { s, _ := e.Data[key].(string); return s }
	w := max(20, c.renderWidth)
	turn := func(mark, label string, col color.Color, body string) string {
		return eventCard(bold(col).Render(mark+" "+label) + "\n" + body)
	}
	firstLine := func(s string) string {
		s = strings.TrimSpace(residentText(s))
		if i := strings.IndexByte(s, '\n'); i >= 0 {
			s = s[:i] + " …"
		}
		return s
	}
	lines := func(s string, limit int) []string {
		all := strings.Split(strings.TrimRight(strings.ReplaceAll(residentText(s), "\r\n", "\n"), "\n"), "\n")
		if len(all) > limit {
			all = append([]string{fmt.Sprintf("… %d earlier lines", len(all)-limit)}, all[len(all)-limit:]...)
		}
		return all
	}
	switch e.Kind {
	case "goal_queued":
		label := "you · goal " + shortGoal(e.Goal)
		if k, _ := e.Data["kind"].(string); k == "free" || k == "dream" {
			label = "free time · " + k // the resident's own hours: the invitation, not the human's words
			return toolCard(k, "stretch "+shortGoal(e.Goal)+" started", "", true, c.expanded, lines(str("prompt"), 30), w)
		}
		return turn(g.user, label, cBlue, c.renderMarkdown(str("prompt")))
	case "user_message", "peer_message":
		text := str("text")
		if text == "" {
			return ""
		}
		return turn(g.user, "you · steer "+shortGoal(e.Goal), cBlue, c.renderMarkdown(text))
	case "assistant":
		var parts []string
		// The reply's reasoning is shown once, above the answer, from the durable event: it is never
		// sent back to the model, so this block is display only.
		if text := str("reasoning"); text != "" {
			if c.expanded {
				parts = append(parts, eventCard(envThink.Render(g.expanded+" thinking")+"\n"+envThink.Render(c.renderMarkdown(text))))
			} else {
				parts = append(parts, envDim.Render(g.collapsed+" thinking · "+fmt.Sprintf("%d lines", strings.Count(text, "\n")+1)))
			}
		}
		if text := str("content"); text != "" {
			parts = append(parts, turn(g.agent, "agent", cGreen, c.renderMarkdown(text)))
		}
		calls, _ := e.Data["tool_calls"].([]any)
		for _, raw := range calls {
			call, _ := raw.(map[string]any)
			f, _ := call["function"].(map[string]any)
			name, _ := f["name"].(string)
			if name == "ask_user" || name == "read" || name == "edit" || name == "write" {
				continue
			} // the durable question gets a native UI below; a file tool's result card says what it did
			rawArgs, _ := f["arguments"].(string)
			var args map[string]any
			_ = json.Unmarshal([]byte(rawArgs), &args)
			if command, ok := args["command"].(string); ok {
				parts = append(parts, toolCard("terminal", firstLine(command), "", true, c.expanded, lines(command, 40), w))
				continue
			}
			summary := ""
			for _, key := range []string{"path", "question", "summary", "check", "evidence", "label", "id", "topic", "to", "until"} {
				if v, ok := args[key].(string); ok && v != "" {
					summary = firstLine(v)
					break
				}
			}
			var pretty any
			text := rawArgs
			if json.Unmarshal([]byte(rawArgs), &pretty) == nil {
				if b, err := json.MarshalIndent(pretty, "", "  "); err == nil {
					text = string(b)
				}
			}
			parts = append(parts, toolCard(residentText(name), summary, "", true, c.expanded, lines(text, 40), w))
		}
		return strings.Join(parts, "\n")
	case "tool_result":
		result, _ := e.Data["result"].(map[string]any)
		if str("name") == "ask_user" {
			answer, _ := result["answer"].(string)
			if answer == "" {
				answer, _ = result["decision"].(string)
			}
			return turn(g.user, "you · answer", cBlue, c.renderMarkdown(answer))
		}
		if name := str("name"); (name == "edit" || name == "write") && result["error"] == nil {
			// an edit is a diff card: open when it is short, folded (Ctrl+E) when it is long
			path, _ := result["path"].(string)
			added, _ := result["added"].(float64)
			removed, _ := result["removed"].(float64)
			meta := fmt.Sprintf("+%d −%d", int(added), int(removed))
			if created, _ := result["created"].(bool); created {
				note, _ := result["note"].(string)
				meta = note
			}
			diff, _ := result["diff"].(string)
			var body []string
			for _, ln := range strings.Split(residentText(diff), "\n") {
				if ln != "" && !strings.HasPrefix(ln, "@@") {
					body = append(body, ln)
				}
			}
			return toolCard("edit", residentText(path), meta, true, c.expanded || len(body) <= 12, body, w)
		}
		if str("name") == "read" && result["error"] == nil {
			path, _ := result["path"].(string)
			shown, _ := result["shown"].(string)
			meta := "lines " + shown
			if dir, _ := result["directory"].(bool); dir {
				meta = "directory"
			}
			return toolCard("read", residentText(path), meta, true, false, nil, w)
		}
		if err, ok := result["error"].(string); ok {
			// a failed call stays open: the reason is what the reader needs
			return toolCard("error", residentText(str("name")), firstLine(err), false, true, lines(err, 12), w)
		}
		if output, ok := result["output"].(string); ok {
			meta := fmt.Sprintf("%d lines", strings.Count(strings.TrimRight(output, "\n"), "\n")+1)
			if code, ok := result["exit_code"].(float64); ok {
				meta = fmt.Sprintf("exit %d · ", int(code)) + meta
			} else if running, _ := result["running"].(bool); running {
				meta = "running · " + meta
			}
			if strings.TrimSpace(output) == "" {
				meta = strings.TrimSuffix(meta, " · 1 lines") + " · no output"
			}
			if saved, _ := result["saved"].([]any); len(saved) > 0 {
				// a big output was moved to a file: the card says where, with the whole size
				if first, _ := saved[0].(map[string]any); first != nil {
					path, _ := first["path"].(string)
					lines, _ := first["lines"].(float64)
					meta = strings.Split(meta, " · ")[0] + fmt.Sprintf(" · %d lines → %s", int(lines), residentText(path))
				}
			}
			code, _ := result["exit_code"].(float64)
			return toolCard("output", residentText(str("name")), meta, code == 0, c.expanded, lines(output, 30), w)
		}
		if b, err := json.MarshalIndent(result, "", "  "); err == nil {
			return toolCard("result", residentText(str("name")), "", true, c.expanded, lines(string(b), 30), w)
		}
	case "finished":
		return turn(g.done, "finished", cGreen, c.renderMarkdown(str("summary")))
	case "approval_requested":
		return approvalCard("root", residentText(str("cmd")), residentText(str("reason")), w)
	case "question":
		return turn("?", "question", cYellow, c.renderMarkdown(str("text")))
	case "compacted":
		return toolCard("note", "context compacted", "the whole conversation stays in this transcript", true, false, nil, w)
	case "cancelled":
		return toolCard("note", "goal "+shortGoal(e.Goal)+" cancelled", "", true, false, nil, w)
	case "model_unavailable", "model_truncated", "context_refused", "model_refused", "model_gave_up":
		text := residentText(str("error"))
		body := lines(text, 12)
		if partial, ok := e.Data["partial"].(map[string]any); ok {
			if content, _ := partial["content"].(string); content != "" {
				// what the model produced before it stopped, then why it stopped
				body = append(lines(content, 20), "", text)
			}
		}
		return toolCard("error", strings.ReplaceAll(e.Kind, "_", " "), firstLine(text), false, true, body, w)
	case "judged":
		v := str("verdict")
		return toolCard("gate", "judge: "+strings.ReplaceAll(v, "_", " "), firstLine(str("reason")), v == "met" || v == "unread", v != "met", lines(str("reason"), 6), w)
	case "gate":
		ok, _ := e.Data["passed"].(bool)
		return toolCard("gate", "gate: "+firstLine(str("command")), map[bool]string{true: "passed", false: "failed"}[ok], ok, !ok, lines(str("output"), 12), w)
	case "report":
		if str("level") == "need_you" {
			return turn("!", "needs you", cRed, c.renderMarkdown(str("text")))
		}
		return toolCard("note", "fyi: "+firstLine(str("text")), "", true, c.expanded, lines(str("text"), 20), w)
	case "scheduled":
		at, _ := e.Data["wake_at"].(float64)
		return toolCard("note", "parked until "+time.Unix(int64(at), 0).Format("15:04"), firstLine(str("note")), true, false, nil, w)
	case "woke":
		return toolCard("note", "woke up", firstLine(str("note")), true, false, nil, w)
	case "brain":
		return toolCard("note", "now answering: "+residentText(str("model")), str("brain"), true, false, nil, w)
	}
	return ""
}

func shortGoal(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// chatComposer is where the human types: the shell's composer box (shell.go), its top border saying which goal the
// draft goes to, the editor's one prompt mark on the first line, and a dim hint that says what Enter does now. The
// Tab completion hint rides above the draft, inside the same box.
func (m envModel) chatComposer() string {
	if m.chat.question != nil && m.chat.questionArmed {
		return m.questionBox()
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
		} else if token, ok := pathToken(value); ok {
			// The path so far is rendered in the inline-code role, then what Tab
			// can offer: directories in the navigation accent, files resting, and
			// the absence of any match in the failure color so a typo shows itself.
			input = envDim.Render("Tab completes · ") + envCode.Render(token)
			if matches := pathMatches(token); len(matches) > 0 {
				lines := matches
				if len(lines) > 4 {
					lines = lines[:4]
				}
				for i, l := range lines {
					style := envDim
					if strings.HasSuffix(l, "/") {
						style = envTitle
					}
					if l == m.chat.pathPick && m.chat.pathIdx > 1 {
						style = envSel
					}
					lines[i] = style.Render(l)
				}
				input += envDim.Render(" · " + strings.Join(lines, "  "))
			} else {
				input += envCtxHot.Render(" no match")
			}
			input = envClip(input, max(18, m.width-6)) + "\n" + input
		}
	}
	return composerBox(input, m.composerTitle(), max(18, m.width-2))
}

// composerTitle is what the composer's top border says: which goal a message goes to, or that it starts a new one.
// The old label's other facts (sending, a waiting question, browsing) keep their place there, shortened to a title.
func (m envModel) composerTitle() string {
	title := "new goal"
	switch {
	case m.chat.sending:
		title = "sending…"
	case m.chat.question != nil:
		title = "question waiting · F2 answers"
	case !m.chat.newGoal:
		title = "to goal " + shortGoal(m.chat.target)
		if g := m.selectedResidentGoal(); g != nil && g.ID == m.chat.target && g.State == "completed" {
			title = "continue goal " + shortGoal(m.chat.target)
		}
	}
	if !m.chat.compose.Focused() && !m.chat.sending {
		title += " · browsing"
	}
	return title
}

// composerHint is the composer's placeholder: what Enter does with the draft now.
func (m envModel) composerHint() string {
	if !m.chat.compose.Focused() {
		return "browsing history · Enter returns to typing"
	}
	if m.chat.newGoal {
		return "type a goal · Enter starts it · /help"
	}
	return "message the agent · Enter nudges at the next step · /steer redirects"
}

// chatBottom renders whatever occupies the composer slot: the steering draft,
// the inline question or the Ctrl+P session switcher. syncChat sizes the
// transcript against its measured height.
func (m envModel) chatBottom() string {
	if box := m.promptBox(max(18, m.width-2)); box != "" {
		return box
	}
	if m.chat.picker {
		return m.pickerBox()
	}
	if line := m.todoPinned(); line != "" {
		return line + "\n" + m.chatComposer()
	}
	return m.chatComposer()
}

// todoPinned is the selected goal's todo list (its newest todo event) as one line above the composer, while the goal
// is open. bodyHeight measures chatBottom, so the line takes its row from the transcript.
func (m envModel) todoPinned() string {
	sel := m.selectedResidentGoal()
	if sel == nil || sel.State == "completed" || sel.State == "cancelled" {
		return ""
	}
	for i := len(m.residentEvents) - 1; i >= 0; i-- {
		e := m.residentEvents[i]
		if e.Goal != sel.ID || e.Kind != "todo" {
			continue
		}
		raw, _ := e.Data["items"].([]any)
		var items []todoItem
		for _, r := range raw {
			it, _ := r.(map[string]any)
			text, _ := it["text"].(string)
			state := map[string]string{"done": "done", "in_progress": "now"}[fmt.Sprint(it["state"])]
			if state == "" {
				state = "next"
			}
			if text != "" {
				items = append(items, todoItem{residentText(text), state})
			}
		}
		if len(items) == 0 {
			return ""
		}
		return todoLine(items, max(20, m.width))
	}
	return ""
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
// tps is the measured decode speed of the agent's last streamed reply; it
// only annotates a measured bar, and a count of 0 shows nothing at all.
func contextBar(used, capacity int, threshold, tps float64) (accent, rest string) {
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
	speed := ""
	if tps > 0 {
		speed = fmt.Sprintf(" · %.0f tok/s", tps)
	}
	return fmt.Sprintf("ctx %s %d%%", bar, int(share*100+0.5)),
		fmt.Sprintf(" of %s · compact %.0f%%", humanTokens(capacity), compactTrigger(threshold)*100) + speed
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
	steps, goalText := 0, ""
	if sel := m.selectedResidentGoal(); sel != nil {
		state = sel.State
		steps = sel.Steps
		goalText = strings.Join(strings.Fields(residentText(sel.Prompt)), " ")
		if lipgloss.Width(goalText) > 48 {
			goalText = ansi.Truncate(goalText, 47, "…")
		}
	}
	head := shellHeader(residentText(m.agentName), state, goalText, steps, w)
	foot := m.chatFoot(w)
	if m.chat.help {
		foot += "\n" + clip("/new [goal] · /goals · /goal ID · /cancel · /resume · /compact · /approvals") + "\n" + clip("/newsession NAME · /session NAME · /sessions · /approvals · /remember TEXT · /memory · /context · /audit · /settings") + "\n" + clip("/start · /stop · /theme NAME · /help · /quit · Esc browse · Ctrl+N new · Ctrl+L follow · Ctrl+A approvals · Ctrl+G goals")
	}
	if m.chat.approvals {
		// The overlay takes the transcript's and the composer's place; the header and the footer stay. The floor is
		// small so a short terminal (with /help's extra lines) still gets a box inside the frame.
		h := max(6, max(12, m.height)-2-lipgloss.Height(foot)+1)
		return head + "\n" + m.approvalOverlay(w, h) + "\n" + foot
	}
	if m.chat.goals {
		h := max(6, max(12, m.height)-2-lipgloss.Height(foot)+1)
		return head + "\n" + m.goalsOverlay(w, h) + "\n" + foot
	}
	if m.chat.models.open {
		h := max(6, max(12, m.height)-2-lipgloss.Height(foot)+1)
		return head + "\n" + m.modelsOverlay(w, h) + "\n" + foot
	}
	if m.chat.page >= 0 {
		// a typed prompt opened from the page (a new note) sits under the overlay
		prompt := m.promptBox(max(18, w-2))
		h := max(6, max(12, m.height)-2-lipgloss.Height(foot)-lipgloss.Height(prompt)+1)
		if prompt != "" {
			return head + "\n" + m.pageOverlay(w, h) + "\n" + prompt + "\n" + foot
		}
		return head + "\n" + m.pageOverlay(w, h) + "\n" + foot
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
	} else if !m.chat.sending && !m.chat.approvals && !m.chat.goals && !m.chat.models.open {
		// (the approvals overlay owns the keyboard; a paste must not fill the draft behind it)
		m.chat.compose, cmd = m.chat.compose.Update(msg)
	}
	return m, cmd
}

func (m envModel) chatKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	m.initChat()
	if m.chat.approvals {
		return m.approvalKey(msg)
	}
	if m.chat.goals {
		return m.goalsKey(msg)
	}
	if m.chat.models.open {
		return m.modelsKey(msg)
	}
	if m.chat.page >= 0 {
		return m.pageKey(msg)
	}
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
	case "ctrl+g":
		m.chat.goals, m.chat.goalsSel, m.chat.goalsDetail = true, 0, false
		m.chat.compose.Blur()
		return m, nil
	case "ctrl+a":
		// The Approvals overlay over the conversation: the requests a person has to decide, in the one box every
		// overlay uses. It owns the keys while it is open, so the composer lets go of them.
		m.chat.approvals, m.chat.approvalSel, m.chat.approvalDetail = true, 0, false
		m.chat.armed = ""
		m.chat.compose.Blur()
		return m, nil
	case "tab", "shift+tab":
		if next, done := m.cycleSlash(); done {
			return next, nil
		}
		if next, done := m.cyclePath(); done {
			return next, nil
		}
		return m, nil // the chat is the only screen: Tab completes and does nothing else
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
	case "ctrl+e":
		// open or fold every card; drawn blocks are final, so the transcript is drawn again once
		m.chat.expanded = !m.chat.expanded
		m.chat.content, m.chat.rendered = "", 0
		m.msg = map[bool]string{true: "cards open · Ctrl+E folds", false: "cards folded · Ctrl+E opens"}[m.chat.expanded]
		m.syncChat()
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
		// A pending root request is decided right here, through the Requests page's own handler (the same sudo
		// run, claim, audit entry and answer files): a approves, d denies, e opens it in the Requests editor.
		// Approving runs a root command, so it takes two presses: the first a arms it and shows the exact
		// command, a second a within 10 s approves. Any other key disarms. Denying runs nothing and needs one.
		armed := m.chat.armed
		m.chat.armed = ""
		if i := m.pendingRoot(); i >= 0 && (k == "a" || k == "d" || k == "e") {
			r := m.reqs[i]
			if k == "a" && (armed != r.ID || time.Since(m.chat.armedAt) > 10*time.Second) {
				m.chat.armed, m.chat.armedAt = r.ID, time.Now()
				m.msg = "approve as root: " + terminalText(r.Cmd, false) + " ? press a again (any other key cancels)"
				return m, nil
			}
			m.sel = i
			return m.reqKeys(k) // e opens the edit prompt in the composer's slot
		}
		if armed != "" {
			m.msg = "approval cancelled"
		}
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
		m.chat.approvals, m.chat.approvalSel, m.chat.approvalDetail = true, 0, false
		m.chat.armed = ""
		m.chat.compose.Blur()
	case "/approvals":
		// The overlay over the conversation; /requests still opens the page that edits and answers.
		clear()
		m.chat.approvals, m.chat.approvalSel, m.chat.approvalDetail = true, 0, false
		m.chat.armed = ""
		m.chat.compose.Blur()
	case "/memory", "/context", "/audit", "/settings":
		clear()
		return m.openPage(pageOverlays[command].tab)
	case "/sessions":
		clear()
		m.chat.picker, m.chat.pickerQuery, m.chat.pickerSel = true, "", 0
		m.chat.compose.Blur()
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
		m.chat.goals, m.chat.goalsSel, m.chat.goalsDetail = true, 0, false
		m.chat.compose.Blur()
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
	case "/model", "/models":
		// alone: the Models overlay over the catalog; with an id (provider/id when two serve it): pick it
		clear()
		if id := strings.TrimSpace(arg); id != "" && command == "/model" {
			m.msg = "switching to " + id + "…"
			return m, pickModel(m, id)
		}
		m.chat.models.open, m.chat.models.loading, m.chat.models.err = true, true, ""
		return m, fetchModels(m, false)
	case "/effort":
		clear()
		return m, setEffort(m, strings.TrimSpace(arg))
	case "/update":
		clear()
		m.msg = "uploading the runtime…"
		return m, updateResident(m, strings.TrimSpace(arg) == "now")
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
	case "/steer":
		// A steer redirects: it supersedes conflicting instructions and voids the plan. A plain message in the
		// composer is a nudge, which keeps the plan, so the heavy one has to be asked for.
		text := strings.TrimSpace(arg)
		if text == "" || m.chat.target == "" {
			m.msg = "usage: /steer TEXT — redirects the goal; a plain message nudges it"
			return m, nil
		}
		clear()
		return m, residentAction(m, []string{"send", m.agentName, "--goal-id", m.chat.target,
			"--steer", "--stdin", "--json"}, text)
	case "/theme":
		// The panel's colors; the transcript's markdown is rebuilt below to pick
		// up the new roles. Nothing else in the session changes.
		name := strings.TrimSpace(arg)
		if name == "" {
			m.msg = "themes: " + strings.Join(themeNames(), ", ") + " · /theme NAME"
			return m, nil
		}
		if err := themeSet(name); err != nil {
			m.msg = "no such theme: " + name + " · /theme lists"
			return m, nil
		}
		m.chat.markdown = nil
		m.chatStyles() // the composer's prompt and hint keep the palette's colours
		m.msg = "theme: " + name
		m.syncChat()
		return m, nil
	case "/deny":
		// "No, do this instead": deny the newest root request and send the text to its goal as a steer.
		i := m.pendingRoot()
		if i < 0 {
			m.msg = "no root request is waiting"
			return m, nil
		}
		text := strings.TrimSpace(arg)
		if text == "" {
			m.msg = "usage: /deny TEXT — denies the root request and tells the agent what to do instead"
			return m, nil
		}
		r := m.reqs[i]
		m.sel = i
		next, _ := m.reqKeys("d")
		m = next.(envModel)
		clear()
		goal := r.Goal
		if goal == "" {
			goal = m.chat.target
		}
		return m, residentAction(m, []string{"send", m.agentName, "--goal-id", goal, "--steer", "--stdin", "--json"},
			"Your root request was denied. Instead: "+text)
	case "/glyphs":
		// The symbol set (shell.go): nerd needs a Nerd Font; unicode needs none; ascii is for anything else.
		name := strings.TrimSpace(arg)
		if name == "" {
			name = glyphsNext()
		}
		if err := glyphsSet(name); err != nil {
			m.msg = err.Error()
			return m, nil
		}
		m.msg = "glyphs: " + name + "  " + g.working + " " + g.waiting + " " + g.done + " " + g.root + " " + g.model + " " + g.queue +
			"  (boxes? pick /glyphs unicode)"
		m.syncChat()
		return m, nil
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
	{"/approvals", "open the approvals overlay (Ctrl+A)"},
	{"/cancel", "cancel the selected goal"},
	{"/compact", "summarize this goal's history into memory now"},
	{"/effort", "reasoning depth LEVEL among the model's; alone, its levels"},
	{"/model", "pick the brain: the models overlay; or /model ID"},
	{"/update", "install this caminus's runtime between goals (now: at the next turn)"},
	{"/goal", "select a goal by ID prefix"},
	{"/goals", "the goals overlay (Ctrl+G)"},
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
	{"/steer", "redirect this goal with TEXT (a plain message nudges)"},
	{"/stop", "stop this session's loop"},
	{"/theme", "switch panel colors NAME; /theme lists"},
	{"/glyphs", "symbols: nerd, unicode or ascii; alone, the next one"},
	{"/deny", "deny the waiting root request and tell the agent what to do instead"},
	{"/memory", "memory and notes"},
	{"/context", "what the agent's context holds"},
	{"/audit", "the audit log"},
	{"/settings", "this session's settings"},
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

// pathToken reports the trailing token of the draft when it is a path the user
// is still typing: it contains a separator (/, ~/ , ./, ../) and the draft does
// not end in whitespace, so a finished word never matches. A leading / alone is
// left to slashMatches, which sees commands first; a path wins once the token
// goes deeper than a bare command could (/home) or does not start with / at all.
func pathToken(draft string) (string, bool) {
	if draft == "" || strings.Contains(draft, "\n") || strings.HasSuffix(draft, " ") {
		return "", false
	}
	token := draft
	if i := strings.LastIndex(draft, " "); i >= 0 {
		token = draft[i+1:]
	}
	if !strings.Contains(token, "/") {
		return "", false
	}
	if strings.HasPrefix(token, "/") && !strings.HasPrefix(token, "//") {
		if rest := token[1:]; !strings.Contains(rest, "/") {
			// A bare /word still belongs to the command completer until no
			// command could grow into it; /h stays /help, /home is a path.
			if isCommandPrefix(rest) {
				return "", false
			}
		}
	}
	return token, true
}

// isCommandPrefix reports whether the word after the leading slash could still
// complete to any slash command.
func isCommandPrefix(token string) bool {
	for _, c := range slashCommands {
		if strings.HasPrefix(c.name[1:], token) {
			return true
		}
	}
	return false
}

// pathMatches lists what the typed path points at, directories first with
// their trailing separator, so Tab keeps descending into a directory and the
// same token prefix re-lists its children. Hidden entries only surface once
// the typed prefix includes their dot. The list is capped so /usr/lib never
// floods the hint line.
func pathMatches(token string) []string {
	dir, prefix := token, ""
	if !strings.HasSuffix(token, "/") {
		i := strings.LastIndex(token, "/")
		prefix = token[i+1:]
		dir = token[:i+1]
	}
	if dir == "~" {
		dir = "~/"
	}
	fsDir := dir
	if strings.HasPrefix(fsDir, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil
		}
		fsDir = home + fsDir[1:]
	}
	entries, err := os.ReadDir(fsDir)
	if err != nil {
		return nil
	}
	var dirs, files []string
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		if strings.HasPrefix(name, ".") && !strings.HasPrefix(prefix, ".") {
			continue
		}
		if e.IsDir() {
			dirs = append(dirs, dir+name+"/")
		} else {
			files = append(files, dir+name)
		}
	}
	out := append(dirs, files...)
	if len(out) > 64 {
		out = out[:64]
	}
	return out
}

// cyclePath completes the trailing path token on Tab. The first Tab takes the
// best match; later Tabs cycle the rest of that list; taking a directory puts
// its separator on the draft, so the next Tab walks into it. A token that
// already names exactly one thing leaves Tab free. picks state (pathPrefix,
// pathPick, pathIdx) survives only while the token is untouched.
func (m envModel) cyclePath() (tea.Model, bool) {
	value := m.chat.compose.Value()
	token, ok := pathToken(value)
	if !ok {
		return m, false
	}
	prefix, idx := token, 0
	switch {
	case token == m.chat.pathPick && strings.HasSuffix(token, "/"):
		prefix, idx = token, 0
		m.chat.pathIdx = 0
	case token == m.chat.pathPrefix || token == m.chat.pathPick:
		// pathIdx already points at the next pick of the running cycle.
		prefix, idx = m.chat.pathPrefix, m.chat.pathIdx
	}
	matches := pathMatches(prefix)
	if len(matches) == 0 {
		return m, false
	}
	if len(matches) == 1 && matches[0] == token {
		return m, false
	}
	pick := matches[idx%len(matches)]
	m.chat.pathIdx = idx + 1
	m.chat.pathPrefix = prefix
	m.chat.pathPick = pick
	m.chat.compose.SetValue(value[:len(value)-len(token)] + pick)
	return m, true
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

// chatFoot is the status line (shell.go) and, when something is only sometimes true, a dim line under it; bodyHeight
// measures it so the frame never outgrows the terminal.
func (m envModel) chatFoot(w int) string {
	clip := func(s string) string { return ansi.Truncate(s, w, "") }
	queued := 0
	for _, gl := range m.resident.Goals {
		if gl.State == "queued" {
			queued++
		}
	}
	// The status line: brain, context, speed and queue, then the keys that matter now (shell.go). Everything that is
	// only sometimes true (scroll position, the live hold, history loading, a message) rides on the line under it.
	keys := []string{"esc browse", "^P sessions", "/help"}
	if len(m.reqs) > 0 {
		keys = append([]string{fmt.Sprintf("%s %d pending ^A", g.root, len(m.reqs))}, keys...)
	}
	statusBar := statusLine(status{brain: residentText(m.resident.Model), used: m.resident.ContextUsed,
		window: m.resident.Context, compact: m.resident.CompactThreshold, usd: -1, tps: m.resident.TokPerS,
		queue: queued, keys: keys}, w)
	status := []string{}
	if !m.sshOK {
		status = append(status, envWarn.Render("SSH …"))
	}
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
	foot := statusBar
	if len(status) > 0 {
		foot += "\n" + clip(strings.Join(status, envDim.Render(" · ")))
	}
	return foot
}

// pendingRoot is the index in m.reqs of the newest root request from this resident still waiting, or -1.
func (m envModel) pendingRoot() int {
	for i := len(m.reqs) - 1; i >= 0; i-- {
		r := m.reqs[i]
		if r.kind() == "root" && (r.Agent == "" || r.Agent == m.agentName) {
			return i
		}
	}
	return -1
}
