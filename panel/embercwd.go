// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 jdominick05

package main

import (
	"encoding/json"
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
				if dir, ok := expandRuntimeVars(home, strings.Trim(g[1], `"'`)); ok {
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

// expandRuntimeVars replaces the two variables the resident's own shell sets — $WORKSPACE / ${WORKSPACE}, the
// session's home (terminal.py's env["WORKSPACE"]), and $HOME / ${HOME}, the agent account's home — in a leading
// cd's folder. ok is false when the folder holds anything else the shell would expand (another variable, a
// ${VAR:-default} modifier, a command substitution): the status line cannot know that folder, and a guessed one
// is worse than the one it already shows.
func expandRuntimeVars(home, p string) (string, bool) {
	if strings.Contains(p, "`") || strings.Contains(p, "$(") {
		return "", false
	}
	acct := path.Dir(path.Dir(home))
	var b strings.Builder
	for i := 0; i < len(p); {
		if p[i] != '$' {
			b.WriteByte(p[i])
			i++
			continue
		}
		rest, name, consumed := p[i:], "", 0
		if strings.HasPrefix(rest, "${") {
			if end := strings.IndexByte(rest, '}'); end > 2 {
				name, consumed = rest[2:end], end+1
			}
		} else {
			j := 1
			for j < len(rest) && (rest[j] == '_' || rest[j] >= '0' && rest[j] <= '9' ||
				rest[j] >= 'a' && rest[j] <= 'z' || rest[j] >= 'A' && rest[j] <= 'Z') {
				j++
			}
			if j > 1 {
				name, consumed = rest[1:j], j
			}
		}
		switch name {
		case "WORKSPACE":
			b.WriteString(home)
		case "HOME":
			b.WriteString(acct)
		default:
			return "", false
		}
		i += consumed
	}
	return b.String(), true
}

// clipPath keeps a path's end, the part that says where: "…/inventory/tests".
func clipPath(p string, w int) string {
	if ansi.StringWidth(p) <= w {
		return p
	}
	r := []rune(p)
	return "…" + string(r[len(r)-max(1, w-1):])
}
