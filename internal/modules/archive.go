package modules

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"

	"mmx/internal/assets"
	"mmx/internal/protocol"
)

// archiveRecord is the file in an extracted directory that records what was
// extracted there, so an unchanged run needs no rescan.
const archiveRecord = ".mmx-archive"

// stagingName is the private directory beside a release path that an
// extraction or clone fills before renaming it into place. The next attempt
// replaces one an interruption left behind.
func stagingName(name string) string { return ".mmx-staging-" + name }

// installStaged creates the release directory name within parent. fill builds
// it in the staging directory, which it receives by name; one sync then makes
// the tree durable before it appears, since per-file fsync would dominate a
// large release. The rename happens only while name is still absent, and the
// parent is synced after it. installed reports whether the directory reached
// name; an error after that is the parent's sync. path is the directory's
// full path, which decides the SELinux labels of the tree.
func installStaged(ctx context.Context, parent *os.Root, path string, fill func(staging string) error) (installed bool, err error) {
	name := filepath.Base(path)
	staging := stagingName(name)
	if err := parent.RemoveAll(staging); err != nil {
		return false, err
	}
	if err := parent.Mkdir(staging, 0700); err != nil {
		return false, err
	}
	err = fill(staging)
	if err == nil {
		err = labelTree(ctx, parent, staging, path)
	}
	if err == nil {
		// syscall.Sync returns nothing on Linux, where the node runs, and an
		// error only on macOS, where it is excluded from errcheck.
		syscall.Sync()
		err = sameFileAt(parent, name, nil)
	}
	if err == nil {
		err = parent.Rename(staging, name)
	}
	if err != nil {
		_ = parent.RemoveAll(staging)
		return false, err
	}
	return true, syncDirectory(parent)
}

// Archive extracts a frozen tar (plain, or compressed with gzip, xz, bzip2, or
// zstd) or zip into a new directory. Entries are untrusted: every one is
// checked before it is created within the staging root, and links may not
// lead out of the directory.
func (run Runtime) Archive(ctx context.Context, a Archive) protocol.Result {
	if err := ValidateArchive(a); err != nil {
		return protocol.ConfigurationError(err)
	}
	parent, info, err := openParent(a.Path, true)
	if err != nil {
		return protocol.Failed(err)
	}
	if parent == nil && a.State != "absent" {
		if r := run.missingParent(a.Path); r != nil {
			return *r
		}
	}
	if parent != nil {
		defer parent.Close()
	}
	name := filepath.Base(a.Path)
	var record *ArchiveRecord
	if info != nil && info.IsDir() {
		if record, err = readArchiveRecord(parent, name); err != nil {
			return protocol.Failed(err)
		}
	}
	if a.State == "absent" {
		return run.removeArchive(ctx, parent, a.Path, info, record)
	}
	if a.Asset == nil {
		return protocol.Failed(fmt.Errorf("unpack_archive: the archive has no saved asset"))
	}
	want := ArchiveRecord{SHA256: a.Asset.SHA256, UnpackWithoutWrappingFolder: a.UnpackWithoutWrappingFolder, Owner: string(a.Owner), Group: string(a.Group)}
	if info != nil {
		if record != nil && *record == want {
			return protocol.Success("unchanged")
		}
		return protocol.Failed(fmt.Errorf("%s exists but has no matching record for the requested archive and extraction settings; extract each version to a new path", a.Path))
	}
	owner, err := metadataFor(nil, 0755, "", a.Owner, a.Group)
	if err != nil {
		return run.accountFailure(err)
	}
	file, err := assets.Open(a.Asset.Path)
	if err != nil {
		return protocol.Failed(err)
	}
	defer file.Close()
	// The files may fill the filesystem only up to the part that stays free.
	extracted := archiveSpace{path: filepath.Dir(a.Path), space: math.MaxInt64}
	if parent != nil {
		if extracted.space, err = assets.WritableSpace(extracted.path); err != nil {
			return protocol.Failed(err)
		}
	}
	if run.Dryrun {
		count, err := readArchive(ctx, file, a.Asset.Size, a.UnpackWithoutWrappingFolder, extracted, nil)
		if err != nil {
			return protocol.Failed(err)
		}
		r := protocol.Success("would_change")
		r.Value = map[string]any{"entries": count}
		return r
	}
	if err := ctx.Err(); err != nil {
		return protocol.Failed(err)
	}
	if err := run.Mutation(a.Path); err != nil {
		return protocol.Failed(err)
	}
	count := 0
	installed, err := installStaged(ctx, parent, a.Path, func(staging string) error {
		root, err := parent.OpenRoot(staging)
		if err != nil {
			return err
		}
		defer root.Close()
		x := extraction{ctx: ctx, root: root, uid: owner.uid, gid: owner.gid}
		if count, err = readArchive(ctx, file, a.Asset.Size, a.UnpackWithoutWrappingFolder, extracted, x.create); err != nil {
			return err
		}
		return x.finish(want)
	})
	if !installed {
		return protocol.Failed(err)
	}
	r := protocol.Success("changed")
	r.Value = map[string]any{"entries": count}
	if err != nil {
		r.Status, r.Reason = "failed", err.Error()
	}
	return r
}

// removeArchive removes a directory an unpack_archive task extracted. It is renamed
// to its staging name first, so an interrupted removal never leaves a partial
// tree at path without its record. A staging directory that an interrupted or
// failed removal or extraction left behind is deleted too, also when path is
// gone: otherwise the old tree would stay on disk while every later run
// reported the removal unchanged.
func (run Runtime) removeArchive(ctx context.Context, parent *os.Root, full string, info os.FileInfo, record *ArchiveRecord) protocol.Result {
	name := filepath.Base(full)
	staging := stagingName(name)
	tree := name
	if info == nil {
		if parent == nil {
			return protocol.Success("unchanged")
		}
		left, err := parent.Lstat(staging)
		if errors.Is(err, os.ErrNotExist) || err == nil && !left.IsDir() {
			return protocol.Success("unchanged")
		}
		if err != nil {
			return protocol.Failed(err)
		}
		tree = staging
	} else if record == nil {
		return protocol.Failed(fmt.Errorf("%s has no %s record; unpack_archive removes only directories created by unpack_archive", full, archiveRecord))
	}
	// Deleting through a mount point inside would delete another
	// filesystem's files.
	dir, err := parent.Open(".")
	if err != nil {
		return protocol.Failed(err)
	}
	defer dir.Close()
	device, err := deviceOf(dir)
	if err == nil {
		_, err = deletionTree(ctx, dir, device, tree, filepath.Join(filepath.Dir(full), tree))
	}
	if err != nil {
		return protocol.Failed(err)
	}
	if run.Dryrun {
		return protocol.Success("would_change")
	}
	if err := ctx.Err(); err != nil {
		return protocol.Failed(err)
	}
	if err := run.Mutation(full); err != nil {
		return protocol.Failed(err)
	}
	err = parent.RemoveAll(staging)
	if err == nil && info == nil {
		return protocol.Success("changed")
	}
	if err == nil {
		err = sameFileAt(parent, name, info)
	}
	if err == nil {
		err = parent.Rename(name, staging)
	}
	if err != nil {
		return protocol.Failed(err)
	}
	r := protocol.Success("changed")
	if err := parent.RemoveAll(staging); err != nil {
		r.Status, r.Reason = "failed", err.Error()
	}
	return r
}

// readArchiveRecord reads path's record, or returns nil when it has none. A
// record that does not parse is an error: it cannot establish that MMX
// extracted the directory, so removal must leave it untouched.
func readArchiveRecord(parent *os.Root, name string) (*ArchiveRecord, error) {
	dir, err := parent.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	text, err := readTextAt(dir, archiveRecord)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var record ArchiveRecord
	if err := json.Unmarshal([]byte(text), &record); err != nil {
		return nil, fmt.Errorf("The archive record %s cannot be read: %w. MMX requires a readable archive record before removing an extracted directory. Check the archive record and the extracted directory's contents before retrying.", filepath.Join(parent.Name(), name, archiveRecord), err)
	}
	return &record, nil
}

func syncDirectory(root *os.Root) error {
	d, err := root.Open(".")
	if err == nil {
		err = d.Sync()
		_ = d.Close()
	}
	return err
}

// archiveEntry is one checked entry: a clean relative name, and for a link
// its target (a symlink's text, or the entry a hard link names).
type archiveEntry struct {
	name   string
	kind   byte // 'd' directory, 'f' regular file, 'l' symlink, 'h' hard link
	mode   os.FileMode
	target string
	// parents are the missing directories above name, created before it.
	parents []string
}

// archiveSpace is how many bytes of files an extraction into the directory
// at path may write: its filesystem's space beyond the part that stays free.
type archiveSpace struct {
	path  string
	space int64
}

// readArchive checks every entry of a tar, plain or compressed, or a zip, in
// archive order, and passes each one it keeps to create with a regular
// file's contents. With a nil create it only checks and counts. The sizes the
// entries declare, which the readers enforce, must fit in extracted.space.
func readArchive(ctx context.Context, file *os.File, size int64, withoutWrappingFolder bool, extracted archiveSpace, create func(archiveEntry, io.Reader) error) (int, error) {
	check := archiveCheck{withoutWrappingFolder: withoutWrappingFolder, kinds: map[string]byte{}}
	count := 0
	written := int64(0)
	entry := func(name string, kind byte, mode os.FileMode, target string, length int64, contents io.Reader) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		e, keep, err := check.entry(name, kind, mode, target)
		if err != nil || !keep {
			return err
		}
		if e.kind == 'f' {
			if written += length; written > extracted.space {
				return fmt.Errorf("archive entry %q: the archive's files need more space than %s has while %d%% of its filesystem stays free", name, extracted.path, assets.KeepFreePercent)
			}
		}
		count++
		if create == nil {
			return nil
		}
		return create(e, contents)
	}
	magic := make([]byte, 262)
	n, err := file.ReadAt(magic, 0)
	if err != nil && err != io.EOF {
		return 0, err
	}
	magic = magic[:n]
	isZip := bytes.HasPrefix(magic, []byte("PK\x03\x04")) || bytes.HasPrefix(magic, []byte("PK\x05\x06"))
	compression := assets.CompressionOf(magic)
	// A tar has "ustar" at offset 257 of its first header, in the POSIX and
	// the GNU format alike.
	plainTar := len(magic) == 262 && bytes.HasPrefix(magic[257:], []byte("ustar"))
	switch {
	case compression != "" || plainTar:
		var stream io.Reader = file
		if compression != "" {
			decompressed, err := assets.Decompressor(compression, file)
			if err != nil {
				return 0, fmt.Errorf("read archive: %s: %w", compression, err)
			}
			defer decompressed.Close()
			stream = decompressed
		}
		entries := tar.NewReader(stream)
		for {
			h, err := entries.Next()
			if err == io.EOF {
				return count, nil
			}
			if err != nil {
				return count, fmt.Errorf("read archive: %w", err)
			}
			kind := tarKinds[h.Typeflag]
			if h.Typeflag == tar.TypeXGlobalHeader {
				continue // pax metadata such as git archive's commit comment
			}
			if kind == 0 {
				return count, fmt.Errorf("archive entry %q is a special file (type %q); only directories, files, and links are allowed", h.Name, h.Typeflag)
			}
			if err := entry(h.Name, kind, os.FileMode(h.Mode)&0777, h.Linkname, h.Size, entries); err != nil {
				return count, err
			}
		}
	case isZip:
		entries, err := zip.NewReader(file, size)
		if err != nil {
			return 0, fmt.Errorf("read archive: %w", err)
		}
		for _, f := range entries.File {
			mode := f.Mode()
			var kind byte
			switch {
			case mode.IsDir():
				kind = 'd'
			case mode&os.ModeSymlink != 0:
				kind = 'l'
			case mode.IsRegular():
				kind = 'f'
			default:
				return count, fmt.Errorf("archive entry %q is a special file; only directories, files, and links are allowed", f.Name)
			}
			// Only Unix and macOS zip tools record permission bits.
			perm := mode.Perm()
			if creator := f.CreatorVersion >> 8; creator != 3 && creator != 19 {
				perm = 0644
				if kind == 'd' {
					perm = 0755
				}
			}
			if err := zipEntry(f, func(contents io.Reader) error {
				target := ""
				if kind == 'l' {
					b, err := io.ReadAll(io.LimitReader(contents, 4096))
					if err != nil {
						return err
					}
					target = string(b)
				}
				return entry(f.Name, kind, perm, target, int64(f.UncompressedSize64), contents)
			}); err != nil {
				return count, err
			}
		}
		return count, nil
	}
	return 0, fmt.Errorf("archive must be a tar, plain or compressed with gzip, xz, bzip2, or zstd, or a zip file")
}

var tarKinds = map[byte]byte{tar.TypeDir: 'd', tar.TypeReg: 'f', tar.TypeSymlink: 'l', tar.TypeLink: 'h'}

func zipEntry(f *zip.File, use func(io.Reader) error) error {
	contents, err := f.Open()
	if err != nil {
		return fmt.Errorf("read archive entry %q: %w", f.Name, err)
	}
	defer contents.Close()
	return use(contents)
}

// archiveCheck holds every name the archive created so far, with its kind.
// Links never lead out of the directory: a symlink is relative, with ".."
// only as leading components that stay inside, and no entry is created at or
// below one, so every path above it is a real directory. A hard link names an
// earlier regular file.
type archiveCheck struct {
	// withoutWrappingFolder leaves out the archive's one top-level folder;
	// wrappingFolder is its name, taken from the first entry inside a folder.
	withoutWrappingFolder bool
	wrappingFolder        string
	kinds                 map[string]byte
}

// archiveName is an entry name's components below the directory, or nil for
// an entry that is skipped: without the wrapping folder, that is the folder
// itself.
func (c *archiveCheck) archiveName(name string) ([]string, error) {
	if strings.HasPrefix(name, "/") {
		return nil, fmt.Errorf("archive entry %q has an absolute path", name)
	}
	var parts []string
	for _, part := range strings.Split(name, "/") {
		switch part {
		case "", ".":
			continue
		case "..":
			return nil, fmt.Errorf("archive entry %q has a .. component", name)
		}
		parts = append(parts, part)
	}
	if !c.withoutWrappingFolder {
		return parts, nil
	}
	if len(parts) == 0 {
		return nil, nil
	}
	if c.wrappingFolder == "" {
		c.wrappingFolder = parts[0]
	}
	if parts[0] != c.wrappingFolder {
		return nil, fmt.Errorf("archive entry %q is outside the one wrapping folder %q; unpack_without_wrapping_folder requires every entry inside that folder", name, c.wrappingFolder)
	}
	return parts[1:], nil
}

func (c *archiveCheck) entry(name string, kind byte, mode os.FileMode, target string) (archiveEntry, bool, error) {
	parts, err := c.archiveName(name)
	if err == nil && c.withoutWrappingFolder && parts != nil && len(parts) == 0 && kind != 'd' {
		return archiveEntry{}, false, fmt.Errorf("archive entry %q is outside a wrapping folder; unpack_without_wrapping_folder requires one top-level directory", name)
	}
	if err != nil || len(parts) == 0 {
		return archiveEntry{}, false, err
	}
	e := archiveEntry{name: strings.Join(parts, "/"), kind: kind, mode: mode}
	if e.name == archiveRecord {
		return e, false, fmt.Errorf("archive entry %q uses the name MMX keeps its record in", name)
	}
	for i := 1; i < len(parts); i++ {
		dir := strings.Join(parts[:i], "/")
		switch c.kinds[dir] {
		case 0:
			c.kinds[dir] = 'd'
			e.parents = append(e.parents, dir)
		case 'd':
		default:
			return e, false, fmt.Errorf("archive entry %q is below %q, which is not a directory", name, dir)
		}
	}
	if existing, ok := c.kinds[e.name]; ok && !(existing == 'd' && kind == 'd') {
		return e, false, fmt.Errorf("archive entry %q appears twice", name)
	}
	switch kind {
	case 'l':
		if !insideSymlink(len(parts)-1, target) {
			return e, false, fmt.Errorf("archive symlink %q -> %q leads out of the directory", name, target)
		}
		e.target = target
	case 'h':
		linked, err := c.archiveName(target)
		if err != nil || linked == nil || c.kinds[strings.Join(linked, "/")] != 'f' {
			return e, false, fmt.Errorf("archive hard link %q -> %q must name an earlier regular file in the directory", name, target)
		}
		e.target = strings.Join(linked, "/")
		kind = 'f'
	}
	c.kinds[e.name] = kind
	return e, true, nil
}

// insideSymlink reports whether a symlink depth directories below the root
// stays inside: relative, with ".." only as up to depth leading components.
func insideSymlink(depth int, target string) bool {
	if target == "" || path.IsAbs(target) || strings.ContainsRune(target, 0) {
		return false
	}
	up, descended := 0, false
	for _, part := range strings.Split(target, "/") {
		switch part {
		case "", ".":
		case "..":
			if descended {
				return false
			}
			up++
		default:
			descended = true
		}
	}
	return up <= depth
}

// extraction creates checked entries within the staging root, owned by the
// task's account with the archive's permission bits, never its IDs.
type extraction struct {
	ctx      context.Context
	root     *os.Root
	uid, gid int
}

func (x extraction) create(e archiveEntry, contents io.Reader) error {
	for _, dir := range e.parents {
		if err := x.directory(dir, 0755); err != nil {
			return err
		}
	}
	switch e.kind {
	case 'd':
		return x.directory(e.name, e.mode)
	case 'l':
		if err := x.root.Symlink(e.target, e.name); err != nil {
			return err
		}
		return x.root.Lchown(e.name, x.uid, x.gid)
	case 'h':
		return x.root.Link(e.target, e.name)
	}
	file, err := x.root.OpenFile(e.name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, err = assets.Copy(x.ctx, file, contents)
	if err == nil {
		_, err = fileMetadata{e.mode, x.uid, x.gid}.apply(file)
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	return err
}

// directory creates name, or sets an existing one's mode when the archive
// lists a directory after a file below it created it.
func (x extraction) directory(name string, mode os.FileMode) error {
	if err := x.root.Mkdir(name, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	dir, err := x.root.Open(name)
	if err != nil {
		return err
	}
	defer dir.Close()
	_, err = fileMetadata{mode, x.uid, x.gid}.apply(dir)
	return err
}

// finish writes the record, root-owned, and gives the directory itself the
// task's owner and mode 0755.
func (x extraction) finish(record ArchiveRecord) error {
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	if err := x.root.WriteFile(archiveRecord, append(data, '\n'), 0644); err != nil {
		return err
	}
	return x.directory(".", 0755)
}

// Archive is an unpack_archive task, a directory extracted from a tar (plain,
// or compressed with gzip, xz, bzip2, or zstd) or zip file: a local source or
// a pinned HTTPS download, frozen like a file's source.
type Archive struct {
	AlsoDeleteEverythingInside bool   `yaml:"also_delete_everything_inside,omitempty" json:"also_delete_everything_inside,omitempty"`
	Source                     string `yaml:"source,omitempty" json:"source,omitempty"`
	URL                        string `yaml:"url,omitempty" json:"url,omitempty"`
	SHA256                     string `yaml:"sha256,omitempty" json:"sha256,omitempty"`
	// DangerouslySkipChecksum takes the place of SHA256 beside URL.
	DangerouslySkipChecksum     bool        `yaml:"dangerously_skip_checksum,omitempty" json:"dangerously_skip_checksum,omitempty"`
	Path                        string      `yaml:"path" json:"path"`
	Owner                       Account     `yaml:"owner,omitempty" json:"owner,omitempty"`
	Group                       Account     `yaml:"group,omitempty" json:"group,omitempty"`
	UnpackWithoutWrappingFolder bool        `yaml:"unpack_without_wrapping_folder,omitempty" json:"unpack_without_wrapping_folder,omitempty"`
	State                       string      `yaml:"state,omitempty" json:"state,omitempty"`
	Asset                       *assets.Ref `yaml:"-" json:"asset,omitempty"`
	LocalPath                   string      `yaml:"-" json:"-"`
	// SourceBase and Variants serve a variable source field (variable_sources.go).
	SourceBase string                    `yaml:"-" json:"-"`
	Variants   map[string]*SourceVariant `yaml:"-" json:"variants,omitempty"`
}

func ValidateArchive(a Archive) error {
	if err := validatePresence(a.State); err != nil {
		return err
	}
	if a.State == "absent" {
		if a.Source != "" || a.URL != "" || a.SHA256 != "" || a.DangerouslySkipChecksum || a.Owner != "" || a.Group != "" || a.UnpackWithoutWrappingFolder {
			return fmt.Errorf("unpack_archive with state: absent accepts only path, state, and also_delete_everything_inside")
		}
		if !a.AlsoDeleteEverythingInside {
			return invalid("also_delete_everything_inside", "Removing this extracted directory also deletes everything inside. Add also_delete_everything_inside: true to allow that deletion.")
		}
		return nil
	}
	if a.AlsoDeleteEverythingInside {
		return invalid("also_delete_everything_inside", "unpack_archive.also_delete_everything_inside goes with state: absent")
	}
	if (a.Source == "") == (a.URL == "") {
		return fmt.Errorf("unpack_archive needs exactly one of source or url")
	}
	if a.Source != "" && a.SHA256 != "" {
		return invalid("sha256", "unpack_archive.sha256 goes with url; a source is frozen with the run")
	}
	if a.Source != "" && a.DangerouslySkipChecksum {
		return invalid("dangerously_skip_checksum", "unpack_archive.dangerously_skip_checksum goes with url; a source is frozen with the run")
	}
	if a.URL != "" {
		if err := validatePin("unpack_archive", "url", "sha256", a.URL, a.SHA256, a.DangerouslySkipChecksum); err != nil {
			return err
		}
	}
	return nil
}

// ArchiveRecord is the .mmx-archive file in an extracted directory. A
// directory whose record matches the task is unchanged.
type ArchiveRecord struct {
	SHA256                      string `json:"sha256"`
	UnpackWithoutWrappingFolder bool   `json:"unpack_without_wrapping_folder"`
	Owner                       string `json:"owner"`
	Group                       string `json:"group"`
}

// unpackArchiveDescription is what mmx describe-module shows for unpack_archive.
var unpackArchiveDescription = Description{
	Name: "unpack_archive", Topic: TopicDirectoriesDownloadsAndReleaseDirectories, Schema: reflect.TypeOf(Archive{}),
	Summary: "a release directory extracted from a tar, plain or compressed with gzip, xz, bzip2, or zstd, or a zip file",
	Fields: []Field{
		{Name: "source", Notes: "local archive file, relative to the project root, or to the role's directory inside a role; one of source or url"},
		{Name: "url", Notes: "HTTPS URL, fetched and verified on the controller like download; one of source or url; may read vars, role inputs, loop items, and .node.name"},
		{Name: "sha256", Notes: "the archive's SHA-256 in hex; with url, exactly one of sha256 or dangerously_skip_checksum"},
		{Name: "dangerously_skip_checksum", Default: "false", Notes: "true in place of sha256: the archive is unpacked as it arrives, with no check that it is the file you expect"},
		{Name: "path", Required: true, Notes: "absolute; the parent must exist; extract each version to its own path"},
		{Name: "owner", Type: "account name or integer", Default: "root", Notes: "owns every extracted entry; the archive's IDs are ignored; a numeric ID is written without quotes"},
		{Name: "group", Type: "group name or integer", Default: "root", Notes: "a numeric ID is written without quotes"},
		{Name: "unpack_without_wrapping_folder", Default: "false", Notes: "true leaves out the one top-level folder many release archives have, so its contents land directly in path; fails if anything lies outside that folder"},
		{Name: "also_delete_everything_inside", Default: "false", Notes: "required with state: absent, because removal deletes the whole extracted tree"},
		{Name: "state", Values: presenceStates, Default: "present", Notes: "absent removes a directory an unpack_archive task extracted, with its whole tree; requires also_delete_everything_inside: true"},
	},
	Example: "- unpack_archive:\n    url: https://releases.example.com/app-1.4.2-linux-amd64.tar.gz\n    sha256: \"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef\"\n    path: /srv/app/releases/1.4.2\n    owner: app\n    group: app\n    unpack_without_wrapping_folder: true\n",
	Results: []string{"status", "change", "value.entries"},
	Dryrun:  "supported; fetches and verifies a url, then checks every entry, and that the files fit while 5% of the filesystem stays free, and counts them without extracting; not_checked while the parent directory is missing, unless an earlier node task of the dryrun would create it",
}

// Prepare checks the inputs when the project loads and finds a source
// archive on the controller machine, relative to base.
func (a *Archive) Prepare(base string) error {
	if err := ValidateArchive(*a); err != nil {
		return err
	}
	a.SourceBase = base
	if a.Source != "" && !Dynamic(a.Source) {
		path, err := localSource(base, "source", a.Source)
		if err != nil {
			return err
		}
		a.LocalPath = path
	}
	return nil
}

// Arguments lists the inputs that may hold a reference, with their rules.
func (a *Archive) Arguments(arguments *ArgumentList) {
	arguments.Public("path", &a.Path, absolute)
	arguments.Owner("owner", &a.Owner)
	arguments.Owner("group", &a.Group)
}

// CleanPaths cleans the resolved path.
func (a *Archive) CleanPaths() {
	a.Path = filepath.Clean(a.Path)
}

// ControllerFiles lists the archive the task unpacks.
func (a *Archive) ControllerFiles() []ControllerFile {
	s := ControllerFile{Base: a.SourceBase, Variants: &a.Variants, Asset: &a.Asset, Local: &a.LocalPath}
	switch {
	case a.Source != "":
		s.Field, s.Kind, s.Path = "source", SourceFile, &a.Source
	case a.URL != "":
		s.Field, s.Kind, s.URL, s.SHA256 = "url", SourceDownload, &a.URL, &a.SHA256
	default:
		return nil
	}
	return []ControllerFile{s}
}

// Run does the module's work on the node.
func (a *Archive) Run(ctx context.Context, task NodeTask) protocol.Result {
	return task.Runtime.Archive(ctx, *a)
}

// ManagedPaths lists the paths the task manages or says it creates, as
// written; a dryrun cannot check a later task's path_exists on them.
func (a *Archive) ManagedPaths() []string {
	return []string{a.Path}
}
