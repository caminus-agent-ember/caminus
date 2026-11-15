// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 jdominick05

package main

// envpages.go: the old tabbed screens' content as overlays over the chat. The chat is the only screen: memory and
// context, audit and settings open in the shared overlay box (shell.go) with its keys (↑↓ move or scroll,
// PgUp/PgDn, Esc closes, q never quits), and a typed prompt (edit a root command, answer a question, leave a note,
// name a session) sits in the composer's slot. Nothing draws the old tab bar any more.

import (
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// pageOverlays maps the slash command to the old page whose content it shows, and its title.
var pageOverlays = map[string]struct {
	tab   int
	title string
}{
	"/memory":   {3, "MEMORY & NOTES"},
	"/context":  {4, "CONTEXT"},
	"/audit":    {5, "AUDIT"},
	"/settings": {8, "SETTINGS"},
}

// pageTitle is the overlay title for a page index.
func pageTitle(tab int) string {
	for _, p := range pageOverlays {
		if p.tab == tab {
			return p.title
		}
	}
	return ""
}

// openPage opens an old page's content as an overlay; it fetches what the page needs, as switching to it did.
func (m envModel) openPage(tab int) (envModel, tea.Cmd) {
	m.chat.page, m.chat.pageTop = tab, 0
	m.chat.compose.Blur()
	saved := m.tab
	m.tab = tab
	m.refreshLocal()
	cmds := sshRefresh(m)
	if tab == 3 {
		cmds = append(cmds, residentSessions(m))
	}
	m.tab = saved
	return m, tea.Batch(cmds...)
}

// pageOverlay draws the open page in the overlay box, scrolled to pageTop.
func (m envModel) pageOverlay(w, h int) string {
	saved := m.tab
	m.tab = m.chat.page
	content := m.pane()
	m.tab = saved
	inner := max(10, w-6)
	lines := strings.Split(ansi.Hardwrap(residentText(content), inner, true), "\n")
	room := max(1, h-6)
	top := max(0, min(m.chat.pageTop, len(lines)-room))
	body := strings.Join(lines[top:min(top+room, len(lines))], "\n")
	keys := "↑↓ PgUp PgDn scroll"
	return overlayBox(pageTitle(m.chat.page), body, keys, w, h)
}

func (m envModel) pageKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	k := msg.String()
	switch k {
	case "ctrl+c":
		return m, tea.Quit
	case "esc":
		m.chat.page = -1
		return m, m.chat.compose.Focus()
	case "pgup":
		m.chat.pageTop = max(0, m.chat.pageTop-max(1, m.height-10))
		return m, nil
	case "pgdown":
		m.chat.pageTop += max(1, m.height-10)
		return m, nil
	}
	switch k {
	case "up", "k":
		m.chat.pageTop = max(0, m.chat.pageTop-1)
	case "down", "j":
		m.chat.pageTop++
	}
	return m, nil
}

// promptTitles names the typed prompts that replace the composer while they are open.
var promptTitles = map[string]string{
	"edit":       "edit the root command · Enter runs it under sudo · Esc cancels",
	"answer":     "answer the question · Enter sends · a number picks a choice · Esc cancels",
	"sessionnew": "new session name · Enter creates it · Esc cancels",
}

// promptBox is a typed prompt in the composer's slot, in the composer's style with its purpose on the border.
func (m envModel) promptBox(w int) string {
	title, ok := promptTitles[m.mode]
	if !ok {
		return ""
	}
	text := envBusy.Render(g.user) + " " + terminalText(m.edit, false) + "█"
	if m.mode == "answer" && m.editRequest != nil {
		text = envDim.Render(ansi.Truncate(residentText(m.editRequest.Text), max(8, w-6), "…")) + "\n" + text
	}
	return borderTitle(lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(cYellow).Padding(0, 1).Width(w).Render(text), title, envWarn)
}
