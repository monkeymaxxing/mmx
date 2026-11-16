package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"testing/iotest"

	"mmx/internal/config"
	"mmx/internal/controller"
	"mmx/internal/durable"
	"mmx/internal/protocol"
)

// Text that a node or a web server sends can hold terminal escape sequences.
// Human output shows them as visible escapes; --json keeps the exact text.
func TestHumanOutputEscapesControlCharactersFromNodes(t *testing.T) {
	const (
		cursorMove = "\x1b[2A\x1b[2K"
		clipboard  = "\x1b]52;c;aGFja2Vk\x07"
		title      = "\x1b]0;all fine\x07"
		c1         = "\u009b31m"
	)
	root := initializedProject(t)
	state := filepath.Join(root, ".mmx")
	r, err := controller.Controller{State: state}.New(context.Background(), config.Project{HistoryRetention: "forever"}, config.Inventory{}, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	result := protocol.Result{
		Task: "call", Status: "failed", Change: "unknown",
		Stdout:       "out" + cursorMove + "\nsecond\tline\n",
		Stderr:       "err" + clipboard + "\n",
		Reason:       "reason" + title,
		ResponseBody: "body" + c1 + "\x7f\n",
		ExitCode:     1, HTTPStatusCode: 200,
		FileChange: &protocol.FileChange{Action: "update", Path: "/etc/x", Diff: "-old\n+new" + cursorMove + "\n"},
	}
	response := protocol.Response{Status: "failed", Error: "node said: " + clipboard, Results: []protocol.Result{result}}
	r.Status = "failed"
	r.Entries = []controller.Entry{{FleetStage: "setup", Node: "web01", JobID: "j", NodeJobAttempt: 1, Response: response}}
	if err := durable.JSON(filepath.Join(state, "runs", r.ID, "run.json"), r); err != nil {
		t.Fatal(err)
	}

	out, _, err := captureCommand(t, "results", r.ID, "--diff")
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{"\x1b", "\x07", "\u009b", "\x7f"} {
		if strings.Contains(out, raw) {
			t.Fatalf("human output holds raw %q:\n%q", raw, out)
		}
	}
	for _, want := range []string{
		`out\x1b[2A\x1b[2K` + "\n", "second\tline",
		`err\x1b]52;c;aGFja2Vk\a`, `reason\x1b]0;all fine\a`,
		`response_body: body\u009b31m\x7f`, `node said: \x1b]52;c;aGFja2Vk\a`,
		`+new\x1b[2A\x1b[2K`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("human output lacks %q:\n%s", want, out)
		}
	}

	jsonOut, _, err := captureCommand(t, "results", r.ID, "--json")
	if err != nil {
		t.Fatal(err)
	}
	var view struct {
		Entries []struct {
			Response protocol.Response
		}
	}
	if err := json.Unmarshal([]byte(jsonOut), &view); err != nil {
		t.Fatal(err)
	}
	got := view.Entries[0].Response.Results[0]
	if got.Stdout != result.Stdout || got.Stderr != result.Stderr || got.Reason != result.Reason || got.ResponseBody != "body"+c1+"\x7f\n" {
		t.Fatalf("--json changed the text: %+v", got)
	}
}

// An error message that quotes node output is escaped where mmx prints it.
func TestErrorMessageQuotingNodeOutputIsEscaped(t *testing.T) {
	var out bytes.Buffer
	printError(&out, errors.New("ssh said: \x1b]0;pwned\x07\nsecond line"))
	if got, want := out.String(), "mmx: ssh said: \\x1b]0;pwned\\a\nsecond line\n"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestEscapeControlCharacters(t *testing.T) {
	for in, want := range map[string]string{
		"plain é — ok\n\t": "plain é — ok\n\t",
		"\x1b[31m":         `\x1b[31m`,
		"\r":               `\r`,
		"\x00\x7f":         `\x00\x7f`,
		"\u0085\u009f":     `\u0085\u009f`,
		"a\xe2\x82":        `a\xe2\x82`,
		"\u00a0\u00a1":     "\u00a0\u00a1",
	} {
		if got := escapeControlCharacters(in); got != want {
			t.Errorf("%q: got %q, want %q", in, got, want)
		}
	}
}

// A full difference is copied in chunks; a character split between chunks is
// not mistaken for invalid bytes.
func TestCopyEscapedKeepsCharactersSplitBetweenReads(t *testing.T) {
	var out bytes.Buffer
	if err := copyEscaped(&out, iotest.OneByteReader(strings.NewReader("é—\x1b[2Kok\xe2"))); err != nil {
		t.Fatal(err)
	}
	if got, want := out.String(), `é—\x1b[2Kok\xe2`; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}
