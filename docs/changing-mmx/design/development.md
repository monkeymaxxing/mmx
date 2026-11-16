# MMX: development, testing, and portable builds

Part of the [main plan](README.md). The organizing principle is that a reader
can find a behavior and follow its execution without discovering a series of
hidden layers. The [acceptance contracts](../acceptance.md) own current coverage
and test commands; the [code map](../code-map.md) lists where the components live and
[How MMX works](../../how-it-works.md) follows one real run.

## Supported platforms

A node is a Linux server that MMX manages. MMX manages Linux servers on
amd64 or arm64. MMX is built around systemd, which nearly every current
Linux distribution uses. MMX is tested on the distributions listed under
[tested distributions](#tested-distributions), and each module's page lists
under "Tested on" the distributions where the module is tested.

MMX has no support levels and no tier names. A reader has two questions:
"is MMX tested on my distribution?" and "is this module tested on my
distribution?". The list of tested distributions answers the first
question, and the module's page answers the second. A distribution is on
the list when the base suite passes there. The base suite covers bootstrap,
both kinds of connection, node jobs, recovery, and every plain-named
module, such as `file`, `user`, and `systemd_service`
([the two module groups](modules.md#where-a-module-works)). Every core
module page has a "Tested on" section, and the
[module index](../../reference/modules/README.md) shows the section as a column.

On a distribution that runs systemd but is not listed under tested
distributions, MMX is expected to work, because the plain-named modules use
the same mechanisms there and a tool module works wherever its tool is
installed, but MMX is not tested there.

The node runner executable, `mmx-runner`, is one static Go program, built
with cgo disabled
([pure-Go implementation](#pure-go-implementation-and-dependencies)), so it
needs no libraries from the distribution. MMX's own parts on the node need
systemd. A plain-named module does its work through the same mechanism
wherever it runs, such as a file written and renamed into place, an account
that `useradd` creates, or a unit that systemd starts.

### Tested distributions

MMX tests these distributions, grouped by family. A family is a group of
distributions that share a package manager and their package names:

| Family | Tested distributions | Package modules |
|---|---|---|
| Debian | Debian 12 and 13, Ubuntu 24.04 and 26.04 | `apt_package`, `apt_repository` |
| Red Hat | Rocky Linux 9, AlmaLinux 10 | `dnf_package`, `dnf_repository` |
| Arch Linux | Arch Linux, the current rolling release | `pacman_package` |
| SUSE | openSUSE Leap, the newest release | `zypper_package`, `zypper_repository` |

The base suite passes on all four families, and each family's package
modules pass their suites there. Each module's page names the exact
releases its suite passed on. The
[package modules design](modules-packages.md#package-modules-for-each-family)
owns the package modules of each family. The
[platform coverage](../testing.md#current-platform-coverage) names the
harness's images and the suites of the base suite.

Rocky Linux and AlmaLinux stand in for Red Hat Enterprise Linux, which has
the same packages and needs a subscription. Red Hat family servers run with
SELinux enforcing, as they are installed, and MMX works without changing
that: every file that MMX creates gets the label that the node's SELinux
policy gives its path
([labels](modules.md#files-get-the-selinux-label-of-their-path)).

Fedora 45 is tested too, with dnf 5, whose version lock and output differ
from the dnf 4 of Rocky Linux 9 and AlmaLinux 10
([how](modules-packages.md)). The Fedora releases before it, and the older
dnf 5 they ship, are not tested.

### Servers without systemd

MMX does not manage servers that systemd does not run, such as Alpine
Linux with OpenRC. This is a deliberate choice of scope: MMX aims to serve
the common case well, not every server. These parts of MMX are built on
systemd:

- the node HTTPS API process, which runs as the systemd service
  `mmx-https-api.service`;
- services, timers, and scheduled jobs: the `systemd_*` modules, such as
  `systemd_service` and `systemd_scheduled_job`;
- owed restores and change activation, which run as systemd services and
  timers that act without the controller process;
- `reboot`, which restarts the node with `systemctl reboot`;
- the node's identity, its systemd machine ID in `/etc/machine-id`.

MMX's first SSH command checks whether systemd runs the node, and a node
that systemd does not run fails at once with a message that says so
([connection failures](../../when-something-fails.md#the-first-check-of-the-server-fails)).
For servers without systemd, use another configuration tool, such as
Ansible or Salt.

On a node, systemd runs ordinary application processes, workers, daemons,
and one-shot jobs, and the core uses each service's systemd unit as its
execution interface. A project that runs another process supervisor on its
nodes, such as supervisord, writes its own module for that supervisor.

The controller executable, `mmx`, supports Linux and macOS. Managed macOS,
Windows, network appliances, and container orchestrators are outside MMX.

### Differences between distributions

MMX does not put a generic layer over the distributions. Each package
manager and each firewall has its own module, named for the tool, and roles
use the distribution's actual package names and paths. Most projects run one
distribution. A project with several distributions puts a condition on the
node task, using the
[distribution facts](configuration.md#facts-and-results-are-observations-not-defaults):
`.node.distribution.family` is `debian`, `redhat`, `arch`, or `suse`, and
`.node.package_manager` is `apt`, `dnf`, `pacman`, or `zypper`.

### Testing on several distributions

Each tested distribution adds test runs, so the rule says exactly which
runs each distribution gets:

- **Ordinary changes** run on Debian 13 and Ubuntu 26.04 only. A change to
  one family's own modules, such as `dnf_package` or the `selinux_`
  modules, also runs that family's suites on one distribution of the family.
- **The base suite** covers bootstrap, both kinds of connection, recovery,
  and the plain-named modules. Before a release, the base suite runs on every
  tested distribution.
- **A tool module's suite** runs by default on the Debian family, and on
  the other distributions that the harness lists for the suite. The module's
  page lists under "Tested on" only the distributions where the suite
  passed. A tool module is a module named for one tool, such as `ufw` or
  `apt_package`.
- **Red Hat family guests are virtual machines,** because SELinux does not
  enforce inside a container.

A distribution is on the tested list when the base suite passes there. A
module's page lists the distribution under "Tested on" only when the
module's suite passes there. The
[testing guide](../testing.md#the-full-catalog-with-incus) gives the
commands for each level.

## One repository, one Go module, two entry points

MMX lives in one repository with one Go module, whose `go.mod` is at the
root. The pruned copies of the dependencies in `third_party/` keep their own
`go.mod` files, because Go needs one in each directory that a `replace` line
names. The controller executable, `mmx`, and the node runner executable,
`mmx-runner`, are developed and released together. The controller executable
targets Linux and macOS. The node runner executable targets the supported
Linux systems.

```text
mmx/
├── go.mod
├── cmd/
│   ├── mmx/                 # Controller
│   └── mmx-runner/          # Serve requests or start a node job
├── internal/
│   ├── config/              # Project files, inventory, input validation
│   ├── controller/          # Target selection, dispatch, limits, results
│   ├── access/              # Credentials used by the controller process,
│   │                        # prompts, SSH signing
│   ├── endpoint/            # Authenticated HTTPS adapter and authorization
│   ├── node/                # Job admission, locking, worker lifecycle, storage
│   ├── engine/              # Task ordering, node task blocks, service recovery
│   ├── facts/               # Linux observations and snapshot comparisons
│   ├── modules/             # Core modules, one file each, their registry,
│   │                        # the primitives, and the Starlark and
│   │                        # executable runners
│   ├── moduleio/            # The JSON protocol of executable modules
│   ├── assets/              # Frozen run files and streamed delivery
│   ├── differences/         # Retained full differences
│   ├── protocol/            # Node messages, results, strict decoding
│   ├── value/               # Typed references, availability, sensitive values
│   ├── secrets/             # Age vault and named secret sources
│   ├── durable/             # Atomic record writes
│   └── release/             # The version of this MMX release
├── official_extra_modules/  # Official extra modules, shipped beside mmx
├── docs/
├── examples/
├── scripts/                 # build.sh: the controller and Linux runners for tests
├── tests/                   # Whole-system workflows and performance
└── third_party/             # Pruned copies of the Go dependencies
```

`internal/` keeps implementation packages private. MMX's resource modules are
features inside the one Go module, not separate Go modules.
[Go layout guidance](https://go.dev/doc/modules/layout). Each core module lives
in one place: one file in `internal/modules`, such as
[`modules/ufw.go`](../../../internal/modules/ufw.go), holds its input type,
input checks, description, examples, the hooks the node task execution engine
needs, and code. Each core module's tests sit beside the module's code.
A registry table in `internal/modules` lists the core modules, one
line each. The configuration loader, `validate`, `describe-module`,
`explain`, and the node task execution engine work through the registry and
know no module by name, so adding a core module touches its file and one
registry line. A core module touches the node through the
[primitives](modules.md#primitives), or the shared code the primitives are
built on, for most of the module's work. Shared configuration and protocol code
must stay usable by the macOS controller without importing Linux-only execution
code.

The [guide to adding a core module](../adding-core-modules.md) owns how
to add a core module or package manager.

## Make the execution path visible

- Keep each `main.go` small: parse options, connect the required components,
  run the command, and report its outcome.
- The node HTTPS API process and the node SSH request process are thin
  adapters to the shared node job admission and execution functions.
  The node HTTPS API process and node SSH request process do not own resource
  behavior.
- The node runner executable's node task block function shows service baselines,
  resource reconciliation, validation, activation, verification, failure
  handling, and cleanup in order.
- Give each responsibility one owner. The controller process coordinates
  nodes. The node runner executable owns execution and recovery on each node,
  including service hash records. Modules understand individual resources.
- Use concrete functions and structs by default. Add small interfaces at
  boundaries where they help execution, substitution, or failure testing.
- Name shared helpers for their actual job. Keep a helper with its owner until
  another real caller needs it.

Keep [`docs/changing-mmx/code-map.md`](../code-map.md) accurate enough for a reviewer to
follow the code path of one `mmx run <playbook>`.

## Pure-Go implementation and dependencies

Write the controller executable (`mmx`), node runner executable (`mmx-runner`),
and core modules in Go. Prefer the standard library; add a small Go library
only for a real requirement. MMX's Go dependencies are:

- `go.yaml.in/yaml/v3`, the strict YAML parser;
- `golang.org/x/crypto`, for SSH keys, SSH signatures, and the user's SSH
  agent, and for encryption in the vault's age format.
  The vault is MMX's encrypted secrets file. MMX's SSH connections go
  through the system's `ssh` program, not `golang.org/x/crypto`;
- `golang.org/x/sys`, for system calls;
- `golang.org/x/text`, for the Unicode normalization of PostgreSQL
  passwords;
- `filippo.io/age`, the vault's encryption, with `filippo.io/hpke`, which
  it imports;
- the xz and zstd decoders, `github.com/ulikunitz/xz` and
  `github.com/klauspost/compress`, which `download.decompress` and
  `unpack_archive` need beside the standard library's gzip and bzip2.
  The xz and zstd decoders are pure Go and add 0.6 MB to the node runner
  executable;
- `go.starlark.net`, the pure-Go Starlark interpreter, which runs [Starlark modules](modules.md#how-a-starlark-script-runs);
- Sprig (`github.com/Masterminds/sprig/v3`), the template function library
  that Helm uses, with the eight small libraries that Sprig needs. MMX offers
  only the [safe part of Sprig](configuration.md#template-functions): no
  function that reads the clock, random values, environment variables, or
  the network.

`go.mod` pins the Go toolchain and the version of every dependency, and the
code of each dependency is in this repository as a
[pruned copy](#pruned-copies-of-the-dependencies). A new dependency must meet
the cgo rule below. The new dependency's contribution to the size of the
controller executable and node runner executable is measured before addition.
Each new dependency gets a pruned copy, like every existing Go dependency.

Dependencies, including transitive ones, must work with cgo disabled on every
supported target; a Go wrapper around a required C library does not meet this
rule. Build with **`CGO_ENABLED=0`** explicitly
([`scripts/build.sh`](../../../scripts/build.sh)). Users need no C compiler,
headers, or third-party C libraries to build or run MMX. The macOS controller
uses Go's normal Apple system integration. Enabling cgo in product builds would
be a design change, not a routine dependency fix. apt, systemd,
OpenSSH, and shell commands remain ordinary process calls. So do the tools
that other core modules drive, such as `docker`, `podman`, `psql`,
`mysql`, `pip`, and `certbot`: a module runs the tool that is on the node and
links no client library for it, which keeps the node runner executable small.
The size of the node runner executable is measured whenever core modules
are added.

Measured on 2026-10-16, the release build of the node runner executable is
16.4 MB for AMD64 and 14.9 MB for ARM64. The controller sends it to a node
once per MMX version, compressed with gzip (6.5 MB for AMD64). Most of it is
Go itself with the HTTPS and encryption code the node needs: a minimal Go
program with an HTTPS client and server is already 7.0 MB, and the core
modules add 2.0 MB. Two reductions were measured and are deliberately not
used:

- **xz or zstd for the transfer** (5.0 MB instead of 6.5 MB). The node
  unpacks the runner before any MMX code runs there, so it can use only a
  program that every node has. `xz` is missing from the minimal images of
  Debian 12 and 13, and `zstd` is missing
  too; `gzip` is everywhere. gzip's strongest
  level saves only 45 KB.
- **MMX's own copy of Go's template execution code** without its lookup of
  methods by name (1.7 MB less on disk). That lookup makes the linker keep
  every exported method of every type stored in an interface. A copy is
  about 2,500 lines of Go's code that MMX would have to keep in step with
  Go's fixes by hand, and templates could no longer call a method on a value,
  such as `(semver "1.2.3").Major`. Upkeep and a lost feature cost more than
  10% of the disk space.

The vault uses `filippo.io/age` in passphrase mode with the standard age
format, built into the controller so users install no encryption tool. MMX
reads the passphrase from the selected source and unlocks once per invocation.
MMX checks the complete encrypted write before replacing a vault. The
[secret contract](configuration.md#secrets) owns secret handling.

`go test -race` needs cgo and a C compiler on the machine running the Go tests.
Using cgo for Go race tests does not permit C dependencies or shipping
race-instrumented binaries. Real-system tests run the cgo-disabled artifacts.
[Go race detector](https://go.dev/doc/articles/race_detector).

`scripts/build.sh` produces cgo-disabled Linux ARM64/AMD64 runners and a
controller for the build machine. The
[Linux-controller workflow](../../../tests/integration/linux_controller.py)
cross-builds a Linux controller executable.

[`scripts/build-release-archives.py`](../../../scripts/build-release-archives.py)
builds one archive for each controller platform: macOS ARM64 and AMD64, and
Linux AMD64 and ARM64. For example, `mmx-0.1.0-macos-arm64.tar.gz` holds one
top directory, `mmx-0.1.0-macos-arm64/`, with:

- `mmx`, the controller program for that platform;
- `mmx-runner-linux-amd64` and `mmx-runner-linux-arm64`, the two Linux node
  runner executables;
- `official_extra_modules/`, including `shipped_with_mmx_version`;
- MMX's `LICENSE`;
- `dependency_license_notices/<module path>/`, containing every `LICENSE*`,
  `NOTICE*`, and `COPYING*` file in that dependency's `third_party/` copy,
  including nested files with their relative paths preserved;
- `dependency_license_notices/go/LICENSE`, from the pinned Go toolchain's
  `GOROOT`, because Go's standard library is compiled into the programs too.

The controller looks for the runners and official extra modules beside its
own executable. An upgrade replaces the whole directory, so the controller
executable, node runner executables, and official extra modules always come
from the same release.

`internal/release.DefaultVersion` is the single default, `0.1.0-dev`.
`internal/release.Version` uses `internal/release.DefaultVersion` unless the
release builder sets `internal/release.Version` with Go's `-ldflags -X`.
Both `mmx version` and `mmx-runner version`
report the stamped version. `mmx version` also prints the Git commit that Go
records in a build made from a Git checkout, with "with uncommitted changes"
when the checkout had them, so a lab can tell whether a development build
has a given commit; release archives are built with `-buildvcs=false` and
identified by their version. The builder stamps the archived
`official_extra_modules/shipped_with_mmx_version` with the version stamped
into the archived executables. The repository's
`official_extra_modules/shipped_with_mmx_version` stays `0.1.0-dev`.
The controller process refuses official extra modules whose recorded version
differs from the controller executable's version, and
[node runner executables](execution.md#node-updates-follow-the-controller-release)
that report another version.

The builder accepts [semantic versions](README.md#compatibility-between-releases),
including prereleases such as `0.1.0-rc.1`, so a candidate can use the same
build and checks as a final release.
CI uses the default development version. The release workflow refuses
`0.1.0-dev` as a release tag.

All programs use `CGO_ENABLED=0`, `-trimpath`, and the toolchain pinned in
`go.mod`. `-buildvcs=false` prevents checkout state from changing the programs'
build metadata. Archives have sorted entries, fixed modification times, zero
owner and group IDs, empty owner and group names, and normalized permissions:
`0755` for directories and programs, `0644` for other files. Gzip records no
filename. Modification times come from `SOURCE_DATE_EPOCH` when set, or the
commit time of `HEAD`. With the same source, version, timestamp, and pinned
toolchain, macOS and Linux builds produce identical archive bytes.
The output directory must not already contain archives of the requested
version, so a build cannot silently overwrite those artifacts.

After writing each archive, the builder runs
[`scripts/check-release-archive.py`](../../../scripts/check-release-archive.py).
The check requires the exact file and directory list, the stamped version
in `official_extra_modules/shipped_with_mmx_version`, and executable
permissions. `scripts/check-release-archive.py` parses Linux ELF program
headers and dynamic tables to reject an interpreter (`PT_INTERP`) or a
shared-library dependency (`DT_NEEDED`). When the archive matches the
checking machine's platform, the check runs its `mmx version` too.
The catalog suite `dns_tls_and_account_lookups.py` checks public DNS and
default TLS trust on the controller machine and node, account and group
lookup on the node, and matching controller and installed node runner versions.
Release platform validation runs the whole catalog with
`--build-and-use-release-archive VERSION`,
so the shared production programs and official extra modules come from the
checked archive.
The [testing guide](../testing.md#release-archives) owns the commands and
the suite's checks. The
[macOS controller check](../testing.md#the-macos-controller-check) lists the
release-validation subset run from a Mac.

The builder writes `SHA256SUMS`, one SHA-256 and archive filename per line.
The [change workflow](../../../.github/workflows/checks.yml) runs Go tests on
Linux and macOS, Go vet on Linux, Python unit tests, formatting checks of
MMX's own Go code, and the complete development archive matrix.
The [release workflow](../../../.github/workflows/release.yml) accepts a tag
`v<version>`, builds and checks the archives, gives every archive a GitHub
build attestation, and attaches the archives and `SHA256SUMS` to the GitHub
release. Actions are pinned to commit hashes. Go setup reads `go.mod`, and
jobs check that the selected toolchain equals the `toolchain` line in `go.mod`.

A build attestation is a signed record of the repository and workflow that
built an archive. A user checks the build attestation and the checksum before
unpacking:

```sh
shasum -a 256 --check --ignore-missing SHA256SUMS
gh attestation verify mmx-0.1.0-macos-arm64.tar.gz --repo monkeymaxxing/mmx
tar -xzf mmx-0.1.0-macos-arm64.tar.gz
```

A checksum finds a damaged download. Someone who can change a release can
also change `SHA256SUMS`; the attestation proves which repository's workflow
built the archive. Verifying which repository's workflow built the archive
matters for a program that runs as root on every node.
A user without the GitHub CLI (`gh`) still has the checksum.

### Pruned copies of the dependencies

The node runner executable runs as root on every node, and the controller
process holds the user's SSH keys and the vault passphrase. Every line of
dependency code that MMX builds runs in the controller process or node runner
executable too. So MMX keeps each dependency as a pruned copy in MMX's
repository:

- **Where a copy lives.** `third_party/<module path>/` holds the copy of
  one module, for example `third_party/golang.org/x/crypto/`. A `replace`
  line in MMX's root `go.mod` points the module at `third_party/<module path>/`.
  The import paths and the dependency's version in MMX's root `go.mod`
  `require` entry stay upstream's.
- **Why.** A build downloads no dependency code, so nobody can change the
  code that MMX builds by publishing a new version or by changing what a
  module proxy serves. Dependency code changes only through a commit in
  this repository, which a reviewer reads like MMX's own code.
- **What a copy keeps.** A pruned copy keeps the packages that MMX's programs
  and tests use. Within each package, keep a file only when a supported
  build compiles the file and MMX needs the file. Supported builds target
  Linux or macOS, on AMD64 or ARM64, with or without the race detector.
  Keep files containing functions that MMX reaches. Keep the
  assembly files and the Go files that declare the assembly functions.
  Keep files declaring types, constants, variables, or methods used by kept
  code.

  Upstream tests, examples, and tools are cut. Of the 1,136,493 lines of Go
  in the 18 upstream modules, the copies keep 129,863.
- **Kept files stay identical to upstream,** so an upstream fix to a kept
  file applies unchanged. To keep upstream fixes applicable without changes,
  nothing reformats the copies:
  `gofmt` checks run on MMX's own directories, not on `third_party/`, where
  some upstream files predate the current `gofmt`.
- **Sprig's family is the exception.** In Sprig and the eight libraries
  that only Sprig uses, the copies also cut, inside files, the functions
  that MMX does not reach and the declarations that nothing uses, and
  Sprig's function table loses the functions that MMX withholds. Sprig and
  the eight libraries used only by Sprig collect small string, list, and
  number functions, of which MMX offers a part. Every dependency except
  Sprig and the eight libraries used only by Sprig keeps whole files.
  The whole-file copies hold code for SSH keys, encryption, system calls,
  downloaded-file decoders, and project-file parsers, where upstream fixes
  matter.
- **The record.** Each copy's `PRUNED_COPY_RECORD.md` names the upstream
  version and its module hash, the kept packages, every file that differs
  from upstream, and the fixes applied since.
  Each copy's `PRUNED_COPY_RECORD.md` has a `Module:` line that names the
  module in backticks. Under `## Applied fixes`, write one bullet per fix,
  starting with the advisory ID, for example
  `- GO-2026-1234: Apply upstream commit <commit> to fix <fault>.`
  Write `None.` when no fixes have been applied.

Compare a copy with upstream, using `golang.org/x/crypto` as an example:

```sh
go mod download golang.org/x/crypto@v0.57.0
upstream=$(go list -m -f '{{.Dir}}' golang.org/x/crypto@v0.57.0)
diff -r "$upstream" third_party/golang.org/x/crypto | grep -v "^Only in $upstream"
```

`go mod download` downloads the upstream version into the Go module cache
and checks it against the Go checksum database; `go list` then names its
directory there, and names none while the version is not in the cache. The
output of `diff` then lists only the record file and the files under
"Changed files" in the record.

When MMX needs code that a copy cut, copy the files from the upstream version
recorded in the existing copy's `PRUNED_COPY_RECORD.md`.
Add any new package to the copy's record.

### When a copy or the toolchain changes

A dependency's copy, and the Go toolchain that `go.mod` pins, change for only
two reasons, and each must be shown, not assumed:

1. **A vulnerability that MMX is exposed to,** with three proofs:
   - **Reached:** `govulncheck`, Go's vulnerability analyzer, shows a call
     path from MMX's code to the vulnerable function.
   - **Open to an attacker:** the input that triggers the vulnerability comes
     from outside the user's control. Examples include a request to the node
     HTTPS API, a downloaded file, or a reply from a node. A user's own
     project files do not count: through them the user already runs commands
     as root.
   - **Reproduced:** a test fails before the proposed vulnerability fix and
     passes after the proposed vulnerability fix. The
     test stays in MMX's tests.
2. **A high-value fault in real use,** shown by a failing test.

These are not reasons to change a copy or the toolchain: a newer upstream
version, an advisory for code that the copy cut or that MMX does not call, a
scanner report without a reproduction, speed, cleanup, or staying current.
MMX's dependencies stay stable and boring on purpose.

Each change is one commit. The commit containing the dependency or toolchain
change names the advisory or fault, holds the test, and names the applied
upstream commit. The copy's record lists the change under "Applied fixes",
with the advisory's ID. When a vulnerability cannot be reproduced on demand,
send call-path evidence and
evidence that the input comes from outside the user's control to the
project's maintainer, who decides.
A timing leak, for example, may resist reproduction.

`govulncheck` cannot see the copies: it skips a module that a `replace` line
points at a directory. So the check runs on a temporary copy of the
repository whose `go.mod` has no `replace` lines, where the same MMX code
calls the upstream versions. `govulncheck` finds the same call paths in the
upstream versions and pruned copies because MMX calls the same functions.
The pruned copies cut only code that MMX does not reach.
[`scripts/check-dependency-vulnerabilities.py`](../../../scripts/check-dependency-vulnerabilities.py)
creates and removes that temporary copy, installs govulncheck v1.9.0, and
reads its JSON findings. Installation and analysis use the repository's
resolved Go toolchain.

The check fails only for a symbol-level finding:
a call path to a vulnerable function whose advisory ID is absent from that
module's record under `## Applied fixes`. An ID in another module's record
does not count. Reached Go standard library and toolchain advisories always
fail the check, because they have no copy record. A finding starts the
investigation above; it does not by itself justify changing a copy or the
toolchain.

The static analysis step of the [release checklist](README.md#release-checklist)
and the weekly workflow
[`dependency-vulnerabilities.yml`](../../../.github/workflows/dependency-vulnerabilities.yml)
run `scripts/check-dependency-vulnerabilities.py`. The
[testing guide](../testing.md#static-analysis-for-a-release) gives the commands.

## Automated code and workflow tests

Tests must prove both the implementation's rules and the workflows a user
actually runs. The [acceptance contracts](../acceptance.md#acceptance-contracts)
define the required outcomes, [acceptance
checks](../acceptance.md#acceptance-checks) map each contract to its tests and
list the test commands, and the [testing guide](../testing.md) owns the
environment and platform coverage. The tests themselves are the detailed record
of covered cases.

### Kinds of tests

- **Go code tests** sit beside their packages (`go test -race ./...`,
  `go vet ./...`). The Go code tests cover parsing, typed references,
  selection, result aggregation, module contracts, node job state, and
  configuration-change activation state. A typed reference names a value
  and preserves the value's type instead of converting the value to text.
  Temporary directories and small substitutes for component operations inject
  failed writes, lost replies, and competing operations deterministically.
- **Native Linux code tests**
  ([`linux_code.py`](../../../tests/integration/linux_code.py)) cross-compile the
  Go test executables and run them as root in a guest, covering Linux-only
  behavior without installing Go in the guest.
- **Workflow suites** under `tests/integration/` run the compiled public CLI
  against disposable Linux machines, mostly using the checked-in examples. A
  fixture that bypasses configuration loading or the public CLI is not an
  end-to-end test.
- **Platform runs** repeat the workflow suites on each tested
  distribution and architecture.
  [`incus.py`](../../../tests/integration/incus.py) runs the catalog, or the
  suites named with `--suite`, in Incus guests on a Linux test server.
  [`hetzner.py`](../../../tests/integration/hetzner.py) runs the catalog or
  selected suites on Hetzner Cloud servers, for AMD64 and ARM64. The
  [testing guide](../testing.md#the-full-catalog-with-incus) gives
  the levels: which suites run on which distributions after an ordinary
  change, at the end of a build step, and before a release. A module that
  exists for one tool, such as `ufw`, has its workflow tests on the
  distributions that its page lists under "Tested on"
  ([testing on several distributions](#testing-on-several-distributions)).
- **Performance measurements** under `tests/performance/` are separate from
  workflow tests; see [performance validation](#performance-validation).

### What a workflow test checks

A workflow test checks outcomes independently of MMX's own report:

- installed files and metadata, package versions, accounts, and unit state;
- the configuration a service actually loaded (for example, the response nginx
  serves) and how many times it was activated;
- served application responses, fleet membership, and node job identities;
- retained controller and node records scanned for secret test values.

A success message is not evidence. Synchronize fault injection with a named
execution point or an observed ready state, using bounded deadlines rather
than fixed sleeps. Keep the original failed test result even if a
rerun passes. Report setup failures separately from failed MMX assertions.

Fault-injection tests must verify the intended failure point before checking
recovery, and retain run records that identify that point. A failure at a
different step cannot prove the intended recovery scenario.

A module page may repeat its example project's installation tasks so the
page stands alone. A Go test keeps the repeated tasks equal to the example
that the workflow suite runs. RabbitMQ's Go tests substitute the
`rabbitmqctl` path in a copy of each script, requiring exactly one matching
line. The RabbitMQ workflow suite runs the shipped script.
Substituting the `rabbitmqctl` path in test copies permits deterministic
stand-ins without changing production module inputs.

### Recovery tests

Recovery tests start from interrupted states, such as a killed node job worker
process or controller process. Another case interrupts execution between
a file write and activation.
Further cases include a lost reply or a node reboot.
The recovery tests check that both a new `run` and `resume` finish outstanding
work. Service tests check the
[configuration-hash contract](execution.md#service-reloads-follow-configuration-hashes)
through loaded content and action counts:

- A changed file is loaded once, even when the next attempt finds the file
  already written.
- An unchanged run after reboot takes no action.
- A package version change restarts the service.
- A dryrun writes no records.

Ordinary recovery tests check that `run` executes current definitions and
`resume` uses saved definitions. Ordinary recovery tests also check that neither
`run` nor `resume` needs a force flag.

The focused cases are in
[`engine/services_test.go`](../../../internal/engine/services_test.go),
[`engine/package_recovery_test.go`](../../../internal/engine/package_recovery_test.go), and
[`engine/recovery_test.go`](../../../internal/engine/recovery_test.go).
[`vm.py test`](../../../tests/integration/vm.py) proves the focused recovery cases
against real systemd.
Tests of a [command record](execution.md#a-command-that-follows-a-change-has-the-same-record)
check the configuration-hash contract by counting command executions:

- After an input changes, the command runs once.
- If a run stops between the input change and the command, the next
  attempt runs the command once.
- If the node reboots between the input change and the command,
  the next attempt runs the command once.
- On first use with unchanged inputs, the command does not run.
- On an unchanged run, the command does not run.
- When two fleet stage loop iterations reach one node and the command fails
  in one of them, the next attempt of that iteration runs the command once,
  also after the other iteration's command succeeded.
- A command that a node runner executable of release 0.1.0 left owed inside
  a fleet stage loop runs once after the upgrade.

### When the suites run

CI runs the Go tests on each change. Run the affected workflow suites for
changes to execution, modules, bootstrap, or the configuration language, at
the [test levels](../testing.md#the-full-catalog-with-incus).
Run the affected workflow suites for platform-sensitive changes too.
The acceptance contracts define a
[full acceptance pass](../acceptance.md#acceptance-checks).

## Disposable test environments

Incus manages Linux system containers and virtual machines. Everyday tests
run in Incus guests on a Linux server with KVM and on
disposable Hetzner Cloud servers. `incus.py` without `--distribution` runs
on guests for every distribution of the
[tested distributions](#tested-distributions) table. Red Hat family guests
are virtual machines that run with SELinux enforcing and firewalld active,
as installed. Each Incus guest is a copy of
a prepared base made from its distribution's image, and each cloud server
starts from the Hetzner image that the harness names for its distribution.
`incus.py` runs on a Linux test server with KVM, and its guests are
AMD64. ARM64 runs only through `hetzner.py` on Hetzner Cloud ARM64 servers,
so no run emulates another architecture. Incus guests and cloud servers run
the same scenarios through one small inventory boundary. Real-node scale
measurements are described in [performance validation](#performance-validation).

Keep provisioning outside MMX in thin harness scripts. The harness scripts
`incus.py` and `hetzner.py` create clean test machines for each run and
delete them afterwards. The harness waits for verified SSH, supplies the
inventory, runs scenarios, and collects diagnostics. A fresh-node test
receives only access and declared prerequisites, never a preinstalled
`mmx-runner` or node HTTPS API process.

Each run's guests and servers carry an ownership record outside the coordinator
so cleanup survives loss of the coordinator.

Each job slot of an `incus.py` or `hetzner.py` run prints, and saves in its
`targets.json`, each test machine's OS, kernel, architecture,
and the image the test machine started from. A test machine's starting image
does not freeze the distribution's package repositories. So the
[validation record](../validation.md) keeps the tool versions used for each
test result. The [testing
guide](../testing.md) owns paths, locking, ports, and cleanup commands.

Booting a guest does not put a distribution on the tested list. The base
suite must pass on the named distribution release, and each module's suite
must pass there before the module's page lists the release.

## Performance validation

Measure unchanged, changing, and first-contact runs. Separate controller
startup and parsing, connection setup, transfer, remote execution, result
handling, and output. Scale node count 10 → 100 → 1,000 → 10,000, recording
controller CPU, peak memory, open files, network bytes, throughput, and the
slowest node; separate test-network limits from controller and node capacity.
Compare SSH and HTTPS, and MMX against Ansible, with identical job semantics.

`tests/performance/controller_scale.py` measures the real controller against
simulated HTTPS nodes (`tests/performance/simnodes`), isolating controller cost
from node and network cost. The real-node MMX and Ansible workload files are
in `tests/performance/fleet/`. The driver for the 1,000-node comparison's
private fleet is not part of this repository. The method in
[Real nodes against Ansible](../../reference/performance.md#real-nodes-against-ansible)
describes the private fleet's guest construction, networking, and Ansible
tuning. Reproducing the comparison needs an independently provisioned fleet
and a driver for that fleet. [Performance](../../reference/performance.md) holds the current
results and remaining performance costs.

Release large-fleet test resources after the evidence is captured.

The current build does not promise recovery of every interrupted controller
state write. An OS crash on the controller machine can leave an entry
unreadable. Start a new `mmx run` to reconcile resources from the nodes' state.
Full-disk and interrupted-write matrices are deferred so the initial feature
build can finish before broader storage fault hardening.

## Beyond the first release

- Full-disk and interrupted-write fault matrices and broader
  competing-controller faults.
- Cross-release tests of resume and node setup.
- The 10,000-node stage, the SSH-versus-HTTPS comparison, and a Salt
  comparison.
