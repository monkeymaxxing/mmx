package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"

	"mmx/internal/config"
	"mmx/internal/secrets"
)

// secretKeyUsage is one secret key in the answer of mmx secrets usage: where
// its value comes from and every place that uses it.
type secretKeyUsage struct {
	Key string `json:"key"`
	// Source is nil for a key that is referenced but not defined.
	Source *secretKeySource   `json:"source"`
	Uses   []config.SecretUse `json:"uses"`
	// OnlyReadByFleetStagesLeftOutBeforeTheRun marks a key that no run with
	// the inventory needs: only fleet stages that the run leaves out before
	// it starts read it (config.SecretUsage).
	OnlyReadByFleetStagesLeftOutBeforeTheRun bool `json:"only_read_by_fleet_stages_left_out_before_the_run,omitempty"`
}

// secretKeySource is where a key's value comes from: the vault file, or the
// environment variable that secrets_from_env binds the key to.
type secretKeySource struct {
	Vault  string `json:"vault,omitempty"`
	EnvVar string `json:"env_var,omitempty"`
}

// secretsUsage shows where each secret key is used across the project. It is
// local, and it reads keys only: it unlocks the vault to learn which keys the
// vault defines, and a project whose vault file does not exist needs no
// passphrase. It exits 1 when a referenced key that a run with the
// inventory needs is not defined.
func secretsUsage(ctx context.Context, o secretsOptions) (int, error) {
	if len(o.arguments) > 1 {
		return 4, fmt.Errorf("mmx secrets usage takes at most one key")
	}
	project, err := config.FindProject(".")
	if err != nil {
		return 4, err
	}
	if err := config.CheckOfficialExtraModulesVersion(config.OfficialExtraModulesDirectory()); err != nil {
		return 4, err
	}
	usage, err := config.SecretUsageOf(project, o.inventory)
	if err != nil {
		return 4, err
	}
	// A key that secrets_from_env binds reads the environment
	// variable instead of the vault entry.
	sources := map[string]*secretKeySource{}
	if _, err := os.Stat(usage.Vault); err == nil {
		password, err := o.store(ctx).Password(false)
		if err != nil {
			return secretsFailure(err)
		}
		values, err := secrets.Read(usage.Vault, password)
		if err != nil {
			return secretsFailure(err)
		}
		for key := range values {
			sources[key] = &secretKeySource{Vault: usage.Vault}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return 4, err
	}
	for key, variable := range usage.FromEnvironment {
		sources[key] = &secretKeySource{EnvVar: variable}
	}
	names := map[string]bool{}
	for key := range sources {
		names[key] = true
	}
	for key := range usage.Uses {
		names[key] = true
	}
	view := struct {
		SchemaVersion             int              `json:"schema_version"`
		Keys                      []secretKeyUsage `json:"keys"`
		ReferencedButNotDefined   []string         `json:"referenced_but_not_defined"`
		DefinedButNeverReferenced []string         `json:"defined_but_never_referenced"`
	}{1, []secretKeyUsage{}, []string{}, []string{}}
	for _, key := range slices.Sorted(maps.Keys(names)) {
		if len(o.arguments) == 1 && key != o.arguments[0] {
			continue
		}
		uses := usage.Uses[key]
		if uses == nil {
			uses = []config.SecretUse{}
		}
		notNeeded := usage.OnlyReadByFleetStagesLeftOutBeforeTheRun[key]
		view.Keys = append(view.Keys, secretKeyUsage{Key: key, Source: sources[key], Uses: uses, OnlyReadByFleetStagesLeftOutBeforeTheRun: notNeeded})
		switch {
		case sources[key] == nil && !notNeeded:
			view.ReferencedButNotDefined = append(view.ReferencedButNotDefined, key)
		case sources[key] != nil && len(uses) == 0:
			view.DefinedButNeverReferenced = append(view.DefinedButNeverReferenced, key)
		}
	}
	if len(o.arguments) == 1 && len(view.Keys) == 0 {
		return 4, fmt.Errorf("no secret key %s: the vault %s and secrets_from_env do not define it, and no playbook, role, or inventory node reads .secrets.%s", o.arguments[0], displayPath(usage.Vault), o.arguments[0])
	}
	code := 0
	if len(view.ReferencedButNotDefined) > 0 {
		code = 1
	}
	if o.asJSON {
		if _, err := encode(view); err != nil {
			return 4, err
		}
		return code, nil
	}
	w := bufio.NewWriter(os.Stdout)
	// One column for the sources and one for what uses a key, each as wide
	// as its longest entry needs.
	keyWidth, placeWidth := 0, 0
	var measure func(uses []config.SecretUse, indent int)
	measure = func(uses []config.SecretUse, indent int) {
		for _, use := range uses {
			placeWidth = max(placeWidth, indent+len(use.Place()))
			measure(use.UsesInsideTheRole, indent+2)
		}
	}
	for _, key := range view.Keys {
		keyWidth = max(keyWidth, len(key.Key))
		measure(key.Uses, 2)
	}
	var show func(uses []config.SecretUse, indent int)
	show = func(uses []config.SecretUse, indent int) {
		for _, use := range uses {
			fmt.Fprintf(w, "%-*s   %s\n", placeWidth, strings.Repeat(" ", indent)+use.Place(), use.UsedBy)
			show(use.UsesInsideTheRole, indent+2)
		}
	}
	for i, key := range view.Keys {
		if i > 0 {
			fmt.Fprintln(w)
		}
		source := "not defined"
		switch {
		case key.Source == nil:
		case key.Source.EnvVar != "":
			source = "environment variable " + key.Source.EnvVar
		default:
			source = "vault " + displayPath(key.Source.Vault)
		}
		fmt.Fprintf(w, "%-*s   %s\n", keyWidth, key.Key, source)
		if len(key.Uses) == 0 {
			fmt.Fprintln(w, "  (never referenced)")
		}
		if key.OnlyReadByFleetStagesLeftOutBeforeTheRun {
			fmt.Fprintln(w, "  (secret key not needed with the selected inventory: only fleet stages that select no nodes, or whose when is false on every node, read the key)")
		}
		show(key.Uses, 2)
	}
	// One key was asked for: its lines are the whole answer.
	if len(o.arguments) == 0 {
		notDefined := make([]string, len(view.ReferencedButNotDefined))
		for i, key := range view.ReferencedButNotDefined {
			notDefined[i] = fmt.Sprintf("%s (%s)", key, usage.Uses[key][0].Place()) //nolint:nilaway // Referenced secret names come from Uses entries created by appending at least one use.
		}
		if len(view.Keys) > 0 {
			fmt.Fprintln(w)
		}
		for _, list := range []struct {
			title string
			keys  []string
		}{{"Referenced but not defined:", notDefined}, {"Defined but never referenced:", view.DefinedButNeverReferenced}} {
			keys := "(none)"
			if len(list.keys) > 0 {
				keys = strings.Join(list.keys, ", ")
			}
			fmt.Fprintf(w, "%-29s %s\n", list.title, keys)
		}
	}
	if err := w.Flush(); err != nil {
		return 4, err
	}
	return code, nil
}
