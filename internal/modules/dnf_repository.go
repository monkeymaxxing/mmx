package modules

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode"

	"mmx/internal/protocol"
	"mmx/internal/value"
)

const (
	dnfRepositories = "/etc/yum.repos.d"
	rpmKeys         = "/etc/pki/rpm-gpg"
)

// Source installs a repository's signing key, then its repository file, and
// imports the key into rpm's key store, so dnf never trusts a key it fetched
// itself; absent removes the repository file, then the key. A task with
// existing_repository switches a repository that is already on the node on
// or off through dnf. Any change makes the next installation refresh dnf's
// index.
func (d *DNF) Source(ctx context.Context, run Runtime, r Repository, s value.Scope) (result protocol.Result) {
	defer func() {
		if result.Change == "changed" {
			d.indexOld = true
		}
	}()
	d.five = isDnfFive(ctx)
	if r.ExistingRepository != "" {
		return d.switchRepository(ctx, run, r)
	}
	repository := filepath.Join(dnfRepositories, r.Name+".repo")
	key := filepath.Join(rpmKeys, "RPM-GPG-KEY-"+r.Name)
	record := filepath.Join(rpmKeys, r.Name+importedKeyIDsSuffix)
	if r.State == "absent" {
		recorded := readImportedKeyIDs(record)
		if len(recorded) > 0 {
			if _, err := lookPath(ctx, "gpg"); err != nil {
				return protocol.Failed(missingProgramError("gpg"))
			}
		}
		result := applyFiles(ctx, run, []File{{Path: repository, State: "absent"}, {Path: key, State: "absent"}, {Path: record, State: "absent"}}, repository)
		if result.Status != "succeeded" || run.Dryrun {
			return result
		}
		removed, failure := removeKeys(ctx, recorded, key, rpmKeys)
		if failure != nil {
			return *failure
		}
		if removed {
			result.Change = "changed"
		}
		return result
	}
	if _, err := lookPath(ctx, "gpg"); err != nil {
		return protocol.Failed(missingProgramError("gpg"))
	}
	text, err := dnfRepositoryText(r, key, s)
	if err != nil {
		return protocol.Failed(err)
	}
	mode := "0644"
	if r.Username != "" {
		mode = "0600"
	}
	keyFile := File{Path: key, Source: r.SigningKeySource, URL: r.SigningKeyURL, SHA256: r.SigningKeySHA256, Asset: r.Key, Owner: "root", Group: "root", Mode: "0644"}
	recorded := readImportedKeyIDs(record)
	keyResult := run.File(ctx, keyFile, nil)
	if keyResult.Status != "succeeded" {
		return keyResult
	}
	// Import a key that changed before anything uses the repository, and one
	// that an earlier try wrote but rpm did not take: its file is unchanged
	// then, but rpm lacks the key.
	imports := keyResult.Change == "changed"
	if !imports && !run.Dryrun {
		for _, id := range rpmKeyIDs(ctx, key) {
			imports = imports || !rpmKeyInstalled(ctx, id)
		}
		if imports {
			keyResult.Change = "changed"
		}
	}
	if imports {
		imported, failure := importKeyFile(ctx, key, record, recorded, rpmKeys)
		if failure != nil {
			return *failure
		}
		recordText := strings.Join(imported, "\n")
		if recordText != "" {
			recordText += "\n"
		}
		if recordResult := run.File(ctx, File{Path: record, Literal: &recordText, Owner: "root", Group: "root", Mode: "0644"}, nil); recordResult.Status != "succeeded" {
			return recordResult
		}
	}
	result = run.File(ctx, File{Path: repository, Literal: &text, Owner: "root", Group: "root", Mode: mode}, nil)
	result.Change = protocol.CombineChange(keyResult.Change, result.Change)
	if result.FileChange == nil {
		result.FileChange = keyResult.FileChange
	}
	return result
}

const importedKeyIDsSuffix = ".imported_key_ids"

// rpmKeyIDs are the short ids of the keys in a key file, which are the
// versions of the gpg-pubkey packages that rpm records for them. A file that
// does not exist or holds no key has none.
func rpmKeyIDs(ctx context.Context, file string) []string {
	if _, err := os.Stat(file); err != nil {
		return nil
	}
	r := Command(ctx, dnfEnv, "", "gpg", "--batch", "--with-colons", "--show-keys", file)
	if r.Status != "succeeded" {
		return nil
	}
	var ids []string
	for _, line := range strings.Split(r.Stdout, "\n") {
		if fields := strings.Split(line, ":"); fields[0] == "pub" && len(fields) > 4 && len(fields[4]) >= 8 {
			ids = append(ids, strings.ToLower(fields[4][len(fields[4])-8:]))
		}
	}
	return ids
}

// readImportedKeyIDs are the ids MMX imported into rpm's key store for a
// repository, as its record file lists them.
func readImportedKeyIDs(record string) []string {
	text, err := os.ReadFile(record)
	if err != nil {
		return nil
	}
	return strings.Fields(string(text))
}

func rpmKeyInstalled(ctx context.Context, id string) bool {
	return rpmKeyPackage(ctx, id) != ""
}

// rpmKeyPackage is the name of the package in which rpm holds the key with a
// short id, or "". Before rpm 6 the package's version is the short id; rpm 6,
// on Fedora 45, makes it the key's whole fingerprint, which holds the short
// id: at its end for a version 4 key, inside it for a version 6 key.
func rpmKeyPackage(ctx context.Context, id string) string {
	if Command(ctx, dnfEnv, "", "rpm", "-q", "gpg-pubkey-"+id).Status == "succeeded" {
		return "gpg-pubkey-" + id
	}
	listed := Command(ctx, dnfEnv, "", "rpm", "-q", "--qf", `%{VERSION}\n`, "gpg-pubkey")
	if listed.Status != "succeeded" {
		return ""
	}
	for _, version := range strings.Fields(strings.ToLower(listed.Stdout)) {
		if strings.Contains(version, id) {
			return "gpg-pubkey-" + version
		}
	}
	return ""
}

// keyHolders are the key files in keyDirectory other than own that hold the
// key: other repositories still need it.
func keyHolders(ctx context.Context, id, own, keyDirectory string) []string {
	var holders []string
	files, _ := os.ReadDir(keyDirectory)
	for _, entry := range files {
		other := filepath.Join(keyDirectory, entry.Name())
		if other == own || !entry.Type().IsRegular() || strings.HasSuffix(other, importedKeyIDsSuffix) {
			continue
		}
		if slices.Contains(rpmKeyIDs(ctx, other), id) {
			holders = append(holders, other)
		}
	}
	return holders
}

// recordedByAnotherRepository says that another repository's record file
// lists the key: that repository imported it, and this one shares it.
func recordedByAnotherRepository(id, ownRecord, keyDirectory string) bool {
	records, _ := filepath.Glob(filepath.Join(keyDirectory, "*"+importedKeyIDsSuffix))
	for _, record := range records {
		if record != ownRecord && slices.Contains(readImportedKeyIDs(record), id) {
			return true
		}
	}
	return false
}

// removeKeys takes keys out of rpm's key store, which every repository on
// the node trusts, except those another key file holds. It says whether it
// removed any.
func removeKeys(ctx context.Context, ids []string, own, keyDirectory string) (bool, *protocol.Result) {
	removed := false
	for _, id := range ids {
		if len(keyHolders(ctx, id, own, keyDirectory)) > 0 {
			continue
		}
		name := rpmKeyPackage(ctx, id)
		if name == "" {
			continue
		}
		if r := Command(ctx, dnfEnv, "", "rpm", "-e", "--allmatches", name); r.Status != "succeeded" {
			r.Change = "changed"
			return removed, &r
		}
		removed = true
	}
	return removed, nil
}

// importKeyFile imports a key file that changed. Only keys that rpm did not
// hold before are the module's own: a key the node held already, such as the
// distribution's, stays whatever happens to the repository. The result is
// the ids to record: those the node did not hold, those this repository
// recorded before, and those another repository recorded, which it shares.
// Ids that the earlier key file had imported and the
// new one does not hold are removed after the import.
func importKeyFile(ctx context.Context, key, ownRecord string, recorded []string, keyDirectory string) ([]string, *protocol.Result) {
	ids := rpmKeyIDs(ctx, key)
	var record []string
	for _, id := range ids {
		if slices.Contains(recorded, id) || !rpmKeyInstalled(ctx, id) || recordedByAnotherRepository(id, ownRecord, keyDirectory) {
			record = append(record, id)
		}
	}
	if r := Command(ctx, dnfEnv, "", "rpm", "--import", key); r.Status != "succeeded" {
		r.Change = "changed"
		return nil, &r
	}
	// An rpm can accept an import and still not hold the key, for example when
	// the key's own dates are in the future. Fail now, not at the first install.
	for _, id := range ids {
		if !rpmKeyInstalled(ctx, id) {
			return nil, &protocol.Result{Status: "failed", Change: "changed", Reason: fmt.Sprintf("rpm --import %s succeeded, but rpm does not list the key gpg-pubkey-%s afterwards; check that the key's creation and expiry dates are valid on this node", key, id)}
		}
	}
	var rotated []string
	for _, id := range recorded {
		if !slices.Contains(ids, id) {
			rotated = append(rotated, id)
		}
	}
	if _, failure := removeKeys(ctx, rotated, key, keyDirectory); failure != nil {
		return nil, failure
	}
	return record, nil
}

// applyFiles applies files in order and shows the change of the one at shown,
// else the first change.
func applyFiles(ctx context.Context, run Runtime, files []File, shown string) protocol.Result {
	result := protocol.Success("unchanged")
	var change *protocol.FileChange
	for _, f := range files {
		next := run.File(ctx, f, nil)
		next.Change = protocol.CombineChange(result.Change, next.Change)
		if next.FileChange != nil && (change == nil || f.Path == shown) {
			change = next.FileChange
		}
		result = next
		if result.Status != "succeeded" {
			break
		}
	}
	result.FileChange = change
	return result
}

// dnfRepositoryText is the repository file with one section. MMX always
// writes gpgcheck=1, with the key it installed.
func dnfRepositoryText(r Repository, key string, s value.Scope) (string, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "[%s]\nname=%s\n", r.Name, r.Description)
	switch {
	case len(r.BaseURL) > 0:
		fmt.Fprintf(&b, "baseurl=%s\n", strings.Join(r.BaseURL, " "))
	case r.Metalink != "":
		fmt.Fprintf(&b, "metalink=%s\n", r.Metalink)
	default:
		fmt.Fprintf(&b, "mirrorlist=%s\n", r.Mirrorlist)
	}
	enabled := 1
	if r.Enabled != nil && !*r.Enabled {
		enabled = 0
	}
	fmt.Fprintf(&b, "enabled=%d\ngpgcheck=1\ngpgkey=file://%s\n", enabled, key)
	if r.Priority != nil {
		fmt.Fprintf(&b, "priority=%d\n", *r.Priority)
	}
	if len(r.IncludePkgs) > 0 {
		fmt.Fprintf(&b, "includepkgs=%s\n", strings.Join(r.IncludePkgs, " "))
	}
	if len(r.Exclude) > 0 {
		fmt.Fprintf(&b, "exclude=%s\n", strings.Join(r.Exclude, " "))
	}
	for _, credential := range []struct{ field, raw string }{{"username", r.Username}, {"password", r.Password}} {
		if credential.raw == "" {
			continue
		}
		v, err := s.Resolve(credential.raw)
		text := ""
		if err == nil {
			text, err = value.Scalar(v)
		}
		if err == nil && strings.ContainsFunc(text, unicode.IsSpace) {
			err = fmt.Errorf("must not contain spaces or line breaks, which a repository file cannot hold")
		}
		if err != nil {
			return "", fmt.Errorf("dnf_repository.%s: %w", credential.field, err)
		}
		fmt.Fprintf(&b, "%s=%s\n", credential.field, text)
	}
	return b.String(), nil
}

// switchRepository switches a repository that is already on the node on or
// off with dnf's config-manager. MMX never edits the file that defines the
// repository, because the package that owns the file rewrites it.
func (d *DNF) switchRepository(ctx context.Context, run Runtime, r Repository) protocol.Result {
	listed := Command(ctx, dnfEnv, "", "dnf", "-q", "repolist", "--all")
	if listed.Status != "succeeded" {
		return listed
	}
	var ids []string
	state := ""
	for i, line := range strings.Split(listed.Stdout, "\n") {
		fields := strings.Fields(line)
		if i == 0 || len(fields) == 0 {
			continue
		}
		ids = append(ids, fields[0])
		if fields[0] == r.ExistingRepository {
			state = fields[len(fields)-1]
		}
	}
	if state == "" {
		return protocol.Failed(fmt.Errorf("dnf_repository: this node defines no repository %s; it has: %s", r.ExistingRepository, strings.Join(ids, ", ")))
	}
	want := *r.Enabled
	if (state == "enabled") == want {
		return protocol.Success("unchanged")
	}
	if run.Dryrun {
		return protocol.Success("would_change")
	}
	if err := run.Mutation("dnf repository " + r.ExistingRepository); err != nil {
		return protocol.Failed(err)
	}
	// dnf 5 has the commands enable and disable; dnf 4 has the options
	// --set-enabled and --set-disabled.
	args := []string{"-q", "config-manager", "--set-disabled", r.ExistingRepository}
	if want {
		args[2] = "--set-enabled"
	}
	if d.five {
		args = []string{"-q", "config-manager", "disable", r.ExistingRepository}
		if want {
			args[2] = "enable"
		}
	}
	result := Command(ctx, dnfEnv, "", "dnf", args...)
	if result.Status != "succeeded" {
		if d.five && strings.Contains(result.Stdout+result.Stderr, `Unknown argument "config-manager"`) {
			result.Reason = "dnf_repository: switching a repository needs dnf's config-manager, which is in the package dnf5-plugins; add a dnf_package task with packages: [{name: dnf5-plugins, version: newest_when_first_installed}] before this one"
		} else if strings.Contains(result.Stdout+result.Stderr, "No such command") {
			result.Reason = "dnf_repository: switching a repository needs dnf's config-manager, which is in the package dnf-plugins-core; add a dnf_package task with packages: [{name: dnf-plugins-core, version: newest_when_first_installed}] before this one"
		}
		result.Change = "unknown"
		return result
	}
	return protocol.Success("changed")
}
