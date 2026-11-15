// Copyright (C) 2026 jdominick05

package main

import (
	"charm.land/glamour/v2/ansi"
	"charm.land/lipgloss/v2"
)

// The panel's one color theme. Every view — monitor, control panel and the
// resident env views — draws from this palette, so a role (accent, healthy,
// needs-you, failing, chrome) always means the same hue on screen. Nord:
// the monitor shipped with it first and the rest of the app follows now.

// ---------- palette ----------

var (
	cDim    = lipgloss.Color("#3b4252") // rules, empty slots, background chrome
	cMuted  = lipgloss.Color("#6b7489") // resting text: hints, echoes, output
	cText   = lipgloss.Color("#d8dee9") // foreground text
	cAccent = lipgloss.Color("#88c0d0") // the app's frost: titles, active panes
	cGreen  = lipgloss.Color("#a3be8c") // healthy: ready, agent voice, bar at rest
	cYellow = lipgloss.Color("#ebcb8b") // needs you or attention: questions, pauses, an approaching compaction
	cOrange = lipgloss.Color("#d08770") // action: tools running, warnings one step from failure
	cRed    = lipgloss.Color("#bf616a") // failure: hot context, errors
	cPurple = lipgloss.Color("#b48ead") // reserved for a distinct second stream of data
	cBlue   = lipgloss.Color("#81a1c1") // the human: goals, steering, your words
	cTeal   = lipgloss.Color("#8fbcbb") // quiet positive chrome
	cGray   = lipgloss.Color("#4c566a") // selection background, borders
)

// ---------- markdown ----------

// envMarkdownStyle is the transcript's markdown in the same palette as the views: headings take the roles
// (frost for h1, blue for h2, teal for h3), inline code the teal accent on the darkest chrome, and fenced
// blocks the Nord chroma theme, so code in a reply is colored the way the monitor colors it.
func envMarkdownStyle() ansi.StyleConfig {
	color := func(s string) *string { return &s }
	flag := func(b bool) *bool { return &b }
	indent := func(u uint) *uint { return &u }

	var st ansi.StyleConfig
	st.Document.Margin = indent(0)
	st.Document.Color = color("#d8dee9")

	st.Heading.Color = color("#88c0d0")
	st.Heading.Bold = flag(true)
	st.H1.Color = color("#88c0d0")
	st.H1.Bold = flag(true)
	st.H2.Color = color("#81a1c1")
	st.H2.Bold = flag(true)
	st.H3.Color = color("#8fbcbb")
	st.H3.Bold = flag(true)
	st.H4.Color = color("#b48ead")
	st.H5.Color = color("#a3be8c")
	st.H6.Color = color("#6b7489")

	st.Emph.Italic = flag(true)
	st.Strong.Bold = flag(true)
	st.Strikethrough.CrossedOut = flag(true)
	st.Link.Color = color("#81a1c1")
	st.LinkText.Color = color("#88c0d0")
	st.LinkText.Underline = flag(true)

	st.Code.Color = color("#8fbcbb")
	st.Code.BackgroundColor = color("#3b4252")
	st.CodeBlock.Theme = "nord"
	st.CodeBlock.Color = color("#d8dee9")
	st.CodeBlock.BackgroundColor = color("#2e3440")
	st.CodeBlock.Margin = indent(1)

	st.BlockQuote.Color = color("#6b7489")
	st.BlockQuote.Indent = indent(1)
	st.HorizontalRule.Color = color("#3b4252")
	st.Item.Color = color("#88c0d0")
	st.Enumeration.Color = color("#88c0d0")
	st.Table.Color = color("#4c566a")
	st.Table.CenterSeparator = color("┼")
	st.Table.ColumnSeparator = color("│")
	st.Table.RowSeparator = color("─")
	return st
}
