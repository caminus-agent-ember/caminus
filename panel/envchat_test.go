// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 jdominick05

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
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
	if m.tab != 0 {
		t.Fatal("/requests did not open approvals")
	}
	m.tab = 6
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
		if !strings.Contains(ansi.Strip(frame), "Steer goal original") {
			t.Fatal("bottom composer missing")
		}
	}
}

func TestChatDisplaysCommandArgumentsAndEscapesControls(t *testing.T) {
	m := chatFixture(t)
	e := residentEvent{Kind: "assistant", Data: map[string]any{"tool_calls": []any{map[string]any{"function": map[string]any{"name": "terminal", "arguments": "{\"command\":\"printf hello #\\u001b[2K\\rhidden\"}"}}}}}
	block := m.chat.eventBlock(e)
	plain := ansi.Strip(block)
	for _, want := range []string{"RUN", "terminal", "printf", `\x1b`, "hidden"} {
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
	m := envModel{tab: 5, width: 80, height: 24, mode: "normal"}
	for i := 0; i < 90; i++ {
		m.events = append(m.events, fmt.Sprintf("audit entry %d with a long but readable message", i))
	}
	frame := m.render()
	if len(strings.Split(frame, "\n")) > m.height {
		t.Fatal("audit page exceeded terminal height")
	}
	for _, line := range strings.Split(frame, "\n") {
		if ansi.StringWidth(line) > m.width {
			t.Fatal("audit page exceeded terminal width")
		}
	}
	next, _ := m.onKey("pgdown")
	m = next.(envModel)
	if m.pageScroll == 0 || ansi.Strip(m.render()) == ansi.Strip(frame) {
		t.Fatal("Page Down did not scroll audit")
	}
	next, _ = m.onKey("1")
	m = next.(envModel)
	if m.tab != 7 || m.pageScroll != 0 {
		t.Fatal("opening sessions retained another page's scroll offset")
	}
}

func TestHistoryWrapPreservesLongUnicodeOutputAndReflowsOnResize(t *testing.T) {
	m := chatFixture(t)
	text := "start_" + strings.Repeat("A", 160) + strings.Repeat("界🙂", 40) + "_end"
	m.receiveResident(residentMsg{agent: "resident", status: m.resident, events: []residentEvent{{Seq: 2, Goal: "original", Kind: "tool_result", Data: map[string]any{"name": "terminal", "result": map[string]any{"output": text}}}}})
	for _, width := range []int{90, 40, 120} {
		m.width = width
		m.syncChat()
		plain := ansi.Strip(m.chat.content)
		if !utf8.ValidString(plain) {
			t.Fatal("wrapping split a UTF-8 character")
		}
		if !strings.Contains(strings.Join(strings.Fields(plain), ""), text) {
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

func TestSlashAutocompleteMatchesAndTabCycles(t *testing.T) {
	if got := slashMatches("/se"); fmt.Sprint(got) != "[/session /sessions]" {
		t.Fatalf("unexpected matches: %v", got)
	}
	if got := slashMatches("/s"); len(got) != 4 || got[0] != "/session" || got[3] != "/stop" {
		t.Fatalf("unexpected matches: %v", got)
	}
	for _, draft := range []string{"", "hello", "/remember queue pace", "/new\nsome goal"} {
		if got := slashMatches(draft); got != nil {
			t.Fatalf("unexpected matches for %q: %v", draft, got)
		}
	}
	for _, c := range slashCommands {
		if !strings.Contains("/help /quit /new /requests /sessions /newsession /remember /goals /goal /agent /session /start /stop /cancel /resume /", c.name+" ") {
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
	if m2.tab != 7 || m2.agentName != "resident" || !m2.chat.ready {
		t.Fatal("a vanished session did not fall back to the homepage")
	}
	m3 := envModel{agentName: "resident", defaultAgent: "resident", tab: 7, width: 90, height: 28}
	m3.initChat()
	openLastSession(&m3)
	if m3.pendingLast == "" {
		t.Fatal("fixture broken: no pending last session")
	}
	next, _ = m3.Update(residentSessionsMsg{agent: "ghost", err: "no such resident"})
	m3 = next.(envModel)
	if m3.tab != 7 || m3.agentName != "resident" || m3.pendingLast != "" {
		t.Fatal("an unresolvable last session did not fall back to the homepage")
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
	for _, want := range []string{"THINKING", "Weighing options.", "RUN · terminal", "Half"} {
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
	if !strings.Contains(durable, "THINKING") || !strings.Contains(durable, "AGENT") || !strings.Contains(durable, "Half an answer.") {
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
// compaction trigger, and never invents a percentage of its own.
func TestContextBarShowsOnlyMeasuredUsage(t *testing.T) {
	if got := contextBar(0, 1048574, .85); got != "ctx 1.0M window" || strings.Contains(got, "%") {
		t.Fatalf("unmeasured context showed usage: %q", got)
	}
	if got := contextBar(0, 0, .85); got != "" || strings.Contains(got, "%") {
		t.Fatalf("no capacity, no bar: %q", got)
	}
	bar := contextBar(314573, 1048574, .85) // 30%
	if filled := strings.Count(bar, "█"); filled != 3 || !strings.Contains(bar, "░") {
		t.Fatalf("30%% carved the bar wrong: %q", bar)
	}
	if !strings.Contains(bar, "30% of 1.0M · compact 85%") {
		t.Fatalf("bar lost counts and trigger: %q", bar)
	}
	if got := contextBar(1048575, 1048574, .85); !strings.HasPrefix(got, "ctx ██████████ 100% of 1.0M") {
		t.Fatalf("usage past the window did not clamp: %q", got)
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
}

// The measured bar lands in the live footer, warning color included, without
// pushing the frame past its width.
func TestChatFooterCarriesTheMeasuredContextBar(t *testing.T) {
	m := chatFixture(t)
	m.resident.ContextUsed = 891288 // at the trigger: hot
	frame := ansi.Strip(m.render())
	if !strings.Contains(frame, "ctx ") || !strings.Contains(frame, "85% of 1.0M · compact 85%") {
		t.Fatalf("footer lost the measured bar:\n%s", frame)
	}
	for _, row := range strings.Split(frame, "\n") {
		if ansi.StringWidth(row) > m.width {
			t.Fatalf("footer line too wide: %q", row)
		}
	}
}
