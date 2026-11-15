// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 jdominick05

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

func chatFixture() envModel {
	m := envModel{agentName: "resident", tab: 6, width: 90, height: 28, mode: "normal"}
	m.initChat()
	m.receiveResident(residentMsg{agent: "resident", status: residentStatus{Name: "resident", Model: "local-model", Context: 1048574, DaemonAlive: true, LastEvent: 1, Goals: []residentGoal{{ID: "original", State: "working", Prompt: "a goal"}}}, events: []residentEvent{{Seq: 1, Goal: "original", Kind: "goal_queued", Data: map[string]any{"prompt": "a goal"}}}})
	return m
}

func TestChatRetainsTranscriptAndScrollAnchorOnRefresh(t *testing.T) {
	m := chatFixture()
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
	m := chatFixture()
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
	m := chatFixture()
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
		m := chatFixture()
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
	m := chatFixture()
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
