// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 jdominick05

package main

import (
	"fmt"
	"image/color"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"charm.land/glamour/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"io/fs"
)

// chatFixture isolates the panel's user-state writes (last-session file) in a
// temporary state dir; a test or benchmark must never touch the human's state.
func chatFixture(tb testing.TB) envModel {
	tb.Helper()
	tb.Setenv("XDG_STATE_HOME", tb.TempDir())
	m := envModel{agentName: "resident", tab: 6, width: 90, height: 28, mode: "normal"}
	m.initChat()
	m.receiveResident(residentMsg{agent: "resident", status: residentStatus{Name: "resident", Model: "local-model", Context: 1048574, DaemonAlive: true, LastEvent: 1, Goals: []residentGoal{{ID: "original", State: "working", Prompt: "a goal"}}}, events: []residentEvent{{Seq: 1, Goal: "original", Kind: "goal_queued", Data: map[string]any{"prompt": "a goal"}}}})
	return m
}

func TestChatRetainsTranscriptAndScrollAnchorOnRefresh(t *testing.T) {
	m := chatFixture(t)
	for i := 2; i < 32; i++ {
		m.receiveResident(residentMsg{agent: "resident", status: m.resident, events: []residentEvent{{Seq: i, Goal: "original", Kind: "assistant", Data: map[string]any{"content": fmt.Sprintf("Observation %d stays in the transcript.", i)}}}})
	}
	m.chat.transcript.GotoTop()
	before := ansi.Strip(m.chat.transcript.View())
	m.chat.compose.SetValue("a draft survives")
	m.receiveResident(residentMsg{agent: "resident", status: m.resident, events: []residentEvent{{Seq: 32, Goal: "original", Kind: "assistant", Data: map[string]any{"content": "new activity"}}}})
	if m.chat.transcript.YOffset() != 0 || ansi.Strip(m.chat.transcript.View()) != before {
		t.Fatal("new activity moved the reader")
	}
	plain := ansi.Strip(m.chat.content)
	if !strings.Contains(plain, "Observation 2 stays") || !strings.Contains(plain, "new activity") {
		t.Fatal("old or new transcript disappeared")
	}
	if m.chat.compose.Value() != "a draft survives" {
		t.Fatal("poll replaced input")
	}
	m.chat.transcript.GotoBottom()
	m.receiveResident(residentMsg{agent: "resident", status: m.resident, events: []residentEvent{{Seq: 33, Goal: "original", Kind: "assistant", Data: map[string]any{"content": "latest activity"}}}})
	if !m.chat.transcript.AtBottom() || !strings.Contains(ansi.Strip(m.chat.transcript.View()), "latest activity") {
		t.Fatal("following the live tail failed")
	}
}

func TestChatComposerCapturesTargetAndKeepsFailedDraft(t *testing.T) {
	m := chatFixture(t)
	m.chat.compose.SetValue("Use existing tools.\nKeep café and 漢字 intact.")
	m.receiveResident(residentMsg{agent: "resident", status: residentStatus{LastEvent: 2, Goals: []residentGoal{{ID: "other", State: "working"}, {ID: "original", State: "working"}}}})
	if m.chat.target != "original" {
		t.Fatal("poll redirected steering")
	}
	next, _ := m.finishCompose(residentComposeResult{agent: "resident", err: "connection failed"})
	m = next.(envModel)
	if !strings.Contains(m.chat.compose.Value(), "café") {
		t.Fatal("failed delivery lost Unicode draft")
	}
	next, _ = m.chatKey(tea.KeyPressMsg{Code: tea.KeyEscape})
	m = next.(envModel)
	if m.chat.compose.Focused() || m.chat.compose.Value() == "" {
		t.Fatal("Escape should browse without deleting the draft")
	}
	next, _ = m.chatKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = next.(envModel)
	if !m.chat.compose.Focused() {
		t.Fatal("Enter should resume editing")
	}
}

func TestChatSlashCommandsAndNewGoalRemainExplicit(t *testing.T) {
	m := chatFixture(t)
	m.chat.compose.SetValue("/new")
	next, cmd := m.submitCompose()
	m = next.(envModel)
	if cmd != nil || !m.chat.newGoal {
		t.Fatal("/new should prepare input, not submit")
	}
	m.receiveResident(residentMsg{agent: "resident", status: m.resident})
	if !m.chat.newGoal || m.chat.target != "" {
		t.Fatal("poll changed an explicit new goal into steering")
	}
	m.chat.compose.SetValue("/requests")
	next, _ = m.submitCompose()
	m = next.(envModel)
	if m.tab != 6 || !m.chat.approvals {
		t.Fatal("/requests did not open the approvals overlay in the chat")
	}
	m.chat.approvals = false
	m.chat.compose.SetValue("/unknown keep this")
	next, cmd = m.submitCompose()
	m = next.(envModel)
	if cmd != nil || m.chat.compose.Value() != "/unknown keep this" {
		t.Fatal("unknown slash command was sent or lost")
	}
}

func TestChatPasteAndFrameStayWithinTerminal(t *testing.T) {
	for _, size := range [][2]int{{40, 18}, {80, 24}, {140, 40}} {
		m := chatFixture(t)
		m.width, m.height = size[0], size[1]
		text := "a Unicode café draft\nsecond line\nthird line\nfourth line"
		next, _ := m.Update(tea.PasteMsg{Content: text})
		m = next.(envModel)
		if m.chat.compose.Value() != text {
			t.Fatalf("paste changed: %q", m.chat.compose.Value())
		}
		frame := m.render()
		rows := strings.Split(frame, "\n")
		if len(rows) > size[1] {
			t.Fatalf("%v: %d lines overflow terminal", size, len(rows))
		}
		for _, line := range rows {
			if ansi.StringWidth(line) > size[0] {
				t.Fatalf("%v: line too wide: %q", size, ansi.Strip(line))
			}
		}
		if !strings.Contains(ansi.Strip(frame), "to goal original") {
			t.Fatal("bottom composer missing its target")
		}
	}
}

// The composer is the shell's: the target rides on its top border (the old label line, moved), one prompt mark sits on
// the first line only, the hint says what Enter does now, and the frame still fits the terminal at both extremes.
func TestComposerCarriesItsTargetOnTheBorder(t *testing.T) {
	for _, size := range [][2]int{{40, 16}, {100, 30}} {
		m := chatFixture(t) // the selected goal "original" is working
		m.width, m.height = size[0], size[1]
		m.syncChat()
		frame := ansi.Strip(m.render())
		if !strings.Contains(frame, "─ to goal original ─") {
			t.Fatalf("%v: the composer's border does not name its goal:\n%s", size, frame)
		}
		box := ansi.Strip(m.chatComposer())
		if n := strings.Count(box, g.user); n != 1 {
			t.Fatalf("%v: the composer drew %d prompt marks, want one:\n%s", size, n, box)
		}
		if !strings.Contains(box, "message the agent") || !strings.Contains(box, "Enter nudges") || !strings.Contains(box, "/steer") {
			t.Fatalf("%v: the hint does not say what Enter does:\n%s", size, box)
		}
		m.chat.compose.SetValue("first line\nsecond line")
		box = ansi.Strip(m.chatComposer())
		if n := strings.Count(box, g.user); n != 1 {
			t.Fatalf("%v: a multi-line draft drew %d prompt marks:\n%s", size, n, box)
		}
		if !strings.Contains(box, "  second line") {
			t.Fatalf("%v: the continuation line lost its indent:\n%s", size, box)
		}
		m.chat.newGoal, m.chat.target = true, ""
		m.chat.compose.Reset()
		m.syncChat()
		box = ansi.Strip(m.chatComposer())
		if !strings.Contains(box, "─ new goal ─") || !strings.Contains(box, "type a goal") || !strings.Contains(box, "Enter starts it") {
			t.Fatalf("%v: a new goal does not say what Enter does:\n%s", size, box)
		}
		frame = ansi.Strip(m.render())
		rows := strings.Split(frame, "\n")
		if len(rows) > size[1] {
			t.Fatalf("%v: %d rows overflow the terminal", size, len(rows))
		}
		for _, line := range rows {
			if ansi.StringWidth(line) > size[0] {
				t.Fatalf("%v: line too wide: %q", size, line)
			}
		}
	}
}

func TestChatDisplaysCommandArgumentsAndEscapesControls(t *testing.T) {
	m := chatFixture(t)
	e := residentEvent{Kind: "assistant", Data: map[string]any{"tool_calls": []any{map[string]any{"function": map[string]any{"name": "terminal", "arguments": "{\"command\":\"printf hello #\\u001b[2K\\rhidden\"}"}}}}}
	block := m.chat.eventBlock(e)
	plain := ansi.Strip(block)
	for _, want := range []string{g.tool + " terminal", "printf", `\x1b`, "hidden"} {
		if !strings.Contains(plain, want) {
			t.Fatalf("missing %q in %q", want, plain)
		}
	}
	if strings.Contains(plain, "\x1b") || strings.Contains(plain, "\r") {
		t.Fatal("untrusted terminal controls survived rendering")
	}
	if !strings.Contains(block, "\x1b[") {
		t.Fatal("markdown/code rendering produced no highlighting")
	}
}

func TestChatLoadsHistoryAcrossMoreThanOnePageAndRejectsOldAgent(t *testing.T) {
	dir := t.TempDir()
	cli := filepath.Join(dir, "fake-cli")
	script := "#!/usr/bin/env python3\nimport sys,json\nif sys.argv[3]=='status': print(json.dumps(dict(name='resident',last_event=1201,goals=[])))\nelse:\n after=int(sys.argv[6]); print(json.dumps([dict(seq=i,goal='g',kind='assistant',data=dict(content=str(i))) for i in range(after+1,min(1202,after+201))]))\n"
	if err := os.WriteFile(cli, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	m := envModel{cli: []string{cli}, agentName: "resident", width: 80, height: 24}
	m.initChat()
	m.receiveResident(residentRefresh(m)().(residentMsg))
	if m.chat.seq != 800 {
		t.Fatalf("bounded catch-up cursor=%d", m.chat.seq)
	}
	m.receiveResident(residentRefresh(m)().(residentMsg))
	if m.chat.seq != 1201 || len(m.residentEvents) != 1201 {
		t.Fatalf("history missing: cursor=%d count=%d", m.chat.seq, len(m.residentEvents))
	}
	next, _ := m.Update(residentMsg{agent: "another", status: residentStatus{LastEvent: 9000}})
	if next.(envModel).resident.LastEvent != 1201 {
		t.Fatal("old session reply contaminated current session")
	}
}

func TestSessionHomeOpensChatAndKeepsFiveMainPages(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	m := envModel{agentName: "resident", tab: 7, sessionNames: []string{"resident", "research"}, sessionSel: 1, width: 90, height: 28}
	next, _ := m.sessionKeys("enter")
	m = next.(envModel)
	if m.tab != 6 || m.agentName != "research" || !m.chat.compose.Focused() {
		t.Fatal("session did not open into composer")
	}
	if len(envNavigation) != 5 || nextEnvTab(7, 1) != 5 || nextEnvTab(6, 1) != 7 {
		t.Fatal("navigation should start at sessions with five pages")
	}
}

func TestChatRejectsResponsesFromAnEarlierVisitToTheSameSession(t *testing.T) {
	m := chatFixture(t)
	m.openResident("other")
	m.openResident("resident")
	next, _ := m.Update(residentMsg{agent: "resident", epoch: 0, status: residentStatus{LastEvent: 900}, events: []residentEvent{{Seq: 900}}})
	m = next.(envModel)
	if m.chat.seq != 0 || len(m.residentEvents) != 0 {
		t.Fatal("old visit skipped initial history pages")
	}
	m.chat.target, m.chat.selectID, m.chat.newGoal = "new-id", "new-id", false
	m.receiveResident(residentMsg{agent: "resident", epoch: m.residentEpoch, status: residentStatus{Goals: []residentGoal{{ID: "old", State: "completed"}}}})
	if m.chat.target != "new-id" || m.chat.newGoal {
		t.Fatal("stale status redirected a just-submitted goal")
	}
}

func TestMainPagesScrollInsideTheTerminal(t *testing.T) {
	// The old audit page is an overlay over the chat now: it fits the terminal and Page Down scrolls it.
	m := chatFixture(t)
	m.width, m.height = 80, 24
	next, _ := m.chatSlash("/audit") // (opening reads the audit file; the fixture fills the lines after)
	m = next.(envModel)
	for i := 0; i < 90; i++ {
		m.events = append(m.events, fmt.Sprintf("audit entry %d with a long but readable message", i))
	}
	frame := m.render()
	if m.tab != 6 || m.chat.page != 5 || !strings.Contains(ansi.Strip(frame), "AUDIT") {
		t.Fatalf("/audit did not open the overlay:\n%s", ansi.Strip(frame))
	}
	if len(strings.Split(frame, "\n")) > m.height {
		t.Fatal("audit overlay exceeded terminal height")
	}
	for _, line := range strings.Split(frame, "\n") {
		if ansi.StringWidth(line) > m.width {
			t.Fatal("audit overlay exceeded terminal width")
		}
	}
	next, _ = m.chatKey(tea.KeyPressMsg{Code: tea.KeyPgDown})
	m = next.(envModel)
	if m.chat.pageTop == 0 || ansi.Strip(m.render()) == ansi.Strip(frame) {
		t.Fatal("Page Down did not scroll the audit overlay")
	}
	if _, cmd := m.chatKey(tea.KeyPressMsg{Code: 'q', Text: "q"}); cmd != nil {
		t.Fatal("q must not quit inside an overlay")
	}
	next, _ = m.chatKey(tea.KeyPressMsg{Code: tea.KeyEscape})
	if next.(envModel).chat.page != -1 {
		t.Fatal("Esc must close the overlay")
	}
}

func TestHistoryWrapPreservesLongUnicodeOutputAndReflowsOnResize(t *testing.T) {
	m := chatFixture(t)
	text := "start_" + strings.Repeat("A", 160) + strings.Repeat("界🙂", 40) + "_end"
	m.chat.expanded = true // output is folded by default; an open card must keep every character
	m.receiveResident(residentMsg{agent: "resident", status: m.resident, events: []residentEvent{{Seq: 2, Goal: "original", Kind: "tool_result", Data: map[string]any{"name": "terminal", "result": map[string]any{"output": text}}}}})
	for _, width := range []int{90, 40, 120} {
		m.width = width
		m.syncChat()
		plain := ansi.Strip(m.chat.content)
		if !utf8.ValidString(plain) {
			t.Fatal("wrapping split a UTF-8 character")
		}
		// The card rule sits at the start of every wrapped line, so drop it before asking whether the
		// unbroken run survived: the invariant is that no character is lost or split, not that the gutter
		// is invisible.
		if !strings.Contains(strings.ReplaceAll(strings.Join(strings.Fields(plain), ""), "│", ""), text) {
			t.Fatal("long output disappeared during wrapping/resize")
		}
		for _, line := range strings.Split(m.chat.content, "\n") {
			if ansi.StringWidth(line) > width {
				t.Fatalf("history line exceeds %d cells", width)
			}
		}
		m.chat.compose.SetValue("draft")
		before := m.chat.content
		next, _ := m.Update(tea.KeyPressMsg{Code: 'x', Text: "x"})
		m = next.(envModel)
		_ = m.View()
		if m.chat.content != before || m.chat.compose.Value() != "draftx" {
			t.Fatal("typing replaced history or lost input")
		}
	}
}

func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

func TestSlashAutocompleteMatchesAndTabCycles(t *testing.T) {
	if got := slashMatches("/se"); fmt.Sprint(sortedCopy(got)) != "[/session /sessions /settings]" {
		t.Fatalf("unexpected matches: %v", got)
	}
	if got := slashMatches("/st"); fmt.Sprint(sortedCopy(got)) != "[/start /steer /stop]" {
		t.Fatalf("unexpected matches: %v", got)
	}
	if got := slashMatches("/s"); len(got) != 6 || got[0] != "/session" || got[5] != "/settings" {
		t.Fatalf("unexpected matches: %v", got)
	}
	for _, draft := range []string{"", "hello", "/remember queue pace", "/new\nsome goal"} {
		if got := slashMatches(draft); got != nil {
			t.Fatalf("unexpected matches for %q: %v", draft, got)
		}
	}
	for _, c := range slashCommands {
		if !strings.Contains("/help /quit /new /requests /approvals /sessions /newsession /remember /goals /goal /agent /session /start /stop /cancel /compact /resume /theme /glyphs /deny /steer /memory /context /audit /settings /", c.name+" ") {
			t.Fatalf("autocomplete offers %q, which chatSlash does not handle", c.name)
		}
	}
	m := chatFixture(t)
	m.chat.compose.SetValue("/s")
	next, done := m.cycleSlash()
	m = next.(envModel)
	if !done || m.chat.compose.Value() != "/session " {
		t.Fatalf("first Tab gave %q", m.chat.compose.Value())
	}
	next, done = m.cycleSlash()
	m = next.(envModel)
	if !done || m.chat.compose.Value() != "/sessions " {
		t.Fatalf("second Tab gave %q", m.chat.compose.Value())
	}
	next, done = m.cycleSlash()
	m = next.(envModel)
	if !done || m.chat.compose.Value() != "/start " {
		t.Fatalf("cycle skipped a longer command: %q", m.chat.compose.Value())
	}
	m.chat.compose.SetValue("/new steer me")
	next, done = m.cycleSlash()
	if done {
		t.Fatal("Tab completed an argument draft")
	}
	if m.chat.compose.Value() != "/new steer me" {
		t.Fatal("cycle changed the draft")
	}
}

func TestQuickSwitcherOpensRecordsAndFiltersSessions(t *testing.T) {
	m := chatFixture(t)
	m.sessionNames = []string{"research", "resident"}
	m.chat.compose.SetValue("an unsent draft")
	next, _ := m.chatKey(tea.KeyPressMsg{Code: 'p', Mod: tea.ModCtrl})
	m = next.(envModel)
	if !m.chat.picker || m.chat.pickerQuery != "" {
		t.Fatal("Ctrl+P did not open the switcher")
	}
	if m.chat.compose.Value() != "an unsent draft" {
		t.Fatal("the switcher destroyed the draft")
	}
	next, _ = m.chatKey(tea.KeyPressMsg{Code: 'r', Text: "r"})
	m = next.(envModel)
	next, _ = m.chatKey(tea.KeyPressMsg{Code: 'c', Text: "c"})
	m = next.(envModel)
	next, _ = m.chatKey(tea.KeyPressMsg{Code: 'h', Text: "h"})
	m = next.(envModel)
	if names := m.pickerNames(); len(names) != 1 || names[0] != "research" {
		t.Fatalf("filter failed: %v", names)
	}
	next, _ = m.chatKey(tea.KeyPressMsg{Code: tea.KeyEscape})
	m = next.(envModel)
	if m.chat.picker || !m.chat.compose.Focused() || m.chat.compose.Value() != "an unsent draft" {
		t.Fatal("Escape did not restore the composer")
	}
	next, _ = m.chatKey(tea.KeyPressMsg{Code: 'p', Mod: tea.ModCtrl})
	m = next.(envModel)
	next, _ = m.chatKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = next.(envModel)
	if m.agentName != "research" || m.chat.picker || m.chat.compose.Value() != "" {
		t.Fatalf("Enter did not open the selected session: %q", m.agentName)
	}
	// A question arriving with the switcher open keeps F2 as the explicit takeover.
	m2 := chatFixture(t)
	m2.sessionNames = []string{"resident"}
	m2.chat.picker = true
	wrong := envReq{ID: "q1", Kind: "question"}
	m2.chat.question = &wrong
	next, _ = m2.chatKey(tea.KeyPressMsg{Code: tea.KeyF2})
	m2 = next.(envModel)
	if m2.chat.picker || !m2.chat.questionArmed || m2.chat.compose.Focused() {
		t.Fatal("F2 with the switcher open did not take over the question")
	}
}

func TestLaunchReopensLastSessionAndFallsBackWhenGone(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", dir)
	saveLastSession("research")
	if got := loadLastSession(); got != "research" {
		t.Fatalf("round trip lost the session: %q", got)
	}
	saveLastSession("\x01bad\nname")
	if got := loadLastSession(); got != "" {
		t.Fatalf("unsanitary name loaded: %q", got)
	}
	m := envModel{agentName: "resident", defaultAgent: "resident", tab: 7, width: 90, height: 28}
	m.initChat()
	saveLastSession("research")
	openLastSession(&m)
	if m.agentName != "research" || m.tab != 6 || m.pendingLast != "research" {
		t.Fatal("launch did not reopen the last session")
	}
	next, _ := m.Update(residentSessionsMsg{agent: "research", names: []string{"resident", "research"}})
	m = next.(envModel)
	if m.tab != 6 || m.pendingLast != "" || m.agentName != "research" {
		t.Fatal("an existing last session was rejected")
	}
	m2 := envModel{agentName: "resident", defaultAgent: "resident", tab: 7, width: 90, height: 28}
	m2.initChat()
	saveLastSession("ghost")
	openLastSession(&m2)
	next, _ = m2.Update(residentSessionsMsg{agent: "ghost", names: []string{"resident"}})
	m2 = next.(envModel)
	if m2.tab != 6 || !m2.chat.picker || m2.agentName != "resident" || !m2.chat.ready {
		t.Fatal("a vanished session did not fall back to the chat with the session picker open")
	}
	m3 := envModel{agentName: "resident", defaultAgent: "resident", tab: 7, width: 90, height: 28}
	m3.initChat()
	openLastSession(&m3)
	if m3.pendingLast == "" {
		t.Fatal("fixture broken: no pending last session")
	}
	next, _ = m3.Update(residentSessionsMsg{agent: "ghost", err: "no such resident"})
	m3 = next.(envModel)
	if m3.tab != 6 || !m3.chat.picker || m3.agentName != "resident" || m3.pendingLast != "" {
		t.Fatal("an unresolvable last session did not fall back to the chat's session picker")
	}
}

func TestChatFrameStaysSlimWithHintsAndSwitcher(t *testing.T) {
	for _, size := range [][2]int{{40, 16}, {110, 32}} {
		m := chatFixture(t)
		m.width, m.height = size[0], size[1]
		for i := 2; i < 60; i++ {
			m.receiveResident(residentMsg{agent: "resident", status: m.resident,
				events: []residentEvent{{Seq: i, Goal: "original", Kind: "assistant", Data: map[string]any{"content": "text line"}}}})
		}
		m.chat.compose.SetValue("/s")
		frame := ansi.Strip(m.render())
		rows := strings.Split(frame, "\n")
		if len(rows) > size[1] {
			t.Fatalf("%v: %d lines overflow the terminal", size, len(rows))
		}
		for _, row := range rows {
			if ansi.StringWidth(row) > size[0] {
				t.Fatalf("%v: line too wide: %q", size, row)
			}
		}
		if !strings.Contains(frame, "Tab completes") || !strings.Contains(frame, "/session") {
			t.Fatalf("%v: autocomplete hint missing", size)
		}
		if len(rows) < 3 {
			t.Fatal("frame lost the header and footer lines")
		}
		if !strings.Contains(frame, "Ctrl+P sessions") && !strings.Contains(frame, "session") {
			t.Fatalf("%v: footer lost the switcher hint", size)
		}
		next, _ := m.chatKey(tea.KeyPressMsg{Code: 'p', Mod: tea.ModCtrl})
		m = next.(envModel)
		frame = ansi.Strip(m.render())
		if !strings.Contains(frame, "OPEN SESSION") {
			t.Fatalf("%v: switcher panel missing", size)
		}
		for _, row := range strings.Split(frame, "\n") {
			if ansi.StringWidth(row) > size[0] {
				t.Fatalf("%v: switcher line too wide", size)
			}
		}
	}
}

func TestLiveTailShowsThinkingToolsAndTextThenYieldsToTheDurableBlock(t *testing.T) {
	m := chatFixture(t)
	// a turn's first line carries the whole text, later lines carry the suffix
	next, _ := m.Update(residentLiveMsg{agent: "resident", turn: "t1", text: "Half", reasoning: "Weighing options.",
		tools: []string{"terminal"}, reset: true})
	m = next.(envModel)
	if !m.chat.live.active {
		t.Fatal("the live tail did not start")
	}
	live := ansi.Strip(m.liveBlock(m.width))
	for _, want := range []string{g.agent + " agent", "Weighing options.", g.tool + " terminal", "Half", "live"} {
		if !strings.Contains(live, want) {
			t.Fatalf("live tail missing %q:\n%s", want, live)
		}
	}
	next, _ = m.Update(residentLiveMsg{agent: "resident", turn: "t1", text: " an answer."})
	m = next.(envModel)
	if !strings.Contains(ansi.Strip(m.liveBlock(m.width)), "Half an answer.") {
		t.Fatal("streamed deltas did not accumulate")
	}
	// the durable reply replaces the tail and shows its reasoning as its own block
	m.receiveResident(residentMsg{agent: "resident", status: m.resident, events: []residentEvent{{Seq: 2, Goal: "original",
		Kind: "assistant", Data: map[string]any{"content": "Half an answer.", "reasoning": "Weighing options."}}}})
	if m.chat.live.active {
		t.Fatal("the durable reply must end the live tail")
	}
	durable := ansi.Strip(m.chat.content)
	if !strings.Contains(durable, g.collapsed+" thinking") || !strings.Contains(durable, g.agent+" agent") || !strings.Contains(durable, "Half an answer.") {
		t.Fatalf("durable blocks missing:\n%s", durable)
	}
	if strings.Contains(ansi.Strip(m.liveBlock(m.width)), "Half an answer.") {
		t.Fatal("the live tail outlived the durable block")
	}
}

func TestLiveTailStaysBoundedAndRejectsAnotherVisit(t *testing.T) {
	long := strings.Repeat("word ", 400)
	if got := strings.Count(envTail(long, 40, 8), "\n"); got > 8 {
		t.Fatalf("live tail kept %d lines", got)
	}
	m := chatFixture(t)
	m.residentEpoch = 3
	next, _ := m.Update(residentLiveMsg{agent: "resident", epoch: 2, turn: "t", text: "ghost", reset: true})
	if next.(envModel).chat.live.active {
		t.Fatal("a stream from an earlier visit painted the tail")
	}
}

func TestQuickSwitcherScrollsPastTheFirstPageWithoutPanicking(t *testing.T) {
	m := chatFixture(t)
	m.height = 12 // four visible rows, so a ten-session list needs paging
	names := []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"}
	m.sessionNames = names
	m.chat.picker = true
	for sel := range names {
		m.chat.pickerSel = sel
		box := ansi.Strip(m.pickerBox()) // panicked with "slice bounds out of range" before the fix
		if !strings.Contains(box, "› "+names[sel]) {
			t.Fatalf("selection %d (%s) is not shown:\n%s", sel, names[sel], box)
		}
	}
}

func TestResidentCommandsWithoutTheCLIReportInsteadOfPanicking(t *testing.T) {
	m := chatFixture(t)
	m.cli = nil
	if cmd := residentCommand(m, []string{"status", "resident", "--json"}, ""); cmd != nil {
		t.Fatal("a missing --cli must not build a command")
	}
	msg := residentSessions(m)()
	if sessions, ok := msg.(residentSessionsMsg); !ok || sessions.err == "" {
		t.Fatalf("sessions without a CLI must report an error, got %#v", msg)
	}
}

// The context bar shows what the daemon measured, graded against the
// compaction trigger, and never invents a percentage of its own. The accent
// piece (label, bar, share) takes the level's color; the counts stay dim.
func TestContextBarShowsOnlyMeasuredUsage(t *testing.T) {
	accent, rest := contextBar(0, 1048574, .85, 0)
	if accent != "" || rest != "ctx 1.0M window" || strings.Contains(rest, "%") {
		t.Fatalf("unmeasured context showed usage: %q / %q", accent, rest)
	}
	if accent, rest := contextBar(0, 0, .85, 0); accent != "" || rest != "" {
		t.Fatalf("no capacity, no bar: %q / %q", accent, rest)
	}
	accent, rest = contextBar(314573, 1048574, .85, 0) // 30%
	if filled := strings.Count(accent, "█"); filled != 3 || !strings.Contains(accent, "░") {
		t.Fatalf("30%% carved the bar wrong: %q", accent)
	}
	if !strings.Contains(accent, "ctx") || !strings.Contains(accent, "30%") || strings.Count(accent, "%") != 1 {
		t.Fatalf("accent lost its bar and share: %q", accent)
	}
	if !strings.Contains(rest, "of 1.0M · compact 85%") {
		t.Fatalf("dim piece lost counts and trigger: %q", rest)
	}
	if _, rest := contextBar(314573, 1048574, .85, 87.3); !strings.Contains(rest, "87 tok/s") || !strings.Contains(rest, "compact 85%") {
		t.Fatalf("measured tok/s did not annotate the dim piece: %q", rest)
	}
	if _, rest := contextBar(314573, 1048574, .85, 0); strings.Contains(rest, "tok/s") {
		t.Fatalf("no speed reported, none may show: %q", rest)
	}
	if _, rest := contextBar(0, 1048574, .85, 87.3); strings.Contains(rest, "tok/s") {
		t.Fatalf("tok/s without a measured context bar: %q", rest)
	}
	if accent, _ := contextBar(1048575, 1048574, .85, 0); !strings.HasPrefix(accent, "ctx ██████████ 100%") {
		t.Fatalf("usage past the window did not clamp: %q", accent)
	}
	if got := humanTokens(999); got != "999" || humanTokens(482000) != "482k" || humanTokens(1048574) != "1.0M" {
		t.Fatalf("token formatting drifted: %s %s %s", humanTokens(999), humanTokens(482000), humanTokens(1048574))
	}
	tests := []struct {
		used, capacity int
		threshold      float64
		level          int
	}{
		{0, 1048574, .85, 0},
		{314573, 1048574, .85, 0},
		{838861, 1048574, .85, 1}, // 80%: within fifteen points of the trigger
		{891288, 1048574, .85, 2}, // at the trigger
	}
	for _, tc := range tests {
		if got := contextBarLevel(tc.used, tc.capacity, tc.threshold); got != tc.level {
			t.Fatalf("level(%d/%d, %.2f) = %d, want %d", tc.used, tc.capacity, tc.threshold, got, tc.level)
		}
	}
	states := map[string]int{"working": 1, "queued": 1, "waiting_model": 1, "listening": 1,
		"waiting": 2, "waiting_user": 2, "paused": 2, "error": 3, "stopped": 0, "completed": 0}
	for state, want := range states {
		if got := contextStateStyle(state); got.Render("#") == envDim.Render("#") && want != 0 {
			t.Fatalf("state %q rendered in the resting tone", state)
		}
	}
	if contextStateStyle("stopped").Render("#") != envDim.Render("#") || contextStateStyle("nonsense").Render("#") != envDim.Render("#") {
		t.Fatal("unknown states must rest in dim")
	}
}

// The measured bar lands in the live footer, warning color included, without
// pushing the frame past its width.
func TestChatFooterCarriesTheMeasuredContextBar(t *testing.T) {
	m := chatFixture(t)
	m.resident.ContextUsed = 891288 // at the trigger: hot
	m.resident.TokPerS = 91.4       // the daemon's last measured decode speed
	raw := m.render()
	frame := ansi.Strip(raw)
	if !strings.Contains(frame, "891K/1M") || !strings.Contains(frame, "91 tok/s") {
		t.Fatalf("footer lost the measured bar:\n%s", frame)
	}
	if !strings.Contains(raw, envCtxHot.Render(strings.Repeat(g.bar, 7))) {
		t.Fatal("at the compaction trigger the bar must be hot")
	}
	for _, row := range strings.Split(frame, "\n") {
		if ansi.StringWidth(row) > m.width {
			t.Fatalf("footer line too wide: %q", row)
		}
	}
}

func TestSlashPrefixedMessageSendsVerbatimWithDoubleSlash(t *testing.T) {
	m := chatFixture(t)
	m.chat.compose.SetValue("//tmp is full, the build failed")
	next, _ := m.submitCompose()
	m = next.(envModel)
	if m.msg != "" || !m.chat.sending {
		t.Fatalf("//text must be sent as a message, got msg=%q sending=%v", m.msg, m.chat.sending)
	}
	m2 := chatFixture(t)
	m2.chat.compose.SetValue("/tmp is full")
	next, _ = m2.submitCompose()
	m2 = next.(envModel)
	if !strings.Contains(m2.msg, "Unknown command") || m2.chat.sending {
		t.Fatalf("an unknown command must be reported, not sent: msg=%q sending=%v", m2.msg, m2.chat.sending)
	}
}

func TestSessionCommandRejectsAnUnknownNameWithoutClearingTheTranscript(t *testing.T) {
	m := chatFixture(t)
	m.sessionNames = []string{"resident", "research"}
	m.receiveResident(residentMsg{agent: "resident", status: m.resident,
		events: []residentEvent{{Seq: 2, Goal: "original", Kind: "assistant", Data: map[string]any{"content": "keep me"}}}})
	before := len(m.residentEvents)
	m.chat.compose.SetValue("/session rescarch")
	next, _ := m.submitCompose()
	m = next.(envModel)
	if m.agentName != "resident" || len(m.residentEvents) != before {
		t.Fatalf("a typo switched sessions: agent=%q events=%d/%d", m.agentName, len(m.residentEvents), before)
	}
	if !strings.Contains(ansi.Strip(m.chat.content), "keep me") {
		t.Fatal("a typo cleared the transcript")
	}
	if !strings.Contains(m.msg, "no such session") {
		t.Fatalf("no warning shown: %q", m.msg)
	}
	m.chat.compose.SetValue("/session research")
	next, _ = m.submitCompose()
	m = next.(envModel)
	if m.agentName != "research" {
		t.Fatalf("a known session did not open: %q", m.agentName)
	}
}

func TestTruncatedReplyShowsWhatTheModelWroteBeforeItStopped(t *testing.T) {
	m := chatFixture(t)
	m.receiveResident(residentMsg{agent: "resident", status: m.resident, events: []residentEvent{{Seq: 2, Goal: "original",
		Kind: "model_truncated", Data: map[string]any{
			"error":   "the reply ended before it was complete; nothing was executed",
			"partial": map[string]any{"content": "Half an answer"}}}}})
	block := ansi.Strip(m.chat.content)
	if !strings.Contains(block, "Half an answer") {
		t.Fatalf("the partial reply is not shown:\n%s", block)
	}
	if !strings.Contains(block, "before it was complete") {
		t.Fatalf("the reason is not shown:\n%s", block)
	}
}

// liveWindow is the visible body of the live block, without its border or label line.
func liveWindow(m envModel) string {
	body := m.chat.live.body(m.width - 4)
	top := m.liveTop(len(body))
	return strings.Join(body[top:min(top+m.liveHeight(), len(body))], "\n")
}

func TestLiveWindowScrollsAndHoldsWhileTheReplyKeepsArriving(t *testing.T) {
	m := chatFixture(t)
	m.height = 30
	next, _ := m.Update(residentLiveMsg{agent: "resident", turn: "t1",
		reasoning: strings.Repeat("thinking line ", 200), reset: true})
	m = next.(envModel)
	if !strings.Contains(ansi.Strip(m.liveBlock(m.width)), g.agent+" thinking") || !strings.Contains(ansi.Strip(m.liveBlock(m.width)), "live ·") {
		t.Fatalf("the live window is not following:\n%s", ansi.Strip(m.liveBlock(m.width)))
	}
	m.scrollLive(-8)
	held := ansi.Strip(m.liveBlock(m.width))
	if !strings.Contains(held, "held") {
		t.Fatalf("scrolling did not hold the window:\n%s", held)
	}
	// more of the reply arrives: the held window must keep showing the same lines
	before := liveWindow(m)
	next, _ = m.Update(residentLiveMsg{agent: "resident", turn: "t1", text: "and now an answer"})
	m = next.(envModel)
	if after := liveWindow(m); after != before {
		t.Fatalf("the held window moved while text arrived:\n%s\n---\n%s", before, after)
	}
	// Ctrl+T resumes following, so the newest text is visible again
	next, _ = m.chatKey(tea.KeyPressMsg{Code: 't', Mod: tea.ModCtrl})
	m = next.(envModel)
	if m.chat.live.hold {
		t.Fatal("Ctrl+T did not resume following")
	}
	if !strings.Contains(ansi.Strip(m.liveBlock(m.width)), "and now an answer") {
		t.Fatal("the resumed window does not show the newest text")
	}
	// scrolling back to the bottom resumes too
	m.scrollLive(-4)
	m.scrollLive(100)
	if m.chat.live.hold {
		t.Fatal("scrolling to the bottom must resume following")
	}
}

func TestLiveWindowTakesHalfTheBodyAndLeavesTheRestToTheTranscript(t *testing.T) {
	m := chatFixture(t)
	m.height = 40
	next, _ := m.Update(residentLiveMsg{agent: "resident", turn: "t1",
		reasoning: strings.Repeat("thinking about it ", 60), reset: true})
	m = next.(envModel)
	body := m.bodyHeight()
	live := lipgloss.Height(m.liveBlock(m.width))
	if live < body/2-2 || live > body/2+2 {
		t.Fatalf("live window is %d lines of a %d-line body; expected about half", live, body)
	}
	transcript := m.chat.transcript.Height()
	if transcript < body-live-2 || transcript > body-live+2 {
		t.Fatalf("transcript is %d lines, expected about %d", transcript, body-live)
	}
}

// sgr is the escape a color renders as, so a test can assert a palette role without hardcoding its hex.
func sgr(color color.Color) string {
	out := lipgloss.NewStyle().Foreground(color).Render("x")
	return strings.SplitN(out, "x", 2)[0]
}

func TestLiveTextIsColoredLikeTheTranscript(t *testing.T) {
	m := chatFixture(t)
	m.chat.live = residentLive{active: true, turn: "t1", text: "run `go test ./...` now", tools: []string{"terminal"}}
	block := m.liveBlock(m.width)
	if !strings.Contains(block, sgr(cTeal)) {
		t.Fatalf("inline code is not colored:\n%q", block)
	}
	if !strings.Contains(block, sgr(cOrange)) {
		t.Fatalf("the running tool is not colored:\n%q", block)
	}
}

func TestEachEventIsFramedAsACardWithItsBodyBehindARule(t *testing.T) {
	m := chatFixture(t)
	m.height = 30
	m.residentEvents = []residentEvent{
		{Seq: 1, Goal: "g1", Kind: "goal_queued", Data: map[string]any{"prompt": "fix the build"}},
		{Seq: 2, Goal: "g1", Kind: "assistant", Data: map[string]any{"content": "on it"}},
	}
	m.chat.rendered, m.chat.content = 0, ""
	m.syncChat()
	lines := strings.Split(ansi.Strip(m.chat.content), "\n")
	if !strings.HasPrefix(lines[0], g.user+" you · goal") {
		t.Fatalf("the first line is not the goal's label: %q", lines[0])
	}
	if !strings.HasPrefix(lines[1], "│ fix the build") {
		t.Fatalf("the body is not behind the card rule: %q", lines[1])
	}
	var seen bool
	for _, ln := range lines {
		if strings.HasPrefix(ln, g.agent+" agent") {
			seen = true
		}
	}
	if !seen {
		t.Fatalf("the reply has no label:\n%s", strings.Join(lines, "\n"))
	}
}

func TestMarkdownColorsFencedCodeWithTheNordTheme(t *testing.T) {
	r, err := glamour.NewTermRenderer(glamour.WithStyles(envMarkdownStyle()), glamour.WithWordWrap(80))
	if err != nil {
		t.Fatal(err)
	}
	out, err := r.Render("```go\nfunc main() { println(\"hi\", 42) }\n```\n")
	if err != nil {
		t.Fatal(err)
	}
	if spans := strings.Count(out, "38;2;"); spans < 3 {
		t.Fatalf("fenced code has %d colored spans, expected syntax coloring:\n%q", spans, out)
	}
}

// themeFixture saves the panel's theme state and restores it after the test;
// the theme is global and no test may leave a switched palette behind.
func themeFixture(tb testing.TB) {
	tb.Helper()
	saved := themeCurrent
	tb.Cleanup(func() {
		themeCurrent = saved
		applyThemeStyles()
	})
}

func TestThemeParsesTheEmbeddedCaminusFile(t *testing.T) {
	data, err := fs.ReadFile(themeFS, "themes/caminus.theme")
	if err != nil {
		t.Fatal(err)
	}
	got, bad := parseTheme(data)
	if len(bad) != 0 {
		t.Fatalf("the built-in caminus theme has bad lines: %v", bad)
	}
	for _, role := range []string{"text", "accent", "ok", "warn", "action", "hot", "user", "chrome", "code_theme"} {
		if got[role] == "" {
			t.Fatalf("the built-in caminus theme does not set %s", role)
		}
	}
}

func TestThemeLoadSwitchesRolesAndStyles(t *testing.T) {
	themeFixture(t)
	before := themeRole("accent")
	if err := themeLoad("nord"); err != nil {
		t.Fatal(err)
	}
	after := themeRole("accent")
	if before == after {
		t.Fatalf("loading nord left accent at %s", after)
	}
	if envTitle.Render("x") == "" {
		t.Fatal("the rebuilt styles no longer render")
	}
	if themeLoad("no-such-theme") == nil {
		t.Fatal("a nonexistent theme loaded without error")
	}
	if themeRole("accent") != after {
		t.Fatal("a failed load changed the palette")
	}
}

func TestThemeNamesListTheBuiltins(t *testing.T) {
	names := themeNames()
	for _, want := range []string{"caminus", "nord", "catppuccin-mocha", "dracula", "gruvbox-dark", "tokyo-night"} {
		found := false
		for _, n := range names {
			if n == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("%s missing from themeNames: %v", want, names)
		}
	}
}

func TestThemeParseToleratesBtopKeysAndUnknownLines(t *testing.T) {
	text := `# a comment
main_fg = #e5e9f0
title_fg = #88c0d0
cpu_load_start_low = #abcdef
not a mapping at all
code_theme = monokai
`
	got, bad := parseTheme([]byte(text))
	if got["text"] != "#e5e9f0" || got["accent"] != "#88c0d0" || got["code_theme"] != "monokai" {
		t.Fatalf("the aliases or code_theme did not land: %v", got)
	}
	if _, present := got["cpu_load_start_low"]; present {
		t.Fatalf("an unmapped btop key was applied as a role: %v", got)
	}
	if len(bad) != 1 || !strings.Contains(bad[0], "not a mapping") {
		t.Fatalf("only lines that are not key = value should be bad, got %v", bad)
	}
}

func TestThemeSetPersistsAndTheSlashCommandSwitches(t *testing.T) {
	themeFixture(t)
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	m := chatFixture(t)
	tm, _ := m.chatSlash("/theme")
	list := tm.(envModel)
	if !strings.Contains(list.msg, "caminus") || !strings.Contains(list.msg, "nord") {
		t.Fatalf("/theme does not list themes: %q", list.msg)
	}
	tn, _ := m.chatSlash("/theme nope")
	nope := tn.(envModel)
	if !strings.Contains(nope.msg, "no such theme") {
		t.Fatalf("/theme nope did not report the failure: %q", nope.msg)
	}
	ts, _ := m.chatSlash("/theme nord")
	m = ts.(envModel)
	// syncChat rebuilds the renderer straight away when markdown was nilled, so
	// the next reply is styled in the new theme, not the old one.
	if m.chat.markdown == nil {
		t.Fatal("/theme nord left the markdown renderer unbuilt")
	}
	if themeRole("accent") != "#88c0d0" {
		t.Fatalf("/theme nord did not switch the palette: %s", themeRole("accent"))
	}
	saved, err := os.ReadFile(filepath.Join(dir, "caminus", "panel-theme"))
	if err != nil || strings.TrimSpace(string(saved)) != "nord" {
		t.Fatalf("the choice was not persisted (err=%v): %q", err, saved)
	}
}

func TestPathTokenDetectsPathsButLeavesCommandsAlone(t *testing.T) {
	for _, want := range []string{"/home/ig", "~/src/", "./build/x", "../notes", "/home"} {
		if _, ok := pathToken(want); !ok {
			t.Fatalf("%q should be a path token", want)
		}
	}
	for _, no := range []string{"", "hello", "/h", "goal one", "see /home later ", "a\nb /home"} {
		if tok, ok := pathToken(no); ok {
			t.Fatalf("%q should not be a path token (got %q)", no, tok)
		}
	}
}

func TestPathMatchesListsDirectoriesFirstAndHidesDotfiles(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "alpha"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "zeta.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".hidden"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "alpha", "inner.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	d := dir + "/"
	if got := pathMatches(d); len(got) != 2 || got[0] != d+"alpha/" || got[1] != d+"zeta.txt" {
		t.Fatalf("unexpected matches: %v", got)
	}
	if got := pathMatches(d + "a"); len(got) != 1 || got[0] != d+"alpha/" {
		t.Fatalf("prefix filter failed: %v", got)
	}
	if got := pathMatches(d + "."); len(got) != 1 || !strings.HasSuffix(got[0], ".hidden") {
		t.Fatalf("dot entries should surface with a dot prefix: %v", got)
	}
	if got := pathMatches(dir + "/nowhere/"); len(got) != 0 {
		t.Fatalf("an unreadable directory should match nothing: %v", got)
	}
}

func TestTabCompletesAWalksIntoDirectoriesAndCyclesFiles(t *testing.T) {
	themeFixture(t)
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "walk", "alpha"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "beta.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "gamma.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "walk", "alpha", "inner.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := chatFixture(t)
	m.chat.compose.SetValue(dir + "/walk/a")
	m.chat.compose.Focus()
	next, _ := m.chatDraftKey(tea.KeyPressMsg{Code: tea.KeyTab})
	m = next.(envModel)
	if m.chat.compose.Value() != dir+"/walk/alpha/" {
		t.Fatalf("first Tab should take the directory: %q", m.chat.compose.Value())
	}
	next, _ = m.chatDraftKey(tea.KeyPressMsg{Code: tea.KeyTab})
	m = next.(envModel)
	if m.chat.compose.Value() != dir+"/walk/alpha/inner.txt" {
		t.Fatalf("Tab in the directory should descend: %q", m.chat.compose.Value())
	}
	// From a files-only view cycling walks the matches in order; directories
	// would have gone first.
	files := t.TempDir()
	if err := os.WriteFile(filepath.Join(files, "beta.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(files, "gamma.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	reset := chatFixture(t)
	reset.chat.compose.SetValue(files + "/")
	for _, want := range []string{files + "/beta.txt", files + "/gamma.txt"} {
		nx, _ := reset.chatDraftKey(tea.KeyPressMsg{Code: tea.KeyTab})
		reset = nx.(envModel)
		if got := reset.chat.compose.Value(); got != want {
			t.Fatalf("cycling files: got %q, want %q", got, want)
		}
	}
}

func TestChatComposerHintsAboutPathsThematically(t *testing.T) {
	themeFixture(t)
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	m := chatFixture(t)
	m.chat.compose.SetValue("./")
	view := m.chatComposer()
	if !strings.Contains(view, "Tab completes") || !strings.Contains(view, "sub/") {
		t.Fatalf("a path in progress gets no completion hint:\n%s", view)
	}
	m.chat.compose.SetValue("./zzz")
	if view := m.chatComposer(); !strings.Contains(view, "no match") {
		t.Fatalf("a dead path should announce itself:\n%s", view)
	}
}

// The redesign's transcript: tool calls fold to one line until Ctrl+E opens every card; a failed call stays open
// with its reason; a root request is the inline approval card.
func TestTranscriptCardsFoldOpenAndShowRootInline(t *testing.T) {
	m := chatFixture(t)
	m.height = 40
	m.residentEvents = []residentEvent{
		{Seq: 1, Goal: "g1", Kind: "assistant", Data: map[string]any{"tool_calls": []any{map[string]any{"function": map[string]any{
			"name": "terminal", "arguments": `{"command":"make test\nmake lint"}`}}}}},
		{Seq: 2, Goal: "g1", Kind: "tool_result", Data: map[string]any{"name": "terminal", "result": map[string]any{
			"output": "41 passed\nall green", "exit_code": float64(0)}}},
		{Seq: 3, Goal: "g1", Kind: "tool_result", Data: map[string]any{"name": "write", "result": map[string]any{
			"error": "permission denied: /etc/hosts"}}},
		{Seq: 4, Goal: "g1", Kind: "approval_requested", Data: map[string]any{"cmd": "apt-get install -y libcmocka-dev", "reason": "the test needs it"}},
	}
	m.chat.rendered, m.chat.content = 0, ""
	m.syncChat()
	folded := ansi.Strip(m.chat.content)
	for _, want := range []string{g.collapsed + " " + g.tool + " terminal  make test …", "exit 0 · 2 lines", "permission denied", "ROOT", "apt-get install -y libcmocka-dev", "a approve", "/deny TEXT"} {
		if !strings.Contains(folded, want) {
			t.Fatalf("folded transcript missing %q:\n%s", want, folded)
		}
	}
	if strings.Contains(folded, "all green") || strings.Contains(folded, "make lint") {
		t.Fatalf("a folded card showed its body:\n%s", folded)
	}
	next, _ := m.chatKey(tea.KeyPressMsg{Code: 'e', Mod: tea.ModCtrl})
	m = next.(envModel)
	open := ansi.Strip(m.chat.content)
	if !m.chat.expanded || !strings.Contains(open, "all green") || !strings.Contains(open, "make lint") {
		t.Fatalf("Ctrl+E did not open the cards:\n%s", open)
	}
	next, _ = m.chatKey(tea.KeyPressMsg{Code: 'e', Mod: tea.ModCtrl})
	if next.(envModel).chat.expanded {
		t.Fatal("a second Ctrl+E must fold them again")
	}
}

// A waiting root request is decided in the chat through the Requests page's own handler: d writes the denial to the
// bridge (and the audit), /deny TEXT denies and steers the goal with what to do instead.
func TestChatDecidesRootRequestsInline(t *testing.T) {
	for _, viaSlash := range []bool{false, true} {
		m := chatFixture(t)
		bridge := t.TempDir()
		writeReq(t, bridge, "r1", "apt-get install -y libfoo-dev", 1)
		m.bridge, m.auditPath, m.seen = bridge, filepath.Join(t.TempDir(), "audit.jsonl"), map[string]bool{}
		m.refreshLocal()
		if m.pendingRoot() != 0 {
			t.Fatal("the waiting request is not found")
		}
		var cmd tea.Cmd
		if viaSlash {
			var next tea.Model
			next, cmd = m.chatSlash("/deny use the vendored copy in third_party/ instead")
			m = next.(envModel)
			if cmd == nil {
				t.Fatal("/deny must also send the steer")
			}
		} else {
			m.chat.compose.Blur()
			next, _ := m.chatKey(tea.KeyPressMsg{Code: 'd', Text: "d"})
			m = next.(envModel)
		}
		data, err := os.ReadFile(filepath.Join(bridge, "res", "r1.json"))
		if err != nil || !strings.Contains(string(data), `"denied"`) {
			t.Fatalf("no denial in the bridge (slash=%v): %s %v", viaSlash, data, err)
		}
		if m.pendingRoot() != -1 {
			t.Fatal("a decided request still shows as waiting")
		}
	}
}

// Approving runs a root command: one stray "a" while reading must not do it. The first press only arms it and names
// the command; a second approves; any other key in between disarms.
func TestChatApprovalNeedsASecondPress(t *testing.T) {
	m := chatFixture(t)
	bridge := t.TempDir()
	writeReq(t, bridge, "r1", "rm -rf /var/cache/foo", 1)
	m.bridge, m.auditPath, m.seen = bridge, filepath.Join(t.TempDir(), "audit.jsonl"), map[string]bool{}
	m.refreshLocal()
	m.chat.compose.Blur()
	press := func(r rune) {
		next, _ := m.chatKey(tea.KeyPressMsg{Code: r, Text: string(r)})
		m = next.(envModel)
	}
	press('a')
	if m.chat.armed != "r1" || !strings.Contains(m.msg, "rm -rf /var/cache/foo") || !strings.Contains(m.msg, "press a again") {
		t.Fatalf("the first a must only arm and name the command: %q", m.msg)
	}
	press('j') // scrolling in between
	if m.chat.armed != "" {
		t.Fatal("another key must disarm the approval")
	}
	press('a')
	if m.chat.armed != "r1" {
		t.Fatal("a fresh a arms again")
	}
	m.chat.armedAt = m.chat.armedAt.Add(-11 * time.Second)
	press('a')
	if m.chat.armed != "r1" {
		t.Fatal("a stale arm (over 10 s) only re-arms")
	}
	if _, err := os.Stat(filepath.Join(bridge, "res", "r1.json")); err == nil {
		t.Fatal("nothing may be decided before a confirmed approval")
	}
}

// An edit is a diff card, open while short; a read is one quiet line; their call cards are not repeated.
func TestFileToolsRenderAsDiffAndReadCards(t *testing.T) {
	m := chatFixture(t)
	m.height = 40
	m.residentEvents = []residentEvent{
		{Seq: 1, Goal: "g1", Kind: "assistant", Data: map[string]any{"tool_calls": []any{map[string]any{"function": map[string]any{
			"name": "edit", "arguments": `{"path":"src/parse.c","old":"n > len","new":"n >= len"}`}}}}},
		{Seq: 2, Goal: "g1", Kind: "tool_result", Data: map[string]any{"name": "edit", "result": map[string]any{
			"path": "src/parse.c", "added": float64(1), "removed": float64(1),
			"diff": "@@ -1,3 +1,3 @@\n int f(void) {\n-    if (n > len) return -1;\n+    if (n >= len) return -1;"}}},
		{Seq: 3, Goal: "g1", Kind: "tool_result", Data: map[string]any{"name": "read", "result": map[string]any{
			"path": "src/parse.c", "shown": "1-40", "text": "secret body"}}},
	}
	m.chat.rendered, m.chat.content = 0, ""
	m.syncChat()
	v := ansi.Strip(m.chat.content)
	for _, want := range []string{g.edit + " edit  src/parse.c  ·  +1 −1", "-    if (n > len) return -1;", "+    if (n >= len) return -1;", "read  src/parse.c  ·  lines 1-40"} {
		if !strings.Contains(v, want) {
			t.Fatalf("missing %q:\n%s", want, v)
		}
	}
	if strings.Contains(v, "@@") || strings.Contains(v, "secret body") || strings.Count(v, "src/parse.c") != 2 {
		t.Fatalf("hunk headers, read bodies or a repeated call card leaked:\n%s", v)
	}
}

// The selected goal's newest todo list is pinned above the composer while the goal is open; the frame still fits.
func TestTodoIsPinnedAboveTheComposer(t *testing.T) {
	m := chatFixture(t)
	m.width, m.height = 100, 24
	todo := func(items ...map[string]any) residentEvent {
		raw := make([]any, len(items))
		for i, it := range items {
			raw[i] = it
		}
		return residentEvent{Seq: 9, Goal: "original", Kind: "todo", Data: map[string]any{"items": raw}}
	}
	m.residentEvents = append(m.residentEvents,
		todo(map[string]any{"text": "stale", "state": "pending"}),
		todo(map[string]any{"text": "find the test", "state": "done"}, map[string]any{"text": "fix the bounds", "state": "in_progress"},
			map[string]any{"text": "run the gate", "state": "pending"}))
	m.syncChat()
	frame := ansi.Strip(m.render())
	if !strings.Contains(frame, "todo") || !strings.Contains(frame, "fix the bounds") || strings.Contains(frame, "stale") {
		t.Fatalf("the newest todo list is not pinned:\n%s", frame)
	}
	if rows := strings.Split(frame, "\n"); len(rows) > m.height {
		t.Fatalf("the pinned line pushed the frame past the terminal: %d rows", len(rows))
	}
	m.resident.Goals[0].State = "completed"
	if strings.Contains(ansi.Strip(m.render()), "fix the bounds") {
		t.Fatal("a finished goal's list must not stay pinned")
	}
}
