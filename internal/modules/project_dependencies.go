package modules

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"mmx/internal/protocol"
)

// The code the five ..._project_dependencies modules share. Each installs
// what an application's lock file lists, into the application's directory,
// as the application's account, and then writes a record of what it
// installed. The record decides an unchanged rerun and is the task's
// tracked input.

// projectDependencies is one lock file module's task, as its module
// describes it to the shared code.
type projectDependencies struct {
	module    string
	directory string
	settings  installerSettings
	// lockFiles are the lock file's possible names, the preferred first.
	lockFiles []string
	manifest  string
	// installedPaths hold the installed packages, relative to the project
	// directory or absolute; one of them must exist.
	installedPaths []string
	// chosen are the task's settings that change what gets installed.
	chosen map[string]any
	// runtime is the language runtime beside the installer, such as node.
	runtime string
	// runtimePath, when the task names the runtime, such as uv's
	// python_program_path, replaces runtime and is written as runtimeField.
	runtimePath, runtimeField string
	// install returns the installer's arguments, once the installer's
	// version is known.
	install func(installerVersion string) []string
	// env is added to the installer's environment.
	env map[string]string
	// preview adds what a dryrun can say about an installation that would
	// run, such as the operations composer lists.
	preview func(ctx context.Context, i *installer, value map[string]any) error
	// value is added to every result's value.
	value map[string]any
}

// dependencyRecord is the record of an installation in the project
// directory. It is written last, so an interrupted installation leaves the
// old record and installs again.
type dependencyRecord struct {
	LockFile       string         `json:"lock_file"`
	LockFileSHA256 string         `json:"lock_file_sha256"`
	Manifest       string         `json:"manifest"`
	ManifestSHA256 string         `json:"manifest_sha256"`
	Settings       map[string]any `json:"settings"`
	Versions       []string       `json:"versions"`
}

func (p projectDependencies) recordPath() string {
	return filepath.Join(p.directory, ".mmx-"+strings.ReplaceAll(p.module, "_", "-"))
}

// readDependencyRecord reads a module's record, "" when there is none.
func readDependencyRecord(path string) (string, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	return string(b), err
}

// installProjectDependencies installs when the record does not match.
func (run Runtime) installProjectDependencies(ctx context.Context, p projectDependencies) protocol.Result {
	if info, err := os.Stat(p.directory); err != nil || !info.IsDir() {
		if errors.Is(err, os.ErrNotExist) {
			return run.notYet(fmt.Errorf("the lock file directory %s does not exist yet; an earlier task, such as a git checkout, may create it", p.directory))
		}
		return protocol.Failed(fmt.Errorf("lock_file_dir %s is not a directory", p.directory))
	}
	// Dependency files may differ from what a run finds after an earlier
	// node task, such as a git checkout, changed it.
	for _, name := range append(slices.Clone(p.lockFiles), p.manifest) {
		if dependencyFile := filepath.Join(p.directory, name); run.EarlierNodeTaskWouldChange(dependencyFile) {
			return run.notYet(fmt.Errorf("an earlier node task would change the dependency file %s, so a dryrun cannot tell what %s would install", dependencyFile, p.module))
		}
	}
	i, stop := run.newInstaller(p.module, p.settings)
	if stop != nil {
		return *stop
	}
	if p.runtimePath != "" && !executableFile(p.runtimePath) {
		return run.notYet(fmt.Errorf("%s: %s %s is not an executable program; an earlier task may install it", p.module, p.runtimeField, p.runtimePath))
	}
	for name, setting := range p.env {
		i.env[name] = setting
	}
	lockFile := ""
	for _, name := range p.lockFiles {
		if _, err := os.Stat(filepath.Join(p.directory, name)); err == nil {
			lockFile = name
			break
		}
	}
	if lockFile == "" {
		return protocol.ConfigurationError(fmt.Errorf("%s installs only what a lock file lists, and %s has no %s; commit the lock file with the application", p.module, p.directory, strings.Join(p.lockFiles, " or ")))
	}
	digest, err := fileSHA256(filepath.Join(p.directory, lockFile))
	if err != nil {
		return protocol.Failed(err)
	}
	manifestDigest, err := fileSHA256(filepath.Join(p.directory, p.manifest))
	if err != nil {
		return protocol.Failed(fmt.Errorf("read dependency manifest %s: %w", p.manifest, err))
	}
	installerVersion, err := i.read(ctx, p.directory, i.program, "--version")
	if err != nil {
		return protocol.Failed(err)
	}
	installerVersion = strings.TrimSpace(installerVersion)
	runtimeVersion := p.runtime + " is not on PATH"
	runtime := p.runtimePath
	if runtime == "" {
		runtime = lookUpProgram(p.runtime, i.env["PATH"])
	}
	if runtime != "" {
		if out, err := i.read(ctx, p.directory, runtime, "--version"); err == nil {
			runtimeVersion = strings.SplitN(strings.TrimSpace(out), "\n", 2)[0]
		}
	}
	wanted := dependencyRecord{LockFile: lockFile, LockFileSHA256: digest, Manifest: p.manifest, ManifestSHA256: manifestDigest, Settings: p.chosen,
		Versions: []string{filepath.Base(i.program) + " " + strings.SplitN(installerVersion, "\n", 2)[0], runtimeVersion}}
	text, err := json.MarshalIndent(wanted, "", "  ")
	if err != nil {
		return protocol.Failed(err)
	}
	record := string(text) + "\n"
	reason := p.reinstallReason(record, wanted)
	result := func(change, reason string) protocol.Result {
		r := protocol.Success(change)
		r.Reason = reason
		v := map[string]any{"lock_file": lockFile}
		for k, x := range p.value {
			v[k] = x
		}
		r.Value = v
		return r
	}
	if reason == "" {
		return result("unchanged", "")
	}
	if run.Dryrun {
		r := result("would_change", "would install: "+reason)
		if p.preview != nil {
			if err := p.preview(ctx, i, r.Value.(map[string]any)); err != nil {
				return protocol.Failed(err)
			}
		}
		return r
	}
	if _, err := i.change(ctx, "install in "+p.directory, p.directory, i.program, p.install(installerVersion)...); err != nil {
		r := protocol.Failed(err)
		r.Change = "unknown"
		return r
	}
	owner, group := "", ""
	if p.settings.account != "" {
		account := run.LookUpAccount(p.settings.account)
		owner, group = p.settings.account, groupName(uint32(account.GroupID))
	}
	// The installation can change the installer in effect: with a Gemfile.lock
	// that names another Bundler, bundle runs the locked one afterwards.
	// Record the version that the next run will read.
	if after, err := i.read(ctx, p.directory, i.program, "--version"); err == nil {
		wanted.Versions[0] = filepath.Base(i.program) + " " + strings.SplitN(strings.TrimSpace(after), "\n", 2)[0]
		if text, err := json.MarshalIndent(wanted, "", "  "); err == nil {
			record = string(text) + "\n"
		}
	}
	run.RetainDiff = nil
	if written := run.WriteFile(ctx, p.recordPath(), record, owner, group, "0644"); written.Error != "" {
		r := result("changed", reason)
		r.Status, r.Reason = "failed", "the packages are installed, but the record "+p.recordPath()+" was not written, so the next run installs them again: "+written.Error
		return r
	}
	return result("changed", "installed: "+reason)
}

// reinstallReason says why the installation must run, "" when the record
// matches and the installed packages are there.
func (p projectDependencies) reinstallReason(record string, wanted dependencyRecord) string {
	text, err := readDependencyRecord(p.recordPath())
	if err != nil || text == "" {
		return "there is no record of an earlier installation"
	}
	var old dependencyRecord
	if json.Unmarshal([]byte(text), &old) != nil {
		return "the record " + p.recordPath() + " cannot be read"
	}
	switch {
	case old.LockFile != wanted.LockFile || old.LockFileSHA256 != wanted.LockFileSHA256:
		return wanted.LockFile + " changed since the last installation"
	case old.Manifest != wanted.Manifest || old.ManifestSHA256 != wanted.ManifestSHA256:
		return wanted.Manifest + " changed since the last installation"
	case !jsonEqual(old.Settings, wanted.Settings):
		return "the task's settings changed since the last installation"
	case !slices.Equal(old.Versions, wanted.Versions):
		return fmt.Sprintf("the installer or language runtime version changed since the last installation: now %s; previously %s", strings.Join(wanted.Versions, ", "), strings.Join(old.Versions, ", "))
	case text != record:
		return "the record " + p.recordPath() + " differs"
	}
	for _, path := range p.installedPaths {
		if !filepath.IsAbs(path) {
			path = filepath.Join(p.directory, path)
		}
		if _, err := os.Lstat(path); err == nil {
			return ""
		}
	}
	return strings.Join(p.installedPaths, " or ") + " is missing"
}

func jsonEqual(a, b any) bool {
	x, errA := json.Marshal(a)
	y, errB := json.Marshal(b)
	return errA == nil && errB == nil && string(x) == string(y)
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// checkProjectDependencies checks what the five modules share.
func checkProjectDependencies(directory, account string, env map[string]any) error {
	switch {
	case directory == "":
		return invalid("lock_file_dir", "lock_file_dir is required: the directory on the node that holds the lock file")
	case account == "":
		return invalid("run_as_user_account", "run_as_user_account is required: the installer runs code from the packages it installs, which should not run as root unless the task says root")
	}
	return validateEnvNames(env)
}

// projectDependencyArguments adds the arguments the five modules share.
func projectDependencyArguments(arguments *ArgumentList, directory, account *string, programField string, programPath *string, env map[string]any) {
	arguments.Public("lock_file_dir", directory, absolute)
	installerArguments(arguments, account, programField, programPath, env)
}

// dependencyStateReader reads a lock file module's tracked input: its
// record.
func dependencyStateReader(p projectDependencies) (string, error) {
	return readDependencyRecord(p.recordPath())
}

// requiredBool is a yes-or-no setting a task must write.
func requiredBool(field string, v *bool) error {
	if v == nil {
		return invalid(field, field+" is required: true or false")
	}
	return nil
}
