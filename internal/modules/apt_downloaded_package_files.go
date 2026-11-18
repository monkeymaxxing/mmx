package modules

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"time"

	"mmx/internal/protocol"
)

// AptDownloadedPackageFiles deletes the package files that apt downloaded
// into its cache, with apt-get clean.
type AptDownloadedPackageFiles struct {
	State                string `yaml:"state" json:"state"`
	WaitForAptLockAtMost string `yaml:"wait_for_apt_lock_at_most,omitempty" json:"wait_for_apt_lock_at_most,omitempty"`
}

// aptDownloadedPackageFilesStates are the states of the package files; the
// task can only delete them.
var aptDownloadedPackageFilesStates = []string{"absent"}

// aptDownloadedPackageFilesDescription is what mmx describe-module shows
// for apt_downloaded_package_files.
var aptDownloadedPackageFilesDescription = Description{
	Name: "apt_downloaded_package_files", Topic: TopicPackagesAndPackageSources, Schema: reflect.TypeOf(AptDownloadedPackageFiles{}),
	Summary: "deletes the package files that apt downloaded into its archives directory, /var/cache/apt/archives/, with apt-get clean; value has deleted_package_files and deleted_bytes",
	Fields: []Field{
		{Name: "state", Values: aptDownloadedPackageFilesStates, Required: true, Notes: "the only state: apt-get clean deletes every file in apt's archives directory and in its partial/ directory, and apt's two package list caches, which the next apt command builds again; installed packages stay as they are"},
		{Name: "wait_for_apt_lock_at_most", Default: "15m", Notes: aptLockWaitNote},
	},
	Example: "- apt_downloaded_package_files:\n    state: absent\n",
	Results: []string{"status", "change", "reason", "value.deleted_package_files", "value.deleted_bytes"},
	Dryrun:  "deletes nothing; reports the number of package files and their bytes, or unchanged when the archives directory holds none; not_checked while apt-get is missing",
}

// Prepare checks the inputs when the project loads.
func (a *AptDownloadedPackageFiles) Prepare(base string) error {
	if a.State != "absent" {
		return invalid("state", "apt_downloaded_package_files.state is required and must be absent: the task deletes the package files that apt downloaded")
	}
	if writtenDuration(a.WaitForAptLockAtMost) {
		if _, err := aptLockWait("apt_downloaded_package_files", a.WaitForAptLockAtMost); err != nil {
			return err
		}
	}
	return nil
}

// Run does the module's work on the node.
func (a *AptDownloadedPackageFiles) Run(ctx context.Context, task NodeTask) protocol.Result {
	wait, err := aptLockWait("apt_downloaded_package_files", a.WaitForAptLockAtMost)
	if err != nil {
		return protocol.ConfigurationError(err)
	}
	return task.Runtime.AptDownloadedPackageFiles(ctx, wait)
}

// aptCacheEntriesThatStay are the names in apt's archives directory that
// apt-get clean leaves: its lock, and its directories.
var aptCacheEntriesThatStay = []string{"lock", "partial", "auxfiles", "lost+found"}

// AptDownloadedPackageFiles runs apt-get clean when the archives directory
// holds package files, waiting up to wait for another apt's lock as a
// refresh does, and reports the files that are gone afterwards.
func (run Runtime) AptDownloadedPackageFiles(ctx context.Context, wait time.Duration) protocol.Result {
	if _, err := lookPath(ctx, "apt-get"); err != nil {
		return run.notYet(errors.New("apt_downloaded_package_files: apt-get is not installed; the module requires a node that uses apt to install packages"))
	}
	archives, err := aptArchivesDirectory(ctx)
	if err != nil {
		return protocol.Failed(err)
	}
	before, err := downloadedPackageFiles(archives)
	if err != nil {
		return protocol.Failed(err)
	}
	if len(before) == 0 {
		r := protocol.Success("unchanged")
		r.Value = map[string]any{"deleted_package_files": 0, "deleted_bytes": int64(0)}
		r.Reason = "apt's archives directory " + archives + " holds no downloaded package file"
		return r
	}
	if run.Dryrun {
		return downloadedPackageFilesResult(true, archives, before)
	}
	if err := run.Mutation("downloaded package files in " + archives); err != nil {
		return protocol.Failed(err)
	}
	cleaned := aptGetWaitingForLocks(ctx, wait, "clean")
	after, err := downloadedPackageFiles(archives)
	if err != nil {
		r := protocol.Failed(fmt.Errorf("apt_downloaded_package_files: after apt-get clean: %w", err))
		r.Change = "unknown"
		return r
	}
	deleted := map[string]int64{}
	for path, size := range before {
		if _, left := after[path]; !left {
			deleted[path] = size
		}
	}
	result := downloadedPackageFilesResult(false, archives, deleted)
	if cleaned.Status != "succeeded" {
		result.Status, result.NoOtherTry = "failed", cleaned.NoOtherTry
		detail := cleaned.Reason
		if stderr := lastLines(strings.TrimSpace(cleaned.Stderr), 20); stderr != "" {
			detail += ": " + stderr
		}
		result.Reason = "apt_downloaded_package_files: apt-get clean: " + detail + "; " + result.Reason
	}
	return result
}

// downloadedPackageFilesResult reports the package files that apt-get clean
// deleted, or would delete in a dryrun.
func downloadedPackageFilesResult(dryrun bool, archives string, files map[string]int64) protocol.Result {
	var bytes int64
	for _, size := range files {
		bytes += size
	}
	r := protocol.Success("changed")
	r.Value = map[string]any{"deleted_package_files": len(files), "deleted_bytes": bytes}
	r.Reason = fmt.Sprintf("deleted %d package files totaling %s from %s", len(files), bytesText(bytes), archives)
	switch {
	case len(files) == 0:
		r.Change, r.Reason = "unchanged", "apt-get clean deleted no package file from "+archives
	case dryrun:
		r.Change, r.Reason = "would_change", fmt.Sprintf("would delete %d package files totaling %s from %s", len(files), bytesText(bytes), archives)
	}
	return r
}

// aptArchivesDirectory asks apt where it keeps the package files it
// downloads, /var/cache/apt/archives/ unless apt's settings move it.
func aptArchivesDirectory(ctx context.Context) (string, error) {
	r := Command(ctx, aptEnv, "/", "apt-config", "shell", "ARCHIVES", "Dir::Cache::archives/d")
	if r.Status != "succeeded" {
		return "", fmt.Errorf("apt_downloaded_package_files: apt-config shell ARCHIVES Dir::Cache::archives/d: %s", failureTextFromStandardError(r))
	}
	// apt-config prints ARCHIVES='/var/cache/apt/archives/'.
	quoted, found := strings.CutPrefix(strings.TrimSpace(r.Stdout), "ARCHIVES=")
	directory := strings.TrimSuffix(strings.TrimPrefix(quoted, "'"), "'")
	if !found || !filepath.IsAbs(directory) || strings.Contains(directory, "'") || len(quoted) != len(directory)+2 {
		return "", fmt.Errorf("apt_downloaded_package_files: apt-config named no archives directory: %q", strings.TrimSpace(r.Stdout))
	}
	return directory, nil
}

// downloadedPackageFiles lists, with their sizes, what apt-get clean
// deletes: every entry other than a directory in the archives directory,
// except apt's lock, and in its partial/ directory.
func downloadedPackageFiles(archives string) (map[string]int64, error) {
	files := map[string]int64{}
	for _, directory := range []string{archives, filepath.Join(archives, "partial")} {
		entries, err := os.ReadDir(directory)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("apt_downloaded_package_files: %w", err)
		}
		for _, entry := range entries {
			if entry.IsDir() || (directory == archives && slices.Contains(aptCacheEntriesThatStay, entry.Name())) {
				continue
			}
			info, err := entry.Info()
			if errors.Is(err, os.ErrNotExist) {
				continue // deleted since
			}
			if err != nil {
				return nil, fmt.Errorf("apt_downloaded_package_files: %w", err)
			}
			files[filepath.Join(directory, entry.Name())] = info.Size()
		}
	}
	return files, nil
}
