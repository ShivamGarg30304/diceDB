package core_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/shivam30303/diceDB/core"
)

func TestTYPECommand(t *testing.T) {
	buf := new(bytes.Buffer)

	// Test non-existing key -> +none\r\n
	cmdTypeNone := core.RedisCmds{{Cmd: "TYPE", Args: []string{"non_existing_key"}}}
	core.EvalAndRespond(cmdTypeNone, buf)
	if buf.String() != "+none\r\n" {
		t.Fatalf("expected +none\\r\\n, got %q", buf.String())
	}

	buf.Reset()

	// Set a string key
	cmdSet := core.RedisCmds{{Cmd: "SET", Args: []string{"mykey", "myval"}}}
	core.EvalAndRespond(cmdSet, buf)
	buf.Reset()

	// Test existing string key -> +string\r\n
	cmdTypeString := core.RedisCmds{{Cmd: "TYPE", Args: []string{"mykey"}}}
	core.EvalAndRespond(cmdTypeString, buf)
	if buf.String() != "+string\r\n" {
		t.Fatalf("expected +string\\r\\n, got %q", buf.String())
	}
}

func evalCmd(t *testing.T, buf *bytes.Buffer, cmd string, args ...string) string {
	t.Helper()
	buf.Reset()
	core.EvalAndRespond(core.RedisCmds{{Cmd: cmd, Args: args}}, buf)
	return buf.String()
}

func TestRENAMECommand(t *testing.T) {
	buf := new(bytes.Buffer)

	// Setup: set source key
	evalCmd(t, buf, "SET", "src", "hello")

	// Case 1: basic rename src -> dst
	resp := evalCmd(t, buf, "RENAME", "src", "dst")
	if resp != "+OK\r\n" {
		t.Fatalf("case 1: expected +OK, got %q", resp)
	}
	// src should no longer exist
	resp = evalCmd(t, buf, "GET", "src")
	if resp != "$-1\r\n" {
		t.Fatalf("case 1: src should be gone, got %q", resp)
	}
	// dst should have the value
	resp = evalCmd(t, buf, "GET", "dst")
	if !strings.Contains(resp, "hello") {
		t.Fatalf("case 1: dst should contain 'hello', got %q", resp)
	}

	// Case 2: rename to same key (no-op — key must still exist)
	evalCmd(t, buf, "SET", "samekey", "value")
	resp = evalCmd(t, buf, "RENAME", "samekey", "samekey")
	if resp != "+OK\r\n" {
		t.Fatalf("case 2: expected +OK, got %q", resp)
	}
	resp = evalCmd(t, buf, "GET", "samekey")
	if !strings.Contains(resp, "value") {
		t.Fatalf("case 2: key should still exist after rename-to-self, got %q", resp)
	}

	// Case 3: rename non-existing key -> error
	resp = evalCmd(t, buf, "RENAME", "ghost", "dst2")
	if !strings.Contains(resp, "ERR no such key") {
		t.Fatalf("case 3: expected ERR no such key, got %q", resp)
	}

	// Case 4: rename overwrites existing destination key
	evalCmd(t, buf, "SET", "k1", "val1")
	evalCmd(t, buf, "SET", "k2", "val2")
	resp = evalCmd(t, buf, "RENAME", "k1", "k2")
	if resp != "+OK\r\n" {
		t.Fatalf("case 4: expected +OK, got %q", resp)
	}
	resp = evalCmd(t, buf, "GET", "k2")
	if !strings.Contains(resp, "val1") {
		t.Fatalf("case 4: k2 should now hold val1, got %q", resp)
	}
	resp = evalCmd(t, buf, "GET", "k1")
	if resp != "$-1\r\n" {
		t.Fatalf("case 4: k1 should be gone, got %q", resp)
	}
}
