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
	if err := writeAnswer(bridge, "human", envReq{ID: "x"}, "approved", "id -u", &rc, "0\n", "", ""); err != nil {
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
	m.refreshLocal()
	v := m.render()
	for _, want := range []string{"CAMINUS ENV", "systemctl restart foo", "a approve", "ssh:", "requests", "jobs", "audit"} {
		if !strings.Contains(v, want) {
			t.Fatalf("view missing %q:\n%s", want, v)
		}
	}
	m.tab = 5
	v = m.render()
	if !strings.Contains(v, `"event":"run"`) {
		t.Fatalf("audit tab missing the event:\n%s", v)
	}
	m.tab = 4
	if !strings.Contains(m.render(), "SERVERS") {
		t.Fatal("ctx tab missing")
	}
}

func TestKeyDenyAndSkip(t *testing.T) {
	bridge := t.TempDir()
	writeReq(t, bridge, "r1", "boom", 1)
	audit := filepath.Join(t.TempDir(), "a.jsonl")
	m := envModel{user: "agent", bridge: bridge, auditPath: audit, key: "k",
		seen: map[string]bool{}, mode: "normal", width: 120, height: 40}
	m.refreshLocal()
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
	m.refreshLocal()
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
	m.refreshLocal()
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

func TestQuestionFlow(t *testing.T) {
	bridge := t.TempDir()
	writeReq(t, bridge, "q1", "", 1)
	// overwrite as a question
	q := envReq{ID: "q1", Pid: 7, User: "agent", CWD: "/home/agent", Kind: "question",
		Text: "which model?", Choices: []string{"coder", "swift"}, Ts: 1}
	data, _ := json.Marshal(q)
	if err := os.WriteFile(filepath.Join(bridge, "req", "q1.json"), data, 0o660); err != nil {
		t.Fatal(err)
	}
	audit := filepath.Join(t.TempDir(), "a.jsonl")
	m := envModel{user: "agent", bridge: bridge, auditPath: audit, key: "k",
		seen: map[string]bool{}, mode: "normal", width: 120, height: 40}
	m.refreshLocal()
	if len(m.reqs) != 1 || m.reqs[0].kind() != "question" {
		t.Fatalf("question not loaded: %+v", m.reqs)
	}
	v := m.render()
	if !strings.Contains(v, "ASK") || !strings.Contains(v, "which model?") || !strings.Contains(v, "coder | swift") {
		t.Fatalf("view missing question:\n%s", v)
	}
	mm, _ := m.onKey("a")
	m = mm.(envModel)
	if m.mode != "answer" {
		t.Fatalf("a on a question should open answer mode, mode=%s", m.mode)
	}
	mm, _ = m.onKey("2")
	m = mm.(envModel)
	mm, _ = m.onKey("enter")
	m = mm.(envModel)
	if m.mode != "normal" || len(m.reqs) != 0 {
		t.Fatalf("after enter: mode=%s reqs=%+v", m.mode, m.reqs)
	}
	res, err := os.ReadFile(filepath.Join(bridge, "res", "q1.json"))
	if err != nil {
		t.Fatal(err)
	}
	var ans map[string]any
	if json.Unmarshal(res, &ans) != nil {
		t.Fatal("bad res json")
	}
	if ans["decision"] != "answered" || ans["answer"] != "swift" {
		t.Fatalf("choice 2 should map to swift: %v", ans)
	}
}

func TestParseJobsNotesServers(t *testing.T) {
	js := parseJobs("build|123|alive|~/env-logs/build.log\nold|99|dead|~/env-logs/old.log\n")
	if len(js) != 2 || js[0].Name != "build" || js[0].State != "alive" || js[1].State != "dead" {
		t.Fatalf("parseJobs: %+v", js)
	}
	ns := parseNotes(`{"id":"n1","from":"human","text":"focus on TP"}` + "\n" + `{"id":"n2","from":"human","text":"x"}` + "\n")
	if len(ns) != 2 || ns[0].Text != "focus on TP" {
		t.Fatalf("parseNotes: %+v", ns)
	}
	ss := parseServers("PORT 8092\n" + `{"model_alias":"coder","n_ctx":262144}` + "\nPORT 8093\n" + `{"model":"x"}` + "\n")
	if len(ss) != 2 || ss[0].Port != 8092 || ss[0].Model != "coder" || ss[0].Nctx != "262144" || ss[1].Nctx != "<nil>" {
		t.Fatalf("parseServers: %+v", ss)
	}
}

func TestWriteNote(t *testing.T) {
	bridge := t.TempDir()
	id, err := writeNote(bridge, "human", "remember this")
	if err != nil {
		t.Fatal(err)
	}
	ns := parseNotes(mustRead(t, filepath.Join(bridge, "notes", id+".json")) + "\n")
	if len(ns) != 1 || ns[0].From != "human" || ns[0].Text != "remember this" {
		t.Fatalf("writeNote roundtrip: %+v", ns)
	}
}

func mustRead(t *testing.T, p string) string {
	t.Helper()
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestTabsAndJobKeys(t *testing.T) {
	m := envModel{seen: map[string]bool{}, width: 120, height: 40}
	mm, _ := m.onKey("tab")
	if mm.(envModel).tab != 1 {
		t.Fatal("tab should move to 1")
	}
	mm, _ = mm.(envModel).onKey("3")
	if mm.(envModel).tab != 2 {
		t.Fatal("key 3 should select procs tab")
	}
	// jobs tab: K on a selected job builds a kill script (via returned cmd; just check no panic and selection moves)
	m = mm.(envModel)
	m.tab = 1
	m.jobs = []job{{Name: "build", Pid: "123", State: "alive", Log: "~/env-logs/build.log"}}
	mm, _ = m.onKey("down")
	if mm.(envModel).jobSel != 0 {
		t.Fatal("jobSel should stay 0 with one job")
	}
	mm, cmd := m.onKey("K")
	if cmd == nil {
		t.Fatal("K on a job should return the kill command")
	}
	_ = mm
}
