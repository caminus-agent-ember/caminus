// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 jdominick05

package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// Matches resident/approval.py: persistent lock inode, withdrawn tombstone,
// executing claim before sudo, and token-matched completion. Never replay claims.
func withBridgeLock(bridge, id string, fn func() error) error {
	if !validBridgeID(id) {
		return fmt.Errorf("invalid bridge request id")
	}
	parent := filepath.Join(bridge, "req")
	path := filepath.Join(parent, id+".lock")
	flags := os.O_RDWR | syscall.O_NOFOLLOW | syscall.O_NONBLOCK
	f, err := os.OpenFile(path, flags|os.O_CREATE|os.O_EXCL, 0o660)
	if os.IsExist(err) {
		f, err = os.OpenFile(path, flags, 0)
	} else if err == nil {
		info, e := os.Stat(parent)
		if e == nil {
			e = f.Chmod(0o660)
		}
		if e == nil {
			if stat, ok := info.Sys().(*syscall.Stat_t); ok {
				e = f.Chown(-1, int(stat.Gid))
			}
		}
		if e != nil {
			f.Close()
			return e
		}
	}
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("bridge lock must be a regular file")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if err != syscall.EAGAIN && err != syscall.EWOULDBLOCK {
			return err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("bridge request is busy; try again")
		}
		time.Sleep(10 * time.Millisecond)
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return fn()
}

func requestDigest(data []byte) string { return fmt.Sprintf("%x", sha256.Sum256(data)) }

func checkPendingRequest(bridge string, r envReq) (envReq, error) {
	var current envReq
	if _, err := os.Lstat(filepath.Join(bridge, "res", r.ID+".json")); !os.IsNotExist(err) {
		return current, fmt.Errorf("request already answered, withdrawn or executing")
	}
	data, err := os.ReadFile(filepath.Join(bridge, "req", r.ID+".json"))
	if err != nil {
		return current, err
	}
	if err = json.Unmarshal(data, &current); err != nil {
		return current, err
	}
	if current.ID != r.ID {
		return current, fmt.Errorf("bridge request id mismatch")
	}
	if r.Digest != "" && requestDigest(data) != r.Digest {
		return current, fmt.Errorf("request changed; review it again")
	}
	return current, nil
}

func claimRoot(bridge, actor string, r envReq, decision, cmd string) (string, error) {
	token := ""
	err := withBridgeLock(bridge, r.ID, func() error {
		current, err := checkPendingRequest(bridge, r)
		if err != nil {
			return err
		}
		if current.kind() != "root" || (decision != "approved" && decision != "edited") || cmd == "" {
			return fmt.Errorf("invalid root decision")
		}
		if decision == "approved" && current.Cmd != cmd {
			return fmt.Errorf("approved command changed")
		}
		nonce := make([]byte, 16)
		if _, err = rand.Read(nonce); err != nil {
			return err
		}
		token = fmt.Sprintf("%x", nonce)
		value := map[string]any{"id": r.ID, "decision": "executing", "claim": token, "final_decision": decision,
			"approved_by": actor, "cmd": cmd, "ts": float64(time.Now().UnixMilli()) / 1000}
		data, _ := json.Marshal(value)
		return writeBridgeJSON(filepath.Join(bridge, "res", r.ID+".json"), data)
	})
	return token, err
}

func finishRoot(bridge, id, token string, rc int, out string) error {
	return withBridgeLock(bridge, id, func() error {
		path := filepath.Join(bridge, "res", id+".json")
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var value map[string]any
		if err = json.Unmarshal(data, &value); err != nil {
			return err
		}
		if token == "" || value["decision"] != "executing" || value["claim"] != token {
			return fmt.Errorf("root execution claim changed; inspect the audit log")
		}
		value["decision"] = value["final_decision"]
		delete(value, "final_decision")
		value["rc"], value["out"], value["err"], value["ts"] = rc, out, "", float64(time.Now().UnixMilli())/1000
		data, _ = json.Marshal(value)
		return writeBridgeJSON(path, data)
	})
}
