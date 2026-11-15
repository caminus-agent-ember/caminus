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

func TestResidentSteeringCapturesGoalAndUsesStdin(t *testing.T) {
	dir := t.TempDir()
	argsPath, inputPath := filepath.Join(dir, "args"), filepath.Join(dir, "input")
	cli := filepath.Join(dir, "fake-cli")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$STEER_ARGS\"\ncat > \"$STEER_INPUT\"\nprintf '{\"queued\":true}\\n'\n"
	if err := os.WriteFile(cli, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("STEER_ARGS", argsPath)
	t.Setenv("STEER_INPUT", inputPath)
	m := envModel{cli: []string{cli}, agentName: "resident", resident: residentStatus{Goals: []residentGoal{{ID: "original", State: "working"}}}}
	next, cmd := m.residentKeys("t")
	m = next.(envModel)
	if cmd != nil || m.mode != "agentsteer" || m.steerGoal != "original" {
		t.Fatalf("steering editor did not capture the selected goal: %+v", m)
	}
	// A background refresh replaces the selection while the user types.
	m.resident.Goals = []residentGoal{{ID: "different", State: "working"}}
	message := "Try another approach; literal $(text) stays in stdin."
	m.edit = message
	next, cmd = m.submitInput()
	m = next.(envModel)
	if cmd == nil || m.steerGoal != "" || m.mode != "normal" {
		t.Fatal("submission state was not cleared")
	}
	if result := cmd().(residentActionMsg); strings.Contains(result.text, "error") {
		t.Fatal(result.text)
	}
	args, _ := os.ReadFile(argsPath)
	input, _ := os.ReadFile(inputPath)
	// The steering draft is the explicit form: it redirects the goal, so it carries --steer.
	if string(args) != "env\nagent\nsend\nresident\n--goal-id\noriginal\n--steer\n--stdin\n--json\n" || string(input) != message {
		t.Fatalf("wrong steering destination or input: %q / %q", args, input)
	}
}

func TestResidentSteeringRejectsFinishedGoalsAndCancelsDraft(t *testing.T) {
	for _, state := range []string{"completed", "cancelled"} {
		m := envModel{resident: residentStatus{Goals: []residentGoal{{ID: "finished", State: state}}}}
		next, cmd := m.residentKeys("t")
		if cmd != nil || next.(envModel).mode == "agentsteer" {
			t.Fatalf("steering offered for %s", state)
		}
	}
	m := envModel{mode: "agentsteer", steerGoal: "original", edit: "a draft"}
	next, cmd := m.onKey("esc")
	if cmd != nil || next.(envModel).steerGoal != "" || next.(envModel).edit != "" {
		t.Fatal("escape sent or retained a steering draft")
	}
	m = envModel{mode: "agentsteer", steerGoal: "original", edit: "  "}
	_, cmd = m.submitInput()
	if cmd != nil {
		t.Fatal("blank steering became a send")
	}
}

func TestResidentPaneShowsHumanSteering(t *testing.T) {
	m := envModel{agentName: "resident", resident: residentStatus{Goals: []residentGoal{{ID: "g", State: "working"}}},
		residentEvents: []residentEvent{{Goal: "g", Kind: "user_message", Data: map[string]any{"text": "Use the installed utility."}}}}
	if !strings.Contains(m.residentPane(), "Use the installed utility.") {
		t.Fatal("human steering absent from activity feed")
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

func TestPanelCallsAreBoundedSoAStallCannotHangTheComposer(t *testing.T) {
	m := chatFixture(t)
	m.cli = []string{"/bin/true"}
	if cmd := residentCommand(m, []string{"status", "resident", "--json"}, ""); cmd == nil || cmd.Cancel == nil {
		t.Fatal("a resident CLI call must carry a deadline")
	}
	m.user, m.key = "resident", "/nonexistent-key"
	if cmd := sshCmd(m, "jobs", "true"); cmd.Cancel == nil {
		t.Fatal("an SSH poll must carry a deadline")
	}
}
