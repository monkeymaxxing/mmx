# `dnf_repository`

`dnf_repository` adds a vendor's repository to a node that uses dnf: one
signing key, imported into rpm's key store, and one repository file with one
section. It can also switch on or off a repository that is already on the
node, such as CRB on Enterprise Linux 9. Put the task before the
[`dnf_package`](dnf_package.md) task that installs from the repository. On a
node that uses apt, [`apt_repository`](apt_repository.md) does the same.

```yaml
- dnf_repository:
    name: docker-ce-stable
    description: Docker CE Stable
    baseurl: ["https://download.docker.com/linux/rhel/$releasever/$basearch/stable"]
    signing_key_source: files/docker-rpm-key.asc

- dnf_package:
    packages:
      - {name: docker-ce, version: newest_when_first_installed}
```

On the node, the task writes:

```text
/etc/pki/rpm-gpg/RPM-GPG-KEY-docker-ce-stable
/etc/yum.repos.d/docker-ce-stable.repo:
  [docker-ce-stable]
  name=Docker CE Stable
  baseurl=https://download.docker.com/linux/rhel/$releasever/$basearch/stable
  enabled=1
  gpgcheck=1
  gpgkey=file:///etc/pki/rpm-gpg/RPM-GPG-KEY-docker-ce-stable
```

`name` is the repository id and the base name of the files: letters, digits,
`_`, `-`, and `.`. `description` is the line dnf shows for the repository
(dnf's own `name`). The inputs `baseurl` (a list), `metalink`, `mirrorlist`,
`priority`, `includepkgs`, and `exclude` are dnf's own. dnf's own variables
`$releasever` and `$basearch` pass through unchanged for dnf to fill in.

## Examples

### Add a vendor's repository

The first example above writes Docker's repository with a key from the
project. The signing key comes from exactly one of `signing_key_source`, or
`signing_key_url` with `signing_key_sha256` (or
`dangerously_skip_checksum: true`), as on `apt_repository`:

```yaml
- dnf_repository:
    name: docker-ce-stable
    description: Docker CE Stable
    baseurl: ["https://download.docker.com/linux/rhel/$releasever/$basearch/stable"]
    signing_key_url: https://download.docker.com/linux/rhel/gpg
    signing_key_sha256: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
```

The controller process fetches and verifies the key, as
[files fetched from a web address](../../docs/changing-mmx/design/modules-packages.md#files-fetched-from-a-web-address)
describes. On the node, MMX writes the key to
`/etc/pki/rpm-gpg/RPM-GPG-KEY-docker-ce-stable`, imports it into rpm's key
store, and writes `/etc/yum.repos.d/docker-ce-stable.repo` with one section
that always says `gpgcheck=1`.

### A repository that only one task uses

```yaml
- dnf_repository:
    name: example-tools
    description: Example tools
    baseurl: ["https://repo.example.com/el/$releasever/$basearch/"]
    signing_key_source: files/example-tools-rpm-key.asc
    enabled: false

- dnf_package:
    packages:
      - {name: example-tool, version: newest_when_first_installed}
    repositories_enabled_only_for_this_task: [example-tools]
```

`enabled: false` writes a repository that only a `dnf_package` task naming it
in `repositories_enabled_only_for_this_task` uses.

### Switch a repository that is already on the node

Some repositories arrive switched off, as part of the distribution or of a
package. CRB on Enterprise Linux 9, which EPEL needs, is the usual case:

```yaml
- dnf_package:
    packages:
      - {name: epel-release, version: newest_when_first_installed}

- dnf_repository:
    existing_repository: crb
    enabled: true
```

`existing_repository` takes the place of `name`, and the task then accepts
only `enabled`, which is required. MMX reads the repository's state from dnf
and, when it differs, changes it through dnf's own `config-manager` command.
MMX never edits the file that defines the repository. With dnf 4,
`config-manager` is part of the package `dnf-plugins-core`; when it is
missing, the task says so.

### A private repository

`username` and `password` give the login for a private repository, as on
[`apt_repository`](apt_repository.md#use-a-private-repository): they come
together, and `password` must be one whole reference to a secret, or, inside
a role, to a role input declared `secret: true`. The login is
written into the repository file, which is then root-owned `0600`;
otherwise the file is `0644`. A web address that contains a login is
rejected. dnf sends the login to every address of the repository, so with a
login each address in `baseurl`, `metalink`, or `mirrorlist` must start
with `https://`; an address over plain `http://` would let anyone on the
network path read the password.

### Remove a repository

```yaml
- dnf_repository:
    name: docker-ce-stable
    state: absent
```

`state: absent` accepts only `name` and removes the repository file, then the
key file. Packages installed from the repository stay. A key that MMX
imported into rpm's key store is removed too, with `rpm -e` and the key's
`gpg-pubkey` name, because rpm's key store is trusted by every repository on
the node. MMX imports only keys that rpm did not hold already, and lists them
in `/etc/pki/rpm-gpg/<name>.imported_key_ids`. A key the node held before,
such as the distribution's own or EPEL's, stays when the repository goes.
A key that another file in `/etc/pki/rpm-gpg` also holds stays too, because
another repository still needs it; a repository whose key another repository imported lists it too, so the last repository to go removes it. When the signing key of a repository
changes, MMX imports the new key and then removes the keys it had imported
from the old file, under the same rules. Reading key ids needs `gpg`, from
the package gnupg2; a node without it fails the task before anything
changes. A run also imports a key file whose key rpm does not hold, for example
when rpm refused the key on an earlier try, so a retry does not leave the
repository without its key.

## How it works

The files use the ordinary atomic file installer, key first. A change makes
the next installation in the same node job refresh dnf's index.

A repository file with several sections, such as a vendor's file with debug
and source sections, is one task for each section, or the whole file as a
[`file`](file.md) task.

## Errors and what they mean

- **The node uses another package manager.** The task fails and names the
  module to use instead: on an apt node, `apt_repository`.
- **An `existing_repository` id that the node does not define.** The task
  fails with the ids the node has.
- **`config-manager` is missing.** The task says that the package
  `dnf-plugins-core` is needed (on dnf 5, `dnf5-plugins`). Install it in an earlier `dnf_package` task.
- **A web address that contains a login** is rejected. Give the login in
  `username` and `password`.
- **`dnf_repository.baseurl[0] http://mirror.internal/vendor does not start
  with https://`**: the task has a login, and dnf would send it unencrypted.
  Serve the repository over HTTPS and use its `https://` address, or leave
  out `username` and `password`.

## Tested on

**Distributions:** Rocky Linux 9, AlmaLinux 10, Fedora 45.

The suite `server_resources.py` covers package sources and runs on every
target distribution in the whole catalog: for `dnf_repository`, Rocky Linux
9 (dnf 4.14), AlmaLinux 10 (dnf 4.20), and Fedora 45 (dnf 5.4.7).

## Why it works this way

- **MMX imports the verified key into rpm's key store itself.** Left alone,
  dnf would download a repository's key and trust it without asking the
  first time it installs from the repository; with MMX the key is the one
  you committed or whose SHA-256 the task gives. The repository file always
  says `gpgcheck=1`.
- **MMX never edits the file that defines an existing repository.** The
  package that owns the file rewrites it.
