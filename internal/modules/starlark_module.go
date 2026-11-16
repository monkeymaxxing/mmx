package modules

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"go.starlark.net/lib/json"
	"go.starlark.net/starlark"
	"go.starlark.net/starlarkstruct"
	"go.starlark.net/syntax"

	"mmx/internal/protocol"
	"mmx/internal/value"
)

// A Starlark module is a script that defines run_node_task(inputs). The node
// job worker process runs it itself, once for each try of its node task, with
// the primitives as its only way to touch the node. The changing primitives
// never change the node in a dryrun, and MMX records every call to them: the
// task's change, its difference, and its fingerprint come from those records,
// never from the script. docs/changing-mmx/design/modules.md owns the rules ("How a
// Starlark script runs").

// starlarkFileOptions equals the legacy resolver defaults; the dialect is
// described in docs/changing-mmx/design/modules.md, "How a Starlark script runs".
var starlarkFileOptions = &syntax.FileOptions{
	Set:               true,
	While:             false,
	TopLevelControl:   false,
	GlobalReassign:    false,
	LoadBindsGlobally: false,
	Recursion:         false,
}

// starlarkStatuses are the values of status in what run_node_task returns.
// not_checked is for a dryrun that cannot tell what a run would do.
// missing_prerequisite says that something an earlier node task may still
// provide, such as the tool's program, is not on the node. invalid_inputs
// says that the task cannot succeed as it is written, and that another try
// would end the same way: its inputs break a rule that the module's
// description cannot state, or they cannot work on the node as it is, such
// as a login that the tool's server refuses.
var starlarkStatuses = []string{"succeeded", "failed", "not_checked", "missing_prerequisite", "invalid_inputs"}

// starlarkStepLimit ends a script that runs away; it is far more than a
// module needs. starlarkPrintLimit is the most of what a script prints that a
// result keeps.
const (
	starlarkStepLimit  = 10_000_000
	starlarkPrintLimit = 8 << 10
)

// Starlark runs a Starlark module for one node task.
func (run Runtime) Starlark(ctx context.Context, name string, module Module, inputs map[string]any, scope value.Scope) (result protocol.Result) {
	plain, private, failure := resolveModuleInputs(name, scope, module.Description, inputs)
	if failure != nil {
		return *failure
	}
	var script *starlarkScript
	defer func() {
		result.Secret = result.Secret || private
		// MMX shows the reason of a missing prerequisite and of invalid
		// inputs also for a task that uses secrets, because it writes those
		// reasons itself. Here the script wrote the reason, and the reason
		// can hold the value of an input. So when an input that is no
		// private value holds a secret, this reason does not show either.
		if private && script != nil && script.choseTheKindOfFailure {
			result.Reason = "details suppressed for a task using secrets"
		}
	}()
	if !filepath.IsAbs(module.Directory) {
		return protocol.ConfigurationError(fmt.Errorf("the files for module %s have no absolute directory path on the node", name))
	}
	path := filepath.Join(module.Directory, ModuleScriptFile)
	source, err := os.ReadFile(path)
	if err != nil {
		return protocol.Failed(fmt.Errorf("reading the script of the module %s: %w", name, err))
	}
	script = &starlarkScript{run: run, ctx: ctx}
	thread := &starlark.Thread{Name: name, Print: script.print}
	thread.SetMaxExecutionSteps(starlarkStepLimit)
	stop := context.AfterFunc(ctx, func() { thread.Cancel("the node task's time limit ended the script") })
	defer stop()
	shown := filepath.Join("module", name, ModuleScriptFile)
	globals, err := starlark.ExecFileOptions(starlarkFileOptions, thread, shown, source, script.predeclared())
	if err != nil {
		r := script.failed(name, err)
		// A script that does not parse fails the same way on every try.
		if !errors.As(err, new(*starlark.EvalError)) {
			r.FailureKind = "config"
		}
		return r
	}
	function, ok := globals["run_node_task"].(*starlark.Function)
	if !ok || function.NumParams() != 1 {
		return protocol.ConfigurationError(fmt.Errorf("the script of the module %s must define run_node_task(inputs)", name))
	}
	arguments, err := toStarlark(plain)
	if err != nil {
		return protocol.ConfigurationError(fmt.Errorf("the inputs of the module %s: %w", name, err))
	}
	returned, err := starlark.Call(thread, function, starlark.Tuple{arguments}, nil)
	if err != nil {
		return script.failed(name, err)
	}
	return script.result(name, module, returned)
}

// starlarkScript is one run of a script: the primitives it calls and what the
// changing ones did.
type starlarkScript struct {
	run     Runtime
	ctx     context.Context
	printed strings.Builder
	changed bool
	// mayHaveChanged: a changing command started, or a changing request
	// reached the server, and it ended with an error, such as at the node
	// task's time limit. It may have changed the node.
	mayHaveChanged bool
	// missingProgram is the error of a command whose program is not on the
	// node: a script that fails with it fails the same way on every try.
	missingProgram string
	diff           []string
	// kept are the files, directories, and links the changing primitives
	// keep, by path, for the fingerprint.
	kept map[string]string
	// choseTheKindOfFailure: the script returned missing_prerequisite or
	// invalid_inputs, so the result's reason and its kind are the script's.
	choseTheKindOfFailure bool
}

func (s *starlarkScript) print(_ *starlark.Thread, message string) {
	if s.printed.Len() < starlarkPrintLimit {
		s.printed.WriteString(message + "\n")
	}
}

// failed is the result of a script that ended with an error: one line with
// the file and the line of the script.
func (s *starlarkScript) failed(name string, err error) protocol.Result {
	text := err.Error()
	var evaluation *starlark.EvalError
	if errors.As(err, &evaluation) {
		text = evaluation.Msg
		if len(evaluation.CallStack) > 0 {
			text = evaluation.CallStack.At(0).Pos.String() + ": " + evaluation.Msg
		}
	}
	r := protocol.Failed(fmt.Errorf("the script of the module %s ended with an error: %s", name, text))
	r.Change = s.change()
	r.Stderr = s.printed.String()
	r.Diff = strings.Join(s.diff, "")
	return r
}

// change is the change of the task: what the changing primitives did. A
// call that may have changed the node makes it unknown, unless another call
// changed the node.
func (s *starlarkScript) change() string {
	switch {
	case s.changed && s.run.Dryrun:
		return "would_change"
	case s.changed:
		return "changed"
	case s.mayHaveChanged:
		return "unknown"
	}
	return "unchanged"
}

// result reads what run_node_task returned: None, or a dictionary with an
// optional status, reason, outputs, and fingerprint.
func (s *starlarkScript) result(name string, module Module, returned starlark.Value) protocol.Result {
	r := protocol.Success(s.change())
	r.Stderr = s.printed.String()
	r.Diff = strings.Join(s.diff, "")
	own, tryAgain := "", true
	if returned != starlark.None {
		dict, ok := returned.(*starlark.Dict)
		if !ok {
			return s.failed(name, fmt.Errorf("run_node_task returned %s; run_node_task must return None or a dictionary", returned.Type()))
		}
		for _, item := range dict.Items() {
			key, _ := starlark.AsString(item[0])
			switch key {
			case "status":
				status, _ := starlark.AsString(item[1])
				if !slices.Contains(starlarkStatuses, status) {
					return s.failed(name, fmt.Errorf("status is %s; status must be one of %s", item[1], strings.Join(starlarkStatuses, ", ")))
				}
				r.Status = status
			case "reason":
				r.Reason, _ = starlark.AsString(item[1])
			case "fingerprint":
				own, _ = starlark.AsString(item[1])
			case "try_again":
				again, isBool := item[1].(starlark.Bool)
				if !isBool {
					return s.failed(name, fmt.Errorf("try_again is %s; try_again must be True or False", item[1]))
				}
				tryAgain = bool(again)
			case "outputs":
				outputs, err := fromStarlark(item[1])
				mapping, isMapping := outputs.(map[string]any)
				if err != nil || !isMapping {
					return s.failed(name, fmt.Errorf("outputs must be a dictionary of named outputs"))
				}
				r.ModuleOutputs = mapping
			default:
				return s.failed(name, fmt.Errorf("run_node_task returned the key %q; the keys are status, reason, outputs, fingerprint, and try_again", key))
			}
		}
	}
	if !tryAgain {
		if r.Status != "failed" {
			return s.failed(name, fmt.Errorf("try_again: False requires status: failed; the returned status is %s", r.Status))
		}
		r.NoOtherTry = true
	}
	if r.Status == "failed" && s.missingProgram != "" && strings.Contains(r.Reason, s.missingProgram) {
		r.Status = "missing_prerequisite"
	}
	switch {
	case r.Status != "succeeded" && r.Reason == "":
		return s.failed(name, fmt.Errorf("a %s result needs a reason", strings.ReplaceAll(r.Status, "_", " ")))
	case r.Status == "not_checked" && !s.run.Dryrun:
		return s.failed(name, fmt.Errorf("status: not_checked is allowed only in a dryrun; call is_dryrun() to check whether the node task is running in a dryrun"))
	case r.Status == "missing_prerequisite":
		// Something an earlier node task may still provide is missing: a
		// dryrun cannot check the task yet, and a run fails without another
		// try, as for a core module.
		s.choseTheKindOfFailure = true
		missing := s.run.notYet(errors.New(r.Reason))
		if !s.run.Dryrun {
			missing.Change = s.change()
		}
		missing.Stderr, missing.Diff = r.Stderr, r.Diff
		return missing
	case r.Status == "invalid_inputs":
		// The inputs do not change between tries, so the task fails without
		// another try, in a dryrun as in a run.
		s.choseTheKindOfFailure = true
		invalid := protocol.ConfigurationError(errors.New(r.Reason))
		invalid.Change, invalid.Stderr, invalid.Diff = s.change(), r.Stderr, r.Diff
		return invalid
	case r.Status == "not_checked":
		r.Change = "unknown"
	}
	// A value spelled as an internal record would change its meaning once
	// it is saved or sent to a later fleet stage, as for an executable.
	if value.HasReservedKey(r.ModuleOutputs) {
		return s.failed(name, fmt.Errorf("outputs use a reserved MMX record key ($mmx_secret, $mmx_truncated, or $mmx_binary)"))
	}
	if module.Description != nil && r.ModuleOutputs != nil {
		if err := module.Description.OutputsProblem(name, r.ModuleOutputs); err != nil {
			return s.failed(name, err)
		}
	}
	r.Fingerprint = s.fingerprint(own)
	return r
}

// fingerprint hashes what the changing primitives keep, with the script's own
// fingerprint text.
func (s *starlarkScript) fingerprint(own string) string {
	h := sha256.New()
	paths := make([]string, 0, len(s.kept))
	for path := range s.kept {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		HashField(h, path)
		HashField(h, s.kept[path])
	}
	HashField(h, own)
	return hex.EncodeToString(h.Sum(nil))
}

// record notes one changing primitive call.
func (s *starlarkScript) record(change PathChange, path, state, description string) {
	if s.kept == nil {
		s.kept = map[string]string{}
	}
	s.kept[path] = state
	if !change.Changed || change.Error != "" {
		return
	}
	s.changed = true
	if change.Diff != "" {
		s.diff = append(s.diff, change.Diff)
		if !strings.HasSuffix(change.Diff, "\n") {
			s.diff = append(s.diff, "\n")
		}
		return
	}
	s.diff = append(s.diff, description+"\n")
}

// predeclared are the names a script sees: the primitives, json, and
// nothing else.
func (s *starlarkScript) predeclared() starlark.StringDict {
	builtin := func(name string, f func(args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error)) *starlark.Builtin {
		return starlark.NewBuiltin(name, func(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
			return f(args, kwargs)
		})
	}
	verb := func(change PathChange) starlark.Value {
		return answer(map[string]any{"changed": change.Changed, "error": change.Error})
	}
	return starlark.StringDict{
		"json": json.Module,
		"is_dryrun": builtin("is_dryrun", func(args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
			if err := starlark.UnpackArgs("is_dryrun", args, kwargs); err != nil {
				return nil, err
			}
			return starlark.Bool(s.run.Dryrun), nil
		}),
		"parse_duration": builtin("parse_duration", func(args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
			var durationText string
			if err := starlark.UnpackArgs("parse_duration", args, kwargs, "duration", &durationText); err != nil {
				return nil, err
			}
			duration, err := time.ParseDuration(durationText)
			if err != nil {
				return answer(map[string]any{"seconds": 0, "error": "expected a duration, such as 30s or 2m"}), nil
			}
			return answer(map[string]any{"seconds": duration.Seconds(), "error": ""}), nil
		}),
		"sha256": builtin("sha256", func(args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
			var text string
			if err := starlark.UnpackArgs("sha256", args, kwargs, "text", &text); err != nil {
				return nil, err
			}
			return starlark.String(s.run.SHA256(text)), nil
		}),
		"earlier_node_task_would_change": builtin("earlier_node_task_would_change", func(args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
			var path string
			if err := starlark.UnpackArgs("earlier_node_task_would_change", args, kwargs, "path", &path); err != nil {
				return nil, err
			}
			return starlark.Bool(s.run.EarlierNodeTaskWouldChange(path)), nil
		}),
		"find_program": builtin("find_program", func(args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
			var program string
			if err := starlark.UnpackArgs("find_program", args, kwargs, "program_name", &program); err != nil {
				return nil, err
			}
			location := s.run.FindProgram(s.ctx, program)
			return answer(map[string]any{"found": location.Found, "path": location.Path, "error": location.Error}), nil
		}),
		"read_file": builtin("read_file", func(args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
			var path string
			if err := starlark.UnpackArgs("read_file", args, kwargs, "path", &path); err != nil {
				return nil, err
			}
			text := s.run.ReadFile(path)
			return answer(map[string]any{"content": text.Content, "error": text.Error}), nil
		}),
		"inspect_path": builtin("inspect_path", func(args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
			var path string
			if err := starlark.UnpackArgs("inspect_path", args, kwargs, "path", &path); err != nil {
				return nil, err
			}
			info := s.run.InspectPath(path)
			return answer(map[string]any{"exists": info.Exists, "type": info.Type, "owner": info.Owner, "group": info.Group,
				"permissions_mode": info.PermissionsMode, "link_target": info.LinkTarget, "sha256": info.SHA256, "error": info.Error}), nil
		}),
		"look_up_account": builtin("look_up_account", func(args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
			var account string
			if err := starlark.UnpackArgs("look_up_account", args, kwargs, "user_account", &account); err != nil {
				return nil, err
			}
			info := s.run.LookUpAccount(account)
			groups := make([]any, len(info.Groups))
			for i, g := range info.Groups {
				groups[i] = g
			}
			return answer(map[string]any{"exists": info.Exists, "user_id": info.UserID, "group_id": info.GroupID,
				"home": info.Home, "shell": info.Shell, "groups": groups, "error": info.Error}), nil
		}),
		"run_command": builtin("run_command", func(args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
			arguments, input, err := commandArguments("run_command", args, kwargs)
			if err != nil {
				return nil, err
			}
			return s.commandAnswer(s.run.RunCommand(s.ctx, arguments, input)), nil
		}),
		"run_command_that_changes_the_node": builtin("run_command_that_changes_the_node", func(args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
			arguments, input, err := commandArguments("run_command_that_changes_the_node", args, kwargs)
			if err != nil {
				return nil, err
			}
			out := s.runCommandThatChangesTheNode(arguments, input)
			return s.commandAnswer(out), nil
		}),
		"run_command_that_refreshes_a_package_index": builtin("run_command_that_refreshes_a_package_index", func(args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
			var list *starlark.List
			if err := starlark.UnpackArgs("run_command_that_refreshes_a_package_index", args, kwargs, "arguments", &list); err != nil {
				return nil, err
			}
			arguments, _, err := commandArguments("run_command_that_refreshes_a_package_index", starlark.Tuple{list}, nil)
			if err != nil {
				return nil, err
			}
			return s.commandAnswer(s.run.RunCommandThatRefreshesAPackageIndex(s.ctx, arguments)), nil
		}),
		"http_request": builtin("http_request", func(args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
			var method, address, body, authority string
			headers := new(starlark.Dict)
			if err := starlark.UnpackArgs("http_request", args, kwargs, "method", &method, "url", &address,
				"headers?", &headers, "body?", &body, "certificate_authority_file?", &authority); err != nil {
				return nil, err
			}
			written := map[string]string{}
			for _, item := range headers.Items() {
				k, okKey := starlark.AsString(item[0])
				v, okValue := starlark.AsString(item[1])
				if !okKey || !okValue {
					return nil, fmt.Errorf("http_request: headers must be a dictionary of strings")
				}
				written[k] = v
			}
			response := s.run.HTTPRequest(s.ctx, method, address, written, body, authority)
			if upper := strings.ToUpper(method); upper != "GET" && upper != "HEAD" {
				switch {
				case response.Error == "":
					s.changed = true
					verbs := "sent"
					if s.run.Dryrun {
						verbs = "would send"
					}
					s.diff = append(s.diff, verbs+" "+upper+" to "+hostOf(address)+"\n")
				case response.Sent:
					s.mayHaveChanged = true
					s.diff = append(s.diff, "sent "+upper+" to "+hostOf(address)+", then the answer failed\n")
				}
			}
			headersOut := map[string]any{}
			for k, v := range response.Headers {
				headersOut[k] = v
			}
			return answer(map[string]any{"sent": response.Sent, "http_status_code": response.HTTPStatusCode,
				"headers": headersOut, "body": response.Body, "error": response.Error}), nil
		}),
		"write_file": builtin("write_file", func(args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
			var path, content string
			owner, group, mode := "root", "root", "0644"
			if err := starlark.UnpackArgs("write_file", args, kwargs, "path", &path, "content", &content,
				"owner?", &owner, "group?", &group, "permissions_mode?", &mode); err != nil {
				return nil, err
			}
			change := s.run.WriteFile(s.ctx, path, content, owner, group, mode)
			s.record(change, path, "file "+s.run.SHA256(content)+" "+owner+" "+group+" "+mode, "file "+path)
			return verb(change), nil
		}),
		"make_directory": builtin("make_directory", func(args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
			var path string
			owner, group, mode := "root", "root", "0755"
			if err := starlark.UnpackArgs("make_directory", args, kwargs, "path", &path,
				"owner?", &owner, "group?", &group, "permissions_mode?", &mode); err != nil {
				return nil, err
			}
			change := s.run.MakeDirectory(s.ctx, path, owner, group, mode)
			s.record(change, path, "directory "+owner+" "+group+" "+mode, "directory "+path)
			return verb(change), nil
		}),
		"make_link": builtin("make_link", func(args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
			var path, target string
			if err := starlark.UnpackArgs("make_link", args, kwargs, "path", &path, "target", &target); err != nil {
				return nil, err
			}
			change := s.run.MakeLink(s.ctx, path, target)
			s.record(change, path, "link "+target, "link "+path+" -> "+target)
			return verb(change), nil
		}),
		"remove_path": builtin("remove_path", func(args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
			var path string
			if err := starlark.UnpackArgs("remove_path", args, kwargs, "path", &path); err != nil {
				return nil, err
			}
			change := s.run.RemovePath(s.ctx, path)
			s.record(change, path, "absent", "remove "+path)
			return verb(change), nil
		}),
	}
}

// runCommandThatChangesTheNode runs a command that changes the node and
// records it.
func (s *starlarkScript) runCommandThatChangesTheNode(arguments []string, input string) CommandOutput {
	out := s.run.RunCommandThatChangesTheNode(s.ctx, arguments, input)
	switch {
	case out.Error == "" && len(arguments) > 0:
		s.changed = true
		verbs := "ran"
		if s.run.Dryrun {
			verbs = "would run"
		}
		s.diff = append(s.diff, verbs+": "+strings.Join(arguments, " ")+"\n")
	case out.Ran:
		s.mayHaveChanged = true
		s.diff = append(s.diff, "ran, then ended with an error: "+strings.Join(arguments, " ")+"\n")
	}
	return out
}

func hostOf(address string) string {
	if _, rest, found := strings.Cut(address, "://"); found {
		host, _, _ := strings.Cut(rest, "/")
		if at := strings.LastIndex(host, "@"); at >= 0 {
			host = host[at+1:]
		}
		return host
	}
	return "the address"
}

func commandArguments(name string, args starlark.Tuple, kwargs []starlark.Tuple) ([]string, string, error) {
	var list *starlark.List
	input := ""
	if err := starlark.UnpackArgs(name, args, kwargs, "arguments", &list, "standard_input?", &input); err != nil {
		return nil, "", err
	}
	var arguments []string
	for i := 0; i < list.Len(); i++ {
		text, ok := starlark.AsString(list.Index(i))
		if !ok {
			return nil, "", fmt.Errorf("%s: arguments must be a list of strings", name)
		}
		arguments = append(arguments, text)
	}
	return arguments, input, nil
}

func (s *starlarkScript) commandAnswer(out CommandOutput) starlark.Value {
	if out.ProgramNotFound {
		s.missingProgram = out.Error
	}
	return commandAnswer(out)
}

func commandAnswer(out CommandOutput) starlark.Value {
	return answer(map[string]any{"ran": out.Ran, "exit_code": out.ExitCode, "standard_output": out.StandardOutput,
		"standard_error": out.StandardError, "error": out.Error})
}

// answer is a primitive's result as a Starlark struct.
func answer(fields map[string]any) starlark.Value {
	members := starlark.StringDict{}
	for k, v := range fields {
		members[k], _ = toStarlark(v)
	}
	return starlarkstruct.FromStringDict(starlarkstruct.Default, members)
}

// toStarlark converts inputs as JSON or YAML gives them.
func toStarlark(v any) (starlark.Value, error) {
	switch x := v.(type) {
	case nil:
		return starlark.None, nil
	case bool:
		return starlark.Bool(x), nil
	case string:
		return starlark.String(x), nil
	case int:
		return starlark.MakeInt(x), nil
	case int64:
		return starlark.MakeInt64(x), nil
	case float64:
		if whole, ok := asWholeNumber(x); ok {
			return starlark.MakeInt64(whole), nil
		}
		return starlark.Float(x), nil
	case []any:
		items := make([]starlark.Value, len(x))
		for i, item := range x {
			converted, err := toStarlark(item)
			if err != nil {
				return nil, err
			}
			items[i] = converted
		}
		return starlark.NewList(items), nil
	case map[string]any:
		dict := starlark.NewDict(len(x))
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			converted, err := toStarlark(x[k])
			if err != nil {
				return nil, err
			}
			_ = dict.SetKey(starlark.String(k), converted)
		}
		return dict, nil
	}
	if n, ok := asNumber(v); ok {
		if whole, ok := asWholeNumber(v); ok {
			return starlark.MakeInt64(whole), nil
		}
		return starlark.Float(n), nil
	}
	return nil, fmt.Errorf("a value of type %T cannot be passed to a Starlark script", v)
}

// fromStarlark converts what a script returns into JSON values.
func fromStarlark(v starlark.Value) (any, error) {
	switch x := v.(type) {
	case starlark.NoneType:
		return nil, nil
	case starlark.Bool:
		return bool(x), nil
	case starlark.String:
		return string(x), nil
	case starlark.Int:
		if i, ok := x.Int64(); ok {
			return i, nil
		}
		return nil, fmt.Errorf("the whole number %s does not fit in a signed 64-bit integer", x)
	case starlark.Float:
		return float64(x), nil
	case *starlark.List:
		out := make([]any, x.Len())
		for i := 0; i < x.Len(); i++ {
			item, err := fromStarlark(x.Index(i))
			if err != nil {
				return nil, err
			}
			out[i] = item
		}
		return out, nil
	case starlark.Tuple:
		out := make([]any, len(x))
		for i, item := range x {
			converted, err := fromStarlark(item)
			if err != nil {
				return nil, err
			}
			out[i] = converted
		}
		return out, nil
	case *starlark.Dict:
		out := map[string]any{}
		for _, item := range x.Items() {
			key, ok := starlark.AsString(item[0])
			if !ok {
				return nil, fmt.Errorf("a dictionary's keys must be strings")
			}
			converted, err := fromStarlark(item[1])
			if err != nil {
				return nil, err
			}
			out[key] = converted
		}
		return out, nil
	}
	return nil, fmt.Errorf("a Starlark script cannot return a value of type %s", v.Type())
}
