package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mmx/internal/controller"
)

func TestConnectionOptionIsSavedForEveryNodeAndResume(t *testing.T) {
	for _, command := range []string{"dryrun", "run", "run-command"} {
		t.Run(command, func(t *testing.T) {
			root := oneTimeProject(t)
			writeProjectFile(t, "inventories/example.yml", "nodes:\n  app01: {address: app01.example.net}\n  app02: {address: app02.example.net, connection: https_with_ssh_fallback}\n  app03: {address: app03.example.net, connection: ssh_only}\n")
			// A refused SSH connection leaves unfinished work for resume,
			// without contacting any real node.
			bin := t.TempDir()
			writeProgramFileThroughShellChild(t, filepath.Join(bin, "ssh"), "#!/bin/sh\necho 'connection refused by test node' >&2\nexit 255\n")
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			args := []string{command, "site", "--limit", "app01"}
			if command == "run-command" {
				args = []string{command, "--nodes", "app01"}
			}
			args = append(args, "--connection", "ssh_only", "--json", "--non-interactive")
			if command == "run-command" {
				args = append(args, "--", "true")
			}
			out, code, err := captureCommand(t, args...)
			if code != 1 || err != nil {
				t.Fatal(code, err, out)
			}
			var result struct{ ID string }
			if err := json.Unmarshal([]byte(out), &result); err != nil || result.ID == "" {
				t.Fatal(err, out)
			}
			c := controller.Controller{State: filepath.Join(root, ".mmx")}
			saved, err := c.Load(result.ID, false)
			if err != nil {
				t.Fatal(err)
			}
			if len(saved.Inventory.Nodes) != 3 {
				t.Fatal(saved.Inventory.Nodes)
			}
			for name, node := range saved.Inventory.Nodes {
				if node.Transport != "ssh_only" {
					t.Fatalf("%s: connection %q", name, node.Transport)
				}
			}
			if command == "dryrun" {
				return
			}
			// Resume uses the recorded inventory even if the current file
			// cannot be loaded at all.
			writeProjectFile(t, "inventories/example.yml", "invalid inventory")
			out, code, err = captureCommand(t, "resume", result.ID, "--json", "--non-interactive")
			if code != 1 || err != nil {
				t.Fatal(code, err, out)
			}
			resumed, err := c.Load(result.ID, true)
			if err != nil || resumed.Inventory.Nodes["app01"].Transport != "ssh_only" || !strings.Contains(resumed.Entries[0].Response.Error, "resolve SSH configuration for app01.example.net") {
				t.Fatal(resumed, err)
			}
		})
	}
}

func TestCheckOneTimePlaybooksConnectionOptionReachesEveryDryrun(t *testing.T) {
	oneTimeProject(t)
	standIn := useNodesStandIn(t, nil)
	if out, code, err := captureCommand(t, "check-one-time-playbooks", "--connection", "ssh_only", "--json"); code != 0 || err != nil {
		t.Fatal(code, err, out)
	}
	if len(standIn.dryruns) != 2 {
		t.Fatal(standIn.playbooks())
	}
	for _, r := range standIn.dryruns {
		for name, node := range r.Inventory.Nodes {
			if node.Transport != "ssh_only" {
				t.Fatalf("%s / %s: connection %q", r.Project.PlaybookName, name, node.Transport)
			}
		}
	}
}

// complete-one-time-playbook runs the same check, so it takes the option too.
func TestCompleteOneTimePlaybookConnectionOptionReachesItsDryrun(t *testing.T) {
	oneTimeProject(t)
	standIn := useNodesStandIn(t, nil)
	if out, code, err := captureCommand(t, "complete-one-time-playbook", stopOldMetricsAgent, "--connection", "ssh_only", "--json"); code != 0 || err != nil {
		t.Fatal(code, err, out)
	}
	if len(standIn.dryruns) != 1 {
		t.Fatal(standIn.playbooks())
	}
	for name, node := range standIn.dryruns[0].Inventory.Nodes {
		if node.Transport != "ssh_only" {
			t.Fatalf("%s: connection %q", name, node.Transport)
		}
	}
}

func TestConnectionOptionCommandErrors(t *testing.T) {
	for _, command := range []string{"dryrun", "run", "run-command", "check-one-time-playbooks", "complete-one-time-playbook"} {
		for _, value := range []string{"https_with_ssh_fallback", "https", ""} {
			if code, err := run(context.Background(), []string{command, "--connection=" + value}); code != 4 || err == nil || !strings.Contains(err.Error(), "--connection accepts only ssh_only") {
				t.Errorf("%s %q: %d %v", command, value, code, err)
			}
		}
	}
	for _, command := range []string{"resume", "validate", "explain", "nodes", "status", "stop", "results"} {
		want := command + " does not accept --connection"
		if command == "resume" {
			want = "resume uses the recorded run's connection choice and accepts no --connection; start a new mmx run"
		}
		if code, err := run(context.Background(), []string{command, "--connection", "ssh_only"}); code != 4 || err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %d %v", command, code, err)
		}
	}
	for _, command := range []string{"init", "version", "describe-module", "module-usage", "secrets", "new-one-time-playbook", "module-json"} {
		if code, err := run(context.Background(), []string{command, "--connection", "ssh_only"}); code != 4 || err == nil {
			t.Errorf("%s accepted --connection: %d %v", command, code, err)
		}
	}
	out, code, err := captureCommand(t)
	if code != 0 || err != nil || !strings.Contains(out, "--connection ssh_only (dryrun, run, run-command, check-one-time-playbooks, complete-one-time-playbook)") {
		t.Fatal(code, err, out)
	}
}
