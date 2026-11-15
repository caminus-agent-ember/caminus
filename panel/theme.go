// Copyright (C) 2026 jdominick05

package main

import (
	"charm.land/glamour/v2/ansi"
	"charm.land/lipgloss/v2"
)

// The panel's one color theme. Every view — monitor, control panel and the
// resident env views — draws from this palette, so a role (accent, healthy,
// needs-you, failing, chrome) always means the same hue on screen.
//
// The base is white text on the terminal's own background, grey for what is
// secondary, and color spent only on what the eye should find first: a title,
// the agent's voice, a running tool, a question, a failure.

// ---------- palette ----------

const (
	hexText   = "#ffffff" // what you read: replies, prompts, values
	hexMuted  = "#a8adb6" // resting text: hints, echoes, output
	hexThink  = "#cfd4dc" // thinking: dimmer than the reply, still comfortable to read
	hexDim    = "#5c626d" // rules, empty slots, chrome that recedes
	hexAccent = "#4fd6ff" // bright cyan: titles, the active pane
	hexGreen  = "#4dff9e" // healthy: ready, the agent's own voice
	hexYellow = "#ffd75f" // needs you: questions, pauses, a nearing compaction
	hexOrange = "#ffa25f" // action: tools running
	hexRed    = "#ff5f6d" // failure: errors, hot context
	hexPurple = "#c98fff" // a distinct second stream of data
	hexBlue   = "#6fa8ff" // the human: goals, steering, your words
	hexTeal   = "#4fffd0" // quiet positive chrome, inline code
	hexGray   = "#4a4f5a" // selection background, borders
)

var (
	cDim    = lipgloss.Color(hexDim)
	cMuted  = lipgloss.Color(hexMuted)
	cThink  = lipgloss.Color(hexThink)
	cText   = lipgloss.Color(hexText)
	cAccent = lipgloss.Color(hexAccent)
	cGreen  = lipgloss.Color(hexGreen)
	cYellow = lipgloss.Color(hexYellow)
	cOrange = lipgloss.Color(hexOrange)
	cRed    = lipgloss.Color(hexRed)
	cPurple = lipgloss.Color(hexPurple)
	cBlue   = lipgloss.Color(hexBlue)
	cTeal   = lipgloss.Color(hexTeal)
	cGray   = lipgloss.Color(hexGray)
)

// ---------- markdown ----------

// envMarkdownStyle is the transcript's markdown in the same palette as the views: body text white, headings
// in the bright roles (cyan, blue, teal), inline code teal on a dark plate, links blue and underlined, and
// fenced blocks through a bright chroma theme, so code in a reply is colored rather than dimmed.
func envMarkdownStyle() ansi.StyleConfig {
	color := func(s string) *string { return &s }
	flag := func(b bool) *bool { return &b }
	indent := func(u uint) *uint { return &u }

	var st ansi.StyleConfig
	st.Document.Margin = indent(0)
	st.Document.Color = color(hexText)

	st.Heading.Color = color(hexAccent)
	st.Heading.Bold = flag(true)
	st.H1.Color = color(hexAccent)
	st.H1.Bold = flag(true)
	st.H2.Color = color(hexBlue)
	st.H2.Bold = flag(true)
	st.H3.Color = color(hexTeal)
	st.H3.Bold = flag(true)
	st.H4.Color = color(hexPurple)
	st.H5.Color = color(hexGreen)
	st.H6.Color = color(hexMuted)

	st.Emph.Italic = flag(true)
	st.Strong.Bold = flag(true)
	st.Strikethrough.CrossedOut = flag(true)
	st.Link.Color = color(hexBlue)
	st.LinkText.Color = color(hexBlue)
	st.LinkText.Underline = flag(true)

	st.Code.Color = color(hexTeal)
	st.Code.BackgroundColor = color("#2a2e35")
	st.CodeBlock.Theme = "dracula"
	st.CodeBlock.Color = color(hexText)
	st.CodeBlock.BackgroundColor = color("#16181d")
	st.CodeBlock.Margin = indent(1)

	st.BlockQuote.Color = color(hexMuted)
	st.BlockQuote.Indent = indent(1)
	st.HorizontalRule.Color = color(hexDim)
	st.Item.Color = color(hexAccent)
	st.Enumeration.Color = color(hexAccent)
	st.Table.Color = color(hexGray)
	st.Table.CenterSeparator = color("┼")
	st.Table.ColumnSeparator = color("│")
	st.Table.RowSeparator = color("─")
	return st
}
