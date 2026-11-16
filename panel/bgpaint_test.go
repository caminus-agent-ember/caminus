// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 jdominick05

package main

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

func TestBackgroundPatternsKeepTextAndWidths(t *testing.T) {
	saved := themeCurrent
	defer func() { themeCurrent = saved; applyThemeStyles() }()
	themeCurrent = defaultTheme()
	themeCurrent["bg"], themeCurrent["pattern"], themeCurrent["bg_pattern"] = "#05060a", "#3a4060", "dots"
	frame := strings.Join([]string{
		"hello world" + strings.Repeat(" ", 40),
		"\x1b[48;2;1;2;3m" + strings.Repeat(" ", 30) + "\x1b[0m" + strings.Repeat(" ", 20),
		strings.Repeat(" ", 50),
	}, "\n")
	out := paintBackground(frame, 10)
	rows := strings.Split(out, "\n")
	for i, r := range rows {
		if ansi.StringWidth(r) != ansi.StringWidth(strings.Split(frame, "\n")[i]) {
			t.Fatalf("row %d changed width: %q", i, ansi.Strip(r))
		}
	}
	if !strings.HasPrefix(ansi.Strip(rows[0]), "hello world ") {
		t.Fatalf("the gap between words and the margin after text stay plain: %q", ansi.Strip(rows[0]))
	}
	if !strings.Contains(ansi.Strip(rows[0]), "·") || !strings.Contains(ansi.Strip(rows[2]), "·") {
		t.Fatal("blank runs get the pattern")
	}
	if strings.Contains(ansi.Strip(rows[1])[:30], "·") {
		t.Fatal("a span with its own background is never painted")
	}
	// the composer and status rows (protected) get no glyphs
	if strings.Contains(ansi.Strip(paintBackground(frame, 0)), "·") {
		t.Fatal("protected rows get no pattern")
	}
	// a gradient: every row starts with its own background, and gets it back after a reset
	themeCurrent["bg_pattern"], themeCurrent["bg2"] = "gradient", "#ffffff"
	g := strings.Split(paintBackground("a\x1b[0mb\nc", 10), "\n")
	if !strings.HasPrefix(g[0], "\x1b[48;2;5;6;10m") || !strings.Contains(g[0], "\x1b[0m\x1b[48;2;5;6;10m") ||
		!strings.HasPrefix(g[1], "\x1b[48;2;255;255;255m") {
		t.Fatalf("the gradient runs top to bottom and survives a reset: %q", g)
	}
}

func TestATinyBackgroundThemeSetsTheTerminalBackground(t *testing.T) {
	saved := themeCurrent
	defer func() { themeCurrent = saved; applyThemeStyles() }()
	if err := themeLoad("sakura"); err != nil {
		t.Fatal(err)
	}
	m := chatFixture(t)
	v := m.View()
	if v.BackgroundColor == nil || v.ForegroundColor == nil {
		t.Fatal("a theme with bg sets the terminal's background and default text colour")
	}
	if err := themeLoad("caminus"); err != nil {
		t.Fatal(err)
	}
	if m.View().BackgroundColor != nil {
		t.Fatal("a theme without bg keeps the terminal's own")
	}
}
