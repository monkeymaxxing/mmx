package controller

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"

	"mmx/internal/access"
	"mmx/internal/config"

	"golang.org/x/crypto/ssh"
)

// ErrNodeNotIdle refuses to remove MMX from a node while a node job runs on
// it or an owed restore waits on it: removing MMX then would end the node job
// halfway or drop the restore that protects the node's network configuration.
var ErrNodeNotIdle = errors.New("not removing MMX")

// idleCheckScript prints one line for each node job that runs ("running_job ID",
// found by a held worker.lock, the lock the node job worker process holds
// until it exits) and each owed restore ("owed_restore UNIT", found by its
// systemd unit, which the node removes after a successful restore).
const idleCheckScript = `for job in /var/lib/mmx/jobs/*/worker.lock; do
  test -e "$job" || continue
  command -v flock >/dev/null || { echo "flock (package util-linux) is missing, so MMX cannot tell whether a node job runs" >&2; exit 1; }
  flock -n "$job" true || echo "running_job $(basename "$(dirname "$job")")"
done
for unit in /etc/systemd/system/mmx-owed-restore-*.service; do
  test -e "$unit" && echo "owed_restore $(basename "$unit")"
done
exit 0`

// removalScript removes what MMX installed on the node, as root, and prints
// one line for each thing it removed or kept. /etc/mmx goes only when it is
// empty, so it stays while a scheduled job reads its secrets there, and while
// do_not_manage exists, which only the person who made it removes.
const removalScript = `unit=mmx-https-api.service
unit_file=/etc/systemd/system/$unit
if test -e "$unit_file"; then
  systemctl disable --now "$unit" >/dev/null 2>&1 || { echo "systemctl disable --now $unit failed" >&2; exit 1; }
  echo "stopped and disabled $unit"
  rm -f "$unit_file"
  systemctl daemon-reload
  echo "removed $unit_file"
fi
for path in /opt/mmx /var/lib/mmx /run/mmx; do
  if test -e "$path"; then rm -rf "$path" && echo "removed $path"; fi
done
rmdir /etc/mmx/systemd_scheduled_jobs 2>/dev/null && echo "removed /etc/mmx/systemd_scheduled_jobs"
if test -d /etc/mmx; then
  if rmdir /etc/mmx 2>/dev/null; then echo "removed /etc/mmx"
  else echo "kept /etc/mmx: it still holds $(ls -A /etc/mmx | tr '\n' ' ')because MMX removes neither the secrets of a scheduled job nor do_not_manage"; fi
fi
exit 0`

// authorizedKeyRemovalScript removes, as the SSH account, each line of its
// authorized_keys without options whose key is the managed key (%[1]s is the
// key's base64 text), whether or not the line carries the comment. It prints
// how many lines it removed.
const authorizedKeyRemovalScript = `file="$HOME/.ssh/authorized_keys"
test -f "$file" || exit 0
count=$(awk -v key=%[1]s '$2 == key { n++ } END { print n + 0 }' "$file") || exit 1
test "$count" -gt 0 || exit 0
copy=$(mktemp) || exit 1
awk -v key=%[1]s '$2 != key' "$file" > "$copy" && cat "$copy" > "$file"
status=$?
rm -f "$copy"
test "$status" -eq 0 || exit 1
echo "removed $count from $file"`

// RemoveMMXFromNode removes MMX from the node and from the controller
// machine, and returns one line for each thing it removed or kept. It
// refuses (ErrNodeNotIdle) while a node job runs or an owed restore waits on
// the node. It opens verified SSH, never HTTPS, and leaves the inventory and
// playbooks alone. A second call finds nothing and returns no lines.
func (c Controller) RemoveMMXFromNode(ctx context.Context, p config.Project, name string, h config.Node) ([]string, error) {
	r := &Run{Project: p}
	c.bindSecrets(r)
	settings, err := resolveSSH(ctx, h)
	if err != nil {
		return nil, err
	}
	destination := nodeDestination(settings)
	keyPath, err := access.KeyPath(destination)
	if err != nil {
		return nil, err
	}
	session, err := openSSH(ctx, h, c.Credentials, p.Secrets, keyPath, settings.Identities, settings.User+"@"+net.JoinHostPort(settings.Hostname, settings.Port))
	if err != nil {
		return nil, err
	}
	defer session.Close()

	found, err := session.privileged(ctx, idleCheckScript, nil)
	if err != nil {
		return nil, err
	}
	if err := refuseWhileBusy(name, string(found)); err != nil {
		return nil, err
	}
	var removed []string
	output, err := session.privileged(ctx, removalScript, nil)
	if err != nil {
		return nil, err
	}
	removed = append(removed, linesOf(name+": ", string(output))...)

	// The managed key is the last thing removed from the node: this session
	// may have logged in with it.
	var managedKey ssh.PublicKey
	pub, err := os.ReadFile(keyPath + ".pub")
	if err == nil {
		key, _, _, _, parseErr := ssh.ParseAuthorizedKey(pub)
		if parseErr != nil {
			return removed, fmt.Errorf("read %s: %w", keyPath+".pub", parseErr)
		}
		managedKey = key
		blob := base64.StdEncoding.EncodeToString(key.Marshal())
		output, err := session.ssh(ctx, fmt.Sprintf(authorizedKeyRemovalScript, quote(blob)))
		if err != nil {
			return removed, err
		}
		removed = append(removed, linesOf(name+": ", string(output))...)
	} else if !os.IsNotExist(err) {
		return removed, err
	}

	for _, file := range []string{
		filepath.Join(c.State, "nodes", destination+".json"),
		keyPath, keyPath + ".pub", keyPath + ".lock",
	} {
		switch err := os.Remove(file); {
		case err == nil:
			removed = append(removed, "controller machine: removed "+file)
		case !os.IsNotExist(err):
			return removed, err
		}
	}
	if managedKey != nil {
		// Adoption after an SSH port change copies the key to the new name.
		copies, err := removeCopiesOfManagedKey(keyPath, filepath.Join(c.State, "nodes"), managedKey)
		removed = append(removed, copies...)
		if err != nil {
			return removed, err
		}
	}
	return removed, nil
}

// refuseWhileBusy turns the lines of idleCheckScript into the error that says
// what to finish first, and how.
func refuseWhileBusy(name, found string) error {
	var problems []string
	for _, line := range strings.Split(strings.TrimSpace(found), "\n") {
		kind, what, _ := strings.Cut(line, " ")
		switch kind {
		case "running_job":
			problems = append(problems, fmt.Sprintf("node job %s is running: let the run end, or stop it with mmx stop on the controller machine that started it (mmx status shows the run, mmx resume repeats unfinished work)", what))
		case "owed_restore":
			problems = append(problems, fmt.Sprintf("an owed restore waits as %s: it puts back a network configuration that MMX changed, unless a run confirms the change (mmx status RUN_ID shows the run; after you correct the cause of a failed restore, run on %s: systemctl start %s)", what, name, what))
		}
	}
	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("%w from %s: %s; then run mmx remove-mmx-from-node %s again", ErrNodeNotIdle, name, strings.Join(problems, "; and "), name)
}

func linesOf(prefix, output string) []string {
	var lines []string
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		if line != "" {
			lines = append(lines, prefix+line)
		}
	}
	return lines
}

// nodeDestination is the SHA-256 that names a node's saved identity in
// .mmx/nodes/ and its managed key in ~/.ssh/mmx/: of the address MMX connects
// to, the SSH port, and the SSH account.
func nodeDestination(settings sshSettings) string {
	hash := sha256.Sum256([]byte(settings.Hostname + "\x00" + settings.Port + "\x00" + settings.User))
	return hex.EncodeToString(hash[:])
}
