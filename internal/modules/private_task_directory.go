package modules

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// A node task's private directory
//
// A node task sometimes has to hand a tool a file that no other account may
// read, for as long as the task runs:
//
//   - a login: git reads a user name, a password, and an SSH key from files,
//     docker and podman read a registry login from one, the client of MySQL
//     and MariaDB reads its login from one, and PostgreSQL's psql reads its
//     login password from one;
//   - a copy of a new file for a check that an ordinary account runs: the
//     account's own systemd-analyze verify of a new unit file, and Podman's
//     generator on a new Quadlet file;
//   - scratch files that a tool would otherwise write into a home directory:
//     Docker Compose's dry run.
//
// The task writes such files into a private directory of its own and removes
// the directory when it ends. makePrivateTaskDirectory is the one place that
// makes such a directory, and these are its rules:
//
//   - The directory is made in one place of the node,
//     /run/mmx/private_task_directories. The node makes that place on the
//     memory-backed /run and hands it to the node task execution engine,
//     which passes it on as Runtime.PrivateTaskDirectoryRoot. So no password
//     reaches a disk, and what a killed node job worker process leaves
//     behind is in the one place that the node empties before the next node
//     job starts.
//   - The directory belongs to the account that the tool runs as and has mode
//     0700: that account reads its files, and no other account can. The
//     directories above it belong to root and let every account pass through
//     without listing them (mode 0711), because a node task can run git,
//     podman, and a check as an ordinary account.
//   - Root writes every file while the directory is still root's own and
//     closed, then gives the files to the account, and the directory last.
//     From then on the account can put a link where a file was, so root
//     writes nothing more into the directory. Root only removes it, and
//     os.RemoveAll follows no link.

// makePrivateTaskDirectory makes a private directory for one node task and
// returns it with the function that removes it. module names the task's
// module in the directory's name. write puts the task's files into the
// directory, with modes that it sets itself; nil leaves the directory empty.
// account is the account that the tool runs as: nil is the runner's own,
// root.
func (run Runtime) makePrivateTaskDirectory(module string, account *syscall.Credential, write func(directory string) error) (string, func(), error) {
	if run.PrivateTaskDirectoryRoot == nil {
		return "", nil, errors.New("internal error: the node task has no configured root directory for private task directories")
	}
	root, err := run.PrivateTaskDirectoryRoot()
	if err != nil {
		return "", nil, err
	}
	// os.MkdirTemp makes the directory with mode 0700 and a name that no
	// other task has.
	directory, err := os.MkdirTemp(root, module+"-")
	if err != nil {
		return "", nil, err
	}
	remove := func() { _ = os.RemoveAll(directory) }
	if write != nil {
		err = write(directory)
	}
	if err == nil && account != nil {
		err = giveToAccount(directory, account)
	}
	if err != nil {
		remove()
		return "", nil, err
	}
	return directory, remove, nil
}

// giveToAccount makes account the owner of everything in directory, and of
// directory itself last: until then the account cannot open the directory,
// so root changes the owner only of what root itself put there.
func giveToAccount(directory string, account *syscall.Credential) error {
	uid, gid := int(account.Uid), int(account.Gid)
	err := filepath.WalkDir(directory, func(path string, _ fs.DirEntry, err error) error {
		if err != nil || path == directory {
			return err
		}
		return os.Lchown(path, uid, gid)
	})
	if err != nil {
		return err
	}
	return os.Lchown(directory, uid, gid)
}
