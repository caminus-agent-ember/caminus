// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 jdominick05

package main

import (
	"encoding/json"
	"os"
	"path"
	"regexp"
	"strings"

	"github.com/charmbracelet/x/ansi"
)

var leadingCd = regexp.MustCompile(`^\s*cd\s+("[^"]+"|'[^']+'|[^\s;&|]+)\s*(&&|;)`)

// agentCwd is the folder the agent works in: the runtime keeps no shell between calls, each terminal call runs in its
// cwd (the session's home when none is given), so it is the newest terminal call's, with a leading "cd DIR &&" taken
// into account. The agent account's home reads "~". Empty when the status has not named the home yet.
func (m emberModel) agentCwd() string {
	home := m.resident.Home
	if home == "" {
		return ""
	}
	cwd := home
	for i := len(m.residentEvents) - 1; i >= 0; i-- {
		e := m.residentEvents[i]
		if e.Kind != "assistant" {
			continue
		}
		calls, _ := e.Data["tool_calls"].([]any)
		found := false
		for j := len(calls) - 1; j >= 0 && !found; j-- {
			c, _ := calls[j].(map[string]any)
			f, _ := c["function"].(map[string]any)
			if name, _ := f["name"].(string); name != "terminal" {
				continue
			}
			var args struct {
				Cwd     string `json:"cwd"`
				Command string `json:"command"`
			}
			raw, _ := f["arguments"].(string)
			if json.Unmarshal([]byte(raw), &args) != nil {
				continue
			}
			found = true
			cwd = resolveIn(home, home, args.Cwd)
			if g := leadingCd.FindStringSubmatch(args.Command); g != nil {
				if dir, ok := expandWord(home, g[1]); ok {
					cwd = resolveIn(home, cwd, dir)
				}
			}
		}
		if found {
			break
		}
	}
	// the agent account's home is the parent of homes/NAME
	if dir, base := path.Split(path.Dir(home)); base == "homes" {
		acct := strings.TrimSuffix(dir, "/")
		if cwd == acct {
			return "~"
		}
		if strings.HasPrefix(cwd, acct+"/") {
			return "~/" + strings.TrimPrefix(cwd, acct+"/")
		}
	}
	return cwd
}

// resolveIn resolves p against base, with ~ as the agent account's home (two levels above the session home).
func resolveIn(home, base, p string) string {
	switch {
	case p == "":
		return base
	case p == "~" || strings.HasPrefix(p, "~/"):
		return path.Clean(path.Join(path.Dir(path.Dir(home)), strings.TrimPrefix(p, "~")))
	case strings.HasPrefix(p, "/"):
		return path.Clean(p)
	}
	return path.Clean(path.Join(base, p))
}

// expandWord expands the variables the resident's own shell sets (WORKSPACE, the session's home from terminal.py's
// env["WORKSPACE"], and HOME, the agent account's home) in a leading cd's folder, following the shell's quote
// rules: a variable inside single quotes is a literal part of the name (the shell expands nothing there), and a
// word stitched from quoted and unquoted pieces is expanded piece by piece with no quote left behind. ok is false
// when the folder holds anything the shell would expand and this status line cannot know (another variable, a
// ${VAR:-default} modifier, a command substitution, an unclosed quote): a guessed folder is worse than the one it
// already shows.
func expandWord(home, word string) (string, bool) {
	acct := path.Dir(path.Dir(home))
	bad := false
	get := func(name string) string {
		switch name {
		case "WORKSPACE":
			return home
		case "HOME":
			return acct
		}
		bad = true
		return ""
	}
	var out, run strings.Builder
	// flush expands the run of characters the shell would expand (unquoted, or inside double quotes). A command
	// substitution is not something this status line can know, and os.Expand would hand it back as literal text,
	// which reads as a folder that does not exist.
	flush := func() bool {
		seg := run.String()
		if seg == "" {
			return true
		}
		run.Reset()
		if strings.Contains(seg, "`") || strings.Contains(seg, "$(") {
			return false
		}
		out.WriteString(os.Expand(seg, get))
		return !bad
	}
	quote := byte(0) // 0 unquoted, '\'' inside single quotes, '"' inside double quotes
	for i := 0; i < len(word); i++ {
		c := word[i]
		switch {
		case c == '\'' && quote != '"':
			if quote == '\'' {
				quote = 0
				continue
			}
			if !flush() {
				return "", false
			}
			quote = '\''
		case c == '"' && quote != '\'':
			if quote == '"' {
				quote = 0
				continue
			}
			if !flush() {
				return "", false
			}
			quote = '"'
		case c == '\\' && quote != '\'' && i+1 < len(word):
			// the escape names a literal character, so it ends the expandable run
			if !flush() {
				return "", false
			}
			i++
			if quote == '"' && !strings.ContainsRune("$`\"\\", rune(word[i])) {
				out.WriteByte('\\') // inside double quotes a backslash is literal before any other character
			}
			out.WriteByte(word[i])
		case quote == '\'':
			out.WriteByte(c)
		default:
			run.WriteByte(c)
		}
	}
	if quote != 0 || !flush() {
		return "", false
	}
	return out.String(), true
}

// clipPath keeps a path's end, the part that says where: "…/inventory/tests".
func clipPath(p string, w int) string {
	if ansi.StringWidth(p) <= w {
		return p
	}
	r := []rune(p)
	return "…" + string(r[len(r)-max(1, w-1):])
}
