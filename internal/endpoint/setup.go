package endpoint

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"mmx/internal/durable"
	"mmx/internal/node"

	"golang.org/x/crypto/ssh"
)

type SetupRequest struct {
	User          string   `json:"user"`
	Keys          [][]byte `json:"keys"`
	AddKey        []byte   `json:"add_key,omitempty"`
	Remote, Local string
	SSHPort       int `json:"ssh_port"`
}
type SetupResult struct {
	Info    Info   `json:"info"`
	Key     []byte `json:"key,omitempty"`
	NeedKey bool   `json:"need_key,omitempty"`
	// Reason says why the last offered key does not authorize HTTPS.
	Reason string `json:"reason,omitempty"`
}

// HTTPSAPIUnit is the systemd unit of the node HTTPS API process.
const HTTPSAPIUnit = "mmx-https-api.service"

func Setup(ctx context.Context, server node.Server, request SetupRequest) (SetupResult, error) {
	var result SetupResult
	if request.SSHPort < 1 || request.SSHPort > 65535 {
		return result, fmt.Errorf("invalid SSH port %d", request.SSHPort)
	}
	err := server.Install(func() error {
		authorizer := Authorizer{SSHPort: request.SSHPort}
		account, settings, err := authorizer.account(ctx, request.User, request.Remote, request.Local)
		if err != nil {
			return err
		}
		for _, raw := range request.Keys {
			key, err := ssh.ParsePublicKey(raw)
			if err != nil {
				return err
			}
			if err := authorizer.Authorize(ctx, request.User, key, request.Remote, request.Local); err != nil {
				result.Reason = err.Error()
			} else {
				result.Key, result.Reason = raw, ""
				break
			}
		}
		if len(result.Key) == 0 && len(request.AddKey) > 0 {
			key, err := ssh.ParsePublicKey(request.AddKey)
			if err != nil {
				return err
			}
			paths, err := authorizedPaths(account, settings)
			if err != nil {
				return err
			}
			if len(paths) == 0 {
				return fmt.Errorf("SSH has no AuthorizedKeysFile for automatic key setup")
			}
			if err := appendKey(paths[0], account, key); err != nil {
				return err
			}
			if err := authorizer.Authorize(ctx, request.User, key, request.Remote, request.Local); err != nil {
				return err
			}
			result.Key, result.Reason = request.AddKey, ""
		}
		if len(result.Key) == 0 {
			result.NeedKey = true
			return nil
		}
		identity, err := LoadIdentity(server.Root)
		if err != nil {
			return err
		}
		result.Info, err = NodeInfo(server.Binary, identity)
		if err != nil {
			return err
		}
		// The unit's command carries every endpoint setting, so the running
		// process shows exactly what it serves.
		command := []string{server.Binary, "serve", strconv.Itoa(request.SSHPort)}
		unit := []byte("[Unit]\nDescription=MMX HTTPS service (mmx-runner serve)\nAfter=network.target\n\n[Service]\nType=notify\nExecStart=" + strings.Join(command, " ") + "\nRestart=on-failure\nUMask=0077\nKillMode=process\n\n[Install]\nWantedBy=multi-user.target\n")
		path := "/etc/systemd/system/" + HTTPSAPIUnit
		old, err := os.ReadFile(path)
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		changed := !bytes.Equal(old, unit)
		if changed {
			if err := durable.Write(path, unit, 0644); err != nil {
				return err
			}
		}
		// Decide from systemd and the running process, not from what an earlier
		// setup wrote: one interrupted after writing the unit left the old
		// endpoint running and systemd's loaded unit stale.
		state, err := readEndpoint(ctx)
		if err != nil {
			return err
		}
		if changed || state.NeedReload {
			if out, err := exec.CommandContext(ctx, "systemctl", "daemon-reload").CombinedOutput(); err != nil {
				return fmt.Errorf("reload systemd unit files for mmx-https-api.service: %v: %s", err, out)
			}
		}
		if out, err := exec.CommandContext(ctx, "systemctl", "enable", HTTPSAPIUnit).CombinedOutput(); err != nil {
			return fmt.Errorf("enable mmx-https-api.service at boot: %v: %s", err, out)
		}
		if !changed && !restartEndpoint(state, command) {
			return nil
		}
		if out, err := exec.CommandContext(ctx, "systemctl", "restart", HTTPSAPIUnit).CombinedOutput(); err != nil {
			return fmt.Errorf("restart mmx-https-api.service: %v: %s", err, out)
		}
		return nil
	})
	return result, err
}

// endpointState is what systemd and /proc report about the running endpoint.
type endpointState struct {
	Active, NeedReload bool
	Executable         string
	Command            []string
}

func readEndpoint(ctx context.Context) (endpointState, error) {
	out, err := exec.CommandContext(ctx, "systemctl", "show", "--property=ActiveState,NeedDaemonReload,MainPID", HTTPSAPIUnit).Output()
	if err != nil {
		return endpointState{}, fmt.Errorf("inspect mmx-https-api.service: %w", err)
	}
	var state endpointState
	pid := 0
	for _, line := range strings.Split(string(out), "\n") {
		key, value, _ := strings.Cut(line, "=")
		switch key {
		case "ActiveState":
			state.Active = value == "active"
		case "NeedDaemonReload":
			state.NeedReload = value == "yes"
		case "MainPID":
			pid, _ = strconv.Atoi(value)
		}
	}
	if pid > 0 {
		// A process that exits meanwhile reads as empty, which restarts it.
		state.Executable, _ = os.Readlink(fmt.Sprintf("/proc/%d/exe", pid))
		cmdline, _ := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
		state.Command = strings.Split(strings.TrimSuffix(string(cmdline), "\x00"), "\x00")
	}
	return state, nil
}

// restartEndpoint reports whether the endpoint must restart to serve command.
// It keeps only an active endpoint whose loaded unit is current and whose main
// process runs exactly command. A replaced binary reads as "<path> (deleted)".
func restartEndpoint(state endpointState, command []string) bool {
	return !state.Active || state.NeedReload || state.Executable != command[0] || !slices.Equal(state.Command, command)
}

// managedKeyComment is the comment on the line MMX adds, so that a person
// reading authorized_keys can tell which line is MMX's.
const managedKeyComment = "mmx managed key"

// authorizedKeyLine is the line MMX adds for its managed key.
func authorizedKeyLine(key ssh.PublicKey) []byte {
	return append(bytes.TrimSuffix(ssh.MarshalAuthorizedKey(key), []byte("\n")), " "+managedKeyComment+"\n"...)
}

// holdsKey reports whether authorized_keys content already authorizes key
// without options. It compares the key itself, not the comment, so a line that
// an earlier MMX wrote without the comment still counts as MMX's.
func holdsKey(data []byte, key ssh.PublicKey) bool {
	for _, line := range strings.Split(string(data), "\n") {
		other, _, options, _, err := ssh.ParseAuthorizedKey([]byte(line))
		if err == nil && len(options) == 0 && bytes.Equal(other.Marshal(), key.Marshal()) {
			return true
		}
	}
	return false
}

func appendKey(path string, account *user.User, key ssh.PublicKey) error {
	uid, _ := strconv.Atoi(account.Uid)
	gid, _ := strconv.Atoi(account.Gid)
	dir := filepath.Dir(path)
	_, existed := os.Stat(dir)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	if os.IsNotExist(existed) && strings.HasPrefix(dir, account.HomeDir+string(os.PathSeparator)) {
		if err := os.Chown(dir, uid, gid); err != nil {
			return err
		}
	}
	for current := dir; ; current = filepath.Dir(current) {
		st, err := os.Stat(current)
		if err != nil {
			return err
		}
		owner := st.Sys().(*syscall.Stat_t).Uid
		if (owner != 0 && int(owner) != uid) || st.Mode().Perm()&0022 != 0 {
			return fmt.Errorf("unsafe SSH key directory %s", current)
		}
		if current == "/" {
			break
		}
	}
	data, err := readKeys(path, account.Uid)
	if err != nil {
		return err
	}
	if st, err := os.Lstat(path); err == nil {
		if !st.Mode().IsRegular() {
			return fmt.Errorf("automatic key setup requires a regular AuthorizedKeysFile")
		}
		uid, gid = int(st.Sys().(*syscall.Stat_t).Uid), int(st.Sys().(*syscall.Stat_t).Gid)
	} else if !os.IsNotExist(err) {
		return err
	}
	// Setup is repeatable without duplicating the generated public key.
	if holdsKey(data, key) {
		return nil
	}
	if len(data) > 0 && data[len(data)-1] != '\n' {
		data = append(data, '\n')
	}
	data = append(data, authorizedKeyLine(key)...)
	f, err := os.CreateTemp(dir, ".mmx-key-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if err := f.Chmod(0600); err != nil {
		return err
	}
	if err := f.Chown(uid, gid); err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}
