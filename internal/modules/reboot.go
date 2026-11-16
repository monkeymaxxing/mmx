package modules

import (
	"context"
	"mmx/internal/protocol"
	"reflect"
	"time"
)

// Reboot restarts the node as its fleet stage's last task. Its guards follow the
// shell guard rules; the controller waits for the node to answer on a new boot.
type Reboot struct {
	SkipIfThisPathExists    string `yaml:"skip_if_this_path_exists,omitempty" json:"skip_if_this_path_exists,omitempty"`
	RunOnlyIfThisPathExists string `yaml:"run_only_if_this_path_exists,omitempty" json:"run_only_if_this_path_exists,omitempty"`
	OnlyIf                  *Guard `yaml:"run_only_if_this_command_succeeds,omitempty" json:"run_only_if_this_command_succeeds,omitempty"`
	OnlyIfNot               *Guard `yaml:"skip_if_this_command_succeeds,omitempty" json:"skip_if_this_command_succeeds,omitempty"`
	WaitAtMost              string `yaml:"wait_at_most,omitempty" json:"wait_at_most,omitempty"`
}

// Wait is how long the controller waits for the node to return on a new
// boot: timeout, or ten minutes.
func (r Reboot) Wait() (time.Duration, error) {
	if r.WaitAtMost == "" {
		return 10 * time.Minute, nil
	}
	d, err := time.ParseDuration(r.WaitAtMost)
	if err != nil || d <= 0 {
		return 0, invalid("wait_at_most", "reboot.wait_at_most must be a positive duration")
	}
	return d, nil
}

// rebootDescription is what mmx describe-module shows for reboot.
var rebootDescription = Description{
	Name: "reboot", Topic: TopicCommandsChecksAndReboot, Schema: reflect.TypeOf(Reboot{}),
	Summary: "reboots the node as the last task of a fleet stage, not in a role, task block, or loop; the controller waits for the node to return on a new boot. MMX never reboots the machine that runs the controller process: a fleet stage that ends with reboot fails on that node before its first node task, in a dryrun and in a run",
	Fields: []Field{
		{Name: "skip_if_this_path_exists", Notes: "skip reboot when this path exists; relative paths start at /"},
		{Name: "run_only_if_this_path_exists", Notes: "reboot only when this path exists; relative paths start at /"},
		{Name: "run_only_if_this_command_succeeds", Type: "command or mapping", Notes: "reboot only when the check exits 0; 1 skips; as for shell"},
		{Name: "skip_if_this_command_succeeds", Type: "command or mapping", Notes: "reboot only when the check exits 1; 0 skips"},
		{Name: "wait_at_most", Default: "10m", Notes: "how long the controller waits for the node to return on a new boot before the node's fleet stage fails"},
	},
	Example:   "- reboot:\n    run_only_if_this_command_succeeds: test -f /var/run/reboot-required\n    wait_at_most: 15m\n",
	Results:   []string{"status", "change", "value.boot_id_before", "value.boot_id_after"},
	Dryrun:    "runs the guards and reports would_change or the skip; does not reboot or wait",
	WaitLimit: "wait_at_most bounds the wait for the new boot",
}

// Prepare checks the inputs when the project loads.
func (r *Reboot) Prepare(base string) error {
	if writtenDuration(r.WaitAtMost) {
		if _, err := r.Wait(); err != nil {
			return err
		}
	}
	return validateGuards(r.OnlyIf, r.OnlyIfNot)
}

// Arguments lists the reference-capable path guards.
func (r *Reboot) Arguments(arguments *ArgumentList) {
	arguments.Text("skip_if_this_path_exists", &r.SkipIfThisPathExists)
	arguments.Text("run_only_if_this_path_exists", &r.RunOnlyIfThisPathExists)
}

// Reads lists the guards' scripts, which run as written, as shell's do.
func (r *Reboot) Reads() Reads {
	scripts := map[string]string{}
	if r.OnlyIf != nil {
		scripts["run_only_if_this_command_succeeds.script"] = r.OnlyIf.Script
	}
	if r.OnlyIfNot != nil {
		scripts["skip_if_this_command_succeeds.script"] = r.OnlyIfNot.Script
	}
	return Reads{ShellScripts: scripts}
}

// Run does the module's work on the node.
func (r *Reboot) Run(ctx context.Context, task NodeTask) protocol.Result {
	return task.TaskBlock.Reboot(ctx, *r)
}
