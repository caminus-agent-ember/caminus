package main

// shell.go: the building blocks of the chat-first env shell (the redesign): glyph tiers, the header, tool cards, the
// inline approval card, the todo line, the overlay box, the status line, and the streaming renderer that keeps a live
// reply cheap to watch. Everything here is a pure function of its inputs and the live theme, so the mockups
// (shell_mockup_test.go) and the real views draw the same thing.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// ---------------------------------------------------------------- glyph tiers

// glyphs is one tier's symbols. unicode is the default and needs no special font; nerd needs a Nerd Font (e.g.
// MesloLGS NF from the powerlevel10k README) and adds icons and powerline segments; ascii is for terminals that
// render neither. A terminal cannot report its font, so the tier is the owner's setting (/glyphs cycles it).
type glyphs struct {
	name                                 string
	working, waiting, done, idle, failed string // goal states
	collapsed, expanded                  string // cards
	pass, fail                           string // gates
	todoDone, todoNow, todoNext          string
	user, agent, tool, edit, gate, root  string // card kinds
	model, clock, cost, speed, queue     string // status line
	segL, segR                           string // status segment edges (powerline in nerd)
	bar, barEmpty                        string
	spinner                              []string
}

var glyphTiers = map[string]glyphs{
	"unicode": {name: "unicode", working: "●", waiting: "◐", done: "✓", idle: "○", failed: "✗",
		collapsed: "▸", expanded: "▾", pass: "✓", fail: "✗", todoDone: "●", todoNow: "◐", todoNext: "○",
		user: "›", agent: "◆", tool: "$", edit: "±", gate: "✓", root: "⚿",
		model: "◇", clock: "◷", cost: "$", speed: "⇢", queue: "≡", segL: "", segR: " · ",
		bar: "▓", barEmpty: "░", spinner: []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}},
	"nerd": {name: "nerd", working: "", waiting: "", done: "", idle: "", failed: "",
		collapsed: "", expanded: "", pass: "", fail: "", todoDone: "", todoNow: "", todoNext: "",
		user: "", agent: "\U000f06a9", tool: "", edit: "", gate: "", root: "",
		model: "", clock: "", cost: "", speed: "", queue: "", segL: "", segR: "",
		bar: "█", barEmpty: "░", spinner: []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}},
	"ascii": {name: "ascii", working: "*", waiting: "~", done: "+", idle: "o", failed: "x",
		collapsed: ">", expanded: "v", pass: "+", fail: "x", todoDone: "[x]", todoNow: "[~]", todoNext: "[ ]",
		user: ">", agent: "*", tool: ">", edit: "+-", gate: "+", root: "#",
		model: "", clock: "", cost: "$", speed: "", queue: "", segL: "", segR: " | ",
		bar: "#", barEmpty: ".", spinner: []string{"|", "/", "-", "\\"}},
}

// unicode is the shipped default: it needs no special font, so it works in any terminal and over SSH from a phone.
// nerd is a per-user choice saved by /glyphs (most terminals have no Nerd Font and would show empty boxes).
var g = glyphTiers["unicode"]

// init applies the user's saved tier (CAMINUS_GLYPHS wins).
func init() {
	name := strings.TrimSpace(os.Getenv("CAMINUS_GLYPHS"))
	if name == "" {
		if p := glyphsSavePath(); p != "" {
			if b, err := os.ReadFile(p); err == nil {
				name = strings.TrimSpace(string(b))
			}
		}
	}
	if t, ok := glyphTiers[name]; ok {
		g = t
	}
}

// glyphsSet switches the tier and saves it, like themeSet does for colours.
func glyphsSet(name string) error {
	t, ok := glyphTiers[name]
	if !ok {
		return fmt.Errorf("unknown glyphs %q (nerd, unicode, ascii)", name)
	}
	g = t
	if p := glyphsSavePath(); p != "" {
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		_ = os.WriteFile(p, []byte(name+"\n"), 0o644)
	}
	return nil
}

func glyphsSavePath() string {
	if d, err := os.UserConfigDir(); err == nil {
		return filepath.Join(d, "caminus", "panel-glyphs")
	}
	return ""
}

// glyphsNext is the tier after the current one, for /glyphs without a name.
func glyphsNext() string {
	order := []string{"nerd", "unicode", "ascii"}
	for i, n := range order {
		if n == g.name {
			return order[(i+1)%len(order)]
		}
	}
	return "unicode"
}

// ---------------------------------------------------------------- pieces

// stateStyle is the one accent per state: working blue, waiting amber, needs-you red, done green.
func stateStyle(state string) (string, lipgloss.Style) {
	switch state {
	case "working", "queued":
		return g.working, lipgloss.NewStyle().Foreground(cBlue)
	case "waiting_model", "waiting_root", "paused":
		return g.waiting, lipgloss.NewStyle().Foreground(cYellow)
	case "waiting_user", "error":
		return g.failed, lipgloss.NewStyle().Foreground(cRed)
	case "completed", "free", "dream":
		return g.done, lipgloss.NewStyle().Foreground(cGreen)
	}
	return g.idle, lipgloss.NewStyle().Foreground(cMuted)
}

// shellHeader: the agent, its state, what it is doing, and on the right the step count.
func shellHeader(agent, state, goal string, steps, w int) string {
	icon, st := stateStyle(state)
	detail := ""
	if goal != "" {
		detail = lipgloss.NewStyle().Foreground(cBlue).Render(goal)
	}
	return screenHeader(agent, icon+" "+strings.ReplaceAll(state, "_", " "), st, detail, envDim.Render(fmt.Sprintf("%d steps", steps)), w)
}

// screenHeader is every screen's first two lines (the env chat, caminus panel, caminus top): CAMINUS · what · its
// state in its one colour · dim details, something on the right, and a chrome rule under it.
func screenHeader(what, state string, st lipgloss.Style, detail, right string, w int) string {
	left := envTitle.Render("CAMINUS") + envDim.Render(" · ") + lipgloss.NewStyle().Foreground(cText).Render(what)
	if state != "" {
		left += " " + st.Render(state)
	}
	if detail != "" {
		left += envDim.Render(" · ") + detail
	}
	gap := w - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 1 {
		left = ansi.Truncate(left, max(1, w-lipgloss.Width(right)-2), "…")
		gap = max(1, w-lipgloss.Width(left)-lipgloss.Width(right))
	}
	return left + strings.Repeat(" ", gap) + right + "\n" + lipgloss.NewStyle().Foreground(cGray).Render(strings.Repeat("─", w))
}

// shellMessage: a turn of the conversation, with a thin rule instead of a box.
func shellMessage(who, text string, w int) string {
	var mark, name string
	var st lipgloss.Style
	if who == "you" {
		mark, name, st = g.user, "you", lipgloss.NewStyle().Foreground(cBlue).Bold(true)
	} else {
		mark, name, st = g.agent, who, lipgloss.NewStyle().Foreground(cGreen).Bold(true)
	}
	label := st.Render(mark+" "+name) + "  "
	lw := lipgloss.Width(label)
	body := lipgloss.NewStyle().Foreground(cText).Render(ansi.Wordwrap(text, max(10, w-lw), ""))
	lines := strings.Split(body, "\n")
	out := []string{label + lines[0]}
	for _, l := range lines[1:] {
		out = append(out, strings.Repeat(" ", lw)+l)
	}
	return strings.Join(out, "\n")
}

// toolCard: one line per tool call (kind, what, result, time); expanded, its body under a dim rule.
func toolCard(kind, what, meta string, ok bool, expanded bool, body []string, w int) string {
	icon, st := g.tool, envTool
	switch kind {
	case "edit":
		icon, st = g.edit, lipgloss.NewStyle().Foreground(cPurple)
	case "gate":
		icon, st = g.gate, envCtxOK
		if !ok {
			icon, st = g.fail, envCtxHot
		}
	case "error":
		icon, st = g.fail, envCtxHot
	case "output", "result":
		icon, st = "", envDim
		if !ok {
			icon, st = g.fail, envWarn
		}
	case "note":
		icon, st = g.idle, envDim
	case "read":
		icon, st = g.collapsed, envDim
	case "free", "dream":
		icon, st = g.done, envCtxOK
	}
	label := strings.TrimSpace(icon + " " + kind)
	fold := g.collapsed
	if expanded {
		fold = g.expanded
	}
	line := envDim.Render(fold) + " " + st.Render(label) + "  " + lipgloss.NewStyle().Foreground(cText).Render(what)
	if meta != "" {
		line += envDim.Render("  ·  " + meta)
	}
	line = ansi.Truncate(line, w, "…")
	if !expanded || len(body) == 0 {
		return line
	}
	rule := lipgloss.NewStyle().Foreground(cGray).Render("│ ")
	out := []string{line}
	for _, b := range body {
		st := envDim
		switch {
		case strings.HasPrefix(b, "+"):
			st = envCtxOK
		case strings.HasPrefix(b, "-"):
			st = envCtxHot
		}
		// an open card shows everything: long lines wrap under the rule, nothing is cut
		for _, piece := range strings.Split(ansi.Hardwrap(b, max(4, w-6), true), "\n") {
			out = append(out, "   "+rule+st.Render(piece))
		}
	}
	return strings.Join(out, "\n")
}

// approvalCard: a root request or a question, inline where the human is already looking.
func approvalCard(kind, cmd, why string, w int) string {
	return approvalCardKeys(kind, cmd, why, true, w)
}

// approvalCardKeys draws the card; inline false says where to decide instead of offering keys the view does not
// handle yet (the chat decides root requests in /requests until its own keys are wired).
func approvalCardKeys(kind, cmd, why string, inline bool, w int) string {
	title := g.root + " ROOT"
	if kind == "question" {
		title = "? QUESTION"
	}
	keys := envDim.Render("esc, then ") + envWarn.Render("a a") + envDim.Render(" approve  ") + envWarn.Render("g g") +
		envDim.Render(" grant for the goal  ") + envWarn.Render("e") +
		envDim.Render(" edit  ") + envWarn.Render("d") + envDim.Render(" deny  ·  ") + envWarn.Render("/deny TEXT") +
		envDim.Render(" says what to do instead")
	if !inline {
		keys = envDim.Render("decide in ") + envWarn.Render("/requests")
	}
	inner := lipgloss.NewStyle().Foreground(cText).Bold(true).Render(cmd) + "\n" +
		envDim.Render("why: "+why) + "\n" + keys
	box := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(cYellow).Padding(0, 1).
		Width(min(w, 72)).Render(inner)
	// the title rides on the top border
	return borderTitle(box, title, envWarn.Bold(true))
}

// borderTitle writes a title into a rendered box's top border, replacing the rule there: "⚿ ROOT" on the approval
// card, the composer's target. A box too narrow for the title keeps its plain border, so a narrow terminal loses the
// label and never the frame.
func borderTitle(box, title string, st lipgloss.Style) string {
	if title == "" {
		return box
	}
	label := st.Render(" " + title + " ")
	lines := strings.Split(box, "\n")
	if len(lines) == 0 || lipgloss.Width(lines[0]) <= lipgloss.Width(label)+4 {
		return box
	}
	top := lines[0]
	lines[0] = ansi.Truncate(top, 2, "") + label + ansi.TruncateLeft(top, 2+lipgloss.Width(label), "")
	return strings.Join(lines, "\n")
}

// todoLine: the agent's plan, pinned above the composer.
type todoItem struct {
	text  string
	state string // done, now, next
}

func todoLine(items []todoItem, w int) string {
	parts := []string{envDim.Render("todo ")}
	for _, it := range items {
		switch it.state {
		case "done":
			parts = append(parts, envCtxOK.Render(g.todoDone)+" "+envDim.Render(it.text))
		case "now":
			parts = append(parts, envBusy.Render(g.todoNow)+" "+lipgloss.NewStyle().Foreground(cText).Render(it.text))
		default:
			parts = append(parts, envDim.Render(g.todoNext+" "+it.text))
		}
	}
	return ansi.Truncate(strings.Join(parts, "  "), w, "…")
}

// composer: where the human types. The target rides on the top border, the mark sits on the first line only, and the
// hint says what Enter does now.
func composer(draft, hint, title string, w int) string {
	text := envDim.Render(hint)
	if draft != "" {
		text = lipgloss.NewStyle().Foreground(cText).Render(draft) + "█"
	}
	return composerBox(envBusy.Render(g.user)+" "+text, title, w)
}

// composerBox frames the editor's rendered lines as the composer: a rounded box in the chrome colour, with the target
// on its top border. The caller draws the prompt mark (the editor's own, so a wrapped line keeps its indent).
func composerBox(input, title string, w int) string {
	return borderTitle(envBorder.Padding(0, 1).Width(w).Render(input), title, envDim)
}

// statusLine: brain, context, money and speed, then the keys that matter now. In the nerd tier the segments are
// powerline pills; elsewhere they are joined by a dot.
type status struct {
	brain        string
	used, window int
	compact      float64 // the compaction threshold as a fraction of the window; the bar warms toward it
	usd          float64 // this goal's spend; negative hides the segment (an unpriced brain)
	usdDay       float64 // today's spend across goals; shown with usd when positive
	tps          float64
	queue        int
	keys         []string
}

func statusLine(s status, w int) string {
	frac := 0.0
	if s.window > 0 {
		frac = float64(s.used) / float64(s.window)
	}
	cells := 8
	full := min(cells, int(frac*float64(cells)+0.5))
	trigger := s.compact
	if trigger <= 0 || trigger > 1 {
		trigger = .85
	}
	barSt := envCtxOK
	if frac >= trigger*.85 {
		barSt = envWarn
	}
	if frac >= trigger {
		barSt = envCtxHot
	}
	bar := barSt.Render(strings.Repeat(g.bar, full)) + envDim.Render(strings.Repeat(g.barEmpty, cells-full))
	segs := []string{
		strings.TrimSpace(g.model + " " + s.brain),
		envDim.Render(fmt.Sprintf("%s/%s ", short(s.used), short(s.window))) + bar,
	}
	if s.usd >= 0 {
		seg := strings.TrimSpace(fmt.Sprintf("%s%.2f", g.cost, s.usd))
		if s.usdDay > 0 {
			seg += envDim.Render(fmt.Sprintf(" · %.2f today", s.usdDay))
		}
		segs = append(segs, seg)
	}
	if s.tps > 0 {
		segs = append(segs, strings.TrimSpace(fmt.Sprintf("%s %.0f tok/s", g.speed, s.tps)))
	}
	if s.queue > 0 {
		segs = append(segs, strings.TrimSpace(fmt.Sprintf("%s %d queued", g.queue, s.queue)))
	}
	return footBar(segs, s.keys, w)
}

// footBar is the status line every screen ends with (the env chat, caminus panel): the segments as pills on the left
// (plain dim text with separators under the plain glyph set), the keys that matter now, dim, on the right.
func footBar(segs, keyList []string, w int) string {
	var left string
	if g.name == "nerd" {
		pill := lipgloss.NewStyle().Background(cGray).Foreground(cText)
		edge := lipgloss.NewStyle().Foreground(cGray)
		for _, sg := range segs {
			left += edge.Render(g.segL) + pill.Render(sg) + edge.Render(g.segR) + " "
		}
	} else {
		// each segment styled on its own: a segment with its own colours (the bar) would end the outer style early
		styled := make([]string, len(segs))
		for i, sg := range segs {
			if strings.Contains(sg, "\x1b[") {
				styled[i] = sg
			} else {
				styled[i] = envDim.Render(sg)
			}
		}
		left = strings.Join(styled, envDim.Render(g.segR))
	}
	keys := envDim.Render(strings.Join(keyList, "  "))
	gap := w - lipgloss.Width(left) - lipgloss.Width(keys)
	if gap < 2 {
		return ansi.Truncate(left, w, "…")
	}
	return left + strings.Repeat(" ", gap) + keys
}

func short(n int) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.0fM", float64(n)/1e6)
	case n >= 1000:
		return fmt.Sprintf("%dK", n/1000)
	}
	return fmt.Sprint(n)
}

// overlayBox: every overlay is the same box with the same keys; Esc always closes.
func overlayBox(title, body, keys string, w, h int) string {
	inner := envTitle.Render(title) + "\n" + lipgloss.NewStyle().Foreground(cGray).Render(strings.Repeat("─", max(4, w-4))) +
		"\n" + body
	lines := strings.Split(inner, "\n")
	for len(lines) < h-4 {
		lines = append(lines, "")
	}
	if len(lines) > h-4 {
		lines = lines[:h-4]
	}
	lines = append(lines, envDim.Render(ansi.Truncate(keys+"  ·  esc close", max(8, w-4), "…")))
	return lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(cAccent).Padding(0, 1).
		Width(w).Render(strings.Join(lines, "\n"))
}

// ---------------------------------------------------------------- streaming renderer

// streamView keeps a live conversation cheap to watch. Finished blocks are rendered once (markdown and all) and
// kept as lines; only the live tail is formatted while tokens arrive, with a cheap incremental formatter (wrapping and
// a little inline styling, carrying fence state), and it gets its one full render when it completes. Deltas are
// buffered; the view is rebuilt at most once per frame, and only the visible lines are joined.
type streamView struct {
	width   int
	done    []string // rendered lines of finished blocks
	live    strings.Builder
	inFence bool
	speaker string                     // who is talking in the live block
	frame   int                        // spinner frame, advanced once per drawn frame by the caller
	render  func(string, int) []string // the full renderer for a finished block (glamour in the panel)
}

func newStreamView(width int, render func(string, int) []string) *streamView {
	return &streamView{width: width, render: render, speaker: "resident"}
}

// Append adds a delta to the live block (cheap: no rendering here).
func (v *streamView) Append(delta string) { v.live.WriteString(delta) }

// Finish renders the live block once and moves it to the cache.
func (v *streamView) Finish() {
	if v.live.Len() == 0 {
		return
	}
	v.done = append(v.done, v.render(v.live.String(), v.width)...)
	v.live.Reset()
	v.inFence = false
}

// liveLines formats the live tail cheaply: soft wrap, code fences dimmed, no markdown parse.
func (v *streamView) liveLines() []string {
	var out []string
	fence := v.inFence
	for _, raw := range strings.Split(v.live.String(), "\n") {
		if strings.HasPrefix(strings.TrimSpace(raw), "```") {
			fence = !fence
			out = append(out, envDim.Render(raw))
			continue
		}
		st := lipgloss.NewStyle().Foreground(cText)
		if fence {
			st = envCode
		}
		for _, l := range strings.Split(ansi.Wordwrap(raw, max(8, v.width-3), ""), "\n") {
			out = append(out, st.Render(l))
		}
	}
	return out
}

// View returns the last h lines: cached finished lines plus the formatted live tail, under its speaker's label.
func (v *streamView) View(h int) string {
	live := v.liveLines()
	if v.live.Len() > 0 {
		label := lipgloss.NewStyle().Foreground(cGreen).Bold(true).Render(g.agent+" "+v.speaker) + " " + envBusy.Render(g.spinner[v.frame%len(g.spinner)])
		live = append([]string{label}, live...)
	}
	need := h - len(live)
	var head []string
	if need > 0 {
		start := max(0, len(v.done)-need)
		head = v.done[start:]
	} else {
		live = live[len(live)-h:]
	}
	return strings.Join(append(append([]string{}, head...), live...), "\n")
}
