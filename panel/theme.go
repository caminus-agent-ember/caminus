// Copyright (C) 2026 jdominick05

package main

import "charm.land/lipgloss/v2"

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
