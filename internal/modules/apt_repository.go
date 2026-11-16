package modules

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/url"
	"path/filepath"
	"strings"
	"unicode"

	"mmx/internal/assets"
	"mmx/internal/protocol"
	"mmx/internal/value"
)

const (
	aptSources  = "/etc/apt/sources.list.d"
	aptKeyrings = "/etc/apt/keyrings"
	aptLogins   = "/etc/apt/auth.conf.d"
)

// Source installs a repository's signing key and login, then the deb822
// source file that names the key; absent removes the source file, then the
// key, then the login. The files use the ordinary file installer. Any change
// marks the job's indexes stale, so its next installation or upgrade
// refreshes them.
func (a *APT) Source(ctx context.Context, run Runtime, r Repository, s value.Scope) (result protocol.Result) {
	defer func() {
		if result.Change == "changed" {
			a.indexesFresh = false
		}
	}()
	source := filepath.Join(aptSources, r.Name+".sources")
	result = protocol.Success("unchanged")
	var files []File
	if r.State == "absent" {
		// The key's format is not known here; either name may hold it.
		files = []File{{Path: source, State: "absent"}}
		for _, ext := range []string{".asc", ".gpg"} {
			files = append(files, File{Path: filepath.Join(aptKeyrings, r.Name+ext), State: "absent"})
		}
		files = append(files, File{Path: filepath.Join(aptLogins, r.Name+".conf"), State: "absent"})
	} else {
		ext, err := keyExtension(r.Key)
		if err != nil {
			return protocol.Failed(err)
		}
		key, sourceFile := repositoryFiles(r, ext)
		files = []File{key, sourceFile}
		if r.Username != "" {
			login, err := repositoryLogin(r, s)
			if err != nil {
				return protocol.Failed(err)
			}
			files = []File{key, login, sourceFile}
		} else {
			// A login taken out of the task goes after the source that used it.
			files = append(files, File{Path: filepath.Join(aptLogins, r.Name+".conf"), State: "absent"})
		}
		// Missing keyrings and login directories are created 0755, as apt ships them.
		for _, dir := range []string{aptKeyrings, aptLogins} {
			if dir == aptLogins && r.Username == "" {
				continue
			}
			next := run.Directory(ctx, Directory{Path: dir}, nil)
			next.Change = protocol.CombineChange(result.Change, next.Change)
			if result = next; result.Status != "succeeded" {
				return result
			}
		}
	}
	var shown *protocol.FileChange
	for _, f := range files {
		next := run.File(ctx, f, nil)
		next.Change = protocol.CombineChange(result.Change, next.Change)
		// Show the source file's change, else the key's; never the login's.
		if next.FileChange != nil && filepath.Dir(f.Path) != aptLogins && (shown == nil || f.Path == source) {
			shown = next.FileChange
		}
		result = next
		if result.Status != "succeeded" {
			break
		}
	}
	result.FileChange = shown
	return result
}

// repositoryFiles are a present repository's root-owned key and source file.
func repositoryFiles(r Repository, keyExt string) (key, source File) {
	key = File{Path: filepath.Join(aptKeyrings, r.Name+keyExt), Source: r.SigningKeySource, URL: r.SigningKeyURL, SHA256: r.SigningKeySHA256,
		Asset: r.Key, Owner: "root", Group: "root", Mode: "0644"}
	var b strings.Builder
	b.WriteString("Types: deb\n")
	for _, field := range []struct {
		name   string
		values []string
	}{{"URIs", r.URIs}, {"Suites", r.Suites}, {"Components", r.Components}, {"Architectures", r.Architectures}} {
		if len(field.values) > 0 {
			fmt.Fprintf(&b, "%s: %s\n", field.name, strings.Join(field.values, " "))
		}
	}
	fmt.Fprintf(&b, "Signed-By: %s\n", key.Path)
	text := b.String()
	source = File{Path: filepath.Join(aptSources, r.Name+".sources"), Literal: &text, Owner: "root", Group: "root", Mode: "0644"}
	return key, source
}

// repositoryLogin is the root-only file apt reads a private repository's
// login from: one entry for each of the task's addresses. An entry without a
// scheme serves https only, so apt never sends the login unencrypted.
func repositoryLogin(r Repository, s value.Scope) (File, error) {
	login := map[string]string{}
	for field, raw := range map[string]string{"username": r.Username, "password": r.Password} {
		v, err := s.Resolve(raw)
		if err == nil {
			login[field], err = value.Scalar(v)
		}
		if err == nil && strings.ContainsFunc(login[field], unicode.IsSpace) {
			err = fmt.Errorf("must not contain spaces or line breaks, which apt's login file cannot hold")
		}
		if err != nil {
			return File{}, fmt.Errorf("apt_repository.%s: %w", field, err)
		}
	}
	var b strings.Builder
	for _, uri := range r.URIs {
		u, err := url.Parse(uri)
		if err != nil || u.Host == "" {
			return File{}, fmt.Errorf("apt_repository: a login needs addresses with a host name, not %q", uri)
		}
		fmt.Fprintf(&b, "machine %s%s login %s password %s\n", u.Host, strings.TrimSuffix(u.Path, "/"), login["username"], login["password"])
	}
	text := b.String()
	return File{Path: filepath.Join(aptLogins, r.Name+".conf"), Literal: &text, Owner: "root", Group: "root", Mode: "0600"}, nil
}

// keyExtension names a key file for apt, which reads an ASCII-armored key from
// a .asc file and a binary key from a .gpg file. MMX does not interpret the
// key further.
func keyExtension(key *assets.Ref) (string, error) {
	if key == nil {
		return "", fmt.Errorf("apt_repository: the signing key has no saved asset")
	}
	file, err := assets.Open(key.Path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	head, err := io.ReadAll(io.LimitReader(file, 512))
	if err != nil {
		return "", err
	}
	if bytes.HasPrefix(bytes.TrimLeft(head, " \t\r\n"), []byte("-----BEGIN PGP PUBLIC KEY BLOCK-----")) {
		return ".asc", nil
	}
	return ".gpg", nil
}
