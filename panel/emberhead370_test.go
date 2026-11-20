// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 jdominick05

package main

// #370: three P2s from the maintainer's review of #316 — the expansion is a view of the goal it was opened on (so
// Ctrl+N or a switch to another goal hides it, and the next Esc is not eaten), the scroll hint is cut to the
// terminal's width, and the click that clears a copy left on the screen does not also toggle the goal under it.

import (
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// openHead expands the goal under the title through the key the chat really reads.
func openHead(t *testing.T, m emberModel) emberModel {
	t.Helper()
	next, _ := m.chatKey(tea.KeyPressMsg{Code: 'g', Mod: tea.ModAlt})
	m = next.(emberModel)
	if !m.headShown() {
		t.Fatalf("fixture: Alt+G did not open the goal (headOpen %v, for %q)", m.chat.headOpen, m.chat.headFor)
	}
	return m
}

// TestTheExpansionDoesNotEatEscInNewGoalMode: Ctrl+N hides the goal under the title, so the Esc after it goes back to
// the goal you were talking to instead of closing an expansion that is not on the screen; back on its goal the
// expansion is drawn again, and the Esc after that collapses it.
func TestTheExpansionDoesNotEatEscInNewGoalMode(t *testing.T) {
	m := openHead(t, longGoalChat(t, 100, 30))
	next, _ := m.chatKey(tea.KeyPressMsg{Code: 'n', Mod: tea.ModCtrl})
	m = next.(emberModel)
	if m.chat.target != "" || !m.chat.forceNew {
		t.Fatalf("fixture: Ctrl+N did not start a new goal (target %q)", m.chat.target)
	}
	if m.headShown() {
		t.Fatal("the goal is still drawn under the title over a new-goal composer")
	}
	if head := ansi.Strip(m.chatHead(100)); strings.Contains(head, "the header must expand") {
		t.Fatalf("the header draws the goal it has blanked:\n%s", head)
	}
	next, _ = m.chatKey(tea.KeyPressMsg{Code: tea.KeyEscape})
	m = next.(emberModel)
	if m.chat.target != "original" {
		t.Fatalf("Esc did not go back to the goal (target %q, message %q)", m.chat.target, m.msg)
	}
	next, _ = m.chatKey(tea.KeyPressMsg{Code: tea.KeyEscape})
	m = next.(emberModel)
	if m.headShown() {
		t.Fatal("the Esc back on the goal did not collapse the expansion")
	}
}

// TestSwitchingGoalsDoesNotExpandTheNewGoal: the header draws the selected goal, so an expansion opened on another
// goal is hidden rather than showing the wrong goal's text expanded, and the paging keys do not scroll it.
func TestSwitchingGoalsDoesNotExpandTheNewGoal(t *testing.T) {
	m := openHead(t, longGoalChat(t, 100, 30))
	m.resident.Goals = append(m.resident.Goals, residentGoal{ID: "second", State: "queued", Prompt: "A short second goal."})
	m.goalSel = 1
	if m.headShown() {
		t.Fatal("the expansion stayed open after a switch to another goal")
	}
	head := ansi.Strip(m.chatHead(100))
	if !strings.Contains(head, "A short second goal.") {
		t.Fatalf("the header does not show the selected goal:\n%s", head)
	}
	if strings.Contains(head, "the header must expand and collapse") {
		t.Fatalf("the header draws the goal the expansion was opened on:\n%s", head)
	}
	next, _ := m.chatKey(tea.KeyPressMsg{Code: tea.KeyPgDown})
	m = next.(emberModel)
	if m.chat.headTop != 0 {
		t.Fatalf("PgDn scrolled an expansion that is not on the screen (top %d)", m.chat.headTop)
	}
}

// TestTheExpandedHeadFitsNarrowTerminals: the scroll hint is cut to the frame's width as the footer's help lines are.
// Left long it wrapped at 40 or 60 columns, and the wrapped row pushed the title off the top of the screen.
func TestTheExpandedHeadFitsNarrowTerminals(t *testing.T) {
	for _, w := range []int{40, 60} {
		t.Run(fmt.Sprint(w), func(t *testing.T) {
			m := longGoalChat(t, w, 24)
			m.syncChat() // (a resize rebuilds the renderer at the width the frame is drawn at)
			m = openHead(t, m)
			head := ansi.Strip(m.chatHead(w))
			if !strings.Contains(head, "… lines") {
				t.Fatalf("the open goal lost the hint that says how to scroll:\n%s", head)
			}
			for _, row := range strings.Split(head, "\n") {
				if got := ansi.StringWidth(row); got > w {
					t.Fatalf("a header row is %d columns wide at %d: %q", got, w, row)
				}
			}
			for _, row := range strings.Split(ansi.Strip(m.render()), "\n") {
				if got := ansi.StringWidth(row); got > w {
					t.Fatalf("the frame is %d columns wide at %d: %q", got, w, row)
				}
			}
		})
	}
}

// TestTheClickThatClearsACopyDoesNotToggleTheGoal: a drag over the header copies the whole goal, and the click that
// clears that copy must only clear it. The press is over the header's rows, so clickAt's toggle used to run too.
func TestTheClickThatClearsACopyDoesNotToggleTheGoal(t *testing.T) {
	m := openHead(t, longGoalChat(t, 100, 30))
	_ = m.render() // (the frame the selection maps onto)
	// what a drag over the header leaves: the copy shown on the rows it took
	m.chat.sel = chatSelection{shown: true, x0: 0, x1: 20, y0: 1, y1: 3, a: [2]int{2, 1}, b: [2]int{2, 3}}
	next, _ := m.chatUpdate(tea.MouseClickMsg{X: 2, Y: 1, Button: tea.MouseLeft})
	m = next.(emberModel)
	if !m.chat.sel.cleared {
		t.Fatal("the press over a shown copy did not mark the click as clearing it")
	}
	next, _ = m.chatUpdate(tea.MouseReleaseMsg{X: 2, Y: 1, Button: tea.MouseLeft})
	m = next.(emberModel)
	if m.chat.sel.shown || m.chat.sel.dragging {
		t.Fatalf("the copy was not cleared (shown %v, dragging %v)", m.chat.sel.shown, m.chat.sel.dragging)
	}
	if !m.headShown() {
		t.Fatal("the click that cleared the copy also closed the goal")
	}
	// With nothing shown, a click on the header is the toggle again.
	next, _ = m.chatUpdate(tea.MouseClickMsg{X: 2, Y: 1, Button: tea.MouseLeft})
	m = next.(emberModel)
	if m.chat.sel.cleared {
		t.Fatal("a press with nothing shown is marked as clearing a copy")
	}
	next, _ = m.chatUpdate(tea.MouseReleaseMsg{X: 2, Y: 1, Button: tea.MouseLeft})
	m = next.(emberModel)
	if m.headShown() {
		t.Fatal("a plain click on the header no longer closes the goal")
	}
}
