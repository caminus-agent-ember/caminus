// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 jdominick05

package main

// envthemes.go: the theme screen (/theme with no name). Themes were a name typed blind (/theme NAME) out of a list
// of six. Now the screen lists every theme with a strip of its colours, and beside it every element the chat draws,
// in the theme under the cursor: moving previews it across the whole screen, Enter keeps it, Esc goes back to the
// one you had.

import (
	"os"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

type themeScreen struct {
	open  bool
	names []string
	sel   int
	was   string // the theme in force when the screen opened (Esc restores it)
}

// themeInUse is the saved theme's name ("caminus" when none was chosen).
func themeInUse() string {
	if p := themeSavePath(); p != "" {
		if b, err := os.ReadFile(p); err == nil {
			if n := strings.TrimSpace(string(b)); n != "" {
				return n
			}
		}
	}
	return "caminus"
}

func (m envModel) openThemes() envModel {
	names := themeNames()
	was := themeInUse()
	sel := 0
	for i, n := range names {
		if n == was {
			sel = i
		}
	}
	m.chat.themes = themeScreen{open: true, names: names, sel: sel, was: was}
	m.chat.compose.Blur()
	return m
}

// previewTheme puts a theme in force without saving it, and restyles what caches its colours.
func (m *envModel) previewTheme(name string) {
	if themeLoad(name) != nil {
		return
	}
	m.chat.markdown = nil
	m.chat.content, m.chat.rendered = "", 0
	m.chatStyles()
	m.syncChat()
}

func (m envModel) themesKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	t := &m.chat.themes
	n := len(t.names)
	switch msg.String() {
	case "up", "k", "shift+tab":
		if n > 0 {
			t.sel = (t.sel - 1 + n) % n
			m.previewTheme(t.names[t.sel])
		}
	case "down", "j", "tab":
		if n > 0 {
			t.sel = (t.sel + 1) % n
			m.previewTheme(t.names[t.sel])
		}
	case "enter":
		if n > 0 {
			name := t.names[t.sel]
			if err := themeSet(name); err != nil {
				m.msg = "could not load " + name + ": " + err.Error()
			} else {
				m.msg = "theme: " + name
			}
			m.previewTheme(name)
		}
		m.chat.themes = themeScreen{}
		return m, m.chat.compose.Focus()
	case "esc", "q":
		m.previewTheme(t.was)
		m.chat.themes = themeScreen{}
		return m, m.chat.compose.Focus()
	case "ctrl+c":
		return m, tea.Quit
	}
	return m, nil
}

// themeSwatch is a strip of a theme's colours, read from its file without putting it in force.
func themeSwatch(name string) string {
	saved := themeCurrent
	defer func() { themeCurrent = saved }()
	if themeLoad(name) != nil {
		return ""
	}
	var b strings.Builder
	for _, role := range []string{"accent", "ok", "warn", "action", "hot", "purple", "user", "teal"} {
		b.WriteString(lipgloss.NewStyle().Foreground(themeColor(role)).Render("█"))
	}
	return b.String()
}

// themesOverlay: the list on the left, the elements in the theme under the cursor on the right.
func (m envModel) themesOverlay(w, h int) string {
	t := m.chat.themes
	listW := 34
	// the swatches load each theme's file: put the previewed one back in force afterwards
	current := t.names[t.sel]
	var list []string
	for i, n := range t.names {
		mark := "  "
		name := lipgloss.NewStyle().Foreground(themeColor("muted")).Render(n)
		if i == t.sel {
			mark = lipgloss.NewStyle().Foreground(themeColor("accent")).Bold(true).Render("› ")
			name = lipgloss.NewStyle().Foreground(themeColor("text")).Bold(true).Render(n)
		}
		if n == t.was {
			name += envDim.Render(" (yours)")
		}
		sw := themeSwatch(n)
		list = append(list, mark+name+strings.Repeat(" ", max(1, listW-10-ansi.StringWidth(ansi.Strip(mark+name))))+sw)
	}
	_ = themeLoad(current)
	applyThemeStyles()
	room := max(4, h-6)
	top := 0
	if t.sel >= room {
		top = t.sel - room + 1
	}
	list = list[top:min(len(list), top+room)]
	pw := max(30, w-listW-8)
	preview := themePreview(current, pw)
	left := lipgloss.NewStyle().Width(listW).Render(strings.Join(list, "\n"))
	body := lipgloss.JoinHorizontal(lipgloss.Top, left, "  ", preview)
	return overlayBox("THEMES  "+current, body, "↑↓ preview · Enter keep · Esc back", w, h)
}

// themePreview draws one of every element the chat uses, in the theme in force.
func themePreview(name string, w int) string {
	fgc := func(role string) lipgloss.Style { return lipgloss.NewStyle().Foreground(themeColor(role)) }
	var out []string
	add := func(s ...string) { out = append(out, s...) }
	add(shellHeader("resident", "working", "add tests for the parser and make them pass", 42, w))
	add("")
	add(fgc("user").Render(g.user+" ")+fgc("text").Render("add tests for the parser, then make them pass"), "")
	add(envThink.Render("  I should read the parser first and see which cases have no test."), "")
	add(fgc("ok").Render(g.agent+" ")+fgc("text").Render("The parser has no test for empty input. I'll add one in ")+
		envCode.Render("parser_test.go")+fgc("text").Render("."), "")
	add("  "+envTool.Render(g.tool+" terminal")+"  "+fgc("muted").Render("go test ./parser/..."))
	add("  "+fgc("muted").Render("ok   parser  0.41s"), "")
	add(formatText("markdown", "```go\nfunc TestEmpty(t *testing.T) {\n\tif _, err := Parse(\"\"); err == nil {\n\t\tt.Fatal(\"want an error\")\n\t}\n}\n```", w)...)
	add("")
	add(envWarn.Render("? the agent asks: may I install golangci-lint?"))
	add(envCtxHot.Render("✗ the gate fails: 1 test failed"))
	add(todoLine([]todoItem{{"read the parser", "done"}, {"add the tests", "now"}, {"open a PR", "next"}}, w))
	add(envSel.Render(" a selected row "), "")
	add(borderTitle(envBorder.Render(fgc("muted").Render("a framed box: menus, the composer, Ember's thoughts")), "a title", envDim))
	add(footBar([]string{g.model + " " + name, "120K/1M", "$0.42"}, []string{"esc browse", "/help"}, w))
	return strings.Join(out, "\n")
}
