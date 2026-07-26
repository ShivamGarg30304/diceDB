package core_test

import (
	"bytes"
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
