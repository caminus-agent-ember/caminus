// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 jdominick05

package main

// #316: the goal at the top of the chat expands and collapses — by Alt+G and by a click on the header, at 80, 100
// and 160 columns, with the "… (N more lines, Alt+G expands)" hint only while it is cut, the whole goal drawn as
// Markdown (lists, code), and select-to-copy taking the whole goal rather than the lines the header had room for.

import (
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// longGoal is about the 2,000 characters the issue saw, so it is cut even at 160 columns, with Markdown the expanded
// form has to draw (a list and a fenced block) and a last line only the expansion shows.
const longGoal = "Review this PR properly.\n\n" +
	"- the header must expand and collapse\n" +
	"- the collapsed form must say it is cut\n" +
	"- the full goal must render as Markdown\n" +
	"- select-to-copy must copy the whole goal\n\n" +
	"```go\nfunc chatHead(w int) string {\n\treturn m.headExpanded(w)\n}\n```\n\n" +
	"Keep the three-line summary readable while the header is closed, and draw the open goal with the same renderer " +
	"the replies use, so a list, a table and the fenced block above come out as they would in a reply rather than as " +
	"the plain source a screen cut in three lines shows. The transcript under it keeps its own scrolling, and the " +
	"composer keeps typing while the goal is open. A goal this long is the normal case here: PR reviews and long " +
	"design briefs are sent as one message, and none of that text should be unreachable from the chat.\n\n" +
	"The last line of the goal: Esc collapses it again.\n"

func longGoalChat(t *testing.T, w, h int) emberModel {
	t.Helper()
	m := chatFixture(t)
	m.width, m.height = w, h
	m.resident.Goals = []residentGoal{{ID: "original", State: "working", Prompt: longGoal}}
	m.goalSel = 0
	m.chat.target = "original"
	return m
}

func rows(s string) int { return strings.Count(s, "\n") + 1 }

// TestTheLongGoalExpandsByKeyAndClickAtEveryWidth covers the toggle by Alt+G and by a click on the header, the hint
// while cut, and Esc collapsing it again, at the three widths the issue names.
func TestTheLongGoalExpandsByKeyAndClickAtEveryWidth(t *testing.T) {
	for _, w := range []int{80, 100, 160} {
		t.Run(fmt.Sprint(w), func(t *testing.T) {
			m := longGoalChat(t, w, 30)
			collapsed := ansi.Strip(m.chatHead(w))
			if !m.headCut(w) {
				t.Fatalf("fixture: the goal is cut at %d columns:\n%s", w, collapsed)
			}
			if !strings.Contains(collapsed, "more line") || !strings.Contains(collapsed, "Alt+G expands") {
				t.Fatalf("the collapsed header says it is cut and names the key:\n%s", collapsed)
			}
			if strings.Contains(collapsed, "The last line of the goal") {
				t.Fatalf("fixture: the whole goal is in the collapsed header:\n%s", collapsed)
			}
			// Alt+G opens it: the whole Markdown, its list and its code drawn as such.
			next, _ := m.chatKey(tea.KeyPressMsg{Code: 'g', Mod: tea.ModAlt})
			m = next.(emberModel)
			if !m.chat.headOpen {
				t.Fatal("Alt+G did not open the goal")
			}
			open := ansi.Strip(m.chatHead(w))
			for _, want := range []string{"the header must expand", "select-to-copy must copy the whole goal",
				"func chatHead(w int) string", "Review this PR properly"} {
				if !strings.Contains(open, want) {
					t.Fatalf("the open goal lost %q at %d columns:\n%s", want, w, open)
				}
			}
			if strings.Contains(open, "Alt+G expands") {
				t.Fatalf("the hint goes while the goal is open:\n%s", open)
			}
			if !strings.Contains(open, "PgUp/PgDn scrolls") {
				t.Fatalf("a goal taller than half the screen says how to scroll:\n%s", open)
			}
			// PgDn reaches the rest: the last line is only in the scrolled part.
			for i := 0; i < 20; i++ {
				next, _ = m.chatKey(tea.KeyPressMsg{Code: tea.KeyPgDown})
				m = next.(emberModel)
			}
			if end := ansi.Strip(m.chatHead(w)); !strings.Contains(end, "The last line of the goal") {
				t.Fatalf("PgDn did not reach the last line of the goal at %d columns:\n%s", w, end)
			}
			next, _ = m.chatKey(tea.KeyPressMsg{Code: tea.KeyPgUp})
			m = next.(emberModel)
			if got := rows(m.chatHead(w)); got > max(12, m.height)/2+2 {
				t.Fatalf("the open goal takes %d rows: more than half the screen at %d", got, w)
			}
			// Esc collapses it, and the header is exactly what it was.
			next, _ = m.chatKey(tea.KeyPressMsg{Code: tea.KeyEscape})
			m = next.(emberModel)
			if m.chat.headOpen {
				t.Fatal("Esc did not collapse the goal")
			}
			if back := ansi.Strip(m.chatHead(w)); back != collapsed {
				t.Fatalf("collapsed again, the header differs:\n%s\n--- was:\n%s", back, collapsed)
			}
			// A click on the header toggles it, at every width.
			y := -1
			for i, row := range strings.Split(ansi.Strip(m.render()), "\n") {
				if strings.Contains(row, "CAMINUS") {
					y = i
					break
				}
			}
			if y < 0 {
				t.Fatal("no header on the screen")
			}
			m, _ = click(m, 2, y)
			if !m.chat.headOpen {
				t.Fatal("a click on the header did not open the goal")
			}
			m, _ = click(m, 2, y)
			if m.chat.headOpen {
				t.Fatal("a second click did not close it")
			}
		})
	}
}

// TestTheCutLineCountsWhatTheHeaderHides: the number in the hint is the goal's own wrapped lines, not the rendered
// Markdown's rows.
func TestTheCutLineCountsWhatTheHeaderHides(t *testing.T) {
	m := longGoalChat(t, 100, 30)
	_, _, one := m.headParts()
	total := len(headWrappedLines(one, 100))
	head := ansi.Strip(m.chatHead(100))
	cut := total - headLines
	want := "… (" + plural(cut, "more line", "more lines") + ", Alt+G expands)"
	if !strings.Contains(head, want) {
		t.Fatalf("the hint counts the hidden goal lines (%q):\n%s", want, head)
	}
}

// TestAShortGoalHasNoHintAndDoesNotExpand: a goal that fits is not offered an expansion that would do nothing.
func TestAShortGoalHasNoHintAndDoesNotExpand(t *testing.T) {
	m := chatFixture(t)
	m.width, m.height = 100, 30
	if m.headCut(100) {
		t.Fatal("a goal of one short line is not cut")
	}
	next, _ := m.chatKey(tea.KeyPressMsg{Code: 'g', Mod: tea.ModAlt})
	m = next.(emberModel)
	if m.chat.headOpen {
		t.Fatal("Alt+G opened a goal that already fits")
	}
	if strings.Contains(ansi.Strip(m.chatHead(100)), "Alt+G expands") {
		t.Fatal("a goal that fits does not say it is cut")
	}
}

// TestPgUpAndPgDnPageTheTranscriptUnderAGoalThatFits: an expanded goal with nowhere to scroll does not take the
// paging keys from the history under it.
func TestPgUpAndPgDnPageTheTranscriptUnderAGoalThatFits(t *testing.T) {
	m := chatFixture(t)
	m.width, m.height = 100, 30
	// long enough to be cut in the collapsed header (4+ wrapped lines), short enough to render in fewer rows than
	// the expansion shows at once.
	goal := strings.Repeat("A short brief. ", 20)
	m.resident.Goals = []residentGoal{{ID: "original", State: "working", Prompt: goal}}
	m.goalSel, m.chat.target = 0, "original"
	if !m.headCut(100) {
		t.Fatalf("fixture: the goal is cut in the collapsed header")
	}
	// a history long enough to page through
	for i := 0; i < 40; i++ {
		m.receiveResident(residentMsg{agent: "resident", status: m.resident, events: []residentEvent{{
			Seq: 100 + i, Goal: "original", Kind: "assistant",
			Data: map[string]any{"content": fmt.Sprintf("Observation %d stays in the transcript.", i)}}}})
	}
	next, _ := m.chatKey(tea.KeyPressMsg{Code: 'g', Mod: tea.ModAlt})
	m = next.(emberModel)
	if !m.chat.headOpen {
		t.Fatal("Alt+G did not open the goal")
	}
	lines := lipgloss.Height(m.headExpandedBody(100))
	if lines > m.headRows() {
		t.Fatalf("fixture: the open goal fits (%d rows of %d)", lines, m.headRows())
	}
	if m.chat.headTop != 0 {
		t.Fatalf("fixture: the open goal starts at row %d", m.chat.headTop)
	}
	// PgUp over an open goal that cannot scroll pages the history instead of doing nothing.
	if !m.chat.transcript.AtBottom() {
		t.Fatalf("fixture: the transcript starts at the bottom")
	}
	beforeView := ansi.Strip(m.chat.transcript.View())
	next, _ = m.chatKey(tea.KeyPressMsg{Code: tea.KeyPgUp})
	m = next.(emberModel)
	if ansi.Strip(m.chat.transcript.View()) == beforeView {
		t.Fatalf("PgUp under a goal that fits left the transcript where it was (offset %d)", m.chat.transcript.YOffset())
	}
	if m.chat.headTop != 0 {
		t.Fatalf("a goal that fits scrolled to row %d", m.chat.headTop)
	}
	// PgDn walks back down.
	up := m.chat.transcript.YOffset()
	next, _ = m.chatKey(tea.KeyPressMsg{Code: tea.KeyPgDown})
	m = next.(emberModel)
	if m.chat.transcript.YOffset() <= up {
		t.Fatalf("PgDn under a goal that fits did not page the transcript down (offset %d, was %d)",
			m.chat.transcript.YOffset(), up)
	}
}

// TestCopyingTheHeaderTakesTheWholeGoal: a selection over the header copies the goal as written, not the three lines
// the screen shows.
func TestCopyingTheHeaderTakesTheWholeGoal(t *testing.T) {
	m := longGoalChat(t, 100, 30)
	_ = m.render() // (the frame the selection maps onto)
	m.chat.sel = m.selectionAt(2, 1)
	m.chat.sel.b = [2]int{1, 20}
	m.chat.sel.moved, m.chat.sel.dragging = true, false
	got := m.selectedText()
	for _, want := range []string{"the header must expand and collapse", "select-to-copy must copy the whole goal",
		"func chatHead(w int) string", "The last line of the goal"} {
		if !strings.Contains(got, want) {
			t.Fatalf("the copy of the header lost %q:\n%s", want, got)
		}
	}
	if !strings.Contains(got, "\n") {
		t.Fatalf("the whole goal kept its lines:\n%q", got)
	}
}
