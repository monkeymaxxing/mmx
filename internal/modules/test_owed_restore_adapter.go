//go:build mmx_owed_restore_test

package modules

// This module is compiled only for tests/integration/owed_restores.py. Its
// saved data selects fixed Go code; no command text is read from a record.
import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	"mmx/internal/durable"
	"mmx/internal/protocol"
)

const testOwedRestoreModule = "test_change_with_owed_restore"

var testActivationActions = []string{"succeed", "stop_ssh_listener", "stop_https_listener", "fail", "hang", "wait_for_release"}

type testOwedRestoreInputs struct {
	Path                      string `yaml:"path" json:"path"`
	Content                   string `yaml:"content" json:"content"`
	Activation                string `yaml:"activation" json:"activation"`
	ConfirmReachabilityWithin string `yaml:"confirm_reachability_within" json:"confirm_reachability_within"`
}

func init() {
	coreModules = append(coreModules, CoreModule{Name: testOwedRestoreModule, Description: &testOwedRestoreDescription,
		NewInputs: func() Inputs { return &testOwedRestoreInputs{} }, NewChangeActivation: newTestChangeActivation})
}

var testOwedRestoreDescription = Description{
	Name: testOwedRestoreModule, Schema: reflect.TypeOf(testOwedRestoreInputs{}),
	Summary: "test-only file change and fixed activation faults for the owed restore guest suite",
	Fields: []Field{
		{Name: "path", Required: true, Notes: "absolute destination in the disposable test directory"},
		{Name: "content", Notes: "literal proposed contents"},
		{Name: "activation", Required: true, Values: testActivationActions, Notes: "fixed test action"},
		{Name: "confirm_reachability_within", Required: true, Notes: "positive confirmation duration, at most two minutes"},
	},
	Example: "- test_change_with_owed_restore:\n    path: /var/tmp/mmx-integration/owed-restores/example/configuration\n    content: proposed\n    activation: succeed\n    confirm_reachability_within: 8s\n",
	Results: []string{"status", "change", "owed_restore"}, Dryrun: "compares the file without creating a record, unit, or test marker",
}

func testRestorePath(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || !strings.HasPrefix(path, "/var/tmp/mmx-integration/owed-restores/") || filepath.Base(path) != "configuration" {
		return fmt.Errorf("test_change_with_owed_restore.path must be /var/tmp/mmx-integration/owed-restores/<case>/configuration")
	}
	return nil
}
func (in *testOwedRestoreInputs) Prepare(string) error {
	if err := testRestorePath(in.Path); err != nil {
		return err
	}
	if !slices.Contains(testActivationActions, in.Activation) {
		return fmt.Errorf("test_change_with_owed_restore.activation must be one of %s", strings.Join(testActivationActions, ", "))
	}
	duration, err := time.ParseDuration(in.ConfirmReachabilityWithin)
	if err != nil || duration <= 0 || duration > 2*time.Minute {
		return fmt.Errorf("test_change_with_owed_restore.confirm_reachability_within must be positive and at most two minutes")
	}
	return nil
}

func (in *testOwedRestoreInputs) Run(ctx context.Context, task NodeTask) protocol.Result {
	if err := in.Prepare(""); err != nil {
		return protocol.ConfigurationError(err)
	}
	if task.OwedRestores == nil {
		return protocol.Failed(fmt.Errorf("test change requires the node owed restore service"))
	}
	if err := task.OwedRestores.CheckChangeAllowed(ctx, in.Path); err != nil {
		return protocol.Failed(err)
	}
	file, result := task.Runtime.PrepareFileChange(ctx, File{Mode: "0644", Path: in.Path, Content: &in.Content}, task.Scope, nil)
	if file == nil {
		return result
	}
	defer file.Close()
	adapter := testChangeActivation{destination: in.Path, Activation: in.Activation}
	if in.Activation == "stop_ssh_listener" {
		// Sockets first, so stopping cannot cause socket activation of the daemon.
		// Query the actual guest: aliases, socket activation, and vendor defaults
		// differ between Debian/Ubuntu and Red Hat images.
		for _, unit := range []string{"ssh.socket", "sshd.socket", "ssh.service", "sshd.service"} {
			output := task.Runtime.RunCommand(ctx, []string{"systemctl", "show", unit, "--property=Id", "--property=ActiveState", "--property=KillMode"}, "")
			if output.Error != "" {
				return protocol.Failed(fmt.Errorf("inspect SSH listener: %s", output.Error))
			}
			props := map[string]string{}
			for _, line := range strings.Split(output.StandardOutput, "\n") {
				k, v, ok := strings.Cut(line, "=")
				if ok {
					props[k] = v
				}
			}
			if props["ActiveState"] != "active" {
				continue
			}
			if strings.HasSuffix(unit, ".service") && props["KillMode"] != "process" {
				return protocol.Failed(fmt.Errorf("test requires SSH listener %s with KillMode=process; got %s", unit, props["KillMode"]))
			}
			if !slices.Contains(adapter.SSHUnits, props["Id"]) {
				adapter.SSHUnits = append(adapter.SSHUnits, props["Id"])
			}
		}
		if len(adapter.SSHUnits) == 0 {
			return protocol.Failed(fmt.Errorf("test found no active SSH listener service or socket"))
		}
	}
	if err := durable.Write(filepath.Join(filepath.Dir(in.Path), "node_job_worker_process_id"), []byte(strconv.Itoa(os.Getpid())), 0600); err != nil {
		return protocol.Failed(err)
	}
	data, err := json.Marshal(adapter)
	if err != nil {
		return protocol.Failed(err)
	}
	duration, _ := time.ParseDuration(in.ConfirmReachabilityWithin)
	return task.OwedRestores.ApplyChange(ctx, ChangeWithOwedRestore{PreparedFile: file, RestoreConfirmationWait: duration, ModuleRestoreData: data})
}

type testChangeActivation struct {
	destination string
	Activation  string   `json:"activation"`
	SSHUnits    []string `json:"ssh_listener_units,omitempty"`
}

func newTestChangeActivation(destination string, data json.RawMessage) (ChangeActivation, error) {
	if err := testRestorePath(destination); err != nil {
		return nil, err
	}
	adapter := &testChangeActivation{destination: destination}
	if err := protocol.Decode(data, adapter); err != nil {
		return nil, err
	}
	if !slices.Contains(testActivationActions, adapter.Activation) {
		return nil, fmt.Errorf("invalid test activation in owed restore record")
	}
	for _, unit := range adapter.SSHUnits {
		if !slices.Contains([]string{"ssh.socket", "sshd.socket", "ssh.service", "sshd.service"}, unit) {
			return nil, fmt.Errorf("invalid test SSH listener unit %q", unit)
		}
	}
	// The factory runs before RestorePreviousFile. Check here, not in
	// PrepareActivation, which runs after the previous file is already back.
	if adapter.Activation == "hang" && len(os.Args) > 1 && os.Args[1] == "run-owed-restore" {
		if err := checkTestActivationProcessesStopped(filepath.Dir(destination), "/proc"); err != nil {
			return nil, err
		}
	}
	return adapter, nil
}

func checkTestActivationProcessesStopped(markerDirectory, processDirectory string) error {
	for _, name := range []string{"change_activation_parent_process_id", "change_activation_child_process_id"} {
		data, err := os.ReadFile(filepath.Join(markerDirectory, name))
		if err != nil {
			return err
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
		if err != nil || pid <= 0 {
			return fmt.Errorf("invalid test activation process id in %s", name)
		}
		stat, err := os.ReadFile(filepath.Join(processDirectory, strconv.Itoa(pid), "stat"))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		// The command name can contain spaces and parentheses. Only the final
		// closing parenthesis separates it from the process state.
		last := strings.LastIndex(string(stat), ") ")
		if last < 0 || !strings.HasPrefix(string(stat)[last+2:], "Z ") {
			return fmt.Errorf("test activation process %d still runs before file restoration", pid)
		}
	}
	return nil
}

func (a *testChangeActivation) marker(name string) string {
	return filepath.Join(filepath.Dir(a.destination), name)
}
func (a *testChangeActivation) PrepareActivation(ctx context.Context, run Runtime, previous bool) error {
	if !previous {
		return nil
	}
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return err
	}
	network := run.RunCommand(ctx, []string{"systemctl", "is-active", "network.target"}, "")
	if err := commandFailure(network, "read network.target at restore start"); err != nil {
		return err
	}
	return durable.JSON(a.marker("owed_restore_started.json"), map[string]any{"node_time": time.Now().UTC(), "node_boot_id": strings.TrimSpace(string(boot)), "network_target_state": strings.TrimSpace(network.StandardOutput)})
}
func (a *testChangeActivation) Activate(ctx context.Context, run Runtime, previous bool) error {
	if previous {
		if _, err := os.Stat(a.marker("fail_previous_config_activation")); err == nil {
			return fmt.Errorf("test previous configuration activation refused by fail_previous_config_activation")
		} else if !os.IsNotExist(err) {
			return err
		}
		units := a.SSHUnits
		if a.Activation == "stop_https_listener" {
			units = []string{"mmx-https-api.service"}
		}
		if len(units) > 0 {
			return commandFailure(run.RunCommandThatChangesTheNode(ctx, append([]string{"systemctl", "start"}, units...), ""), "restart test listener")
		}
		return nil
	}
	if err := durable.Write(a.marker("change_activation_started"), []byte("started\n"), 0600); err != nil {
		return err
	}
	switch a.Activation {
	case "stop_ssh_listener", "stop_https_listener":
		units := a.SSHUnits
		if a.Activation == "stop_https_listener" {
			units = []string{"mmx-https-api.service"}
		}
		// Stop sockets before services; each command completes before the next.
		for _, unit := range units {
			if err := commandFailure(run.RunCommandThatChangesTheNode(ctx, []string{"systemctl", "stop", unit}, ""), "stop test listener "+unit); err != nil {
				return err
			}
		}
	case "fail":
		return fmt.Errorf("test proposed configuration activation failed")
	case "wait_for_release":
		for {
			if _, err := os.Stat(a.marker("release_change_activation")); err == nil {
				break
			} else if !os.IsNotExist(err) {
				return err
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(50 * time.Millisecond):
			}
		}
	case "hang":
		script := `trap '' TERM
 echo $$ > "$1/change_activation_parent_process_id"
 /bin/sh -c 'trap "" TERM; echo $$ > "$1/change_activation_child_process_id"; while :; do sleep 1; done' test-child "$1" &
 wait`
		return commandFailure(run.RunCommandThatChangesTheNode(ctx, []string{"/bin/sh", "-c", script, "test-parent", filepath.Dir(a.destination)}, ""), "test hanging activation")
	}
	return nil
}
func (a *testChangeActivation) RemoveObsoleteVirtualDevices(context.Context, Runtime, bool) error {
	return nil
}
func (a *testChangeActivation) CheckActivatedConfiguration(_ context.Context, _ Runtime, previous bool) error {
	if previous {
		return durable.Write(a.marker("previous_config_activated"), []byte("checked\n"), 0600)
	}
	return nil
}
