package modules

import (
	"context"
	"encoding/json"
	"mmx/internal/protocol"
	"mmx/internal/value"
	"reflect"
)

// Assert is a condition that must hold; That is a boolean reference or a
// predicate, evaluated like when.
type Assert struct {
	That    any    `yaml:"that" json:"that"`
	Message string `yaml:"message,omitempty" json:"message,omitempty"`
}

// assertDescription is what mmx describe-module shows for assert.
var assertDescription = Description{
	Name: "assert", Topic: TopicCommandsChecksAndReboot, Schema: reflect.TypeOf(Assert{}),
	Summary: "a condition that must hold; otherwise the task fails with a message",
	Fields: []Field{
		{Name: "that", Type: "boolean or predicate", Required: true, Notes: "a whole boolean reference or a predicate, as for when; to check a command or HTTP response, assert on its task's result"},
		{Name: "message", Default: "the condition as written", Notes: "the failure reason; references resolve; it shows unless it reads a secret, also after a node task that read one"},
	},
	Example: "- assert:\n    that:\n      equals: [\"{{ .node.distribution.id }}\", debian]\n    message: \"The app role supports Debian only, not {{ .node.distribution.id }}\"\n",
	Results: []string{"status", "change", "reason"},
	Dryrun:  "runs; changes nothing; a value not known yet reports not checked",
}

// Prepare checks the inputs when the project loads.
func (a *Assert) Prepare(base string) error {
	if a.That == nil {
		return invalid("that", "assert needs that: a boolean reference or a predicate")
	}
	return nil
}

// Arguments lists the inputs that may hold a reference, with their rules.
func (x *Assert) Arguments(arguments *ArgumentList) {
	if x.Message != "" {
		arguments.Private("message", x.Message, scalar)
	}
}

// WrittenArguments lists nothing: mmx explain shows the condition as part of
// the task, not as arguments.
func (x *Assert) WrittenArguments() []WrittenArgument { return nil }

// Reads lists the condition that must hold.
func (x *Assert) Reads() Reads {
	return Reads{Conditions: map[string]any{"that": x.That}}
}

// Run checks the condition. It succeeds unchanged when the condition holds and
// otherwise fails with its message, or with the condition as written. A value
// that is not known yet blocks it, which a dryrun reports as not checked.
func (x *Assert) Run(ctx context.Context, task NodeTask) protocol.Result {
	ok, err := task.Scope.Predicate(x.That)
	if err != nil {
		return protocol.Failed(task.InputError("that", x.That, err))
	}
	if ok {
		return protocol.Success("unchanged")
	}
	written, _ := json.Marshal(x.That)
	message, readsASecret := "assert.that is false: "+string(written), false
	if x.Message != "" {
		resolved, err := task.Scope.Resolve(x.Message)
		if err == nil {
			message, err = value.Scalar(resolved)
		}
		if err != nil {
			return protocol.Failed(task.InputError("message", x.Message, err))
		}
		// A message that reads a secret, also a result of a hidden node task,
		// resolves to a secret value.
		readsASecret = value.HasSecrets(resolved)
	}
	// The text comes only from values, never from a file on the node, so a
	// text that reads no secret shows also after a node task that read one.
	return protocol.Result{Status: "failed", Change: "unchanged", Reason: message, ReasonReadsNoSecret: !readsASecret}
}
