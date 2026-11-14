// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 jdominick05

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResidentGoalCommandUsesStdin(t *testing.T) {
	m := envModel{cli: []string{"caminus"}, agentName: "resident"}
	c := residentCommand(m, []string{"send", "resident", "--stdin", "--json"}, "goal with $(shell) and secrets")
	if strings.Contains(strings.Join(c.Args, " "), "secrets") || c.Stdin == nil {
		t.Fatalf("goal leaked into argv: %v", c.Args)
	}
	if strings.Join(c.Args, " ") != "caminus env agent send resident --stdin --json" {
		t.Fatal(c.Args)
	}
}

func TestResidentPaneRetainsGoalAndSanitizesOutput(t *testing.T) {
	m := envModel{agentName: "resident", resident: residentStatus{DaemonAlive: true, Goals: []residentGoal{{ID: "g", Prompt: "implement feature", State: "waiting_root"}}},
		residentEvents: []residentEvent{{Goal: "g", Kind: "assistant", Data: map[string]any{"content": "observed\x1b[2Joutput"}}}}
	view := m.residentPane()
	for _, s := range []string{"resident", "listening", "implement feature", "waiting_root", "observed"} {
		if !strings.Contains(view, s) {
			t.Fatalf("missing %q: %s", s, view)
		}
	}
	if strings.ContainsRune(residentText("observed\x1b[2Joutput"), '\x1b') {
		t.Fatal("agent terminal escape retained")
	}
}

func TestBridgeRejectsPathIDsAndIgnoresPredictableTempSymlink(t *testing.T) {
	root := t.TempDir()
	bridge := filepath.Join(root, "bridge")
	os.MkdirAll(filepath.Join(bridge, "res"), 0o770)
	target := filepath.Join(root, "protected")
	os.WriteFile(target, []byte("unchanged"), 0o600)
	writeReq(t, bridge, "ok", "id -u", 1)
	os.Symlink(target, filepath.Join(bridge, "res", "ok.json.tmp"))
	if err := writeAnswer(bridge, "human", envReq{ID: "../protected"}, "denied", "", nil, "", "", ""); err == nil {
		t.Fatal("path ID accepted")
	}
	if err := writeAnswer(bridge, "human", envReq{ID: "ok"}, "denied", "", nil, "", "", ""); err != nil {
		t.Fatal(err)
	}
	value, _ := os.ReadFile(target)
	if string(value) != "unchanged" {
		t.Fatal("symlink target overwritten")
	}
	info, _ := os.Stat(filepath.Join(bridge, "res", "ok.json"))
	if info.Mode().Perm() != 0o660 {
		t.Fatalf("bridge mode: %o", info.Mode().Perm())
	}
}

func TestSSHWarningDoesNotBecomeProtocolOutput(t *testing.T) {
	dir := t.TempDir()
	script := "#!/bin/sh\nprintf 'ControlSocket already exists; disabling multiplexing\\n' >&2\nprintf 'agent\\n'\n"
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	m := envModel{user: "agent", key: "fixture"}
	msg := probeCmd(m)().(probeMsg)
	if !msg.ok || msg.err != "" {
		t.Fatalf("warning broke successful probe: %+v", msg)
	}
	output, err := sshOutput(sshCmd(m, "jobs", "fixture"))
	if err != nil || output != "agent\n" {
		t.Fatalf("diagnostic entered protocol output: %q %v", output, err)
	}
}
