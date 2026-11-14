// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 jdominick05

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeReq(t *testing.T, bridge, id, cmd string, ts float64) {
	t.Helper()
	for _, d := range []string{"req", "res"} {
		if err := os.MkdirAll(filepath.Join(bridge, d), 0o770); err != nil {
			t.Fatal(err)
		}
	}
	data, _ := json.Marshal(envReq{ID: id, Pid: 42, User: "agent", CWD: "/home/agent", Cmd: cmd, Ts: ts})
	if err := os.WriteFile(filepath.Join(bridge, "req", id+".json"), data, 0o660); err != nil {
		t.Fatal(err)
	}
}

func TestLoadRequestsSkipsAnsweredAndSorts(t *testing.T) {
	bridge := t.TempDir()
	writeReq(t, bridge, "b", "second", 200)
	writeReq(t, bridge, "a", "first", 100)
	writeReq(t, bridge, "c", "answered", 300)
	if err := os.WriteFile(filepath.Join(bridge, "res", "c.json"), []byte(`{"id":"c"}`), 0o660); err != nil {
		t.Fatal(err)
	}
	reqs, err := loadRequests(bridge)
	if err != nil {
		t.Fatal(err)
	}
	if len(reqs) != 2 || reqs[0].ID != "a" || reqs[1].ID != "b" {
		t.Fatalf("want [a b] oldest first, got %+v", reqs)
	}
	if reqs[0].Cmd != "first" || reqs[0].User != "agent" {
		t.Fatalf("fields not parsed: %+v", reqs[0])
	}
}

func TestWriteAnswerAtomicAndSchema(t *testing.T) {
	bridge := t.TempDir()
	writeReq(t, bridge, "x", "id -u", 1)
	rc := 0
	if err := writeAnswer(bridge, "human", envReq{ID: "x"}, "approved", "id -u", &rc, "0\n", ""); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(bridge, "res", "x.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	if m["decision"] != "approved" || m["approved_by"] != "human" || m["rc"].(float64) != 0 || m["out"] != "0\n" {
		t.Fatalf("bad answer: %v", m)
	}
	if _, err := os.Stat(filepath.Join(bridge, "res", "x.json.tmp")); !os.IsNotExist(err) {
		t.Fatal("tmp file left behind")
	}
	// and loadRequests now hides it
	reqs, _ := loadRequests(bridge)
	if len(reqs) != 0 {
		t.Fatalf("answered request still queued: %+v", reqs)
	}
}

func TestAppendAuditMatchesPythonSchema(t *testing.T) {
	p := filepath.Join(t.TempDir(), "state", "env-audit.jsonl")
	if err := appendAudit(p, map[string]any{"event": "root_decision", "id": "x", "decision": "denied"}); err != nil {
		t.Fatal(err)
	}
	lines := tailLines(p, 10)
	if len(lines) != 1 {
		t.Fatalf("want 1 line, got %d", len(lines))
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &m); err != nil {
		t.Fatal(err)
	}
	if m["event"] != "root_decision" || m["decision"] != "denied" {
		t.Fatalf("bad event: %v", m)
	}
	if _, ok := m["ts"].(float64); !ok {
		t.Fatal("ts missing or not a number (python audit.log writes float ts)")
	}
	if got := tailLines(p, 0); len(got) != 0 {
		t.Fatal("tail 0 should be empty")
	}
	if got := tailLines(filepath.Join(t.TempDir(), "missing.jsonl"), 5); len(got) != 0 {
		t.Fatal("missing file should read empty")
	}
}

func TestExecRootArgv(t *testing.T) {
	c, tmp, err := execRoot("rm -rf /tmp/x && echo 'hi there'")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmp)
	if filepath.Base(c.Path) != "bash" || c.Args[1] != "-c" {
		t.Fatalf("want bash -c, got %v", c.Args)
	}
	s := c.Args[2]
	if !strings.Contains(s, "set -o pipefail") || !strings.Contains(s, "sudo sh -c 'rm -rf /tmp/x && echo '\\''hi there'\\'''") {
		t.Fatalf("script does not quote the command safely: %s", s)
	}
	if !strings.Contains(s, "tee "+shQuote(tmp)) {
		t.Fatalf("script does not tee to the temp file: %s", s)
	}
}

func TestShQuote(t *testing.T) {
	if got := shQuote("a'b"); got != `'a'\''b'` {
		t.Fatalf("shQuote: %s", got)
	}
}

func TestViewShowsQueueAndAudit(t *testing.T) {
	bridge := t.TempDir()
	writeReq(t, bridge, "r1", "systemctl restart foo", 1)
	audit := filepath.Join(t.TempDir(), "a.jsonl")
	if err := appendAudit(audit, map[string]any{"event": "run", "cmd": "id"}); err != nil {
		t.Fatal(err)
	}
	m := envModel{user: "agent", bridge: bridge, auditPath: audit, key: "k",
		seen: map[string]bool{}, mode: "normal", width: 120, height: 40, msg: "hello"}
	m.refresh()
	v := m.render()
	for _, want := range []string{"CAMINUS ENV", "systemctl restart foo", `"event":"run"`, "a approve", "ssh:"} {
		if !strings.Contains(v, want) {
			t.Fatalf("view missing %q:\n%s", want, v)
		}
	}
}

func TestKeyDenyAndSkip(t *testing.T) {
	bridge := t.TempDir()
	writeReq(t, bridge, "r1", "boom", 1)
	audit := filepath.Join(t.TempDir(), "a.jsonl")
	m := envModel{user: "agent", bridge: bridge, auditPath: audit, key: "k",
		seen: map[string]bool{}, mode: "normal", width: 120, height: 40}
	m.refresh()
	// deny
	mm, _ := m.onKey("d")
	m = mm.(envModel)
	if len(m.reqs) != 0 {
		t.Fatalf("denied request still visible: %+v", m.reqs)
	}
	data, _ := os.ReadFile(filepath.Join(bridge, "res", "r1.json"))
	var ans map[string]any
	if err := json.Unmarshal(data, &ans); err != nil || ans["decision"] != "denied" {
		t.Fatalf("deny did not write the answer: %s err=%v", data, err)
	}
	// skip keeps it on disk but hides it this session
	writeReq(t, bridge, "r2", "later", 2)
	m.refresh()
	if len(m.reqs) != 1 || m.reqs[0].ID != "r2" {
		t.Fatalf("want r2 only, got %+v", m.reqs)
	}
	mm, _ = m.onKey("s")
	m = mm.(envModel)
	if len(m.reqs) != 0 {
		t.Fatal("skip did not hide the request")
	}
	if _, err := os.Stat(filepath.Join(bridge, "res", "r2.json")); !os.IsNotExist(err) {
		t.Fatal("skip must not write an answer")
	}
	// edit mode: type, backspace, enter would exec (not run here); esc cancels
	writeReq(t, bridge, "r3", "cmd3", 3)
	m = envModel{user: "agent", bridge: bridge, auditPath: audit, key: "k",
		seen: map[string]bool{"r2": true}, mode: "normal", width: 120, height: 40}
	m.refresh()
	if len(m.reqs) != 1 || m.reqs[0].ID != "r3" {
		t.Fatalf("want r3 selected, got %+v", m.reqs)
	}
	m.sel = 0
	mm, _ = m.onKey("e")
	m = mm.(envModel)
	if m.mode != "edit" || m.edit != "cmd3" {
		t.Fatalf("edit mode: %+v", m)
	}
	mm, _ = m.onKey("x")
	m = mm.(envModel)
	mm, _ = m.onKey("backspace")
	m = mm.(envModel)
	if m.edit != "cmd3" {
		t.Fatalf("typing+backspace: %q", m.edit)
	}
	mm, _ = m.onKey("esc")
	m = mm.(envModel)
	if m.mode != "normal" {
		t.Fatal("esc did not leave edit mode")
	}
}
