# Testing MMX

MMX tests run commands on a controller machine and check the resulting state
on nodes, the Linux machines that MMX manages. The controller machine is
where the `mmx` command runs. A controller process is one running `mmx`
command. Guest suites create disposable nodes and verify changes through SSH,
HTTPS, and the guest provider's inspection commands.

Run commands in `docs/changing-mmx/testing.md` from the repository root.
The [acceptance contracts](acceptance.md#tests) define the behavior to prove;
the [validation record](validation.md) records results for the current build.

## Test machine requirements

- Go, Python 3, OpenSSH (`ssh`, `ssh-keygen`, and `ssh-agent`), and network
  access for builds and package downloads. Use the Go version in `go.mod`.
- For guest suites: a Linux test machine with KVM and Incus, permission to
  manage Incus instances, a configured storage pool, and the `incusbr0`
  bridge with working DNS, forwarding, and firewall rules. A copy-on-write
  storage pool, such as btrfs, makes guest copies fast. The nested
  virtualization suites need KVM available inside their VM guests too.
- For cloud and ARM64 suites: a Hetzner Cloud project, an API token with
  read and write access, a registered SSH key, and network access to the
  created servers. See [Hetzner Cloud](#the-full-catalog-on-hetzner-cloud-servers)
  for the required settings. Cloud suites need no local KVM or Incus.
- For the [macOS controller check](#the-macos-controller-check): a Mac with
  the build and SSH tools above, and the required
  [Hetzner Cloud settings](#the-full-catalog-on-hetzner-cloud-servers).
- `gpg` on the controller machine for the server-resource suite's signed
  apt repository, and `git` for the secrets suite's comparison with a commit.

Allow disk space for guest images, prepared bases, builds, and suite logs.
Remove superseded disposable test roots and unused guest images when no run
uses them. The memory and run-time measurements below are examples from
measured runs, not machine requirements.

## Test environment isolation

The test root holds build outputs, generated projects, inventories, login
keys, controller run history, and results. The default is
`~/.cache/mmx-testing`. Set `MMX_TEST_ROOT` to select a complete independent
environment, including its build outputs. A job slot uses two disposable
nodes named `mmx-dev` and `mmx-peer` within its own environment.

Use a separate checkout or worktree and a separate test root for independent
code changes tested concurrently. Keep other projects' build outputs,
credentials, guests, ports, and test roots separate. Independent roots avoid
state collisions; runs still compete for CPU, memory, and disk capacity.
Concurrent Incus runs on one machine must use the same
[shared guest slot directory](#the-full-catalog-with-incus) to coordinate capacity.

Harness invocations set the development environment variables `MMX_STATE_DIR`
for controller run history and `MMX_RUNNER_DIR` for node runner executables.
Generated projects use `mmx_project_settings.yml` (project
settings), `inventories/<name>.yml` (nodes and their connections), and
`playbooks/<name>.yml` (the ordered work). A playbook lists fleet stages;
each fleet stage selects nodes and their node tasks, the actions each node
performs.

Guest suites run only on disposable test machines created by
[`incus.py`](#the-full-catalog-with-incus) or
[`hetzner.py`](#the-full-catalog-on-hetzner-cloud-servers).
For each job slot, `incus.py` or `hetzner.py` writes a JSON file describing
the slot's two nodes and sets `MMX_TEST_TARGETS` to the JSON file's path.
A suite without `MMX_TEST_TARGETS` refuses to run and names the two entry
points; a suite never creates a test machine of its own.

## MMX commands and locking

Run the [test commands listed in the acceptance contracts](acceptance.md#tests)
from the repository root. The VM suites run through `incus.py` or
`hetzner.py`, as the sections below describe.
All VM suite entry points and `vm.py up/test/reset` automatically
hold an OS file lock on `<test-root>/.test.lock` for the whole invocation.
`scripts/build.sh` locks the parent of its output directory; its default output
is `<MMX_TEST_ROOT>/bin`. Builds launched by a suite inherit its existing lock.

A competing command prints the test-root path, the lock owner's process ID,
and the lock owner's command, then waits.
Ctrl-C cancels the waiting command. The OS releases a lock when its last owning
descriptor closes, including after a process exits or is killed.
The `.test.lock` file may remain afterward, but the lock file's presence does
not mean the environment is busy. Do not delete `.test.lock` to unlock the
environment, because waiting commands must continue using the existing
`.test.lock` file.

Independent roots have independent locks. `go test` and `go vet` do not acquire
the VM lock. Test the harness's locking without starting VMs:

```sh
python3 -m unittest discover -s tests/integration -p 'test_*.py' -v
```

For manual commands that change a test environment, use
`tests/integration/testenv.py`. The `testenv.py` wrapper holds the lock of
the test root that `--root` names while the command after `--` runs.
The default root is `MMX_TEST_ROOT`, or `~/.cache/mmx-testing` when unset:

```sh
python3 tests/integration/testenv.py --root <test-root> -- rm -rf <test-root>/workflows
```

The lock coordinates the VM suite entry points, `vm.py up/test/reset`,
`scripts/build.sh`, and commands wrapped by `testenv.py`.
A raw `incus` or filesystem command does not participate.
Scope manual maintenance to the named project and acquire the project's lock
first. Read-only inventory needs no lock.
After an interrupted suite, the next suite resets its fixtures normally.
Stopping the harness does not guarantee a previously admitted node job
stopped at the same instant.

## Release archives

Build and check the four controller archives in a new output directory:

```sh
python3 scripts/build-release-archives.py --version 0.1.0 --output-dir "$TMPDIR/mmx-release-archives"
```

The builder checks every archive and writes `SHA256SUMS`; the
[release design](design/development.md#pure-go-implementation-and-dependencies)
owns the contents, version stamping, and reproducibility rules.

Check one archive again, including running `mmx version` when its platform
matches the checking machine:

```sh
python3 scripts/check-release-archive.py "$TMPDIR/mmx-release-archives/mmx-0.1.0-macos-arm64.tar.gz" --version 0.1.0
```

Check all downloaded archives against the checksum file:

```sh
(cd "$TMPDIR/mmx-release-archives" && shasum -a 256 --check SHA256SUMS)
```

For a reproducibility check, build the first build's source and version into
a second output directory. Use the first build's `SOURCE_DATE_EPOCH` value,
or leave `SOURCE_DATE_EPOCH` unset for the first and second builds.
Then compare the archives with `cmp`.

The archive checker proves the layout, versions, and absence of Linux shared
library dependencies.

The catalog suite
[`dns_tls_and_account_lookups.py`](../../tests/integration/dns_tls_and_account_lookups.py)
checks system interactions: public DNS and default HTTPS certificate trust
on the controller machine and node. The suite checks named account and group
lookup on the node. The suite compares the controller executable's version
with the installed node runner executable's version for equality.
`dns_tls_and_account_lookups.py` uses one playbook run and an immutable Go
module URL with a pinned SHA-256 for the
controller machine's download. The suite installs no extra trust certificates
or account lookup libraries. `dns_tls_and_account_lookups.py` runs in an
Incus container on every target distribution, or on a Hetzner Cloud server.

Before a release, run the whole catalog with
`--build-and-use-release-archive VERSION` for the Linux platform validation,
through [Incus](#the-full-catalog-with-incus) on AMD64 and
[Hetzner Cloud](#the-full-catalog-on-hetzner-cloud-servers) on ARM64. Include
`dns_tls_and_account_lookups.py` in the
[macOS controller check](#the-macos-controller-check), with
`--build-and-use-release-archive VERSION`.

## Static analysis for a release

Run static analysis only as a [release step](design/README.md#release-checklist),
from the repository root on the development machine:

```sh
scripts/static-analysis.sh
```

`scripts/static-analysis.sh` installs pinned tools in a temporary directory,
prints every report, and exits non-zero if any check fails.
`scripts/static-analysis.sh` runs the following commands, with the installed
tool directory on each tool's path:

```sh
go vet ./...
staticcheck ./...
errcheck -ignoretests -exclude errcheck_excluded_functions.txt ./...
exhaustive -d ./...
nilaway -test=false -include-pkgs=mmx ./...
python3 scripts/check-dependency-vulnerabilities.py
```

For exhaustive v0.15.0, `-d` treats a default branch as exhaustive.
To run only the dependency vulnerability check, use:

```sh
python3 scripts/check-dependency-vulnerabilities.py
```

The dependency check can run from any directory and checks the repository
containing `scripts/check-dependency-vulnerabilities.py`.
The dependency check needs Python 3, Go, and network access.
The dependency check exits 0 when no reached advisory remains unhandled,
1 for an unhandled reached advisory, and 2 when the check could not run.
The [development design](design/development.md#when-a-copy-or-the-toolchain-changes)
owns the upstream-copy method and the rule for counting recorded fixes.

Reviewed exceptions have their reasons beside them: `staticcheck.conf`
excludes ST1005 because MMX's user errors are full sentences;
the `lint:ignore SA1019` comment in `internal/endpoint/server.go` permits
naming the deprecated DSA constant only to reject that algorithm. The
`lint:ignore S1005` comment in `internal/controller/assets.go` keeps an explicit
discard of a cancelled transfer error after another failure determines the result.
Errcheck exceptions are in `errcheck_excluded_functions.txt` for whole functions
and in commented `_ =` assignments for individual calls. NilAway exceptions
are `//nolint:nilaway` comments beside the reported statements. Each exception
has a one-line reason. Errcheck and NilAway leave out test files.
`scripts/static-analysis.sh` builds NilAway from pinned source with
`scripts/nilaway-call-through-function-variable.patch` applied; the patch
file says what it fixes and when it goes away.

## Fuzzing for a release

Fuzz tests change input bytes to find parser failures. Run the fuzz tests as a
[release step](design/README.md#release-checklist), from the repository root
in a writable copy of the working tree on a Linux test machine:

```sh
scripts/run-fuzz-tests.sh
```

The script runs nine targets, one at a time, for 10 minutes each with four
worker processes. To change the 10-minute duration and four-worker count:

```sh
scripts/run-fuzz-tests.sh --fuzz-time-per-target 30s --fuzz-workers 2
```

The `--fuzz-time-per-target 30s --fuzz-workers 2` options give each target
30 seconds and two workers. `scripts/run-fuzz-tests.sh` prints each result,
continues after failures, and exits non-zero if any target fails.

Go writes a failing input in the target's Go package directory, under
`testdata/fuzz/<Target>/`. Preserve the failing input file before replacing
the working-tree copy. Use the reproduction command Go prints with the
package path. For example:

```sh
go test ./internal/config -run='^FuzzProjectSettingsFile/<input-hash>$' -count=1
```

Replace `<input-hash>` with the filename Go printed. Fix the fault, rerun
the failing input and the affected tests, then commit the fix and input
together.

Ordinary `go test ./...` runs the supplied seeds and committed failure inputs
without starting fuzzing. Keep small inputs that protect distinct faults;
do not commit Go's generated fuzz cache.

## Go tests that never depend on the machine's speed

A Go test must pass on a loaded machine as it does on an idle one, including
beside other tests. A test that passes idle and fails once in a while loaded
is broken, also when a rerun passes. Such a failure looks like a fault of
MMX, costs an investigation each time, and teaches people to ignore a failed
run.

- **Wait for the event, not for a time.** A test that expects something to
  happen polls for the expected event with a deadline far above the normal
  time. The wait returns as soon as the event happens, so a long deadline
  costs nothing.
  A fixed sleep followed by an assertion that something happened is a fault.
- **The test controls every time span that its outcome depends on.** When
  the outcome depends on a window, an interval, or a delay of MMX itself,
  the test holds the span open until its own work is done and then ends it,
  or sets the span far above what that work takes. A short real span that
  the test's own work must fit into is a fault, and so is an exact count of
  periodic events during a fixed interval measured by the real clock.
- **An upper limit on elapsed time is wide.** The upper limit proves only
  that something ended instead of hanging or waiting for a default:
  at least ten times the
  normal time and at least 3 seconds, and below the time that the fault
  would take.
- **A wait that proves that something did not happen** may be a fixed
  sleep. A slow machine cannot make the absence check fail, but the check
  can only miss an event. Keep the fixed sleep short.
  A lower limit on elapsed time is fine too, because real time never
  runs faster.
- **A test that runs in parallel shares no process-wide state with other
  tests.** A test that calls `t.Parallel()` must not share the standard
  logger, environment variables, the working directory, or package
  variables with other tests. Under the race detector, one such test
  caused 17 unrelated tests to fail.
- **Prove it under stress.**
  [`scripts/run-go-tests-under-stress.sh`](../../scripts/run-go-tests-under-stress.sh)
  builds every Go test package with the race detector and runs the tests
  with Go's own `stress` tool (`golang.org/x/tools/cmd/stress` v0.48.0):
  by default 40 runs, 4 at once, which loads the machine. Run the tests that
  a change adds or changes by name, and every test before a release:

  ```sh
  scripts/run-go-tests-under-stress.sh --tests-matching 'TestConfirmationHTTPSConnectionsAreFreshAndProgressStillReuses|TestWaitEntryKeepsConfirmingAfterProgressExhaustsRetries'
  scripts/run-go-tests-under-stress.sh
  ```

  Run the whole suite, like the fuzz tests, from a writable copy of the
  working tree on a suitably sized Linux test machine. A measured full run
  took about 90 minutes. Run named tests locally while iterating.
  `--runs N` changes the number of runs, `--runs-at-once N` changes how many
  run at once, and `--tests-matching REGEXP` selects tests as `go test -run`
  does. Exit 0 means every run passed; the script removes its work directory.
  Exit 1 lists each failed test and names the files that keep the whole
  output of each failed run. Exit 2 means no test matches.
- **A test that failed once is fixed at once.** Reproduce the failure
  with `scripts/run-go-tests-under-stress.sh`, find the cause, and fix the
  cause by the rules in "Go tests that never depend on the machine's speed".
  Prove the fix with the stress command used to reproduce the failure.
  Never add a retry of a failed assertion, a skip on a slow machine, or a
  longer sleep. The rules for fixing a failed test also apply to a
  VM-suite scenario.
  The harness keeps failed-suite logs under the run's test root.
  Find the cause from the failed-suite logs.

The [test levels](#the-full-catalog-with-incus) say when to run the checks in
"Go tests that never depend on the machine's speed". The
[release checklist](design/README.md#release-checklist) requires
every Go test to pass under stress.

## Run the full VM catalog

The catalog of VM suites is `SUITES` in [`all.py`](../../tests/integration/all.py):
every VM suite in the [acceptance contracts' test list](acceptance.md#tests),
ordered by expected execution duration, longest first.
[`incus.py`](#the-full-catalog-with-incus) and
[`hetzner.py`](#the-full-catalog-on-hetzner-cloud-servers) run the catalog.
For each distribution, job slots take suites from a shared queue until the
queue is empty. Each slot starts with two new test machines for successive
suites assigned to the slot. The harness reports each result plus the total
wall time.

Each slot has an isolated test root, `slots/<n>` below the distribution's
directory in the overall test root.
The slots of one run share one build: the first suite that needs MMX or the
Go test programs builds them in the run's `build` directory, and every other
slot links those files, so that slots do not compile the same code at once.
`hetzner.py` and `incus.py`
share one build across all their distributions. Suites still acquire their
ordinary environment lock, each in its slot's root. Concurrent runs sharing
a root serialize on that root's lock. `all.py` makes no test machines of its
own and refuses to run by itself.

Full output goes to each slot's `logs/<suite>.log`.
The next invocation of the suite in the slot replaces the suite's log.
When a suite fails, `all.py` prints the log path and last 30 lines.
Any suite failure, and any suite that no slot could run, makes the invoking
`incus.py` or `hetzner.py` command exit nonzero.

To end a run before it finishes, use the harness:

```sh
python3 tests/integration/vm.py stop
```

`vm.py stop` ends the command holding the test-root lock and the commands
holding the root's job-slot locks, with every process started by those
commands. The test root is `MMX_TEST_ROOT`, or `~/.cache/mmx-testing` when
unset. Then `vm.py stop` deletes remaining guests from `incus.py` runs in
the selected test root. The stop command also deletes remaining servers
from `hetzner.py` runs in the selected test root.
Each environment's lock file names the process holding the environment's
lock. The `vm.py stop` command ends only the lock-owning process and
processes started by the lock-owning process.
Runs in other test environments, and other projects' runs on the test
machine where `vm.py stop` runs, go on. Never end a run with `pkill` or
`killall`: a name pattern also matches the runs of others on a shared test
machine.

## The MMX harness

The Python harness uses only the standard library and `ssh`. The harness
reaches each test machine through the file that `MMX_TEST_TARGETS` names.
Two-node suites use `mmx-dev` and `mmx-peer`, the two test machines of a job
slot. Tests reset MMX state and their named fixtures inside the disposable
machines. The facts suite resizes its test machine to give the test machine
more CPUs and memory, then tries to restore the original size, as the
[Incus](#the-full-catalog-with-incus)
and [Hetzner Cloud](#the-full-catalog-on-hetzner-cloud-servers) sections
describe. `vm.py up` checks that a test machine carries its disposable-test
marker and runs without failed units. `vm.py reset` resets the test machine's
MMX state. `vm.py test` runs the first-server and recovery scenarios.
Add `--vm mmx-peer` for the second machine. Binaries and diagnostic histories
stay in the test root; source and test definitions stay in the repository.
`linux_code.py` cross-compiles the Go test executables and runs three
executables at a time in the second VM, which needs no Go installation.
Running executables sequentially made `linux_code.py` the longest suite,
mostly waiting.

By default, each run builds development programs with `scripts/build.sh`.
With `--build-and-use-release-archive VERSION`, `incus.py` and `hetzner.py`
first run `scripts/build-release-archives.py` against the working tree. The
archives remain in the run's `release-archives/` directory under the test
root. The harness unpacks the archive for the controller machine's own
operating system and architecture into the run's shared `build/bin/`.
Every slot's `vm.build()` then uses the unpacked controller archive's
`mmx`, both Linux node runner executables, and
`official_extra_modules/`, without running
`scripts/build.sh`. A release build failure ends the run.

The Go test programs that `linux_code.py` needs are still compiled
separately. Suites that explicitly build a Linux controller or a test-only
runner still build the Linux controller or test-only runner separately.
A suite that builds only `mmx` and uses the run's node runner executables
stamps its `mmx` with their version (`vm.release_version_flag()`), because
`mmx` refuses node runner executables of another release.

The harness prints the selected archive's filename and SHA-256. Compare the
printed archive filename and SHA-256 with the published release's
`SHA256SUMS`: matching source, version, timestamp, and toolchain produce
identical archives. `--build-and-use-release-archive VERSION` takes only
the version, so a wrapper can forward it to another test machine without
transferring a local archive path.

Suites use HTTPS after automatic setup, through the harness's SSH port
forwards. See
[SSH connections and HTTPS tunnels](#ssh-connections-and-https-tunnels).
TLS verification and request authorization still happen in MMX. Each suite
checks server state independently of MMX's reported status: reload counts, for
example, come from systemd's own records, not an MMX task marker.

`docker_containers.py` and `podman_containers.py` pull images from a test
image registry and never from Docker Hub. A suite starts the registry on
the suite's guest with
[`image_registry.py`](../../tests/integration/image_registry.py).
The small Python server runs as `mmx-test-image-registry.service`, listens
on the guest's `127.0.0.1:5000`, and answers pull requests.

The harness also makes the test image. The image holds no shell and one
program,
[`container_test_program`](../../tests/integration/container_test_program/main.go),
which serves HTTP and reports its image version, its configuration file, and
its health.

The suites stay away from Docker Hub for three reasons. Docker Hub
limits pulls without a login from each address, and guests on one test
machine can share its public address. The suites need a tag that moves
to a newer image and a private repository that needs a login. The registry
supplies the moving tag and login-protected repository. The registry also
logs every request, so a suite can prove that a dryrun fetched no image layer.

### systemd-networkd file guest suite

`systemd_networkd_files.py` is a VM suite using the ordinary production binaries
from `vm.build()`. `systemd_networkd_files.py` runs on Debian 12 and Debian 13.
On other distributions, `systemd_networkd_files.py` prints a skip with the
reason. The suite needs Incus because network loss must not stop
observations. The suite adds a
second card with `incus config device add <guest> second-interface nic nictype=p2p mtu=1500`
and removes the second card at the end. The suite discovers the main card
from the default route and the second card from the added sysfs entry,
without assuming interface names.
For every case that restores a file, if networkd reports the main card's
distribution file also manages the second card, plain test setup gives the
second card a static address and `RequiredForOnline=no`. The suite removes
the second card's static-address file after the case. The static-address
file prevents the disconnected test card from delaying each restore's
online wait or blocking boot while the card waits for DHCP.
The first case manages the second card's own file and needs no fixture.
The harness still requires that the node is not degraded after its restart;
on systemd 252.23 the non-degraded state also checks that restoration
avoids the reload-at-boot fault.

`systemd_networkd_files.py` covers:

- A second-card file: dryrun, installation, unchanged rerun, changed address,
  and removal.
- Every measured native mistake that logs an attributable warning on the
  suite's guest, including exit-zero reloads. No sleep is added to the
  `systemd_networkd_file` journal check.
- Bridge with address and port, VLAN on the second card, bond, dummy, veth
  and its peer, and an unreferenced rename. Dependencies are removed in
  reverse order. Existing-device changes, another creator's device, and
  removal while referenced fail without a file, device, record, unit, or
  service-journal change.
- Removal of a bridge definition while its own address file stays. A native
  systemd `ExecStartPre` gate lets the suite stop and drain the controller
  process's requests before activation finishes. The suite kills the node
  job worker process too.
  The timer alone recreates the file, bridge, and address.
- Main-card cut-offs with a previous file and with none, checking restored
  bytes, owner, group, mode, modification time, DHCP address, default route,
  new SSH, failed task, human output, and cleanup. Both main-card cut-offs
  use a live node job worker process.
- A cut-off with a killed worker and drained requests, a reboot that must
  restore before its deadline, and a native restore failure repaired by an
  administrator starting the same restore service.

The suite uses a twenty-second confirmation limit for ordinary changes, which
end as soon as the confirmations arrive. The limit is twelve seconds for
ordinary cut-offs, fifteen seconds for bridge recreation, twenty seconds
for a cut-off with process draining, and two minutes for reboot.
`systemd_networkd_files.py` never waits for the reboot deadline.
Related device tasks share one controller process while every file still
receives separate confirmations. Cleanup
checks all paths in one Incus request. Each case prints its elapsed seconds
in its PASS line. Incus shell inspections cannot confirm reachability
or start MMX recovery. The timer cases send no MMX request before restoration
finishes. Failures name the case, expected state, observed record, unit states,
and recent activation, restore, and systemd-networkd journal lines.

Measured run time is five and a half to seven minutes per guest, with the
restore's startup wait at boot and the second-card fixture. The fixture
avoids a ten-second wait for each restore on a
guest whose distribution file also manages the disconnected card. One Debian 12
run had this split, in case order; building, setup, and cleanup add time outside
the cases. Most time goes to the device case's individually confirmed changes.

| Case | Seconds |
| --- | ---: |
| Second-card file: dryrun, present, unchanged, changed, absent | 20 |
| Native rejection of every measured mistake | 49 |
| Devices, ordering refusals, and reverse removal | 104 |
| Timer recreates the removed bridge and address | 37 |
| Main-card cut with a previous file | 27 |
| Main-card cut with no previous file | 27 |
| Worker killed during the main-card cut | 26 |
| Restart during the main-card cut, before the deadline | 22 |
| Failed restore and administrator repair | 27 |

See [validation](validation.md#systemd-networkd-file-module) for passing guests and pending checks.

### Netplan file guest suite

`netplan_files.py` uses production binaries on Ubuntu 24.04 and Ubuntu 26.04
VM guests. On other distributions, `netplan_files.py` prints a clear skip.
Incus supplies an independent
observation path and the second card, using the setup of
`systemd_networkd_files.py`. No recovery depends on the observation connection.

The suite checks a second card's dryrun, installation, unchanged rerun,
changed address, and removal. During the unchanged rerun, an address monitor
on the main card must receive no event. Removal must leave no global address
or route on the second card. Native rejection covers an unknown key, bad
address, wrong structure, and removal of a bridge another file's VLAN needs.
Each refusal preserves live YAML metadata and bytes, generated filenames and
bytes, devices, restore records, and units. Generated modification times
are deliberately excluded because Netplan can rewrite identical live output.

One file creates a bridge with the second card as its port, VLAN, bond,
dummy, and veth pair. A replacement drops one device. Changed bridge STP,
changed VLAN identifier, and a pre-existing device created outside Netplan
are refused. File removal deletes the devices and releases the port.
A native systemd gate lets the suite drain the controller process before
removal finishes; the timer must recreate the bridge, its address, and its
port without a controller request. Native measurements on Ubuntu 24.04 also
restore the bridge, address, and port with `netplan apply` alone.

Main-card cut-offs cover the existing `50-cloud-init.yaml`, a new
`99-mmx-cut.yaml`, a killed worker, restart before the deadline using the
added file, and a failed apply repaired by starting the same restore service.
The suite checks previous bytes, owner, group, mode, modification time,
addresses, default route, new SSH and HTTPS, failed task results, and unit cleanup.
MMX's HTTPS goes directly to the guest, never through the harness's SSH
tunnel: the tunnel's one long-lived SSH connection can end during a cut, and
HTTPS through the tunnel then fails although the node is fine.
The main file, `50-cloud-init.yaml`, is saved and restored with its
metadata during suite cleanup.
Cases that drain requests stop the node HTTPS API process. Each later case
that needs HTTPS starts the node HTTPS API process before requesting its network change.

Each case prints its duration in the PASS line. Confirmation allowances are
twenty seconds for ordinary changes, which end as soon as the confirmations
arrive, fifteen seconds for cut-offs, twenty
seconds for bridge restoration, twenty-five seconds for process draining,
and two minutes for restart. The suite waits for observed state, not a full
restart deadline. One run takes about four minutes per guest.

### ifupdown interfaces file guest suite

`ifupdown_interfaces_files.py` is a VM suite using the ordinary production
binaries from `vm.build()`. `ifupdown_interfaces_files.py` runs on Debian 12
and Debian 13.
On other distributions, the suite prints a skip with the reason.
The suite's setup switches the guest from
systemd-networkd to ifupdown, as Debian's installer and the installers of
some server providers set a server up: setup installs `ifupdown` and writes a main
file with `source /etc/network/interfaces.d/*`, `lo`, and the main card
static with the main card's DHCP-given address and gateway.
Setup turns off cloud-init's network configuration and systemd-networkd,
points DNS at the bridge gateway, and restarts the guest once.
The suite then adds a second card with
`incus config device add <guest> second-interface nic nictype=p2p mtu=1500`.
Cleanup switches the guest back to systemd-networkd. Incus supplies the
observations while the main card is cut off. The suite reuses the
systemd-networkd suite's observation helpers, and no recovery depends on
the observation helpers.

`ifupdown_interfaces_files.py` covers:

- A new `interfaces.d` file for the second card: dryrun naming the interface
  ifupdown would bring up and writing nothing, installation with the address and
  ifupdown's state up, and an unchanged rerun without a record or
  activation; then a changed address, with the old address and route gone.
- A VLAN on the second card in a second file, created and then removed with
  `state: absent`, which removes the device, its address, and the file.
- A route added to the main card's stanza in the main file, confirmed over
  new SSH and HTTPS connections; the restore data holds interface names and
  classes only.
- A main file that cuts the node off: the node restores the previous bytes,
  owner, group, mode, and modification time, the main address and default
  route, and new SSH, and the task fails with the expected reason.
- An added file whose `up` command fails: the restore runs at once and
  removes the file and the address that the failed `ifup` left.
- Refusals before any change, in dryrun and run: a misspelled option, a
  changed `source` line in the main file, and an `interfaces.d` file when
  the main file reads no `interfaces.d` file.
- A node restart during the confirmation wait of a cut-off: the restore runs
  after `networking.service` on the new boot and puts back the previous main
  file and address before its deadline.

The suite uses twenty-second confirmation limits for ordinary changes, which
end as soon as the confirmations arrive, twelve seconds for the cut-off, and
two minutes for the restart, whose restore it observes without waiting for
the deadline. Each case prints its elapsed seconds in its PASS line.
[Validation](validation.md) records current guest results.

### Owed restore guest suite

`owed_restores.py` is a VM suite on every tested distribution.
`owed_restores.py` tests
confirmation over SSH and HTTPS, lost listeners, failed and hanging activation,
a killed node job worker process, a node restart, failed restore and administrator
repair, refusal of competing file changes and runner replacement, and dryrun.
[Validation](validation.md) records the guests that have passed the
foundation suite `owed_restores.py`.
Accepted changes must remove the saved contents and all three units immediately.
After the original deadline plus two seconds for timer rounding and accuracy,
the suite requires `change_accepted`, the proposed contents, and no
previous-configuration activation. The suite then expires the test job
through normal history cleanup and checks that the test job's retained,
resolved restore record disappears too.

Only `owed_restores.py` builds `mmx` and the guest's `mmx-runner` with the Go build tag
`mmx_owed_restore_test`. The suite's `owed-restore-test-binaries/` directory is separate
from the ordinary shared `bin/`; no other suite selects these binaries.
The `mmx_owed_restore_test` tag adds `test_change_with_owed_restore`, a core
module for tests only.
The test module takes `path`, `content`, `activation`, and `confirm_reachability_within`.
The test module's fixed activation choices are `succeed`, `stop_ssh_listener`,
`stop_https_listener`, `fail`, `hang`, and `wait_for_release`. Saved restore
data selects these compiled actions; it contains no command text.

Cases normally use eight seconds for confirmation. The immediate-failure cases
use 25 seconds to distinguish restoration from deadline expiry. The timer-only
case uses 15 seconds to allow outstanding requests to drain. The reboot case
uses two minutes, but requires completed recovery before that deadline and
never waits for the reboot deadline. A reboot that consumes the allowance
fails explicitly.
Only the hanging-activation case waits for systemd's five-second allowance to
stop a child that ignores SIGTERM. The restore process constructs the test
adapter before putting back the previous file. The test adapter checks the
parent and child during construction.

`timer-alone-after-worker-death` stops the controller process and its request
processes, closes their active shared SSH connections, and drains node requests
while the worker is alive. The case then kills the worker before the deadline.
Only shell inspections follow until the restore has completed; no MMX request
can start recovery. `restore-on-new-boot-before-deadline` uses the request
drain of `timer-alone-after-worker-death` and requires recovery on the new
boot, after `network.target`, before the original deadline.

`restore-after-ssh-listener-cut-with-live-worker` and
`restore-after-confirmed-https-cut-with-ssh-working-and-live-worker` prove
restoration after a cut with a live worker, which may start the restore itself.
The SSH-listener-cut and HTTPS-listener-cut cases do not prove timer-only
recovery. Stopping the HTTPS API ends established
connections too, so the HTTPS-listener-cut case does not prove that
confirmation uses a new connection. The Go test
`TestConfirmationHTTPSConnectionsAreFreshAndProgressStillReuses`
in `internal/endpoint/reachability_test.go` proves that rule with real TLS
connections, while ordinary requests still reuse their connection.
The suite inspects the guest's `ssh.service` or `sshd.service`, active
`ssh.socket` or `sshd.socket`, and `KillMode=process` before stopping listeners.
The suite stops sockets before services and restores the units that were active.
No firewall package is installed. Guest runs must establish the SSH unit,
socket, and `KillMode=process` properties on each distribution.
The SSH properties are not assumed from the distribution's name.

Each failure prints its case, the expectation and observation, the last and
current restore records, all three units' states, and the last journal lines
of the activation and restore services. The suite also saves `failure-diagnostics.txt`
and `last-observed-owed-restore.json` under the case's controller workspace,
`<test-root>/owed-restores/<case>/`. Incus can collect diagnostics and restore
the SSH listener even when SSH no longer works.

Test files live under `/var/tmp/mmx-integration/owed-restores/<case>/` on the
node. Beside `configuration`, the test module writes `node_job_worker_process_id`,
`change_activation_started`, `change_activation_parent_process_id`,
`change_activation_child_process_id`, `owed_restore_started.json`, and
`previous_config_activated` when the case needs the marker files. The suite creates
`release_change_activation` to release the activation gate and
`fail_previous_config_activation` to make restoration fail until repair.
`owed_restore_started.json` contains `node_time`, `node_boot_id`, and
`network_target_state`. The test controls do not exist in production builds.

Run only `owed_restores.py` on the two reference distributions through Incus:

```sh
MMX_TEST_ROOT=$HOME/.cache/mmx-incus python3 tests/integration/incus.py \
  --distribution debian13 --distribution ubuntu2604 --suite owed_restores.py
```

## SSH connections and HTTPS tunnels

Harness checks share one SSH connection per test machine. Control sockets
live in `/tmp/mmx-test-ssh-<hash of the test root>`, because a Unix socket
path is limited to about 100 characters. Port forwards and agent-forwarding
sessions open separate SSH connections through `vm.unshared`.
Stopping or rebooting a test machine drops the shared sockets.
Each harness process removes `/tmp/mmx-test-ssh-<hash of the test root>`
when the process ends. A run on a test server has a new test root, so the
socket directory would otherwise stay in `/tmp`.
For application/HTTPS access, the harness allocates
a free loopback port on the controller machine, establishes an explicit SSH
tunnel to the node's port 7443, checks that the tunnel started, and records the
selected port in that test's inventory. Keep an HTTP webhook receiver inside
a disposable guest when access from the controller machine is unnecessary; any
receiver on the controller machine must also allocate a free port rather than
assume a fixed one.

## The base suite

The base suite is the one set of integration suites that proves MMX runs on
a Linux distribution: first server and recovery, both kinds of connection,
the node facts, and the plain-named modules, whose mechanism is the same
wherever they run. The [development
design](design/development.md#testing-on-several-distributions) owns the
rule for when the base suite runs; this section gives the commands and the
members. `BASE_SUITE` in `tests/integration/all.py` is the one list of the
members, and `--base-suite` of `incus.py` and `hetzner.py` selects them:

```sh
MMX_TEST_ROOT=$HOME/.cache/mmx-incus python3 tests/integration/incus.py \
  --distribution archlinux --distribution opensuseleap161 --base-suite
```

`--base-suite` runs the members, and any `--suite` beside it adds suites.
Every distribution of a run gets every member. A suite that is not a member
is a tool module's suite and runs on the Debian family only, unless
`SUITE_DISTRIBUTIONS` in `all.py` names other distributions for it.

| Members | Proves |
|---|---|
| `vm.py test` | First server and recovery: setup, resource state, validation, resume with frozen files, loss of the worker, the controller process, and the node HTTPS API process, the reboot task, retention |
| `access.py`, `limited_control.py`, `task_secrets.py`, `linux_controller.py`, `controller_machine_as_node.py` | Both connections (SSH and HTTPS): credentials, administrator-key changes, the limited-control account, secrets, a Linux controller machine, the controller machine as a node |
| `owed_restores.py`, `fleet.py`, `node_history.py`, `inspection.py`, `one_time_playbooks.py`, `deployment.py`, `release_directories.py` | Recovery after lost access, fleet stages, saved history and results, one-time playbooks, the rolling deployment, release directories with a Git checkout |
| `node_facts.py`, `linux_code.py`, `external_modules.py`, `module_helper.py` | Facts of each family, the Go tests as native Linux programs, modules of the project's own |
| `file_resources.py`, `directory_resources.py`, `directory_copy.py`, `streamed_assets.py`, `account_units.py`, `downloads.py`, `http_requests.py`, `dns_tls_and_account_lookups.py` | The plain-named modules: `file`, `directory`, `symlink`, `line`, `edit_file`, `read_file`, `user`, `group`, `systemd_unit`, `systemd_service`, `shell`, `http`, `wait_for`, `download`, `authorized_keys`, `reboot`, `assert` |

A member prepares its node with the harness and installs no package with a
package module such as `apt_package`: a module of one package manager would
fail on the other families, and the base suite must run everywhere.
Where a member needs a program on the node, it installs the program with the
node's own package manager through `vm.install_packages`, as a person does
before MMX takes over (`vm.py test` installs nginx this way). A test in
`test_all.py` fails when a member names a package module.
Suites that test a package module, `ufw`, or another tool module are not
members; they are that tool module's suites.

`linux_controller.py` checks a Linux controller machine against the guest
managed by the Linux controller machine. The dedicated forwarded SSH agent
contains only the target's existing test identity. Private login keys never
enter the guest.

To test other distributions, name them with `--distribution` on `incus.py`
or `hetzner.py`; each distribution's job slots have their own directory in
the test root, so their logs and workspaces stay apart. Clean superseded
test roots before adding image and disk storage. Keep other projects'
test environments separate.

## The full catalog with Incus

`incus.py` runs the catalog in Incus system containers and VMs on a Linux
test machine.
Testing with `incus.py` is the fastest method and needs no cloud servers.
A system container boots a distribution's own systemd, accounts, network, and
packages, like a server, and shares the Linux test machine's kernel.
A container starts in seconds, so each job slot gets two new ones.
Run `incus.py` with a test root dedicated to the Incus run:

```sh
MMX_TEST_ROOT=$HOME/.cache/mmx-incus python3 tests/integration/incus.py
```

For the platform validation before a release, use the release build:

```sh
MMX_TEST_ROOT=$HOME/.cache/mmx-incus python3 tests/integration/incus.py \
  --build-and-use-release-archive 0.1.0
```

The `incus.py` command with `--build-and-use-release-archive 0.1.0` runs
the whole catalog on every tested distribution: each distribution gets the
[base suite](#the-base-suite), and the Debian family's four distributions
also get the tool modules' suites. The suites share production
programs from the archive for the Linux controller machine.
The [harness section](#the-mmx-harness) explains archive selection and the
printed checksum. Use the intended release version in place of `0.1.0`.

Test in levels to get quick feedback and establish whether the change works.
The goal is to test, not to wait: run the smallest set that can show whether
the change works, and run a larger set only when it can find something that
the smaller set cannot.

The [development design](design/development.md#testing-on-several-distributions)
owns the rule for which distributions each level runs on; the list below
gives the commands.

- **While iterating**, run `go test` for the Go packages the change touched,
  on the controller machine. When a change adds or changes a Go test that
  waits for something, or code that runs in goroutines or on timers, run
  those tests under stress by name before the commit:
  `scripts/run-go-tests-under-stress.sh --tests-matching 'TestA|TestB'`.
- **After an ordinary change**, level 1: run only the suites the change
  touches, on Debian 13 and Ubuntu 26.04, the two reference distributions.
  Running the affected suites on the two reference distributions takes a
  few minutes. A change to one family's own modules, such as `dnf_package`
  or the `selinux_` modules, also runs that family's suites on one
  distribution of the family, such as `rocky9` (a VM, see below):

  ```sh
  MMX_TEST_ROOT=$HOME/.cache/mmx-incus python3 tests/integration/incus.py \
    --distribution debian13 --distribution ubuntu2604 --suite fleet.py --suite access.py
  ```

- **Once at the end of each build step**, before its report and push, run
  the step's new and changed suites on the distributions where they run: the
  command with `--suite` and without `--distribution`. A suite of the base
  suite runs on every tested distribution, and a tool module's suite runs by
  default on the distributions that its page lists under "Tested on". A run
  can name other distributions with `--distribution`.
  Run the new and changed suites once when integrating the build step. Individual changes use level 1; do not repeat
  the complete check after each fix.

  ```sh
  MMX_TEST_ROOT=$HOME/.cache/mmx-incus python3 tests/integration/incus.py \
    --suite docker_containers.py --suite podman_containers.py
  ```

  Each distribution has two VM pairs by default. When more than two of the
  named suites need a VM, `--vm-pairs-per-distribution 3` runs three of them
  at once: a distribution's three VM suites took about 17 minutes with one
  pair and under 8 minutes with three.

- **When the step changed code that every suite uses**, also run the whole
  catalog on Debian 13 and Ubuntu 26.04 once.
  Shared code includes the node task execution engine, the node program
  `mmx-runner`, the controller program `mmx`, and the SSH and HTTPS
  connection code. Use the command with the two `--distribution` options
  and without `--suite`.
- **The base suite on every tested distribution**, `--base-suite` without
  `--distribution`, runs before a release and after a change to the test
  harness, to a guest's base, or to a plain-named module's mechanism.
- **The whole catalog on every tested distribution**, the command without
  `--suite` or `--distribution`, runs only before a release, as the platform
  validation, and after a change to the test harness or to the guests' bases.
  Each distribution gets the suites that run on it (the base suite, and for
  the Debian family also the tool modules' suites).
  Before a release, add `--build-and-use-release-archive VERSION`.
- **ARM64 Hetzner Cloud servers** run the whole catalog only before a
  release, as part of the platform validation before a release.
  Between releases, run suites on ARM64 servers only after a change to the
  ARM64 code of `hetzner.py`. Run only the suites that check the ARM64 change.
  The AMD64 and ARM64 builds of the node runner executable use identical
  Go source code and disable cgo. ARM64 therefore differs only in rare
  places, such as a package that a distribution does not build for ARM64.
- **When a run fails in one suite**, rerun only the failed suite on the
  distribution where the suite failed, until the suite passes.
  Run a larger set again only when the fix changed something that the
  larger set covers. A suite that fails alone
  and passes in the whole catalog depends on what another suite left in the
  guest: find that and remove it, in both suites.
- **Once at the end of a larger rework, and before a release**, run every Go
  test under stress on a Linux test machine: `scripts/run-go-tests-under-stress.sh`,
  about 90 minutes in a measured run. The stress script runs every Go test
  with the race detector, so the stress run takes the place of
  `go test -race ./...`.
  Follow the [rules for Go tests that never depend on the machine's
  speed](#go-tests-that-never-depend-on-the-machines-speed).
- **A commit that changes only documentation** runs no suite on guests and
  creates no [Hetzner Cloud servers](#the-full-catalog-on-hetzner-cloud-servers).
- **When a suite or a run is slow**, find where the time goes before running
  more, and remove waits and repeats that are not the subject of a test.

Each distribution's suites run in containers, except those that need the
guest's own kernel, which run in Incus VMs. **Every guest of Rocky Linux 9,
AlmaLinux 10, and Fedora 45 is a VM,** because SELinux does not enforce in a container
and a real server of the Red Hat family runs with SELinux enforcing; the
harness prepares these VMs with SELinux enforcing on labeled files
(`SELINUX_DISTRIBUTIONS` in `incus.py`; the Fedora guest first copies
`/usr/share/dnf5/repos.d` into `/etc/yum.repos.d`, where Fedora's repository
files can take the harness's package servers) and prints `SELinux Enforcing` in
each slot's platform line. Arch Linux and openSUSE Leap guests are
containers, with a VM for the suites below:

| Suite | Runs in a VM on | Why a container cannot run the suite |
|---|---|---|
| `account_units.py` | every distribution | The automount unit tested by `account_units.py` needs autofs, which a container cannot use. |
| `directory_resources.py` | every distribution | `chattr +i` needs a capability that an unprivileged container lacks. |
| `kernel_settings.py` | every distribution | Kernel settings and kernel modules belong to the kernel, which a container shares with the Linux test machine. |
| `storage.py` | every distribution | Swap, `mkfs`, and mounts need loop devices and the guest's own kernel. |
| `partitions_and_lvm.py` | every distribution | Loop devices with partitions and LVM's logical volumes need the guest's own kernel. |
| `zfs_datasets.py` | Ubuntu and Debian 13 | ZFS is a kernel module, which a container cannot load. On Debian 12, `zfs_datasets.py` installs no ZFS, tests the module without its programs, and runs in a container. |
| `snap_packages.py` | every distribution | `snapd` mounts each snap as a squashfs image and confines it, which an unprivileged container cannot do. |
| `docker_containers.py` | every distribution | In a Debian or Ubuntu container guest, Docker's daemon cannot read AppArmor's profiles ("Could not check if docker-default AppArmor profile was loaded") and starts no container. |
| `incus_instances.py` | every distribution | A server that runs Incus has its own kernel, and a virtual machine instance needs `/dev/kvm`. In a container guest, the Incus that the suite installs started an instance only with two changes that no real server needs: a smaller range of user IDs for root, and on Debian no AppArmor. |
| `libvirt_domains.py` | every distribution | The domains tested by `libvirt_domains.py` are virtual machines, which need the guest's own `/dev/kvm`. A container has no `/dev/kvm`: libvirt defines no KVM domain there and cannot start a QEMU domain. |
| `owed_restores.py` | every distribution | Recovery after lost access needs a real node restart and systemd's process groups. |
| `systemd_networkd_files.py` | Debian 12 and 13; on other distributions, `systemd_networkd_files.py` skips and prints the reason | Real network changes need a second card, observations through Incus while the guest's network is cut off, and a node restart. |
| `netplan_files.py` | Ubuntu 24.04 and 26.04; on other distributions, `netplan_files.py` skips and prints the reason | Real network changes need a second card, observations through Incus while the guest's network is cut off, and a node restart. |
| `ifupdown_interfaces_files.py` | Debian 12 and 13, switched to ifupdown; on other distributions, `ifupdown_interfaces_files.py` skips and prints the reason | Real network changes need a second card, observations through Incus while the guest's network is cut off, and a node restart. `ifupdown_interfaces_files.py` switches the guest to ifupdown. |
| `wireguard_interfaces.py` | Ubuntu 26.04 only | Ubuntu 26.04 confines `wg-quick` with an AppArmor profile, which an unprivileged container cannot enforce, and `wireguard_interfaces.py` runs every case under that profile, including a `ListenPort` below 1024. |
| `subscription_web_service_example.py` | Ubuntu 24.04 only | The [subscription web service example](../../examples/subscription-web-service-fleet/README.md) sets kernel settings, a swap file, and time synchronization, which belong to the guest's own kernel and clock. |

`blocked_port.py` and `podman_containers.py` run in containers on every
distribution, and `wireguard_interfaces.py` on the other three. A container
can hold a WireGuard
interface when the Linux test machine's kernel has the `wireguard` module,
which the kernel loads on first use.

A suite runs on every distribution of a run, unless `SUITE_DISTRIBUTIONS`
in `tests/integration/all.py` names its distributions, with the reason.
`subscription_web_service_example.py` runs only on Ubuntu 24.04: its
example uses Ubuntu's package names, such as `mysql-server`, and outside
apt repositories whose suite is Ubuntu's release name. `incus.py` and
`hetzner.py` print a line for each selected suite that the run's
distributions leave out. When no selected suite supports any selected
distribution, `incus.py` and `hetzner.py` stop before creating a guest or
a server, with `Nothing to run: none of
the selected suites runs on ...`.

A job slot keeps its two guests for every suite it takes, except after a
suite that `SUITES_AFTER_WHICH_A_SLOT_TAKES_NEW_TEST_MACHINES` in `all.py`
names: the slot then deletes its guests and takes new ones for its next
suite. `subscription_web_service_example.py` requires replacement guests:
the example's firewall drops every forwarded packet, which the container
and virtual machine suites need, and its data servers keep most of a VM's
memory.

A suite that `SUITES_WITH_A_THIRD_TEST_MACHINE` in `all.py` names gets a
third test machine, `mmx-third`, beside the slot's two machines.
`incus.py` starts one more guest of the slot's distribution and kind.
`hetzner.py` creates one more cloud server.
Each harness adds the third machine to the slot's file of test machines
for the suite, then deletes the third machine after the suite.

`pg_auto_failover.py` requires a third test machine because a
pg_auto_failover monitor and two database servers each need a separate
server. `pgbackrest.py` also requires a third test machine because the
suite restores a lost database server onto a new server. The lost server,
the new server, and the monitor each need a separate server.
A slot without room for the third machine fails the suite without running
the suite and prints why.
The third guest holds no shared guest slot of its own, because it runs
for one suite.

The reboot checks run in containers. A container gets a new boot ID when
the container restarts, as a server does.
`--kernel-suites-on-hetzner` runs the kernel suites on
Hetzner Cloud servers instead, for the distributions Hetzner has, as
`hetzner.py` does; the container suites stay on the Linux test machine.
Set the [cloud settings](#the-full-catalog-on-hetzner-cloud-servers) first.
With `--kernel-suites-on-hetzner`, `--max-hetzner-servers-at-once` sets the
cloud-server limit (default 12), independently of the Incus guest-pair limit.

Each distribution's container suites run in up to
`--container-pairs-per-distribution` job slots (default 3), and its kernel
suites in up to `--vm-pairs-per-distribution` (default 2). A slot keeps its
two guests for every suite that the slot takes, so a run starts at most five
pairs for each distribution. A run with fewer VM suites starts fewer
VM pairs. At most `--max-guest-pairs-at-once` slots (default: one and a half times the
machine's CPU count) have guests at once. The remaining slots wait for
capacity.
In one measurement on a 12-thread Linux test machine, 18 pairs kept its
threads busy. With 47 pairs, the machine ran at a load of about 100 and
the tests took longer.

Several runs, including runs of other projects, can share one Linux test
machine by using `--shared-guest-slots DIRECTORY`.
Each guest pair holds a lock on a `slot-*` file in `DIRECTORY` while the
pair runs. A pair waits while other runs hold all the slot locks.
Every run on the machine must use one common directory for
`--shared-guest-slots`, so the combined runs stay within the machine's
guest capacity. The kernel releases a slot lock when the process holding the lock ends,
even when the process is killed.

The shared-slot scheduler limits each run to a fair share while several
runs want slots: the number of slots divided by the number of runs.
A run alone can take all the slots. Without the fair-share limit, the first
run repeatedly reacquired released slots before waiting runs could acquire
slots.

Containers have 2 CPUs and 4 GiB like a Hetzner
`cx23`; VMs 2 CPUs and 2 GiB, because a VM's memory stays
taken.

A container's two CPUs are fixed ones, given out in turn. With a count
instead, Incus chooses the CPUs only after the container has started.
Until CPU assignment finishes, the container sees all of the machine's
CPUs, which MMX's facts reported after a reboot.

The `node_facts.py` suite stops a guest, gives the guest one more CPU and
1 GiB more memory, and starts the guest again.

Each line of output starts with the distribution and guest kind.
Each slot's suite logs are in
`<test-root>/<distribution>/containers/slots/<n>/logs/` or
`.../kernel/slots/<n>/logs/`.

Every guest is a copy of a prepared base of its distribution and kind, which
the first run makes from an image of `images.linuxcontainers.org`.
The harness uses the cloud variant when available. The image is copied
once into Incus's local store, so a run does not depend on the image server.
When several runs on one machine
find a base missing at the same time, one run makes the base and the remaining runs wait.
Each run holds a lock file in `bases/` of the shared guest slots' directory
while the run looks for the base. A run makes a base again when the base
is older than a day: the base's package indexes come from the day it was
made, and after a security update the mirror no longer serves the package
file an older index names, so a suite that installs a package would get
`404 Not Found`.
`--prepare-bases` makes the bases again from the newest images. A base must
look like the distribution's real cloud server, so its preparation corrects
the ways these images differ:

- A copy would keep the base's machine ID, and with it, on Debian, the
  base's DHCP client ID and address. The base ships an empty
  `/etc/machine-id`, as a cloud image does, and each copy sets its own
  hostname.
- A copy would keep the stamps of the base's timers in
  `/var/lib/systemd/timers/`, which say when each timer last ran.
  With the base's stamps, a copy booting on a later day catches up on a
  daily apt upgrade the copy "missed", within an hour of boot.
  During a suite, unattended-upgrade upgraded systemd.
  An MMX command then failed while systemd re-executed itself
  ("Transport endpoint is not connected").
  Base preparation deletes the stamps to prevent the catch-up upgrade, so
  a copy's timers start as on a new cloud server.
- Every VM guest runs with Secure Boot off. With Secure Boot on, a kernel
  loads only the modules that its distribution signed, and it refuses the
  ZFS module of the OpenZFS project and one that is built on the guest. A
  suite cannot change the setting from inside its guest, so the harness
  sets it on each copy. The bases keep the setting that their images start
  with.
- Each base gets an ordered list of package servers: the distribution's own
  server first, then Hetzner's mirror `mirror.hetzner.com` when the
  distribution's package server fails. A secondary mirror provides another
  source when the primary server is unavailable.
  - On Debian and Ubuntu, base preparation replaces each archive's server
    URI in the image's apt sources with
    `mirror+file:/etc/apt/mmx-test-package-servers/<archive>.list`
    (`debian-packages`, `debian-security`, `ubuntu-packages`,
    `ubuntu-security`). The list gives the distribution's server priority 1
    and Hetzner priority 2. The suites and components stay as the image has
    them.
  - The base then fetches its package indexes (`apt-get update`). A dryrun of `apt_package` reads the indexes that the
    node already has, and the fetch shows at once that the servers answer.
    Base preparation runs `apt-get update` instead of fetching indexes at
    each guest's start, because the fetch took 7 seconds on Debian before
    a slot's first suite.
- Containers keep the images' own change to PAM: an unprivileged container
  cannot set a login UID, so `pam_loginuid` is commented out.

`incus.py` uses the login account and test marker described for
[`hetzner.py`](#the-full-catalog-on-hetzner-cloud-servers): `mmx-test` with
passwordless sudo. `incus.py` makes a new login key for each run and a new
SSH host key per guest.
Each run records itself in `<test-root>/incus-runs/<run>/ownership.json` and
labels its guests with the Incus key `user.mmx-test-run`.
Cleanup deletes the run's Incus guests on success, failure, and handled
interruption. Use the `incus.py` run's `MMX_TEST_ROOT` value with
`vm.py stop` to end the run and delete the run's guests. After the controller
machine died, run cleanup with the manifest:

```sh
python3 tests/integration/incus.py --cleanup-from-manifest /absolute/path/to/ownership.json
```

For concurrent runs on one Linux test machine, create one shared directory
and its capacity files before starting runs. Each file allows one guest pair.
For example, create six slots once:

```sh
mkdir -p "$HOME/.cache/shared-test-guest-slots"
for guest_slot in 1 2 3 4 5 6; do
  touch "$HOME/.cache/shared-test-guest-slots/slot-$guest_slot"
done
```

The six `slot-*` files allow at most six guest pairs across concurrent runs.
Choose the number of `slot-*` files for the Linux test machine's CPU,
memory, and storage capacity.
Give every run the shared capacity directory created above,
`$HOME/.cache/shared-test-guest-slots`:

```sh
MMX_TEST_ROOT=$HOME/.cache/mmx-incus python3 tests/integration/incus.py \
  --distribution debian13 --distribution ubuntu2604 \
  --shared-guest-slots "$HOME/.cache/shared-test-guest-slots" \
  --max-guest-pairs-at-once 6
```

The `incus.py` command above runs Debian 13 and Ubuntu 26.04 with up to
six guest pairs. In the command above, `MMX_TEST_ROOT` selects the state
directory for the `incus.py` invocation. `--shared-guest-slots DIRECTORY`
selects the shared capacity files. `--max-guest-pairs-at-once 6` also limits
the `incus.py` invocation above to six pairs. Other runs can set a smaller
per-run limit.
The shared directory must already contain `slot-*` files and be writable.
The harness creates `runs/` for active-run locks and `bases/` for prepared-base
locks inside the directory selected by `--shared-guest-slots`.
Do not remove or replace capacity files while
runs use them.

## The full catalog on Hetzner Cloud servers

`hetzner.py` runs the full catalog on disposable Hetzner Cloud servers instead
of Incus guests, so that many distributions run at once without loading the
test machine. Before starting a cloud run, export
`MMX_TEST_HETZNER_API_TOKEN` and `MMX_TEST_HETZNER_SSH_KEY_NAME`:

```sh
export MMX_TEST_HETZNER_API_TOKEN='replace-with-your-test-project-api-token'
export MMX_TEST_HETZNER_SSH_KEY_NAME='Contributor test key'
```

`MMX_TEST_HETZNER_API_TOKEN` is a Hetzner Cloud API token with read and
write access to the project for MMX's test servers. The harness reads
`MMX_TEST_HETZNER_API_TOKEN`, not `HCLOUD_TOKEN` of Hetzner's `hcloud`
command, so an unrelated `HCLOUD_TOKEN` never creates servers in another
project.
`MMX_TEST_HETZNER_SSH_KEY_NAME` is the exact name of an SSH key already
registered in the project selected by `MMX_TEST_HETZNER_API_TOKEN`.
Replace `replace-with-your-test-project-api-token` with your test project's
API token. Replace `Contributor test key` with your registered SSH key's
name. There is no default key name. An unset or empty setting fails before
any server is created.
Cleanup from a manifest needs `MMX_TEST_HETZNER_API_TOKEN`; it does not look up the key name.

Run the harness with its own test root:

```sh
MMX_TEST_ROOT=$HOME/.cache/mmx-cloud python3 tests/integration/hetzner.py
```

For the ARM64 platform validation before a release, run the whole catalog
with the release build:

```sh
MMX_TEST_ROOT=$HOME/.cache/mmx-cloud python3 tests/integration/hetzner.py \
  --server-architecture arm64 --build-and-use-release-archive 0.1.0
```

Use the intended release version in place of `0.1.0`.
`--build-and-use-release-archive` selects the archive for the controller
machine, independently of `--server-architecture`. Each archive includes
the AMD64 and ARM64 Linux node runner executables. The
[harness section](#the-mmx-harness) explains the shared archive files and
printed checksum.

Hetzner has images of Debian 12 and 13, Ubuntu 24.04 and 26.04, Rocky Linux
9, and AlmaLinux 10, not of Arch Linux or openSUSE Leap. Without options,
`hetzner.py` selects the suites that run on each of those distributions.
Repeat
`--distribution debian13 --distribution ubuntu2604`, `--base-suite`, or `--suite fleet.py` to run fewer; the names are
those of `incus.py`'s distributions and of `all.py`'s suites.

The servers are AMD64 (x86) unless `--server-architecture arm64` asks for
Hetzner's ARM64 servers, each from Hetzner's ARM64 image of its distribution.
Use `--server-architecture arm64` to test ARM64 nodes from an AMD64 test
machine too.
The controller process stays on the machine that runs
`hetzner.py`. With `--server-architecture arm64`, the controller process
installs the ARM64 node runner executable on each server.

Each distribution runs `all.py`'s queue in up to `--pairs-per-distribution`
job slots (default 3). A slot creates two new servers, `mmx-dev` and
`mmx-peer`, with 2 CPUs and 4 GiB: `cx23`, or `cpx22` when Hetzner has sold
out of `cx23`, or on ARM64 `cax11`.

The servers go to `hel1`, or to `fsn1` or `nbg1` when Helsinki has no room.
A location where Hetzner has a larger server type at that moment comes
first, because the facts suite's resize can change a server's type only
within the server's location.

A slot takes suites once the slot's `mmx-dev` and `mmx-peer` servers are
ready. The slot deletes the server pair as soon as the slot's distribution
has no suites left, making room for another slot.

A run holds at most `--max-servers-at-once` servers (default 12).
Choose a value that fits the Hetzner project's available quota.
When the run reaches `--max-servers-at-once` or the project reaches the
project's server quota, slots wait for servers to be deleted instead of
failing. One waiting slot at a time asks Hetzner again.
The run's last line says how many servers the run held at once.

Creating and booting a pair takes a few minutes. So when
`--max-servers-at-once` or the project quota delays server creation, fewer
pairs per distribution finish sooner.

When a suite fails and a slot's servers no longer answer, the slot stops.
The remaining slots take the stopped slot's remaining suites.

Each line of output starts with the slot's distribution.
Each slot's suite logs are in
`<test-root>/<distribution>/slots/<n>/logs/`.
Suites that no slot could run count as failures.

Every server-create request selects the registered SSH key named by
`MMX_TEST_HETZNER_SSH_KEY_NAME` through Hetzner's `ssh_keys` field, so
Hetzner makes no root password. The harness logs in as `mmx-test`, an account
with passwordless sudo. When `~/.ssh/id_rsa.pub` exists, its public key must
match the selected registered key. When `~/.ssh/id_rsa` exists, the harness
uses `~/.ssh/id_rsa` for login. The private key never leaves the machine
running `hetzner.py`. Without `~/.ssh/id_rsa`, the harness generates a key
for the current `hetzner.py` invocation only.
Cloud-init authorizes the current `hetzner.py` invocation's login public
key for `mmx-test`. The registered key is still selected in every
server-create request.
Cleanup removes the generated key after deleting the servers.
Disposable server host keys let SSH verify each server from
the server's first connection. Cloud-init adds only `sudo` and the test marker, with no
MMX endpoint or application configuration.

Each run records its servers and their images, locations, and lifetimes in
`<test-root>/cloud-runs/<run>/ownership.json`. Cleanup runs on success,
failure, and handled interruption, checking that servers and their
auto-deleted primary IPs are gone, and then deletes the run's login key if
the run made one.
`vm.py stop` with the same `MMX_TEST_ROOT` ends a run and deletes its servers.
If the controller machine itself died, repeat cleanup with the manifest:

```sh
python3 tests/integration/hetzner.py --cleanup-from-manifest /absolute/path/to/ownership.json
```

The `node_facts.py` suite's resize moves its server to `cx33`, or `cpx32`
when the server's location has no `cx33`, or on ARM64 to `cax21` or `cax31`,
keeping the disk. The suite then tries to restore the original server type.
If the original server type is sold out, the server keeps the larger type.

The external inventory behind each slot is deliberately small: keys `mmx-dev`
and `mmx-peer`, each containing an ordinary MMX `node` mapping, `architecture`
(`amd64` or `arm64`), `platform`, `image`, and for a cloud server
`hetzner_server_id`, `hetzner_server_type`, and `hetzner_location`.
`MMX_TEST_TARGETS` selects the slot's JSON file. The harness requires
`/etc/mmx-test-vm` with the disposable-test marker before reset.
The harness never provisions or destroys external machines itself.
The provider wrapper provisions and destroys external machines.
Tests use strict SSH and their own loopback HTTPS tunnels.
The Linux-controller check reaches the guest managed by the Linux controller
machine at the supplied address.

Each job slot prints the operating system and kernel of its
test machines, and each suite's outcome. Keep current coverage in
`docs/changing-mmx/testing.md`; private inventories and logs remain under the disposable
test root.

A suite resets MMX state and owns the suite's application fixtures.
Rerun/recovery scenarios retain state deliberately.
A new cloud invocation starts with clean images. Broader
storage faults, additional distributions, and release automation are separate
work, not part of this first feature build.

### The macOS controller check

For the macOS controller check, use a Mac with the required
[Hetzner Cloud settings](#the-full-catalog-on-hetzner-cloud-servers) exported.
Once before each release, run `vm.py test`, `access.py`, `fleet.py`,
`task_secrets.py`, and `dns_tls_and_account_lookups.py` from the Mac.
Running the suites from a Mac puts the controller process on macOS.
Guest suites otherwise run from a Linux test machine.

With `--build-and-use-release-archive VERSION`, the harness uses the archive
for the architecture of the Mac running
`hetzner.py`. The archive includes the Linux node runner executables.
The nodes are new Hetzner Cloud servers, and no VM or container starts on
the Mac:

```sh
MMX_TEST_ROOT=$HOME/.cache/mmx-macos-release-check python3 tests/integration/hetzner.py \
  --distribution debian13 --distribution ubuntu2604 --pairs-per-distribution 1 \
  --build-and-use-release-archive 0.1.0 \
  --suite 'vm.py test' --suite access.py --suite fleet.py --suite task_secrets.py \
  --suite dns_tls_and_account_lookups.py
```

These suites cover what differs on a macOS controller machine: a whole
playbook run, dryrun, and resume (`vm.py test`); SSH keys, passwords, sudo,
and the SSH client's own settings (`access.py`); stopping and resuming a run
(`fleet.py`); the secrets vault (`task_secrets.py`); and public DNS and default
HTTPS trust on the Mac (`dns_tls_and_account_lookups.py`).
`dns_tls_and_account_lookups.py` also checks the node's DNS, HTTPS trust,
account lookup, and installed version.
Use the intended release version in place of `0.1.0`. The example selects
`~/.cache/mmx-macos-release-check` as its test root; choose a fresh directory
for each release check. `--pairs-per-distribution 1` uses one server pair per
distribution. Each `--suite` selects `vm.py test`, `access.py`, `fleet.py`,
`task_secrets.py`, or `dns_tls_and_account_lookups.py`.

## Current platform coverage

The harness supports the native distribution and architecture combinations
listed below. The [validation record](validation.md) distinguishes verified
combinations from combinations not yet verified for the MMX build under
test:

| Distribution | Harness name | ARM64 | AMD64 | systemd, package manager |
|---|---|---|---|---|
| Debian 13 | `debian13` | Hetzner | Incus or Hetzner | systemd 257, apt 3.0 |
| Debian 12 | `debian12` | Hetzner | Incus or Hetzner | systemd 252, apt 2.6 |
| Ubuntu 26.04 LTS | `ubuntu2604` | Hetzner | Incus or Hetzner | systemd 259, apt 3.2 |
| Ubuntu 24.04 LTS | `ubuntu2404` | Hetzner | Incus or Hetzner | systemd 255, apt 2.8 |
| Rocky Linux 9 | `rocky9` | Hetzner | Incus (a VM, SELinux enforcing) or Hetzner | systemd 252, dnf 4.14 |
| AlmaLinux 10 | `almalinux10` | Hetzner | Incus (a VM, SELinux enforcing) or Hetzner | systemd 257, dnf 4.20 |
| Fedora 45, the newest Fedora | `fedora45` | not tested | Incus (a VM, SELinux enforcing) | systemd 262, dnf 5.4.7 |
| Arch Linux, the current rolling release | `archlinux` | not tested | Incus | systemd 262, pacman 7.1 |
| openSUSE Leap 16.1, the newest Leap release | `opensuseleap161` | not tested | Incus | systemd 257, zypper 1.14 |

[`incus.py`](../../tests/integration/incus.py) names each distribution's Incus
image, and [`hetzner.py`](../../tests/integration/hetzner.py) pins each one's
Hetzner image. Each job slot prints the actual operating system and kernel of its test machines; a pinned image does not freeze the
distribution's package repositories.

The suites below observe the following. Of them, only `server_resources.py`,
the suite of `apt_package`, `apt_repository`, and `ufw`, is a tool module's
suite; the others are members of the [base suite](#the-base-suite):

| Suite | Entry point | Required observations |
|---|---|---|
| First server and recovery | `vm.py test` | Setup, resource state, validation, resume with frozen files, worker/controller/HTTPS API process loss, reboot, the reboot task and its lost-controller recovery, retention |
| Server resources | `server_resources.py` | The [security updates example](../../examples/security-updates-on-debian-and-ubuntu/README.md) (daily configuration, active timers, unchanged setup rerun, on-demand dryrun and run); accounts, application-user commands, directories, new unit files, loaded configuration, activation, template/alias drop-ins, one service in two fleet stages, pinned package versions, package sources, whole-node upgrade, ufw rules (with and without a port, one for each node of an inventory group) and SSH safeguard |
| Access | `access.py` | SSH/sudo credentials, verified HTTPS, administrator-key changes, `authorized_keys` onboarding and offboarding, interrupted/repeated setup, replaced server, runner mirror |
| Modules of the project's own | `external_modules.py` | Starlark, Bash, and Go modules, SSH and HTTPS, 18 MiB executable, saved inputs, secrets, failure and stopping, a service and a command that follow a Starlark module, reboot; a Starlark module that runs a tool: a missing program, a server that does not answer, the fixed environment of its commands, and a password that does not hide the result |
| Limited control | `limited_control.py` | Preview/no-op, permissions and repair, fixed restart/result, failure recovery, request during a restart, coalescence, limit/reset, reboot |

`task_secrets.py` covers two-node secret workflows and a failed command that
reads a secret. The failed command's reported reason says that details are
suppressed and names a file on the node. Only root can read the original
failure reason and output in the named file. The suite also covers the five
commands that work on secret keys (`mmx secrets add-random`, `set`,
`list-keys`, `diff-keys`, and `usage`).

`linux_controller.py` checks verified SSH setup, HTTPS apply, and unchanged
rerun from a Linux controller machine against another guest. Run the five
suites in the table, `linux_code.py`, `task_secrets.py`, and
`linux_controller.py` with `incus.py` or `hetzner.py`, one `--suite` for
each, as [the base suite](#the-base-suite) shows. The
[validation record](validation.md) records completed checks for the MMX
build under test.

The full catalog in `all.py` contains 56 suite invocations. The additional
entry points in `all.py` are `subscription_web_service_example.py`,
`owed_restores.py`, `systemd_networkd_files.py`, `netplan_files.py`,
`ifupdown_interfaces_files.py`, `fleet.py`, `deployment.py`,
`account_units.py`, `file_resources.py`, `streamed_assets.py`,
`directory_resources.py`, `directory_copy.py`, `downloads.py`,
`release_directories.py`, `diagnostics.py`, `node_facts.py`,
`node_history.py`, `inspection.py`, `module_helper.py`,
`controller_machine_as_node.py`, `blocked_port.py`,
`access_control_lists_and_capabilities.py`, `ssh_key_pairs.py`,
`wireguard_interfaces.py`, `node_settings.py`, `scheduled_jobs.py`,
`kernel_settings.py`, `storage.py`, `partitions_and_lvm.py`,
`zfs_datasets.py`, `http_requests.py`, `dns_tls_and_account_lookups.py`,
`language_installers.py`, `snap_packages.py`, `docker_containers.py`,
`podman_containers.py`, `postgresql_databases.py`, `mysql_databases.py`,
`mongodb_users.py`, `pg_auto_failover.py`, `pgbackrest.py`, `rabbitmq.py`,
`certbot_certificates.py`, `one_time_playbooks.py`, `incus_instances.py`,
and `libvirt_domains.py`.

`controller_machine_as_node.py` puts the `mmx` program, a project, and an
SSH key of its own for the login account on the first guest. The first guest
acts as a Linux controller machine. The suite runs every `mmx` command on
the first guest as the login account, not as root. The guest manages itself
through SSH to `127.0.0.1` and through the node HTTPS API. The suite checks
a dryrun, a first run, and an unchanged second run.

The suite also kills the controller process while a node job waits. The node
job then finishes, and `mmx resume` retrieves the completed node job's
result. The suite then checks refusal of a fleet
stage ending with a `reboot` task on the first guest. The inventory reaches
the first guest as `127.0.0.1`, by host name, and by network interface
address, in a dryrun and in a run. The node task before the reboot never
ran, the node holds no node job, and the first guest's boot ID remains
unchanged. The fleet stage ending with the `reboot` task patches and reboots
the second guest, which is another machine.

The suite also asks the second guest's node runner executable directly, with
the second guest's boot ID and a different machine ID. A container answers
that the container may reboot, and a server would refuse.

The suite needs two guests that reach each other by address, as the Incus
guests of `incus.py` and the Hetzner Cloud servers of `hetzner.py` do. On
one distribution, `controller_machine_as_node.py` takes about 25 seconds, of
which the second guest's reboot takes 6. What a Docker container reads of
the host server's boot ID is not part of the suite. The
[design](design/execution.md#mmx-never-reboots-the-controller-machine)
records the measurement.

`account_units.py` covers accounts, including accounts for people. The suite
also covers group members and `authorized_keys` with key files.
While `chpasswd` runs, the suite
searches every process command line for the human account's password hash.
The suite also covers every unit kind `systemd_unit` installs: a service
with a drop-in, a timer, a socket, a mount, a target with a member service
in a slice, an automount, and a swap file.

`account_units.py` starts units that print one line and fail. With
`retry: true`, the suite reads the failure reasons from three node task
attempts. Each reason contains the line from the corresponding attempt once,
for a unit that restarts automatically after failure. Without `retry`,
the node task tries once, which the suite counts on the node. In the
required-unit case, the failure reason contains the line from the failed
unit that the requested unit needs.

A separate case checks a unit in an account's own systemd. A child program
started by the unit's command prints a line. The failure reason contains the
line under the unit's heading or the heading for the account's lines without
a unit.

`account_units.py` also covers `state: do_not_start_or_stop` and `masked` on
`systemd_service`, and the failure for a oneshot unit asked to be started.
The suite also covers `run_only_once_marker_file` on `shell`, `wait_for` with
`command_succeeds`, a command with `only_when_tracked_inputs_changed`, and
`checks_before_restart_or_reload`. Another case checks a unit in an
account's own systemd (`in_systemd_of_user_account`). The account checks the
new unit file, with a link in the account's unit directory to a file that
only root can read.

`release_directories.py` covers a Git checkout as a tracked input of a
command with `only_when_tracked_inputs_changed`, and
`files_changed_in_checkout_on_node`.

`http_requests.py` runs a small account API in Python on the node and checks
what it received: a `POST` sent only after a `GET` found no account, never
during a dryrun; a `PUT` with a secret header whose `changed_when` reads the
answer; a `DELETE`; HTTPS with a self-signed certificate in
`trusted_certificate_authority_file`; and the refusal of a write in
`checks_before_restart_or_reload`.

`language_installers.py` installs Python, Ruby, Node.js, npm, Bundler, and
PHP from the distribution's packages, and Composer from a pinned download.
`language_installers.py` covers `pip_package` in a virtual environment the
suite creates as the application's account, with a requirements file and as a tracked
input that runs a command once per new version; `gem_package` with an exact
version; `npm_global_package` with Yarn 1; and the five lock file modules
against lock files the suite makes on the node, as a repository would carry
them. The Python package manager uv comes from its pinned release archive.
The Composer project takes its packages from a path
repository, so it needs no download. `snap_packages.py` installs, switches,
and removes the `hello-world` snap on Debian and Ubuntu, in a VM.

`docker_containers.py` installs Docker with MMX's own tasks: from Docker's
package repository on Debian 12 and Ubuntu 24.04 (the docker-host example)
and from the distribution's own packages on Debian 13 (`docker.io` with
`docker-compose` 2.26.1) and Ubuntu 26.04 (`docker.io` with
`docker-compose-v2` 2.40). Debian 13's Compose is the oldest that MMX
supports.

The suite therefore runs with three Compose release series: 2.26, 2.40, and
the current version 5. The Compose release series print their dry run in two
forms and differ in what happens to a service that depends on a changed
service.

Debian 13's Docker 26 keeps images in the classic image store, and the
Docker 29 of Docker's repository keeps them in the containerd image store.
Ubuntu 26.04's own packages have Docker 29 too; the suite reads which image
store the node's Docker uses. The images come from the
[test image registry](#the-mmx-harness), never from Docker Hub.

The suite covers `docker_image` with a moved tag and both values of
`pull_image`, and a private repository. A separate case checks a container
that systemd runs and restarts once for a new image ID, also after an
interrupted run.

The suite covers `docker_compose_project` in every state.
The Compose cases include a changed
definition, a moved tag, a service removed from the Compose file, an
interruption before container updates, and `registry_logins`. For a project
with an environment file, the suite first runs `docker compose up` by hand.
The first dryrun predicts no recreation, and the run keeps the containers.
Each later dryrun predicts what the node's Compose version does in the run.

A second project directory has the first directory's basename. The task
fails and leaves the first project's container and volume alone. With
`compose_files` and `compose_project_name`, the second project then runs
beside the first project. Separate removal cases cover a project whose
Compose file is gone or cannot be loaded. The removal results name the
volumes.

The suite publishes a reference listing images for two platforms through the
test image registry's functions. The image list changes while the image for
the node's platform stays unchanged. The suite also covers
both Docker forms of `wait_for`, including a project with a service that
ends by itself. The deploy-with-docker-compose example checks the mounted
configuration file, the backup, and both removals.

`docker_containers.py` checks the node with `docker inspect`,
`docker image inspect`, `docker volume ls`, and the containers' own HTTP
answers. The suite reads the registry's request log to prove that no dryrun
fetched an image layer. The suite searches mmx's output, saved runs, node
job records, the journal, and every process command line for the registry
password. The search checks the plain password and Docker's stored login
form. At the end, `docker_containers.py` removes the containers, images,
volumes, Docker packages and repository, `docker0` bridge, and Docker
firewall chains. The next suite therefore finds the guest without Docker.

`podman_containers.py` starts the test image registry of `image_registry.py`
on its guest, installs Podman and skopeo from the distribution's own
packages, and checks the node with `podman`, `systemctl`, `stat`, and the
answers of the container's own program over HTTP. On Debian and Ubuntu,
`podman_containers.py` names the packages that Podman's package only
recommends, as `examples/podman-host` does: a server whose apt installs no
recommended packages, such as Hetzner's Debian images, has no program for
pods or for the containers of an ordinary account otherwise.

`podman_containers.py` prints the node's apt setting, the file setting the
apt value, and the packages installed by the suite.

`podman_containers.py` covers `podman_image` with a tag that moves (both
values of `pull_image`, and a dryrun that fetches no image layer, by the
registry's request log), a registry that does not answer, and a private
repository whose password `podman_containers.py` looks for on every process
command line while the pull runs and afterwards in mmx's output, the saved
runs, the node's records, the journal, the home directories, and Podman's
own login files, as it is and as Podman stores a login.
`podman_containers.py` pulls the private image into an ordinary account's
store too, where the login file belongs to the account during the pull. The
suite covers `podman_quadlet` with a file from a template and a file
rejected by Podman's generator. Another case provides an unreadable file
beside a good installed file with the unreadable file's name. The suite also
covers an unchanged file with a pending systemd reload. The suite checks
each row of the design's table of what restarts a Podman container, and
each row again after a run stopped between the change and the restart.

Other cases cover health-wait failures and `start_at_boot` on a generated
service, checked on the node and at project load. The suite checks a service
from an init script on Debian and Ubuntu, and a one-container pod where
Podman is 5 or newer. The suite covers both removal states, including a
volume still used by a
running container and services that stop together.

`podman_containers.py` also deploys the template-defined container for an
ordinary account created in the container's deployment run, with a link in
the account's Quadlet directory to a file that only root can read, and runs
the example projects `podman-host` and `deploy-with-podman-quadlet` with the
test image, among them the two removal playbooks one after the other. On
Debian 12, whose Podman 4.3 has no Quadlet, `podman_containers.py` checks
the failure naming the installed Podman version and the minimum version
required for Quadlet, and the image tasks and the health wait, which work
there. At the end, `podman_containers.py` removes Podman and everything
created by the suite. The suite checks restoration of the guest's original
state: no Podman program, network device, or firewall table, no
suite-created files, units, or accounts, and systemd running.

`postgresql_databases.py` installs the distribution's own PostgreSQL server
once, with the example project `postgresql-server-for-one-application`, so
the suite runs against version 15 on Debian 12, 16 on Ubuntu 24.04,
17 on Debian 13, and 18 on Ubuntu 26.04.

`postgresql_databases.py` runs in a container on every distribution. Before
installation, the suite checks that `postgresql_role` and
`postgresql_database` name the missing `psql` program. The example's dryrun
reports the two database tasks not checked. `postgresql_databases.py` looks
at the server with `psql` itself, as the server's administrator and never
through MMX: the roles and their attributes, the stored form of each
password, each record of a membership, the databases, and their extensions.

`postgresql_databases.py` covers `postgresql_role` with a name full of
quotes. Sixteen password cases exercise the edges of PostgreSQL's
preparation before hashing (SASLprep). Each case proves two directions: a
role with an MMX-set password logs in over TCP only with the correct
password, and MMX leaves a server-hashed password unchanged. Separate cases
check an md5-stored password and the server's iteration count on PostgreSQL
16 and newer.

Role cases also cover changed and omitted attributes. Membership cases
include a second grantor and a record permitting only administration of
another role, stored as separate records from PostgreSQL 16. Other cases
check a missing role in `member_of_roles`, including `pg_read_all_data` on
PostgreSQL 13, and a name longer than the server accepts. A reserved-name
case checks the server's message in the result of a task with a secret
password.

For connection fields, the suite first logs in over TCP as a superuser with
a password. A wrapper before `psql` records that root runs `psql`, the login
role appears in the `psql` environment and on no command line, and the
environment contains only MMX-set values. The wrapper also checks that
only root can access the
password file and the password file's directory. The suite signs the
server's certificate with a test certificate authority. TLS cases check the
signing authority's file, a different authority's file, and a host name
absent from the certificate.

A local-socket case logs in with a password as the account `postgres`, with
a secret login role. Another case omits the login password while `postgres`
has a home-directory password file that `psql` alone would use. The task
does not log in using the home-directory password file. Other connection
cases use a login role without superuser rights, a port with no server
answering, and a stopped server.

While a run sets a password, a sampler reads the command line and the
environment of every `psql` process. The suite then searches mmx's output,
the saved runs, the node's records, the journal, and the home directories
for each password, for a login password as the password file holds it, and
for the stored forms that it read from the server. `postgresql_databases.py`
also covers `postgresql_database` with a name full of quotes and a locale
spelled differently from the template database's locale. Another encoding and locale
without `template0` fail in a run and dryrun. Separate cases use
`template0`, a missing template database, and an existing database with a
different encoding.

Owner cases cover a changed owner and a missing owner. Extension cases
install and remove extensions while `plpgsql` stays, request an extension
whose files are missing, and try removing an extension needed by another
extension. In an interruption case, the wrapper ends `psql` after
`CREATE DATABASE` and before extension installation. The suite also checks
the SQL file of privileges that [Manage a database](../how-to/manage-a-database.md) shows.

Removal cases check refusal to remove a role still owning a database and
refusal to remove a database with a connected session. The server waits five
seconds before refusing the database removal. Its last check is a stopped
server, which the suite ends at once, as a power failure would: an ordinary
stop first writes every changed page to disk, which took more than twelve
seconds on a Debian 13 guest after the suite's databases were made, and the
server is not started again. At its end the suite removes the server, its
data directory, every package that came with it, and the account and the
groups that the packages made, and checks that the guest's packages,
accounts, and groups are those that it found.

`certbot_certificates.py` gets real certificates with the distribution's own
certbot from a certificate authority on its guest, and never from a public
one. The authority is Pebble, Let's Encrypt's small test server for ACME,
the protocol certbot uses to request certificates: the suite fetches
Pebble's release archive once for a test run, checks it against a pinned
SHA-256, and starts two Pebbles on the guest, one that issues certificates
for 90 days and one for half a minute. Pebble's HTTPS address has a
certificate from an authority that the suite makes and puts into the guest's
trust store, so certbot needs no option for it. A BIND on the guest, which
the suite starts on the DNS port 53 at `127.0.0.1` only, answers every name
below `example.com` with the guest and takes signed updates.
Certbot's `rfc2136` plugin handles the `dns` challenge. A small web server
serves the files of the `webroot` challenge.

Each task explicitly chooses its certificate authority. Certbot's own
`renew --dry-run` asks Let's Encrypt's test server whatever a certificate's
settings say. So the suite guards the guest in three ways.

Before certbot is installed, `certbot_certificates.py` maps
`acme-v02.api.letsencrypt.org` and `acme-staging-v02.api.letsencrypt.org` to
the guest in `/etc/hosts` and checks with `getent` that both resolve nowhere
else. Every task names `certificate_authority_url`; a case that explicitly
chooses Let's Encrypt proves the guard by the connection error that names
its address. And at its end the suite lists every host that certbot
contacted, from certbot's log, and fails when one of them does not resolve
to the guest alone.

The suite itself never runs `certbot renew --dry-run`.

The suite covers a node without certbot and the example project
`certificate-with-certbot`. The example chooses the package source and timer
name from the node's facts. Challenge cases cover `webroot`, `standalone`,
and `dns` with a wildcard name. Another case changes the list of names.

Each changed setting is proved by a renewal that can pass only with the new
value. The web server moves to the newly configured webroot directory, and
each script records the script's own runs.

Other cases change the authority and renew through the distribution's
packaged service. The suite starts the service until the half-minute
certificate is replaced, because certbot versions decide differently when
renewal is due. Failure cases cover an authority-refused name, a failed
challenge, `retry`, and a renewal file with a line certbot cannot read.

A real second certbot holds certbot's lock until the task examines the lock,
then the suite releases the lock. The suite watches the lock file with
inotify, so the check does not depend on server speed. A dryrun case
continues when an earlier node task of the dryrun would install the required
script. The suite also covers `state: absent`.

`certbot_certificates.py` checks the node with `openssl`, certbot's own
files, systemd's records, and the scripts' notes, and proves "no certbot
started" by an unchanged listing of certbot's log directory. On Debian 12,
whose certbot keeps copies of keys outside a certificate's directories,
`certbot_certificates.py` checks that `state: absent` removes exactly the
deleted certificate's copies. The suite's own files on the guest are outside
`/tmp` and `/var/tmp`: Debian's and Ubuntu's renewal service has
`PrivateTmp=true`, so a script that certbot runs in the renewal service sees
private, empty `/tmp` and `/var/tmp` directories. At the end, the suite
removes certbot, BIND, the suite's stand-ins, the
authority from the trust store, and the two lines in `/etc/hosts`. It then
checks that the guest is as it found it, with exactly the packages that it
had before: a package that came with certbot can stay after certbot's
removal (`python3-openssl` on Debian 13), so the suite removes whatever the
guest did not have. `certbot_certificates.py` runs in a container on every
distribution.

`mysql_databases.py` tests `mysql_user` and `mysql_database` against real
MariaDB and MySQL servers at once. `mysql_databases.py` installs MariaDB on
the pair's first guest with the example project
`mariadb-server-for-one-application`, and, where the distribution packages
MySQL, MySQL on the second guest with the tasks that the
[`mysql_database` page](../reference/modules/mysql_database.md#the-same-tasks-for-a-mysql-server)
shows.

The two products cannot share a guest: MariaDB and MySQL packages conflict,
and MariaDB and MySQL use `/var/lib/mysql`. Ubuntu 26.04 has MySQL 8.4, and
Ubuntu 24.04 has MySQL 8.0. Debian packages no MySQL, so the suite covers
only MariaDB there.

Each node task list runs on the MariaDB and MySQL servers in one fleet
stage, so one `mmx` call tests MariaDB and MySQL, and each product is
installed once. The installations are where the suite's time goes.

The suite checks the servers with their own client: the catalog's rows, each
account's stored password, and logins with the clear passwords over TCP.
`mysql_databases.py` covers a first run, an unchanged rerun and dryrun, and
names that need quoting. An account created by the server from a clear
password must remain unchanged. MMX therefore computes the stored password
value using the server's salt. Another case finishes a state prepared by
hand.

The suite checks `privileges_not_listed` with both values at every kind of
place. A case grants `ALL`,
then revokes every privilege by name. Another case removes the grant option
on `*.*` with one statement while keeping every privilege. MySQL's catalog
still shows the option, but the grant table does not, and the next run finds
nothing to change.

New-account cases omit a password with and without
`allow_creating_account_without_password`. The suite also checks failures.
Other cases request
an unknown character set and a collation from another character set.
MariaDB's short collation `uca1400_ai_ci` is stored as
`utf8mb4_uca1400_ai_ci` and remains unchanged on a second run on MariaDB
10.11 and later. On older MariaDB and on MySQL, the task fails before
anything changes.

A failing SQL statement contains a stored password value. The suite also
checks MariaDB's password check plugin. Login cases use TCP and the local
socket, with a password requiring quoting in the login file. A refused login
gets one attempt, while a port with nothing listening gets three. Other
cases cover a server with a moved socket, a stopped server, and a database
task in a node task block.

The database-drop case holds an open transaction in another session after
reading a table. The drop task fails after 10 seconds with server error
1205, and the database remains unchanged. The database-drop case
intentionally costs 10 seconds because the suite tests the wait.

For `database_server_certificate_authority_file`, `mysql_databases.py` makes
two certificate authorities with `openssl` and gives the MySQL server (the
MariaDB server where there is no MySQL) a certificate of the first.

Each node runs the tasks with its own client: MariaDB's client on the
MariaDB node and, where there is a MySQL server, MySQL's client on the MySQL
node, which logs in to its own server through the address in the
certificate. A task with the first authority's file logs in, and one with
the second's is refused by the client. A task without the field shows the
client's own rule: MariaDB's client from 11.8 refuses MySQL, and MySQL's
client checks nothing.

Then the server gets a certificate of the first authority for another
address, and MariaDB's and MySQL's clients refuse the certificate with error
2026.

A wrapper around the client program notes the command line and the
environment of every call, and root has an option file that would send a
plain call of the client elsewhere.
At the end the suite searches what mmx printed, the saved runs, the node's
job records, its journal, its home directories, and the wrapper's notes for
the passwords, plain and as each server stores them, whole and in part.
`mysql_databases.py` then removes exactly the packages added by the suite's
runs, the data and logs left by the packages, and the database server
account. The suite checks restoration of the guests' original state.

`mongodb_users.py` tests `mongodb_user`, an official extra module, against a
real MongoDB server. No distribution packages MongoDB, so the suite installs
the server and MongoDB's shell `mongosh` once, from MongoDB's own package
repository, with the example project `mongodb-server-for-one-application`.
The module's page shows the `mongodb-server-for-one-application` project's
playbook under "Set up", word for word, which a Go test checks. MongoDB
serves packages for all four distributions, and the whole test runs on each
of them, with MongoDB 9.0; the suite writes the series and its signing key
into the example's inventory group, where the example keeps them. The suite
runs in a container on every distribution.

The suite looks at the server with `mongosh` itself and never through MMX:
every user with its roles as the server lists them, a login with each
password and with a wrong one, and a write as the application's user. The
suite's own calls of `mongosh` run with `HOME=/dev/null`, as the module's
calls do, so `mongosh` leaves no log of the supplied JavaScript.
`mongodb_users.py` covers the example's first dryrun on a node without
MongoDB. The administrator-creation and application-user tasks wait for
`mongosh`. The module creates the first administrator without a login. The
suite then checks an unchanged rerun and dryrun, and a changed password.

User cases cover names full of quotes, users without roles, and users
created by a login user in a database other than `admin`. A two-task case
targets one user, with the second task omitting a password. The second task
is not checked in the dryrun and grants a role in the run. A state prepared
by hand is changed with `roles_not_listed: keep`, with `remove`, and with an
empty list.

Failure cases cover a missing role and a port with nothing listening.
The suite checks each input that the module refuses, including removal with
a password and invalid database names: a dot, a space, a slash, and a
64-byte name, each in another input. A wrong login password fails in a
dryrun and in a run. A secret login user's
name appears as `[redacted]` in the reason. Another case uses a login user
without permission to read users.

A failure that another try cannot fix has one try, in a dryrun as in a run,
which the suite counts: a refused login, a command that the server refuses
to the login user, and a replica set whose primary is another server. A port
where nothing listens has three. For a run that stops between two changes,
`mongodb_users.py` makes a login user with a role of its own that may set
passwords and may not grant: the task sets the password, the server refuses
the grant, the result says both, and the next run as the administrator only
grants. For a creation that ends without its result, the suite's wrapper
before `mongosh` lets a changing `mongosh` invocation create the user and
then returns exit code 1 without the invocation's result: the task fails
after one try, says that the server has the user, and reports
`created: true`, and the next run is unchanged. `mongodb_users.py` also
changes the administrator's own password, with the old one as the login
password.

The last part runs on a replica set with access control and a key file, as a
production server has it, from an empty data directory. On a member that was
never initiated, the replica set has no primary: a dryrun is not checked,
and a run fails after three tries. On the empty primary a dryrun would
create the first administrator and leaves every other task not checked, and
a first user that could not manage users is refused.

The suite then starts a second `mongod` by hand, on port 27018 and without a
vote: a task that reaches this secondary names the primary, and with
`replicaSet=rs0` in the connection string the same task reaches the primary.
`mongodb_users.py` ends the second member by process ID.

With access control off, tasks without a login create a user and remove the
administrator. With access control on again, the server has users only
outside `admin`: MongoDB 8.2 and newer still let the first administrator in
without a login, and MongoDB 8.0 asks for a login that nobody has, so the
task fails with the refused login. The suite checks the behavior of the
MongoDB version installed by `mongodb_users.py`.

A wrapper before `mongosh` notes the command line and the environment of
every call that MMX makes: each is the version question or the fixed
command line with `HOME=/dev/null`. At
the end the suite searches what mmx printed, the saved runs, the node's job
records, its journal, the server's log, `/tmp`, and the home directories for
every password, plain and as JSON writes it, and checks that root has no
directory `.mongodb`. `mongodb_users.py` then removes exactly the packages
added by the suite's runs, MongoDB's repository and key, and the server's
data, log, and account. The suite also removes suite-created files and
checks restoration of the guest's original state.

On Debian 13 the suite takes two to four minutes, by how busy the test
server is, of which the installation takes 14 to 60 seconds. The rest is
`mongosh`: a `mongodb_user` task starts it two or three times (the version
question, the read, and the change), at 0.1 to 0.4 seconds each on a quiet
server, and the suite runs about 50 `mmx` commands with one to seven such
tasks.

`pg_auto_failover.py` tests `pg_auto_failover_monitor`,
`pg_auto_failover_node`, and `pg_auto_failover_primary` on three guests: the
monitor on `mmx-third`, the database server `db01` on `mmx-dev`, and `db02`
on `mmx-peer`. `pg_auto_failover.py` runs on all four distributions with
distribution packages, pg_auto_failover 2.0 to 2.2 with PostgreSQL 15 to 18,
in containers.

The guests share one network. pg_autoctl permits connections from the shared
subnet as `autoctl_node`, the monitor's login role. A cloud server with a
public `/32` address shares no subnet with the guests, and `pg_autoctl
create postgres` would retry the refused login without end. So
`pg_auto_failover.py` first compares the monitor's subnet with the nodes'
addresses and, on such servers, fails at once with that reason, before it
changes a test machine. When a full run moves a distribution to cloud
servers, `pg_auto_failover.py` fails on the cloud servers and runs again on
the Incus guests.

`pg_auto_failover.py` first checks each module without `pg_autoctl`, then
installs the packages with the tasks of the monitor's page, "Set up", which
keep Debian's tools from creating a cluster. A dryrun before `db01` exists,
without the packages and with them, reports the `set_vars` task that reads
its `is_primary`, and the task whose `when` reads that variable,
`not_checked`.

`pg_auto_failover.py` reads the formation directly with `pg_autoctl show
state`, `psql`, and `systemctl`, not through MMX. A formation is a group of PostgreSQL servers
managed by pg_auto_failover.

The first-run checks cover the monitor, primary `db01`, and replica `db02`,
which copies `db01` and joins with synchronous replication. The suite checks
`synchronous_standby_names` and `pg_stat_replication`. Other checks cover
TLS logins with each password and a wrong password, and an unchanged rerun
and dryrun.

On each guest, initdb's six `pg_hba.conf` lines use `peer` and
`scram-sha-256` instead of `trust`, while pg_autoctl's own lines stay. The
monitor's `peer` line uses the `pg_autoctl` ident map. The account `nobody`
cannot log in as `postgres` through the socket or `127.0.0.1`. The suite
checks with `ss` that each instance listens only at the guest's address.

A `listen_address` change to `*` succeeds on the monitor and `db02`, then is
changed back, but primary `db01` refuses the change. A restored trust line
in `db02`'s `pg_hba.conf` permits `nobody` to log in until the next run
replaces the line. A monitor with a missing unit is completed by the next
run. Password changes reach the monitor, the primary, and both
`~postgres/.pgpass` files on `db01` and `db02`.

The suite checks failover settings and a switchover to `db02` and back that
preserves every row. Maintenance on the replica permits continued writes on
the primary. A reboot during maintenance leaves the replica's PostgreSQL
stopped until `present` ends maintenance. Maintenance on the primary is
refused. After the first run, switchover, and maintenance end, the run
returns with a settled formation.

Restarting primary `db02`'s `pgautofailover` makes the monitor fail over. A
`db01` node task started during failover waits for the formation to settle
and reports `db01` as primary. With `postgresql_settings` and restarts
set to `never` on both nodes, `work_mem` comes from the module's file in
`pg_settings`. The new `shared_buffers` value waits for a PostgreSQL restart.

With `on_a_replica` on both nodes, `db01` and `db02`, the task restarts
`db02` through maintenance and waits for the
formation to settle. Primary `db01` refuses the restart. A value refused by
`postgres -C` leaves the module's PostgreSQL settings file unchanged.
`ALTER SYSTEM SET work_mem` on `db02` fails the task with a reason naming
`postgresql.auto.conf` until `ALTER SYSTEM RESET`. Before the module's
include exists, a dryrun recognizes `131072kB` as initdb's `128MB` and
reports no setting waiting for a restart.

The suite checks `maintenance_work_mem` as a share of `db02`'s memory
against the guest's `MemTotal`. A broken `db02` case stops the service,
writes an invalid PostgreSQL value into the module's settings file, and
starts the service. The `db01` task continues beside the broken `db02`. The
`db02` node task repairs the invalid value before waiting for PostgreSQL. A
`max_connections` value on `db02` below the primary's value is refused
before the settings file changes.

In another failover case, `db01`'s service stops and the monitor promotes
`db02`. The monitor's task reports `db02` as primary while `db01` still
reports `primary`, then `db01` rejoins as a replica. The suite rebuilds an
empty monitor at the original monitor address. A dryrun of replica `db01`
alone names `db02`'s address as primary. Primary `db02` registers first,
then replica `db01`, preserving every row.

A stopped `db02` is omitted from the monitor's list and dropped by the task.
Dropping a healthy primary is refused. With `db01` alone in the formation,
`on_a_replica` refuses a restart for changed `shared_buffers` and restores
the settings file. `on_a_replica_or_a_node_alone` restarts PostgreSQL in
place. If an invalid value in the module's file prevents PostgreSQL startup,
the `db01` task writes the corrected value immediately.

Removal cases check `absent` on a node dropped by the monitor and on the
last node, keeping the module's settings file. The suite also checks
`deleted_with_all_its_data` on `db01` and `db02`, removing the settings
file, and on the monitor. At the end, `pg_auto_failover.py` searches what
mmx printed, the saved runs, the job records, the journal, the logs, `/etc`,
and the data directories on every guest for the passwords, which may be only
in `~postgres/.pgpass`, and removes the packages added by
`pg_auto_failover.py` and everything they leave. The first run, with the
installation and a join, takes about three and a half minutes; the whole
suite five to six. The guests sit on one bridge, so the monitor's
`host_name` lies in the subnet that pg_autoctl lets the nodes log in from;
on Hetzner Cloud servers without a private network the nodes' logins would
fail.

`pgbackrest.py` tests `pgbackrest_stanza` and `pgbackrest_restore` on three
guests: a pg_auto_failover node `db01` on `mmx-dev`, the new database server
that receives the restored backup of `db01` on `mmx-peer`, and the monitor
on `mmx-third`.

On Debian 13 and Ubuntu 26.04 the repository is an S3-compatible bucket that
keeps versions: the distribution's `python3-moto` server, in memory, behind
nginx, which gives it TLS on port 9000, on `mmx-third`. Debian 12 and Ubuntu
24.04 do not package `python3-moto`; on Debian 12 and Ubuntu 24.04 the
repository is a directory on `mmx-dev`, which the suite copies to `mmx-peer`
before each restore there.

The suite checks `pgbackrest_stanza` and `pgbackrest_restore` without
`pgbackrest`. Then the suite enables archiving with `line` tasks and a
restart. The suite creates the stanza (an
unchanged rerun runs no `check`) and manually starts the scheduled backup
job from the stanza's page.

`pgbackrest.py` writes rows before and after a time and archives the rows with
`pgbackrest check`. The suite then loses the database server by hand:
the service, data directory, pg_autoctl's files, and password file go.
The monitor's task drops the unhealthy
primary.

The restore goes to the time into `mmx-dev`'s emptied data directory, with
the rows written before the selected recovery time, `archive_mode` off, and
no TCP socket on port 5432 (`ss`), and a rerun is unchanged; a rerun to the
end of the archive fails once, in a dryrun as in a run, and names both
targets; a directory with a file is refused, in a dryrun as in a run.

The restore of the whole archive goes to `mmx-peer`, whose restored
`postgresql-auto-failover.conf` names `mmx-dev`'s address: with the bucket,
the suite first deletes the backup from it as an attacker would, so a
restore without `read_repository_as_of` finds no backup and one that reads
the bucket as it stood before restores every row; without the bucket,
`read_repository_as_of` is a missing prerequisite that names the version,
and a restore of
the whole archive restores every row. The restored cluster listens on no TCP
socket until `pg_auto_failover_node` registers it as the primary of the
formation, which has no node, at `mmx-peer`'s own `listen_address`, which
`ss` shows alone, and replaces a trust line that the suite put into its
`pg_hba.conf`.

Then both database servers get a cluster `shop` of Debian's own tools on
port 5433, with its configuration in `/etc/postgresql`: `mmx-dev`'s is
archived with `line` tasks, gets its stanza and a backup, and `mmx-peer`'s
is stopped, emptied, and restored by the node tasks of
`pgbackrest_restore`'s page, with the service `postgresql@<version>-shop`
starting it.

The suite searches for the bucket's secret and the passwords as
`pg_auto_failover.py` does, and leaves the guests as it found them.

`rabbitmq.py` tests the four RabbitMQ modules, `rabbitmq_virtual_host`,
`rabbitmq_user`, `rabbitmq_policy`, and `rabbitmq_plugin`, against a real
RabbitMQ server on its guest. On every distribution, `rabbitmq.py` first
checks the modules without RabbitMQ's programs: each names the program that
it misses and the package, a dryrun is not checked, and a run fails once as
a missing prerequisite. The whole test then runs on all four distributions.
The example project `rabbitmq-server-for-one-application` installs the
server on the suite's guest, once: from the distribution's own package on
Debian 12 (RabbitMQ 3.10) and 13 (4.0) and Ubuntu 24.04 (3.12) and 26.04
(4.0). Each module's page shows, under "Set up", the tasks of the example's
playbook that install and start RabbitMQ, word for word, which a Go test
checks. The suite runs in a container on every distribution.

The suite reads the server with RabbitMQ's own programs and never through
MMX: `rabbitmqctl export_definitions` gives the virtual hosts, users,
permissions, and policies in one call, and `rabbitmq-plugins list` and the
file of enabled plugins give the plugins. `rabbitmq.py` checks each password
against the hash that the server stores: four salt bytes, and the SHA-256 of
the salt and the password. The names, the regular expressions, the password,
and the policy definition of `rabbitmq.py`'s tasks hold a `$` name, a
command in backquotes, quotes, and a backslash, which the `rabbitmqctl`
command of Debian and Ubuntu would change. The example-project checks start
with a dryrun before installation, then a run and an unchanged rerun.
Changing the password makes the dryrun announce only the password change,
and the run changes only the password.

The subsequent cases start with a dryrun before the first run. Tasks
requiring the new virtual host are not checked. An unchanged rerun follows.
Other cases change the password, tags and permissions with `remove` and
`keep`, and every part of a policy. A manually created user keeps the
existing password, an unlisted tag, and an unlisted permission.

A refused permission regular expression fails the run once after user
creation. The next run sets only the permissions. Missing-prerequisite cases
cover a permission and policy for a missing virtual host, a missing plugin,
and a task without a password for a missing user. Each leaves a dryrun not
checked. Another case uses a password that RabbitMQ would cut.

For `apply_to`, RabbitMQ versions before 3.12 refuse `classic_queues`, while
versions from 3.12 accept `classic_queues`. The suite checks the guest's
RabbitMQ response and also tries a policy key unknown to RabbitMQ. Disabling
a plugin also disables dependent plugins, while a plugin enabled explicitly
by name stays enabled.

With the server stopped, `rabbitmq_plugin` works. `rabbitmq_virtual_host`,
`rabbitmq_user`, and `rabbitmq_policy` fail. The virtual host task fails
after three tries. After the server starts, the plugin runs.
The suite also checks every removal.

`rabbitmq.py` searches mmx's output, the saved runs, the node's job records,
its journal, its home directories, and the server's logs for the passwords.
At the end, `rabbitmq.py` removes RabbitMQ, Erlang, their data, their
accounts, and the `epmd` process that Erlang leaves
where no unit owns it, and checks that the guest's packages, accounts,
units, and processes are those that it found.

On a Debian 13 guest the suite takes about four and a half minutes while
other test runs keep the server busy. The installation takes about a minute
of the Debian run, and the removal of the packages at the end of the Debian
run takes 20 seconds. The rest is RabbitMQ's own programs: each call starts
an Erlang runtime and takes half a second to a second, an unchanged user
task makes four calls, and `rabbitmq-plugins disable` takes about seven
seconds whatever it changes. The suite therefore reads the server's whole
state with one call for each check, repeats an unchanged run only where a
new kind of task needs the proof, and disables a plugin twice.

`incus_instances.py` tests the official extra modules `incus_instance` and
`incus_profile` against a real Incus server. Two different Incus servers are
involved, and the suite touches only one: `incus.py` makes the suite's guest
with the Incus of the test server, and the suite installs a second Incus
inside that guest, with the example project `incus-host`, and manages only
that one. The guest is a virtual machine on every distribution.

| Distribution | What the suite runs | Incus |
|---|---|---|
| Debian 12 and 13, Ubuntu 24.04 and 26.04 | The whole test, with containers | 6.0.0 to 6.23 |

The suite never installs the package `incus-agent` of Debian or Ubuntu.
Every VM guest of the harness has a unit file
`/usr/lib/systemd/system/incus-agent.service`, through which the test server
reaches the guest. Installing `incus-agent` replaces the unit file, and
removing `incus-agent` deletes the unit file. So the example installs
`incus-base` on Debian, and before the installation the suite writes, with a
`file` task, the apt setting that installs no recommended packages, which on
Ubuntu 24.04 keeps `incus-agent` away. The apt setting disabling recommended
packages proves on every apt distribution that the example names each
package that Incus needs. At its end the suite checks that the guest's unit
file has the SHA-256 it had.

The instances run the harness's
[`container_test_program`](../../tests/integration/container_test_program/main.go)
as `/sbin/init`, from an image that the suite builds on the controller
machine and imports: no test fetches an image from a public server. The
program ends on SIGTERM, and Incus asks a container to shut down with
SIGPWR, so the instances that must shut down set
`raw.lxc: lxc.signal.halt=SIGTERM`. One nested test container without
`raw.lxc: lxc.signal.halt=SIGTERM` ignores a request to shut down: with
`wait_for_shutdown_at_most: 3s` the task fails after those 3 seconds, which
the check tests on purpose, and with
`power_off_when_shutdown_takes_longer: true` the task stops the instance.
The module prevents retries after a shutdown timeout, so the shutdown wait
runs once rather than three times.

The suite checks the node with the guest's own `incus` (`incus list`,
`incus profile list`, `incus config get`) and with an HTTP request through
an instance's proxy device, whose answer holds the program's start time: an
unchanged start time proves that no task restarted the instance.
`incus_instances.py` covers the example's first dryrun on a node without
Incus, and a first run with a preceding dryrun and an unchanged
rerun. Preservation cases use `keep` and `remove` beside a manually added
key, device, and description. Other cases check a configuration key taking
effect at the next start and each instance state. Frozen and ephemeral
instances are checked, including deletion.

Failure cases check Incus's messages and an image missing from the node's
Incus server. Recovery cases complete a created instance that never started
and handle a profile that Incus saved but reported as failed. Project cases
use Incus projects with and without project-specific profiles.
Client-setting cases change root's default project and default server. Other
cases cover a stopped Incus server, a secret configuration value, and
removal.

No task of the suite sets `max_tries`, so each failure also proves how often
MMX tries it by default: once where the module returns invalid inputs, a
missing prerequisite, or `try_again: False`, and three times for a command
that Incus refused and that left nothing behind.

`incus_instances.py` stops the Incus server while the server has no instance
yet. The installation and the removal of Incus are where
`incus_instances.py` spends most of the suite's time. At its end the suite
removes everything inside Incus while the server still runs, then the units,
exactly the packages that its runs added, and what no package removal takes:
Incus's directories, its groups, the lines for root in `/etc/subuid` and
`/etc/subgid`, the package sources with their keys, and the apt setting.
`incus_instances.py` then checks restoration of the guest's original state.

`one_time_playbooks.py` creates, runs, checks, and completes a one-time
playbook on the pair's guests, `app01` and `app02`, with the three commands
`mmx new-one-time-playbook`, `mmx check-one-time-playbooks`, and
`mmx complete-one-time-playbook`. The playbook removes a directory and a
file and runs a `shell` command with `run_only_once_marker_file`; the suite
reads each node's state itself, with `test`, `cat`, and `wc`: whether the
directory, the file, and the marker file are there, and how often the
command ran.

`one_time_playbooks.py` first checks the generated playbook from
`new-one-time-playbook`, whose example node task changes nothing. The suite
then fills in the generated playbook. A second, older playbook that no node
needs shows the report's order and is completed at once; `run`, `dryrun`,
and a second completion of it then exit with code 4.

For the run, `app02` is unreachable: the inventory gives it an HTTPS address
where nothing listens and an SSH configuration that asks for the address
family which the guest's address does not have, so OpenSSH finds no host and
says so.

MMX previously reached `app02`, so the suite tests a known but unreachable
node, and `app02` stays unchanged.

The run succeeds on `app01`, which records it as the last successful run of
the playbook, under the playbook's dated name; the suite reads that record
on the node and checks that the saved run has no housekeeping.

The suite then follows the states: not known with exit code 3, where the
reason is the SSH client's own text from the run's record, still needed with
exit code 2 once the node answers again, and done with exit code 0.
Completion is refused for `not known` and `still needed`, and accepted for
`done`.

`one_time_playbooks.py` reads the automatic, nodes, summary, and JSON output.
The suite opens the saved dryrun whose run ID the nodes output gives.

A second inventory holds only `app01`: its check is done, and a completion
with it moves the playbook although `app02` still needs it, which is what
the command's note about other inventories warns of.

The suite moves the one-time playbook back, as the note says,
runs the playbook on `app02`, checks the record of the last successful run
on `app01` and `app02`, and completes the playbook with the default
inventory.

On a pair of containers the suite takes about 15 seconds and waits for
nothing: every step is one `mmx` command. The first command that connects
sets MMX up on `app01` and `app02` and takes 3 seconds, and the three
commands that meet the unreachable node take 3 seconds together, because MMX
tries a node's SSH connection three times. At its end the suite removes the
directory, the file, the marker file, the file in which the command notes
each time it runs, and the project's records of the last successful run from
`app01` and `app02`.

`libvirt_domains.py` tests `libvirt_domain`, an official extra module,
against the libvirt and QEMU of each distribution's own packages, from
libvirt 9.0.0 on Debian 12 upward. A libvirt domain is a virtual machine, so
the suite's guest is a VM with nested virtualization on every distribution.
The suite runs the whole test on all four distributions: each one packages
libvirt and QEMU. A guest without `/dev/kvm`, such as a Hetzner Cloud
server, cannot run a domain. On a guest without `/dev/kvm`, the suite runs
only the case of the missing `virsh`, and prints that the rest did not run.
In a VM of `incus.py` a missing `/dev/kvm` fails the suite: it reads from
its file of targets that its guest is such a VM.

The example project `libvirt-host` installs libvirt and QEMU without apt's
recommended packages, starts the socket unit, and defines and starts its
domain. The node tasks under "Set up" on the module's page are the first
tasks of the example's playbook, which a Go test checks, and the suite runs
the page's tasks for libvirt's `default` network as the page writes them.
No domain needs a download: the smallest one has no disk and no operating
system, the example's has an empty disk, and one boots the guest's own
kernel without a root filesystem, so that it ends by itself two seconds
later. The suite reads libvirt's state with `virsh` itself (`dominfo`,
`domstate --reason`, `dumpxml`, and the metadata record) and the QEMU
processes with `ps`.

`libvirt_domains.py` covers the missing `virsh`, a first run, an unchanged
rerun and dryrun, and a changed template. The changed template's dryrun
reports `would_change` for the `file` and `libvirt_domain` tasks.

A guest-restart case shuts off the example's domain and a domain with a card
on the `default` network before restarting the guest. The domains run after
the restart, before any `virsh` call. A file change while the domain runs
defines the domain again with the domain's existing UUID. An interrupted-run
case ends before the record. Network-card cases include and omit a MAC address
in the file.

For `stopped`, a domain ignores the shutdown request, with and without
`power_off_when_shutdown_takes_longer`. Another domain ends by itself within
the wait. For `running`, the suite checks a shut-off domain and a paused
domain. Another case uses a running domain without a stored definition.

Refusal cases use another domain's file and invalid XML with a password in
the line quoted by newer libvirt. A refused start after domain creation
fails after one attempt with `created: true`, followed by another run.
Removal cases use `absent` on a running domain, a domain with a saved memory
image, and a domain with a snapshot. Checkpoints cause refusal until the
checkpoint records are removed,
although libvirt itself would remove the definition.

Other `absent` cases use UEFI variables and an emulated TPM, whose state
stays.

The suite also checks an unresponsive libvirt and searches for the XML
password in mmx's printed and saved output, node records, and journal. The
shutdown cases deliberately wait 3 seconds each for a domain ignoring the
shutdown request. The first shutdown case, without forced power-off, also
proves that the task waits once despite MMX's default of three tries.

At its end the suite removes the domains, the `default` network, exactly the
packages that its runs added, the directories, accounts, and groups that
they leave behind, and its own files. `libvirt_domains.py` then checks
restoration of the guest's original state. On one distribution the suite
takes 3 to 7 minutes while other runs share the server. `libvirt_domains.py`
spends most of the 3 to 7 minutes outside module checks: the installation by
the example project takes 60 to 190 seconds, the restart of the guest 10 to
20 seconds, the packages for UEFI firmware and an emulated TPM 12 to 30
seconds on Debian and Ubuntu, and the removal at the end 65 to 145 seconds
with apt, which removes about a hundred packages at about one second each.
Every case of the module itself takes 2 to 11 seconds, about 80 seconds
together.

`blocked_port.py` tests the notice for a blocked port 7443 against a real
firewall. The harness's ordinary HTTPS tunnel ends at the node's loopback
address, which no firewall filters.

`blocked_port.py` adds a second network namespace on the node (a separate
set of network interfaces inside the same machine), joins it to the node
with a virtual network cable, and sends the controller's HTTPS connections
through the namespace. The redirected HTTPS connections then arrive on a
real network interface. The node's firewall filters the redirected
connections as connections from the controller machine.

The suite uses ufw on Debian and Ubuntu, installing the firewall where the
image lacks it, and restores the firewall state it found. With ufw active
and no rule for the port, the notice must say what MMX saw and that ufw has
no rule for port 7443.

`blocked_port.py` then adds a ufw rule for the port limited to the node's
end of the cable and takes that interface down, as a WireGuard interface
goes down: the notice must say what MMX saw, that the node HTTPS API process
runs, and nothing about ufw. With the interface up again, the rule admits
the port and the run uses HTTPS.

Last, `blocked_port.py` stops the node HTTPS API process, and the next run
must start it again and use HTTPS.

`ssh_key_pairs.py` makes an SSH key pair on the pair's first server with
`ssh_key_pair` and gives its public key to `authorized_keys` on the second
through a fleet stage output, as [Use a result from one server on another](../how-to/use-a-result-from-one-server-on-another.md) shows. `ssh_key_pairs.py`
checks the files with `stat` and the node's own `ssh-keygen`, and searches
mmx's output, the saved runs, the node's job records, and its journal for
the private key.

`wireguard_interfaces.py` builds a WireGuard mesh of the pair's two servers
and a third member that has only its inventory entry, from one template over
the inventory, with `wireguard_interface`. `wireguard_interfaces.py` checks
the nodes with `wg`, `ip`, `systemctl`, `stat`, and the files themselves;
pings between the two servers at 50 ms intervals while a reload adds and
removes the third peer and counts the lost packets; and searches mmx's
output, the saved runs, the nodes' job records, and their journals for the
private keys. On Ubuntu 26.04, the suite first installs the `apparmor`
package on both guests, which a real Ubuntu 26.04 server has and the Incus
image leaves out, so every case runs under the `wg-quick` profile. The
removal must leave `/etc/wireguard` empty.

The first guest then becomes a Linux controller machine, running `mmx` as
the login account. The inventory reaches each guest only through SSH over
WireGuard, including the controller machine as a node. The node HTTPS API
is not set up on the WireGuard addresses. With a 30-second confirmation
wait, a bad peer change on the second guest must fail and restore within
90 seconds. The suite checks the restored file's SHA-256, the running peers,
and a new SSH connection from the controller machine.

Removing the second guest's peer from the controller machine must also
restore, and the failure must name the second guest's missing SSH answer.
The suite repeats the change on the controller machine with `--limit`
selecting only the controller machine. A harmless keepalive change must
succeed with `changed` on each guest and no restore. Removing the keepalive
lines must then restart each interface and turn the keepalive off, because
a reload of older wireguard-tools keeps it. The suite restores the
original mesh and removes the guest controller project and SSH key before
continuing.

After the removal, the first guest gets a second interface with
`ListenPort = 443`. On Ubuntu 26.04, the suite shows the cause without MMX: `wg-quick up` fails with
`Permission denied` at `ip link set ... up`, which opens the port. MMX must
then refuse the file in a
dryrun and in a run, before any change. With the `wg-quick` profile disabled,
as an administrator may do, the same run must succeed with port 443; the
suite then enforces the profile again. On the other distributions, which
load no profile for `wg-quick`, the run with port 443 must succeed directly.

`node_settings.py` sets the host name, the time zone, a locale with the
system default, and choices of the alternatives system on one node, and
checks them with `hostnamectl`, `/proc/sys/kernel/hostname`,
`readlink /etc/localtime`, `date`, `locale -a`, `/etc/default/locale` or
`localectl`, and `update-alternatives --display`. `node_settings.py`
restores the original host name, time zone, and locale files.

`storage.py` makes two loop devices from files inside the VM and uses them
as disks: `create_filesystem_on_empty_device` formats the empty one,
`fstab_mount` mounts it, and `swap_file` makes `/swapfile`. `storage.py`
checks the node with `blkid`, `findmnt`, `swapon`, `stat`, `systemctl`, and
`/etc/fstab` itself, and prepares the second device with another filesystem,
a swap area, and a partition table to test the refusals. `storage.py`
restores the node's `/etc/fstab` and detaches the loop devices at the end.

`partitions_and_lvm.py` tests the official extra modules
`create_partitions_on_empty_disk`,
`grow_partition_into_free_space_after_it`, `lvm_volume_group`,
`lvm_logical_volume`, and `grow_filesystem_to_fill_device` on loop devices
made from sparse files inside the VM.

The filesystem-growth cases enlarge a device with a mounted ext4 or XFS
filesystem, check a dryrun, growth, and an unchanged rerun against the
filesystem tool's own block counts, and check the missing prerequisite when
the filesystem is not mounted. `partitions_and_lvm.py` runs the whole test
on every distribution: `sfdisk`
is on each of them, and the suite installs `lvm2` and growpart's package
with the node tasks that the modules' pages show under "Set up", which it
reads from the pages.

Before installing LVM tools, `partitions_and_lvm.py` checks
`lvm_volume_group` and `lvm_logical_volume` without the required programs.

For partitions, `partitions_and_lvm.py` uses disks with sectors of 512 and
of 4096 bytes and reads each written table with `sfdisk --json`.

`partitions_and_lvm.py` grows the last partition of an enlarged disk while
an ext4 filesystem on it is mounted, and checks that the filesystem stays
mounted with its size, that a file on the filesystem retains the pre-growth
content after the page cache is dropped, and that `blockdev` reports the
partition's new size.

`partitions_and_lvm.py` lets the node's own `sfdisk` write a table with a
last partition without a size, which older and newer versions end at
different sectors, and checks that the task finds its partitions in both.

`partitions_and_lvm.py` prepares disks with a filesystem, with swap, and
with an MBR partition table, and checks by the SHA-256 of a disk's first MiB
that a refused disk stays as it was. The suite also checks refusal and an
unchanged SHA-256 for the first MiB of a disk in use without a signature:
one that device-mapper holds (`dmsetup create`), and one of which the kernel
still has a partition because its table was wiped while a filesystem was
mounted.

On disks that end 33 sectors of 512 bytes, and 5 sectors of 4096 bytes,
behind a whole MiB, the rest of the disk and a partition of 63 MiB must end
at the last usable sector.

A loop device that was attached without `--partscan` gives a table that the
kernel does not read: `create_partitions_on_empty_disk` fails after writing
the partition table, without another try, and in the next run, and is
unchanged once `partx --add` made the partitions.

For LVM the suite checks the node with `vgs`, `pvs`, `lvs`, `wipefs`, and
`df`: a volume group on a partition's link and on a whole device, devices
added to the volume group and devices removed from the group with the LVM
label also removed, a volume created where a removed one had a filesystem,
an ext4 filesystem grown while it is mounted, an xfs filesystem grown while
it is not mounted (where LVM 2.03.31 and newer need another option than
older LVM), an ext3 filesystem grown with its volume, a volume that holds
nothing, which grows alone, a volume that holds swap, which is refused with
`grow_filesystem_with_volume: true` in a dryrun and in a run and grows with
`false`, and a filesystem that is mounted read-only, whose volume grows
without the filesystem: the task fails without another try and stays an
ordinary failure.

A volume with a snapshot (`lvcreate --snapshot`) must stay with its snapshot
when the task wants it deleted, and a device that device-mapper holds must
be refused by LVM itself.

Each task that cannot succeed on the node as it is must fail in a dryrun and
in a run with the same reason, after one try. Each state that an earlier
node task may change (too little free space, a device of another volume
group, a device that holds data, a volume in use or with a snapshot, a
volume group with volumes) must leave a dryrun not checked and fail a run
once.

The suite runs the playbooks of the modules' pages in which one task
prepares the next, and checks that their dryrun leaves the later task not
checked: delete the snapshot and then its volume, delete the volumes and
then their volume group, unmount a filesystem and then delete its volume.

The suite runs the example project `data-disk-with-lvm` on a loop device of
1 GiB and then grows its mounted filesystem through the inventory. At the
end, `partitions_and_lvm.py` detaches the loop devices and removes the
packages added by the suite's runs (`lvm2` with what it pulled, and
`xfsprogs`) and the files that LVM wrote under `/etc/lvm`, and checks that
the guest's packages are those that it found.

On one distribution the suite takes about three minutes when it runs alone
on the server (163 seconds on Debian 13): 30 to 65 seconds to install
`lvm2`, `e2fsprogs`, and `xfsprogs`, about 25 seconds to start and to remove
the packages again, and the rest in about 140 `mmx` commands of under one
second each. `partitions_and_lvm.py` waits for nothing. Beside
other runs loading the server, `partitions_and_lvm.py` took four to eight
minutes.

`zfs_datasets.py` tests the official extra module `zfs_dataset` on a ZFS
pool that is a file of 256 MiB inside the guest. ZFS is a kernel module, and
how it gets onto a node differs by distribution, so the suite has two forms.
On every distribution, `zfs_datasets.py` first checks the module without
ZFS's programs: a dryrun reports the task not checked, and a run fails once
and names the program and its package. `zfs_datasets.py` then runs the whole
test where ZFS installs in under two minutes, and on Debian 13, the
reference distribution, with its build of the kernel module:

| Distribution | What the suite runs | Kind of guest |
|---|---|---|
| Ubuntu 24.04 and 26.04 | The whole test; ZFS 2.2 and 2.4, whose kernel module comes with the kernel. | VM |
| Debian 13 | The whole test; ZFS 2.3, whose kernel module the package `zfs-dkms` builds on the guest in about five minutes. | VM |
| Debian 12 | The test without the programs. | container |

On Debian 12 the whole test is left out: a build of ZFS there takes four to
seven minutes and breaks whenever the distribution's kernel is newer than
ZFS supports. The module's page records what was installed by hand on
Debian 12. `ZFS_DISTRIBUTIONS` in `incus.py` and `INSTALLS_ZFS` in the suite
name the distributions of the whole test, and `test_incus.py` checks that the two
agree.

For the whole test, the suite installs ZFS with the node tasks that the
module's page shows under "Set up", which it reads from the page, and
creates the pool with a guarded `shell` task, as the page shows it.

`zfs_datasets.py` checks the node with `zfs get`, `zfs list`, and `findmnt`:
a dataset created with its properties and mounted, sizes written in other
forms, a dataset below a missing and below an existing parent, properties
set with their old and new values, a value that ZFS stores in another form
(`gzip-6`) for an existing and for a new dataset, an unknown property name,
a property's second name (`compress`, `recsize`), a property that is set
only at creation, a refused value, a volume, a dataset with a child dataset
and a snapshot, which leaves a dryrun not checked and fails a run once, and
a dataset in whose directory a process works.

`zfs_datasets.py` runs the playbooks in which one task prepares the next,
and checks that their dryrun leaves the later task not checked: the parent
dataset and then its child, the child deleted and then its parent.

With `sharenfs` and no NFS server, ZFS exits with an error after it created
the dataset; the task then fails without another try and reports
`created: true`. At its end the suite makes the kernel refuse ZFS's module,
as after a kernel update for which no module was built, and checks that a
dryrun is not checked and that a run fails with ZFS's message.

`zfs_datasets.py` then destroys the pool, unloads the kernel module, and
removes the packages and repository key added by the suite's runs. The suite
checks restoration of the guest's original package set. The harness starts
every VM guest with Secure Boot off, without which only Ubuntu's kernel
loads the module. Nearly all of the suite's time is the installation, which
is shortest on Ubuntu, whose kernel brings the module: on Debian 13 the
suite takes six and a half minutes, of which the build of the kernel module
takes five. The checks themselves take about 50 seconds and wait for
nothing, and the test without the programs takes 7 seconds.

`pacman_packages.py` tests the official extra module `pacman_package` on an
Arch Linux container guest, the only distribution in its entry of
`SUITE_DISTRIBUTIONS`. The suite checks the node with `pacman -Q`,
`systemctl`, and the time of change of pacman's database file
`/var/lib/pacman/sync/core.db`. To give an upgrade something to do without
waiting for the package servers, the suite rewrites the record of one
installed package in pacman's local database to an older version, `which`
before the first installation and `nginx` before the node task block's
restart; the package's files stay, and the next upgrade installs the
repository's version again, or the suite's clean-up puts the record back. The suite installs nginx with a full system
upgrade, reruns unchanged without a refresh, restarts nginx from a node task
block when the recorded version differs, upgrades the whole system, fails to install
`nginx-mainline`, which conflicts with nginx, removes nginx with the module
that depends on it, refuses an unknown name and a provided name, refuses to
remove the group `archlinux-tools` while its member `pacquery` is installed,
and waits for pacman's lock: once while a timer removes the
lock file after five seconds, and once with a lock file that stays, which
fails the task after 60 seconds. The suite removes the packages that it
added; the full system upgrades of its installations stay. On a Linux test server it takes
about 105 seconds, of which the lock that stays takes 60.

`zypper_packages.py` tests the official extra modules `zypper_package` and
`zypper_repository` on an openSUSE Leap 16.1 container guest, the only
distribution in its entry of `SUITE_DISTRIBUTIONS`. The suite checks the node
with `rpm`, `rpmkeys`, `systemctl`, and the request log of its own test
repository, never through MMX. It installs `rpm-build` and `createrepo_c` with
`zypper_package`, builds a handful of small signed packages (a package and
one that needs it, a service, a package that provides a capability, two
packages that own one file, a package that conflicts with another, a package
whose script fails, a package without a signature, and two packages whose
download takes 8 and 75 seconds), signs the repository's metadata, and serves
the repository from a small web server on the guest. The suite installs `tree`
from openSUSE's own repository, adds the test repository with
`zypper_repository`, installs from it, refuses the removal of a package that
another needs, refuses the names that zypper reads as patterns, paths,
capabilities, and versions, fails the two transactions that zypper's dry run
does not check, runs a package whose script fails, checks the version rules,
restarts a service from a node task block when its package changes, updates
the whole system, waits for zypper's lock while a `zypper install
--download-only` holds it for 8 seconds and fails the task with zypper's message
while another holds it for more than 60, shows that a repository whose key was
never imported is refused, and adds a repository with a key from a web address
and its SHA-256, gives a repository the key of openSUSE's own signing and checks that removing the repository leaves the key and that the distribution's repository still refreshes, shares one key between two repositories, replaces a key, and checks the vendor rule with a second vendor of one package. The suite removes the packages that it added, the
repositories, and the keys. On a Linux test server it takes about four minutes,
of which the lock that stays takes 60 seconds.

Platform coverage does not establish release support or large-fleet
performance.

Module pages that rely on this section:
[`apt_package`](../reference/modules/apt_package.md),
[`dnf_package`](../reference/modules/dnf_package.md).
