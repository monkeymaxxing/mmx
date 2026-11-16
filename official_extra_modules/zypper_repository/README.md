# `zypper_repository`

`zypper_repository` adds or removes one software repository on an openSUSE
**node**: the repository's file in `/etc/zypp/repos.d` and its signing key,
which the task gives explicitly. A node is a Linux server that MMX manages.
Put the task before the [`zypper_package`](../zypper_package/README.md) task
that installs the vendor's packages.

```yaml
- zypper_repository:
    name: vendor
    url: https://download.example.org/leap/16.1/
    repository_metadata_is_signed: true
    signing_key_url: https://download.example.org/key.asc
    signing_key_sha256: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

- zypper_package:
    packages:
      - {name: vendor-tool, version: newest_when_first_installed}
```

`zypper_repository` is an **official extra module**: a script in Starlark, a
small language with Python-like syntax, that the MMX project writes, tests,
and ships beside `mmx`. It follows the same rules as
[`apt_repository`](../../docs/reference/modules/apt_repository.md) where the concept is
the same: the signing key is part of the task, and the module never trusts a
key that the task did not name. The rules that differ are in
[zypper's own rules](../../docs/changing-mmx/design/modules-packages.md#zyppers-own-rules).

## Set up

The module installs nothing. zypper, rpm, and gpg2 (which zypper needs) are
part of every openSUSE system. A project with nodes of several distributions
gives the task a condition on the node fact `package_manager`, which is
`zypper` on openSUSE:

```yaml
- when:
    equals: ["{{ .node.package_manager }}", zypper]
  zypper_repository:
    name: vendor
    url: https://download.example.org/leap/16.1/
    repository_metadata_is_signed: true
    signing_key_text: "{{ .vars.vendor_signing_key }}"
```

## Inputs

| Input | Meaning | When omitted |
|---|---|---|
| `name` | A name you choose: letters, digits, and `_ - .`. It is the repository's alias for zypper and the base name of `/etc/zypp/repos.d/NAME.repo` and `/etc/zypp/mmx_repository_keys/NAME.asc`. | Required. |
| `state` | `present` writes the key, imports it into rpm, and writes the repository file. `absent` removes the repository with its cache, the key from rpm, and the key file, and accepts only `name` and `state`. Packages that came from the repository stay installed. | `present`. |
| `url` | The address of the repository, which holds the directory `repodata`. Starts with `https://` or `http://`; with `repository_metadata_is_signed: false`, only `https://`, because nothing else then stops someone on the network from serving an old list of packages that keeps back security updates. | Required with `state: present`. |
| `repository_metadata_is_signed` | `true` when the repository signs its metadata (`repodata/repomd.xml.asc` exists beside `repomd.xml`): zypper then checks the signature. `false` for a repository that does not sign its metadata, which zypper then cannot check. Packages are always checked. | Required with `state: present`: only the repository's vendor knows. |
| `signing_key_text` | The public signing key as ASCII-armored text, usually a reference to a variable. | Exactly one of `signing_key_text` or `signing_key_url` is required with `state: present`. |
| `signing_key_url` | An `https://` address of the ASCII-armored key. The node fetches it, and checks its SHA-256 first. | See above. |
| `signing_key_sha256` | The key's SHA-256 as 64 hexadecimal characters. | With `signing_key_url`, exactly one of `signing_key_sha256` or `dangerously_skip_checksum`. |
| `dangerously_skip_checksum` | `true` in place of `signing_key_sha256`: the key is trusted as it arrives from the web address. | `false`. |

A key from a file on the controller machine goes in as text: put the
file's content in a variable, or write the file to the node with a `file`
task first and give the task `signing_key_url`. MMX's Starlark modules
cannot read a controller machine's file by themselves.

## Outputs

| Output | Holds |
|---|---|
| `created` | `true` when this task created the repository file, and in a dryrun when a run would. |

## What a dryrun reports

A dryrun fetches the key from `signing_key_url` and checks its SHA-256, reads
the repository file and the keys in rpm, and reports what a run would write:

- **A new repository:** `would_change`: `would import the signing key ...;
  would add the repository NAME at URL`.
- **A repository that differs,** such as another address or another value of
  `repository_metadata_is_signed`: `would_change`, `would change the
  repository NAME`, with the file's difference in `--diff`.
- **A repository that is as the task wants it:** `unchanged`.
- **A removal:** the repository, key, and key file that are there.
- **A failure, as in a run,** for the inputs that break a rule, a checksum
  that does not match, and a key that is no OpenPGP key.
- **What the dryrun leaves on the node:** nothing, except gpg's own files in
  `/etc/zypp/mmx_repository_keys/gnupg_home`, where gpg reads the key's
  identity once that directory exists. On a first dryrun that directory is
  not there, and the dryrun says `would import the signing key for the
  repository NAME` without the key's identity.

## Examples

Add a vendor's repository with a key from a web address, then install from
it:

```yaml
- zypper_repository:
    name: vendor
    url: https://download.example.org/leap/16.1/
    repository_metadata_is_signed: true
    signing_key_url: https://download.example.org/key.asc
    # Replace with the SHA-256 of the key you have verified.
    signing_key_sha256: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

- zypper_package:
    packages:
      - {name: vendor-tool, version: newest_when_first_installed}
```

On the node, the `zypper_repository` task fetches the key and checks its
SHA-256, writes it to `/etc/zypp/mmx_repository_keys/vendor.asc`, imports it
into rpm's keyring, and writes `/etc/zypp/repos.d/vendor.repo`. The
`zypper_package` task then refreshes the repositories, because it has work,
and installs `vendor-tool`.

Remove a repository:

```yaml
- zypper_repository:
    name: vendor
    state: absent
```

## How it works

**Adding.** The module makes `/etc/zypp/mmx_repository_keys`, writes the
key, reads its identity with `gpg --show-keys`, and, when rpm's keyring does
not hold the key, imports it with `rpm --import`. Then it writes the
repository file with every setting spelled out:

```text
/etc/zypp/repos.d/vendor.repo:
  [vendor]
  name=vendor
  enabled=1
  autorefresh=1
  baseurl=https://download.example.org/leap/16.1/
  type=rpm-md
  gpgcheck=1
  repo_gpgcheck=1
  pkg_gpgcheck=1
  gpgkey=file:///etc/zypp/mmx_repository_keys/vendor.asc
```

The key goes into rpm's keyring **before** the repository file, so zypper
never asks whether to trust a new key. A non-interactive zypper answers that
question no, and `zypper refresh` then ends with exit code 4 and the text
`New repository or package signing key received`. rpm's keyring is shared by
every repository and every package of the node, so a key that rpm holds can
sign any repository.

**Removing.** The module removes the repository with `zypper removerepo`,
which also removes the repository's cache; removes the keys that it imported
for the repository from rpm (`rpm -e gpg-pubkey-ID`); and removes the key
file and the record of the imported keys. The module records the ids of the
keys that it actually imported in `/etc/zypp/mmx_repository_keys/NAME.imported_key_ids`.
It removes only those, and only when no other repository's key file in
`/etc/zypp/mmx_repository_keys` holds them. So a key that rpm held before the
task, such as one of openSUSE's own that the task gave again, never leaves
rpm, and a key that two repositories share leaves rpm with the last of them.
When a task gives another key for an existing repository, the module imports
the new key and then removes the old one under the same rule. The module does
not remove packages.

**The repository is refreshed by the next package task.** The module does
not refresh. A `zypper_package` task that has work refreshes the
repositories first, in a run, and so finds the new repository's packages.

## Errors and what they mean

Every reason starts with `zypper_repository`.

| The reason holds | What it means, and what to do |
|---|---|
| `the program zypper is not installed; zypper_repository works only on a node whose package manager is zypper ...` | The node is not an openSUSE node. A run fails without another try. Give the node task a condition on the node fact `package_manager`. |
| `the program gpg is not installed; install the package gpg2 in an earlier node task` | gpg2 is missing, which is rare on openSUSE. Install it with a `zypper_package` task. |
| `signing_key_sha256: is not the SHA-256 of the key at signing_key_url, which is ...` | The key at the address differs from the one you verified. A run fails without another try. Check the key, and then change the checksum in the task. |
| `signing_key_text: is not an ASCII-armored OpenPGP public key ...`, `signing_key_text: is not usable: ...` | The text is not an armored public key. Use the file that ends in `.asc`; a binary `.gpg` file is not accepted. |
| `fetching the signing key: ...` | The web address did not answer, or answered with another HTTP status than 200. MMX tries the task again. |
| `importing the signing key into rpm: ...` | `rpm --import` failed; the text behind the colon is rpm's message. MMX tries the task again. |
| `reading the keys of the other repositories: ...` | `find` or `gpg` could not read a key file in `/etc/zypp/mmx_repository_keys`. MMX tries the task again. |
| `removing the repository NAME: ...` | `zypper removerepo` failed, such as when another zypper holds its lock. MMX tries the task again. |

## Tested on

**Distributions:** openSUSE Leap 16.1.

The integration suite `tests/integration/zypper_packages.py` runs the module
on an openSUSE Leap 16.1 test node with zypper 1.14, with a signed test
repository that the suite serves on the node and a key that the suite
fetches from openSUSE's build service over HTTPS.

## Why it works this way

- **The key is part of the task.** A repository whose key zypper would trust
  on first use gives an attacker who controls the repository's address the
  key to everything it serves. As in `apt_repository`, the task names the key,
  and a key from a web address needs its checksum.
- **`repository_metadata_is_signed` is required.** Vendors differ: many sign
  their packages and not the metadata. zypper cannot guess which, and a
  guess of `true` fails the refresh while a guess of `false` skips a check
  silently. Package signatures are always checked; the module offers no
  input that turns them off.
- **The key goes into rpm, not only into the repository file.** zypper
  asks to trust a key named in `gpgkey=`, and a non-interactive zypper
  answers no. Importing it is the explicit trust that this task gives.
- **gpg reads the key's identity in a directory of its own.** rpm names its
  keys by the key's identity, and the module needs the name to tell whether
  rpm holds the key and to remove it later. gpg would otherwise create
  `/root/.gnupg` on the first call.
- **Every setting is written out.** `enabled`, `autorefresh`, and the checks
  are in the file, so zypper's defaults decide nothing.
