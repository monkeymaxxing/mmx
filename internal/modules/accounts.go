package modules

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"

	"mmx/internal/protocol"
	"mmx/internal/value"

	"go.yaml.in/yaml/v3"
)

type missingGroup string

func (e missingGroup) Error() string { return "group " + string(e) + " does not exist yet" }

type missingAccount string

func (e missingAccount) Error() string { return "account " + string(e) + " does not exist yet" }

// useradd reads its defaults with libeconf on openSUSE, which keeps the
// distribution's own files in /usr/etc: the file in /etc replaces the whole
// file in /usr/etc, files ending in .defs in login.defs.d directories change
// single settings of whichever file applies, in the order of their names (a
// file of the same name in /etc replaces the one in /usr/etc), and the
// skeleton of /etc/skel is added to the one of /usr/etc/skel. Other
// distributions have only the files in /etc. Tests replace these paths.
var (
	loginDefinitions        = "/etc/login.defs"
	vendorLoginDefinitions  = "/usr/etc/login.defs"
	loginDefinitionsDropIns = []string{"/usr/etc/login.defs.d", "/etc/login.defs.d"}
	skeletonDirectories     = []string{"/usr/etc/skel", "/etc/skel"}
)

// skeletonForNewHome is the directory that holds the files of a new home
// directory, and a function that removes it when it is a temporary merge. One
// skeleton that holds files is used as it is; two are merged, the later one
// over the earlier, in a temporary directory.
func skeletonForNewHome() (string, func(), error) {
	var present []string
	for _, directory := range skeletonDirectories {
		if entries, err := os.ReadDir(directory); err == nil && len(entries) > 0 {
			present = append(present, directory)
		}
	}
	switch len(present) {
	case 0:
		return skeletonDirectories[len(skeletonDirectories)-1], func() {}, nil
	case 1:
		return present[0], func() {}, nil
	}
	merged, err := os.MkdirTemp("", "mmx-skeleton-")
	if err != nil {
		return "", nil, err
	}
	remove := func() { os.RemoveAll(merged) }
	for _, directory := range present {
		if err := copySkeleton(directory, merged); err != nil {
			remove()
			return "", nil, err
		}
	}
	return merged, remove, nil
}

// copySkeleton copies the directories, files, and links below from into to,
// keeping their modes and replacing files of the same name.
func copySkeleton(from, to string) error {
	return filepath.WalkDir(from, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(from, path)
		if err != nil || relative == "." {
			return err
		}
		target := filepath.Join(to, relative)
		info, err := entry.Info()
		if err != nil {
			return err
		}
		switch {
		case entry.IsDir():
			if err := os.MkdirAll(target, 0o700); err != nil {
				return err
			}
			return os.Chmod(target, info.Mode().Perm())
		case info.Mode()&fs.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			os.Remove(target)
			return os.Symlink(link, target)
		default:
			content, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			os.Remove(target)
			return os.WriteFile(target, content, info.Mode().Perm())
		}
	})
}

// Account modules use the node's NSS lookup and native account tools. They never
// rewrite passwd/group files or infer membership from a previous MMX run.
func account(ctx context.Context, database, name string) ([]string, error) {
	return lookupAccount(ctx, database, name, false)
}

func lookupAccount(ctx context.Context, database, key string, local bool) ([]string, error) {
	args := []string{}
	if local {
		args = append(args, "-s", "files")
	}
	r := Command(ctx, nil, "", "getent", append(args, database, key)...)
	if r.ExitCode == 2 {
		return nil, nil
	}
	if r.Status != "succeeded" {
		return nil, fmt.Errorf("inspect %s %s: %s: %s", database, key, r.Reason, r.Stderr)
	}
	// A shadow entry holds a password hash: no message repeats it.
	fields := strings.Split(strings.TrimSpace(r.Stdout), ":")
	count := map[string]int{"passwd": 7, "group": 4, "shadow": 9}[database]
	if len(fields) != count || (fields[0] != key && (database == "shadow" || fields[2] != key)) {
		return nil, fmt.Errorf("invalid %s entry for %s", database, key)
	}
	return fields, nil
}

func (run Runtime) Group(ctx context.Context, g Group) (result protocol.Result) {
	name := g.Name
	if err := ValidateGroup(g); err != nil {
		return protocol.ConfigurationError(err)
	}
	if runtime.GOOS != "linux" {
		return protocol.Failed(fmt.Errorf("group management requires Linux"))
	}
	existing, err := account(ctx, "group", name)
	if err != nil {
		return protocol.Failed(err)
	}
	if g.State == "absent" {
		return run.removeAccount(ctx, "group", name, existing, false)
	}
	if err := checkAccountID(ctx, "group", name, g.GID, existing); err != nil {
		return protocol.ConfigurationError(err)
	}
	add, remove, err := planMembers(ctx, g, existing)
	if err != nil {
		var missing missingAccount
		if errors.As(err, &missing) {
			return run.notYet(err)
		}
		return protocol.Failed(err)
	}
	changes := map[string]any{}
	if existing == nil {
		changes["group_created"] = true
	}
	if len(add) > 0 {
		changes["members_added"] = add
	}
	if len(remove) > 0 {
		changes["members_removed"] = remove
	}
	if len(changes) == 0 {
		return protocol.Success("unchanged")
	}
	if existing != nil {
		if err := localAccount(ctx, "group", name, existing); err != nil {
			return protocol.ConfigurationError(err)
		}
	}
	if run.Dryrun {
		r := protocol.Success("would_change")
		r.Value = changes
		return r
	}
	if err := run.Mutation("group " + name); err != nil {
		return protocol.Failed(err)
	}
	// From here the value lists only what the task did, so that a failed
	// task never reports a change that did not happen.
	done := map[string]any{}
	defer func() {
		if len(done) > 0 {
			result.Value = done
		}
	}()
	change := "unknown"
	failed := func(err error) protocol.Result {
		r := protocol.Failed(err)
		r.Change = change
		return r
	}
	if existing == nil {
		args := []string{}
		if g.System != nil && *g.System {
			args = append(args, "--system")
		}
		if g.GID != nil {
			args = append(args, "--gid", fmt.Sprint(g.GID))
		}
		if r := Command(ctx, nil, "", "groupadd", append(args, "--", name)...); r.Status != "succeeded" {
			r.Change = "unknown"
			return r
		}
		change = "changed"
		done["group_created"] = true
	}
	// gpasswd changes one supplementary membership in the group and its shadow entry.
	for _, member := range add {
		if r := Command(ctx, nil, "", "gpasswd", "--add", member, "--", name); r.Status != "succeeded" {
			return failed(fmt.Errorf("add %s to group %s: %s: %s", member, name, r.Reason, r.Stderr))
		}
		change = "changed"
		done["members_added"] = append(listed(done["members_added"]), member)
	}
	for _, member := range remove {
		if r := Command(ctx, nil, "", "gpasswd", "--delete", member, "--", name); r.Status != "succeeded" {
			return failed(fmt.Errorf("remove %s from group %s: %s: %s", member, name, r.Reason, r.Stderr))
		}
		change = "changed"
		done["members_removed"] = append(listed(done["members_removed"]), member)
	}
	existing, err = account(ctx, "group", name)
	if err == nil && existing == nil {
		err = fmt.Errorf("group %s was not created", name)
	}
	if err == nil {
		err = checkAccountID(ctx, "group", name, g.GID, existing)
	}
	if err == nil {
		add, remove, err = planMembers(ctx, g, existing)
		if err == nil && len(add)+len(remove) > 0 {
			err = fmt.Errorf("the supplementary members of group %s do not match the requested members", name)
		}
	}
	if err != nil {
		return failed(err)
	}
	return protocol.Success(change)
}

// planMembers returns the accounts to add to the group's supplementary
// members and those to remove, in the order the task lists them and in the
// group's order. An account that has the group as its primary group is never
// added or removed.
func planMembers(ctx context.Context, g Group, existing []string) (add, remove []string, err error) {
	if g.SupplementaryMembers == nil {
		return nil, nil, nil
	}
	gid, current := "", []string{}
	if existing != nil {
		gid = existing[2]
		for _, m := range strings.Split(existing[3], ",") {
			if m != "" {
				current = append(current, m)
			}
		}
	} else if g.GID != nil {
		gid = fmt.Sprint(g.GID)
	}
	primary := func(name string) (bool, error) {
		a, err := account(ctx, "passwd", name)
		return a != nil && a[3] == gid, err
	}
	for _, m := range g.SupplementaryMembers {
		a, err := account(ctx, "passwd", m)
		if err != nil {
			return nil, nil, err
		}
		if a == nil {
			return nil, nil, missingAccount(m)
		}
		if a[3] != gid && !slices.Contains(current, m) && !slices.Contains(add, m) {
			add = append(add, m)
		}
	}
	if g.SupplementaryMembersNotListed == "remove" {
		for _, m := range current {
			if slices.Contains(g.SupplementaryMembers, m) {
				continue
			}
			isPrimary, err := primary(m)
			if err != nil {
				return nil, nil, err
			}
			if !isPrimary {
				remove = append(remove, m)
			}
		}
	}
	return add, remove, nil
}

func supplementaryGroups(ctx context.Context, name, primary string) ([]string, error) {
	r := Command(ctx, nil, "", "id", "-nG", "--", name)
	if r.Status != "succeeded" {
		return nil, fmt.Errorf("inspect groups for %s: %s: %s", name, r.Reason, r.Stderr)
	}
	var groups []string
	for _, g := range strings.Fields(r.Stdout) {
		if g != primary {
			groups = append(groups, g)
		}
	}
	slices.Sort(groups)
	return slices.Compact(groups), nil
}

// userPlan is what a user task changes, in the order it is applied.
type userPlan struct {
	args     []string // useradd or usermod arguments; empty for neither on an existing account
	home     string   // the home directory to create for an existing account
	password string   // the shadow password field to write through chpasswd
	lock     string   // --lock or --unlock when only the lock changes
	changes  map[string]any
}

func (p userPlan) empty() bool { return len(p.changes) == 0 }

func (run Runtime) User(ctx context.Context, u User, s value.Scope) (result protocol.Result) {
	if err := ValidateUser(u); err != nil {
		return protocol.ConfigurationError(err)
	}
	if runtime.GOOS != "linux" {
		return protocol.Failed(fmt.Errorf("user management requires Linux"))
	}
	current, err := account(ctx, "passwd", u.Name)
	if err != nil {
		return protocol.Failed(err)
	}
	if u.State == "absent" {
		return run.removeAccount(ctx, "passwd", u.Name, current, u.AlsoDeleteHomeDirectoryAndMail)
	}
	if err := checkAccountID(ctx, "passwd", u.Name, u.UID, current); err != nil {
		return protocol.ConfigurationError(err)
	}
	// The hash is read through the task's scope, so the engine treats the
	// whole result as secret. It goes only to chpasswd's standard input.
	hash := ""
	if u.PasswordHash != "" {
		v, err := s.Resolve(u.PasswordHash)
		if err == nil {
			hash, err = value.Scalar(v)
		}
		if err != nil {
			return protocol.Failed(fmt.Errorf("user.password_hash: %w", err))
		}
	}
	plan, err := planUser(ctx, u, current, hash)
	if err != nil {
		var missing missingGroup
		if errors.As(err, &missing) {
			return run.notYet(err)
		}
		return protocol.Failed(err)
	}
	if plan.empty() {
		return protocol.Success("unchanged")
	}
	if current != nil {
		if err := localAccount(ctx, "passwd", u.Name, current); err != nil {
			return protocol.ConfigurationError(err)
		}
	}
	if run.Dryrun {
		r := protocol.Success("would_change")
		r.Value = plan.changes
		return r
	}
	if err := run.Mutation("user " + u.Name); err != nil {
		return protocol.Failed(err)
	}
	// From here the value lists only what the task did, so that a failed
	// task never reports a change that did not happen. Each step copies its
	// part of the plan after its command succeeded.
	done := map[string]any{}
	defer func() {
		if len(done) > 0 {
			result.Value = done
		}
	}()
	doneFields := func(fields ...string) {
		for _, field := range listed(plan.changes["fields_changed"]) {
			if slices.Contains(fields, field) {
				done["fields_changed"] = append(listed(done["fields_changed"]), field)
			}
		}
	}
	change := "unknown"
	failed := func(err error) protocol.Result {
		r := protocol.Failed(err)
		r.Change = change
		return r
	}
	if current == nil || len(plan.args) > 0 {
		command := "usermod"
		if current == nil {
			command = "useradd"
		}
		r := Command(ctx, nil, "", command, append(plan.args, "--", u.Name)...)
		if r.Status != "succeeded" {
			r.Change = change
			return r
		}
		change = "changed"
		for _, key := range []string{"account_created", "groups_added", "groups_removed"} {
			if v, ok := plan.changes[key]; ok {
				done[key] = v
			}
		}
		// useradd --create-home makes a new account's home directory.
		if v, ok := plan.changes["home_directory_created"]; ok && plan.home == "" {
			done["home_directory_created"] = v
		}
		doneFields(accountDatabaseFields...)
	}
	if plan.home != "" {
		if err := createHome(ctx, u.Name, plan.home); err != nil {
			return failed(err)
		}
		change = "changed"
		done["home_directory_created"] = plan.home
	}
	if plan.password != "" {
		// chpasswd -e stores the field as given; it reads it from standard
		// input, so the hash never appears on a command line.
		input := strings.NewReader(u.Name + ":" + plan.password + "\n")
		if r := runProcess(ctx, nil, "", input, nil, "chpasswd", "--encrypted").Result; r.Status != "succeeded" {
			return failed(fmt.Errorf("chpasswd for %s: %s: %s", u.Name, r.Reason, r.Stderr))
		}
		change = "changed"
		doneFields("password_hash", "password_locked")
	}
	if plan.lock != "" {
		if r := Command(ctx, nil, "", "usermod", plan.lock, "--", u.Name); r.Status != "succeeded" {
			return failed(fmt.Errorf("usermod %s %s: %s: %s", plan.lock, u.Name, r.Reason, r.Stderr))
		}
		change = "changed"
		doneFields("password_locked")
	}
	current, err = account(ctx, "passwd", u.Name)
	if err == nil && current == nil {
		err = fmt.Errorf("user %s was not created", u.Name)
	}
	if err == nil {
		err = checkAccountID(ctx, "passwd", u.Name, u.UID, current)
	}
	if err == nil {
		var after userPlan
		after, err = planUser(ctx, u, current, hash)
		if err == nil && !after.empty() {
			err = fmt.Errorf("user account %s does not match the requested settings", u.Name)
		}
	}
	if err != nil {
		return failed(err)
	}
	return protocol.Success(change)
}

// planUser compares the account with the task. hash is the resolved password
// hash, or empty without password_hash.
func planUser(ctx context.Context, u User, current []string, hash string) (userPlan, error) {
	plan := userPlan{changes: map[string]any{}}
	args, err := userArguments(ctx, u, current, plan.changes)
	if err != nil {
		return plan, err
	}
	plan.args = args
	home := newAccountHome(u)
	if current != nil {
		home = current[5]
		if u.Home != "" {
			home = u.Home
		}
	} else {
		plan.changes["account_created"] = true
	}
	if *u.CreateHomeDirectory {
		_, err := os.Lstat(home)
		switch {
		case errors.Is(err, os.ErrNotExist):
			plan.changes["home_directory_created"] = home
			if current == nil {
				plan.args[0] = "--create-home" //nolint:nilaway // current == nil makes userArguments append --no-create-home before returning; argument errors return before indexing.
			} else {
				plan.home = home
			}
		case err != nil:
			return plan, fmt.Errorf("inspect home directory %s: %w", home, err)
		}
	}
	if hash == "" && u.PasswordLocked == nil {
		return plan, nil
	}
	// A new account has no password until this task sets one.
	field := "!"
	if current != nil {
		shadow, err := account(ctx, "shadow", u.Name)
		if err != nil {
			return plan, err
		}
		if shadow == nil {
			return plan, fmt.Errorf("account %s has no entry in the shadow database", u.Name)
		}
		field = shadow[1]
	}
	stored, locked := passwordField(field)
	wanted := stored
	if hash != "" && (u.ApplyPasswordHash == "on_every_run" || current == nil || stored == "") {
		wanted = hash
	}
	lock := locked
	if u.PasswordLocked != nil {
		lock = *u.PasswordLocked
	}
	if wanted == "" {
		// Without a hash, password login is already impossible: there is
		// nothing to lock, and unlocking would allow login without a password.
		if u.PasswordLocked != nil && !*u.PasswordLocked {
			return plan, fmt.Errorf("password_locked: false: account %s has no password hash, and unlocking it would allow login without a password; give password_hash", u.Name)
		}
		return plan, nil
	}
	var fields []string
	if f, ok := plan.changes["fields_changed"].([]string); ok {
		fields = f
	}
	switch {
	case wanted != stored:
		plan.password = wanted
		if lock {
			plan.password = "!" + wanted
		}
		fields = append(fields, "password_hash")
		if lock != locked && stored != "" {
			fields = append(fields, "password_locked")
		}
	case lock != locked:
		plan.lock = "--unlock"
		if lock {
			plan.lock = "--lock"
		}
		fields = append(fields, "password_locked")
	}
	if len(fields) > 0 {
		plan.changes["fields_changed"] = fields
	}
	return plan, nil
}

// accountDatabaseFields are the entries of fields_changed that useradd or
// usermod changes; chpasswd and usermod --lock change the password fields.
var accountDatabaseFields = []string{"primary_group", "home", "shell", "full_name"}

// listed returns a list of names that a value of a module's result holds,
// or nil when it holds none.
func listed(v any) []string {
	names, _ := v.([]string)
	return names
}

// passwordField reads a shadow password field: the stored hash, or empty
// when the account has never had one ("!", "!!", "*", or nothing), and
// whether a stored hash is locked with a leading "!".
func passwordField(field string) (hash string, locked bool) {
	hash = strings.TrimLeft(field, "!")
	if hash == "" || hash == "*" {
		return "", false
	}
	return hash, hash != field
}

func newAccountHome(u User) string {
	switch {
	case u.Home != "":
		return u.Home
	case u.IsSystem():
		return "/nonexistent"
	}
	return "/home/" + u.Name
}

// userArguments returns the useradd arguments for a new account, starting
// with --no-create-home, or the usermod arguments for an existing one, and
// notes the fields and groups that change in changes.
func userArguments(ctx context.Context, u User, current []string, changes map[string]any) ([]string, error) {
	var args []string
	primary := ""
	if u.PrimaryGroup != "" {
		g, err := account(ctx, "group", u.PrimaryGroup)
		if err != nil {
			return nil, err
		}
		if g == nil {
			return nil, missingGroup(u.PrimaryGroup)
		}
		primary = g[2]
	}
	for _, name := range u.SupplementaryGroups {
		g, err := account(ctx, "group", name)
		if err != nil {
			return nil, err
		}
		if g == nil {
			return nil, missingGroup(name)
		}
	}
	if current == nil {
		args = append(args, "--no-create-home")
		if u.UID != nil {
			args = append(args, "--uid", fmt.Sprint(u.UID))
		}
		if u.IsSystem() {
			args = append(args, "--system")
		}
		if u.PrimaryGroup != "" {
			args = append(args, "--gid", u.PrimaryGroup)
		} else {
			g, err := account(ctx, "group", u.Name)
			if err != nil {
				return nil, err
			}
			if g != nil {
				args = append(args, "--gid", u.Name)
			} else {
				args = append(args, "--user-group")
			}
		}
		shell := u.Shell
		if shell == "" {
			shell = "/bin/sh"
			if u.IsSystem() {
				shell = "/usr/sbin/nologin"
			}
		}
		args = append(args, "--home-dir", newAccountHome(u), "--shell", shell)
		if u.FullName != "" {
			args = append(args, "--comment", u.FullName)
		}
		if len(u.SupplementaryGroups) > 0 {
			args = append(args, "--groups", strings.Join(u.SupplementaryGroups, ","))
		}
		return args, nil
	}
	var fields []string
	if u.PrimaryGroup != "" && current[3] != primary {
		args = append(args, "--gid", u.PrimaryGroup)
		fields = append(fields, "primary_group")
	}
	if u.Home != "" && current[5] != u.Home {
		args = append(args, "--home", u.Home)
		fields = append(fields, "home")
	}
	if u.Shell != "" && current[6] != u.Shell {
		args = append(args, "--shell", u.Shell)
		fields = append(fields, "shell")
	}
	if u.FullName != "" && current[4] != u.FullName {
		args = append(args, "--comment", u.FullName)
		fields = append(fields, "full_name")
	}
	if len(fields) > 0 {
		changes["fields_changed"] = fields
	}
	if u.SupplementaryGroups != nil {
		// id -gn reports the current primary name even when this task will change it.
		r := Command(ctx, nil, "", "id", "-gn", "--", u.Name)
		if r.Status != "succeeded" {
			return nil, fmt.Errorf("inspect primary group for %s: %s", u.Name, r.Reason)
		}
		groups, err := supplementaryGroups(ctx, u.Name, strings.TrimSpace(r.Stdout))
		if err != nil {
			return nil, err
		}
		wanted := append([]string{}, u.SupplementaryGroups...)
		// A primary membership is already satisfied and is not supplementary.
		targetPrimary := strings.TrimSpace(r.Stdout)
		if u.PrimaryGroup != "" {
			targetPrimary = u.PrimaryGroup
		}
		wanted = slices.DeleteFunc(wanted, func(g string) bool { return g == targetPrimary })
		groups = slices.DeleteFunc(groups, func(g string) bool { return g == targetPrimary })
		slices.Sort(wanted)
		wanted = slices.Compact(wanted)
		var missing, extra []string
		for _, g := range wanted {
			if !slices.Contains(groups, g) {
				missing = append(missing, g)
			}
		}
		for _, g := range groups {
			if !slices.Contains(wanted, g) {
				extra = append(extra, g)
			}
		}
		if u.SupplementaryGroupsNotListed == "remove" {
			if len(missing)+len(extra) > 0 {
				args = append(args, "--groups", strings.Join(wanted, ","))
				if len(extra) > 0 {
					changes["groups_removed"] = extra
				}
			}
		} else if len(missing) > 0 {
			args = append(args, "--append", "--groups", strings.Join(missing, ","))
		}
		if len(missing) > 0 {
			changes["groups_added"] = missing
		}
	}
	return args, nil
}

// createHome makes an existing account's missing home directory from the
// node's skeleton directory, with the permissions useradd would give it
// (login.defs HOME_MODE, or the mode its UMASK leaves), then gives the new
// tree the SELinux labels of its path. Rocky Linux 9's mkhomedir_helper
// ignores its home-mode argument, so MMX sets the mode itself.
func createHome(ctx context.Context, name, home string) error {
	umask, mode := homeModes()
	skeleton, removeSkeleton, err := skeletonForNewHome()
	if err != nil {
		return fmt.Errorf("create home directory %s: %w", home, err)
	}
	defer removeSkeleton()
	r := Command(ctx, nil, "", "mkhomedir_helper", name, fmt.Sprintf("%04o", umask), skeleton, fmt.Sprintf("%04o", mode))
	if r.Status != "succeeded" {
		return fmt.Errorf("create home directory %s with mkhomedir_helper: %s: %s", home, r.Reason, r.Stderr)
	}
	parent, info, err := openParent(home, false)
	if err != nil {
		return err
	}
	defer parent.Close()
	if info == nil || !info.IsDir() {
		return fmt.Errorf("mkhomedir_helper did not create home directory %s", home)
	}
	if err := parent.Chmod(filepath.Base(home), os.FileMode(mode)); err != nil {
		return err
	}
	return labelTree(ctx, parent, filepath.Base(home), home)
}

// homeModes reads UMASK and HOME_MODE as useradd does: UMASK defaults to
// 022, and HOME_MODE to the permissions UMASK leaves.
func homeModes() (umask, mode uint64) {
	umask = 022
	set := map[string]uint64{}
	for _, path := range loginDefinitionFiles() {
		f, err := os.Open(path)
		if err != nil {
			continue
		}
		lines := bufio.NewScanner(f)
		for lines.Scan() {
			fields := strings.Fields(lines.Text())
			if len(fields) == 2 && (fields[0] == "UMASK" || fields[0] == "HOME_MODE") {
				if n, err := strconv.ParseUint(fields[1], 8, 32); err == nil {
					set[fields[0]] = n & 0777
				}
			}
		}
		f.Close()
	}
	if n, ok := set["UMASK"]; ok {
		umask = n
	}
	mode = 0777 &^ umask
	if n, ok := set["HOME_MODE"]; ok {
		mode = n
	}
	return umask, mode
}

// loginDefinitionFiles are the files whose settings apply, in order, the
// later over the earlier: the file in /etc, or else the distribution's own,
// then the drop-ins in the order of their names.
func loginDefinitionFiles() []string {
	files := []string{vendorLoginDefinitions}
	if _, err := os.Stat(loginDefinitions); err == nil {
		files = []string{loginDefinitions}
	}
	dropIns := map[string]string{}
	for _, directory := range loginDefinitionsDropIns {
		entries, _ := os.ReadDir(directory)
		for _, entry := range entries {
			if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".defs") {
				dropIns[entry.Name()] = filepath.Join(directory, entry.Name())
			}
		}
	}
	for _, name := range slices.Sorted(maps.Keys(dropIns)) {
		files = append(files, dropIns[name])
	}
	return files
}

func checkAccountID(ctx context.Context, database, name string, id any, current []string) error {
	if id == nil {
		return nil
	}
	wanted := fmt.Sprint(id)
	if current != nil {
		if current[2] != wanted {
			return fmt.Errorf("%s %s has ID %s; requested %s (renumbering is not supported)", database, name, current[2], wanted)
		}
		return nil
	}
	other, err := account(ctx, database, wanted)
	if err != nil {
		return err
	}
	if other != nil {
		return fmt.Errorf("%s ID %s is already used by %s", database, wanted, other[0])
	}
	return nil
}

func localAccount(ctx context.Context, database, name string, observed []string) error {
	local, err := lookupAccount(ctx, database, name, true)
	if err != nil {
		return err
	}
	if !slices.Equal(local, observed) {
		return fmt.Errorf("%s %s does not match the local account database entry; use the account provider to manage the account", database, name)
	}
	return nil
}

// removeAccount removes a local account or group with the native tools,
// without force. alsoDeleteHome removes an account's home directory and mail
// spool with it.
func (run Runtime) removeAccount(ctx context.Context, database, name string, current []string, alsoDeleteHome bool) protocol.Result {
	if current == nil {
		return protocol.Success("unchanged")
	}
	if err := localAccount(ctx, database, name, current); err != nil {
		return protocol.ConfigurationError(err)
	}
	if run.Dryrun {
		return protocol.Success("would_change")
	}
	if err := run.Mutation(database + " " + name); err != nil {
		return protocol.Failed(err)
	}
	command, args := "userdel", []string{}
	if database == "group" {
		command = "groupdel"
	}
	if alsoDeleteHome {
		args = append(args, "--remove")
	}
	// No force flag. Private-group handling follows the node's userdel
	// policy; explicit groups remain ordinary independent resources.
	r := Command(ctx, nil, "", command, append(args, "--", name)...)
	after, err := account(ctx, database, name)
	if err == nil && after == nil {
		r.Change = "changed"
		// userdel reports a missing home directory or mail spool with a
		// non-zero exit; only a home directory still in place is a failure.
		if alsoDeleteHome && r.Status != "succeeded" {
			if _, statErr := os.Lstat(current[5]); errors.Is(statErr, os.ErrNotExist) {
				r.Status, r.Reason, r.ExitCode = "succeeded", "", 0
			} else {
				// Another try finds no account and would report unchanged,
				// so the leftover home directory is named now.
				r.Reason = fmt.Sprintf("%s; the account %s is removed, but its home directory %s is still there; delete it by hand, because a later run finds no account and reports unchanged", r.Reason, name, current[5])
				r.NoOtherTry = true
			}
		}
		return r
	}
	if r.Status != "succeeded" {
		r.Change = "unknown"
		return r
	}
	if err == nil {
		err = fmt.Errorf("%s %s still exists after removal", database, name)
	}
	r = protocol.Failed(err)
	r.Change = "unknown"
	return r
}

type Group struct {
	Name   string `yaml:"name" json:"name"`
	State  string `yaml:"state,omitempty" json:"state,omitempty"`
	GID    any    `yaml:"gid,omitempty" json:"gid,omitempty"`
	System *bool  `yaml:"system,omitempty" json:"system,omitempty"`
	// Members are the accounts that have the group as a supplementary group.
	SupplementaryMembers          []string `yaml:"supplementary_members,omitempty" json:"supplementary_members"`
	SupplementaryMembersNotListed string   `yaml:"supplementary_members_not_listed,omitempty" json:"supplementary_members_not_listed,omitempty"`
}

type User struct {
	Name                         string   `yaml:"name" json:"name"`
	State                        string   `yaml:"state,omitempty" json:"state,omitempty"`
	UID                          any      `yaml:"uid,omitempty" json:"uid,omitempty"`
	System                       *bool    `yaml:"system,omitempty" json:"system,omitempty"`
	PrimaryGroup                 string   `yaml:"primary_group,omitempty" json:"primary_group,omitempty"`
	SupplementaryGroups          []string `yaml:"supplementary_groups,omitempty" json:"supplementary_groups"`
	SupplementaryGroupsNotListed string   `yaml:"supplementary_groups_not_listed,omitempty" json:"supplementary_groups_not_listed,omitempty"`
	Home                         string   `yaml:"home,omitempty" json:"home,omitempty"`
	Shell                        string   `yaml:"shell,omitempty" json:"shell,omitempty"`
	FullName                     string   `yaml:"full_name,omitempty" json:"full_name,omitempty"`
	// CreateHomeDirectory is required for a present account.
	CreateHomeDirectory *bool `yaml:"create_home_directory,omitempty" json:"create_home_directory,omitempty"`
	// PasswordHash is always one whole secret reference; the hash itself is
	// never in the definition.
	PasswordHash                   string `yaml:"password_hash,omitempty" json:"password_hash,omitempty"`
	ApplyPasswordHash              string `yaml:"apply_password_hash,omitempty" json:"apply_password_hash,omitempty"`
	PasswordLocked                 *bool  `yaml:"password_locked,omitempty" json:"password_locked,omitempty"`
	AlsoDeleteHomeDirectoryAndMail bool   `yaml:"also_delete_home_directory_and_mail,omitempty" json:"also_delete_home_directory_and_mail,omitempty"`
}

// IsSystem reports whether the task asks for a system account.
func (u User) IsSystem() bool { return u.System != nil && *u.System }

var accountName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_-]*\$?$`)

func ValidateGroup(g Group) error {
	if err := validatePresence(g.State); err != nil {
		return err
	}
	if g.State == "absent" && (g.GID != nil || g.System != nil || g.SupplementaryMembers != nil || g.SupplementaryMembersNotListed != "") {
		return fmt.Errorf("group with state: absent accepts only name and state")
	}
	if err := validateNotListed("supplementary_members", g.SupplementaryMembers != nil, g.SupplementaryMembersNotListed); err != nil {
		return err
	}
	// Written names are checked by their argument rule; this adds repeats.
	seen := map[string]bool{}
	for i, member := range g.SupplementaryMembers {
		if !Dynamic(member) && seen[member] {
			return invalid(fmt.Sprintf("supplementary_members[%d]", i), "account listed twice: "+member)
		}
		seen[member] = true
	}
	return validateAccountID("group.gid", g.GID)
}

func ValidateUser(u User) error {
	if err := validatePresence(u.State); err != nil {
		return err
	}
	if u.State == "absent" && (u.UID != nil || u.System != nil || u.PrimaryGroup != "" || u.SupplementaryGroups != nil || u.SupplementaryGroupsNotListed != "" || u.Home != "" || u.Shell != "" ||
		u.FullName != "" || u.CreateHomeDirectory != nil || u.PasswordHash != "" || u.ApplyPasswordHash != "" || u.PasswordLocked != nil) {
		return fmt.Errorf("user with state: absent accepts only name, state, and also_delete_home_directory_and_mail")
	}
	if u.State != "absent" && u.AlsoDeleteHomeDirectoryAndMail {
		return invalid("also_delete_home_directory_and_mail", "also_delete_home_directory_and_mail is only for state: absent")
	}
	if u.State != "absent" && u.CreateHomeDirectory == nil {
		return invalid("create_home_directory", "create_home_directory is required with state: present: true creates a missing home directory, false does not create a home directory")
	}
	if err := validateNotListed("supplementary_groups", u.SupplementaryGroups != nil, u.SupplementaryGroupsNotListed); err != nil {
		return err
	}
	if u.PasswordHash != "" && !WholeSecretReference(u.PasswordHash) {
		return invalid("password_hash", `password_hash must be one whole secret reference such as "{{ .secrets.alice_password_hash }}", or, in a role, a reference to an input declared secret: true such as "{{ .inputs.password_hash }}"; a hash written in the YAML would be readable by everyone who can read the project`)
	}
	switch {
	case u.ApplyPasswordHash != "" && !slices.Contains(applyPasswordHashChoices, u.ApplyPasswordHash):
		return invalid("apply_password_hash", "apply_password_hash must be only_when_account_has_no_password or on_every_run")
	case u.PasswordHash != "" && u.ApplyPasswordHash == "":
		return invalid("apply_password_hash", "apply_password_hash is required with password_hash: only_when_account_has_no_password keeps a password the person set later, on_every_run sets the given hash on every run")
	case u.PasswordHash == "" && u.ApplyPasswordHash != "":
		return invalid("apply_password_hash", "apply_password_hash requires password_hash")
	}
	return validateAccountID("user.uid", u.UID)
}

// validateNotListed checks the <things>_not_listed setting of a list a task
// owns: required whenever the task gives the list, and only then.
func validateNotListed(list string, given bool, choice string) error {
	field := list + "_not_listed"
	switch {
	case choice != "" && !slices.Contains(notListed, choice):
		return invalid(field, field+" must be remove or keep")
	case given && choice == "":
		return invalid(field, field+" is required with "+list+": remove makes "+list+" the whole set, keep leaves other entries alone")
	case !given && choice != "":
		return invalid(field, field+" requires "+list+" (use [] with remove to remove every entry)")
	}
	return nil
}

// Account is the owner or group of a file, directory, download, or unpacked
// archive: an account name, or a numeric ID. YAML text is a name and a YAML
// integer is an ID, so one spelling has one meaning; text made only of digits
// is refused. An ID is kept as its decimal digits, which no name can be.
type Account string

// IsID reports whether the account is a numeric ID, used with no account lookup.
func (a Account) IsID() bool {
	return a != "" && strings.Trim(string(a), "0123456789") == ""
}

func (a *Account) UnmarshalYAML(n *yaml.Node) error {
	var v any
	if err := n.Decode(&v); err != nil {
		return err
	}
	if err := accountRule(v); err != nil {
		return err
	}
	text, _ := value.Scalar(v)
	*a = Account(text)
	return nil
}

// literal is the account as it was written: an integer for an ID.
func (a Account) literal() any {
	if a.IsID() {
		if id, err := strconv.ParseInt(string(a), 10, 64); err == nil {
			return id
		}
	}
	return string(a)
}

// accountRule is the rule for a written or resolved owner or group.
func accountRule(v any) error {
	text, isText := value.Plain(v).(string)
	if !isText {
		if _, err := AccountID(value.Plain(v)); err != nil {
			return fmt.Errorf("must be an account name or a numeric ID: %w", err)
		}
		return nil
	}
	if text == "" {
		return fmt.Errorf("must be an account name or a numeric ID")
	}
	if Account(text).IsID() {
		return fmt.Errorf("%q is text made only of digits; to use the numeric ID, write it without quotes", text)
	}
	return nil
}

func validatePresence(state string) error {
	if state != "" && !slices.Contains(presenceStates, state) {
		return fmt.Errorf("state must be present or absent")
	}
	return nil
}

// An ID is an integer or one whole reference; the ID rule checks its value.
func validateAccountID(field string, v any) error {
	if s, ok := v.(string); ok && Dynamic(s) && value.Reference.FindString(s) != s {
		return fmt.Errorf("%s must be an integer or one whole reference", field)
	}
	return nil
}

// AccountID accepts YAML integers and their JSON/runtime numeric equivalents.
// The all-ones value is reserved by Unix account APIs.
func AccountID(v any) (uint32, error) {
	invalid := fmt.Errorf("account ID must be an integer from 0 through 4294967294")
	var n float64
	switch v := v.(type) {
	case int:
		n = float64(v)
	case int64:
		n = float64(v)
	case uint64:
		n = float64(v)
	case json.Number:
		var err error
		n, err = v.Float64()
		if err != nil {
			return 0, invalid
		}
	case float64:
		n = v
	default:
		return 0, invalid
	}
	// Every permitted ID is exactly representable as float64.
	if math.IsNaN(n) || n < 0 || n > 4294967294 || math.Trunc(n) != n {
		return 0, invalid
	}
	return uint32(n), nil
}

// userDescription is what mmx describe-module shows for user.
var userDescription = Description{
	Name: "user", Topic: TopicAccountsAndServiceUnits, Schema: reflect.TypeOf(User{}),
	Summary: "a local user account: groups, home, shell, full name, password hash, password lock, explicit ID, or removal",
	Fields: []Field{
		{Name: "name", Required: true},
		{Name: "system", Notes: "affects ID allocation on creation and the defaults of home and shell"},
		{Name: "uid", Type: "integer", Notes: "used on creation, checked on an existing account; never renumbered"},
		{Name: "primary_group", Notes: "primary group; must exist"},
		{Name: "supplementary_groups", Notes: "supplementary groups; must exist; needs supplementary_groups_not_listed"},
		{Name: "supplementary_groups_not_listed", Values: notListed, Notes: "required with supplementary_groups; remove makes supplementary_groups the whole set, and supplementary_groups: [] clears it; keep adds the listed groups and leaves other memberships alone"},
		{Name: "home", Notes: "the home directory path in the account database; a new account gets /home/<name>, a new system account /nonexistent; changing it never moves a directory"},
		{Name: "shell", Notes: "a new account gets /bin/sh, a new system account /usr/sbin/nologin"},
		{Name: "full_name", Notes: "the whole comment field of the account; : and line breaks are rejected"},
		{Name: "create_home_directory", Required: true, Notes: "required with state: present; true creates a missing home directory from the node's skeleton directory, also for an existing account; false never creates one; an existing home directory is never changed"},
		{Name: "password_hash", Notes: "a crypt hash ($6$..., $y$...) as one whole secret reference, such as {{ .secrets.alice_password_hash }}, or in a role one whole reference to an input declared secret: true; given to chpasswd on standard input, never on a command line; the task's result is suppressed"},
		{Name: "apply_password_hash", Values: applyPasswordHashChoices, Notes: "required with password_hash; only_when_account_has_no_password sets it when no hash is stored, on a new or existing account, and then keeps a password the person set; on_every_run sets it whenever the stored hash differs"},
		{Name: "password_locked", Notes: "true locks password login and keeps the stored hash; false unlocks; omit to leave it"},
		{Name: "also_delete_home_directory_and_mail", Default: "false", Notes: "only with state: absent; true also removes the home directory and the mail spool"},
		{Name: "state", Values: presenceStates, Default: "present", Notes: "absent accepts only name, state, and also_delete_home_directory_and_mail; without also_delete_home_directory_and_mail the account's files are kept"},
	},
	Example: "- user:\n    name: app\n    system: true\n    create_home_directory: false\n    primary_group: app\n    home: /srv/app\n",
	Results: []string{"status", "change", "value.account_created", "value.fields_changed", "value.home_directory_created", "value.groups_added", "value.groups_removed"},
	Dryrun:  "supported; inspects only and lists the fields, groups, and home directory that would change; with password_hash the whole result is suppressed, as for every task that reads a secret",
}

// groupDescription is what mmx describe-module shows for group.
var groupDescription = Description{
	Name: "group", Topic: TopicAccountsAndServiceUnits, Schema: reflect.TypeOf(Group{}),
	Summary: "a local group, with an explicit ID, its supplementary members, or removal",
	Fields: []Field{
		{Name: "name", Required: true},
		{Name: "system", Notes: "affects ID allocation on creation only"},
		{Name: "gid", Type: "integer", Notes: "used on creation, checked on an existing group; never renumbered"},
		{Name: "supplementary_members", Notes: "accounts that have the group as a supplementary group; they must exist; needs supplementary_members_not_listed; accounts with the group as their primary group are not touched"},
		{Name: "supplementary_members_not_listed", Values: notListed, Notes: "required with supplementary_members; remove makes supplementary_members the whole set, and supplementary_members: [] removes them all; keep adds the listed accounts and leaves other members alone"},
		{Name: "state", Values: presenceStates, Default: "present", Notes: "absent accepts only name and state"},
	},
	Example: "- group:\n    name: app\n    system: true\n",
	Results: []string{"status", "change", "value.group_created", "value.members_added", "value.members_removed"},
	Dryrun:  "supported; inspects only and lists the members that would be added or removed",
}

// Prepare checks the inputs when the project loads.
func (u *User) Prepare(base string) error {
	return ValidateUser(*u)
}

// Prepare checks the inputs when the project loads.
func (g *Group) Prepare(base string) error {
	return ValidateGroup(*g)
}

// Arguments lists the inputs that may hold a reference, with their rules.
func (g *Group) Arguments(arguments *ArgumentList) {
	arguments.Public("name", &g.Name, accountNameRule)
	if g.GID != nil {
		arguments.Public("gid", &g.GID, identifierID)
	}
	for i := range g.SupplementaryMembers {
		arguments.Public(fmt.Sprintf("supplementary_members[%d]", i), &g.SupplementaryMembers[i], accountNameRule)
	}
}

// Arguments lists the inputs that may hold a reference, with their rules.
func (u *User) Arguments(arguments *ArgumentList) {
	arguments.Public("name", &u.Name, accountNameRule)
	if u.UID != nil {
		arguments.Public("uid", &u.UID, identifierID)
	}
	if u.PrimaryGroup != "" {
		arguments.Public("primary_group", &u.PrimaryGroup, accountNameRule)
	}
	for i := range u.SupplementaryGroups {
		arguments.Public(fmt.Sprintf("supplementary_groups[%d]", i), &u.SupplementaryGroups[i], accountNameRule)
	}
	if u.Home != "" {
		arguments.Public("home", &u.Home, accountPath)
	}
	if u.Shell != "" {
		arguments.Public("shell", &u.Shell, accountPath)
	}
	if u.FullName != "" {
		arguments.Public("full_name", &u.FullName, fullName)
	}
	if u.PasswordHash != "" {
		arguments.WholeSecretReference("password_hash", u.PasswordHash, passwordHash)
	}
}

// Run does the module's work on the node.
func (u *User) Run(ctx context.Context, task NodeTask) protocol.Result {
	return task.Runtime.User(ctx, *u, task.Scope)
}

// Run does the module's work on the node.
func (g *Group) Run(ctx context.Context, task NodeTask) protocol.Result {
	return task.Runtime.Group(ctx, *g)
}
