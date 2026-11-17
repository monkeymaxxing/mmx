# MMX implementation rules

MMX keeps each kind of job done one way, so a reviewer can check a rule once
instead of tracing every combination of connection, resume, stop, and failure.
The [acceptance contracts](acceptance.md#acceptance-contracts) define required
outcomes; the [code map](code-map.md) explains the components
and follows work through the code. Reviews check every changed path against
the implementation rules in this document.

## One status vocabulary

`internal/protocol/status.go` defines statuses for node jobs, execution entries,
fleet stage loop iterations, node tasks, and playbook runs, once each.
The status table defines terminal, completed (needs no further attempt),
uncertain (the node may still be working), failure, and display rank.

`internal/protocol/status.go` also defines the one change precedence:
`changed` > `would_change` > `unknown` > `unchanged`.
Resume selection, run status, pruning, exit codes, loop outcomes, and display
derive from the status table in `internal/protocol/status.go`.
No other code lists statuses by hand.

A comparison that says whether execution ran (`succeeded || failed`) is not
a second status list. Keep the comparison explicit rather than
adding a status-table property used only for the comparison.

## Uncertainty has one source

An execution entry is uncertain only when the node may still be working on
the entry's node job. The possible conditions are:

- The node job is running.
- The node job requested a reboot, but the node has not answered from a new
  boot (`rebooting`).
- A connection failure left the outcome unknown after the node job ID was
  published.
- The node has no record of the node job yet (`not_found`), because an
  in-flight submission can still be admitted.

A node rejection is a definite answer and fails the submitted node job
attempt. Rejections include a busy node, conflicting identity, or missing
asset.
Only a response to job submission counts as a rejection: an HTTPS error
response or the node SSH request process's own error exit (status 1).
A failed status or stop request, a lost SSH connection, or a crashed
`mmx-runner` process leaves the node job uncertain.

Uncertain entries keep the playbook run unfinished.
`status` and `resume` reconcile uncertain entries by node job ID.
Resume starts a new attempt only after the node answers.

A proven replacement of the node job's machine (`replacedError`) is also a
definite answer. The entry fails, naming the replacement.
The node admits one active node job at a time, so a late admission cannot
overlap a new attempt.

After a stop or `stop_run`, the controller process continues walking fleet
stages without submitting new node jobs. The walk records each remaining
entry, loop iteration, and selection as `stopped`, with the cause.
When the walk reaches each selection, the walk resolves the selection with
`.fleet_stages` outcomes unavailable. A selection reading `.fleet_stages`
outcomes stays unresolved until resume.

Only a controller process failure leaves work unwalked.

## Node locks

`admission.lock` guards a short critical section: admission, upload setup,
status/stop, history expiry, and difference reads.
Every caller waits for `admission.lock`.
`execution.lock` means exactly "a worker is running".
Only the node job worker process and installation of the node runner executable
take `execution.lock`, always after `admission.lock` when acquiring both locks.
Housekeeping learns whether a worker is active from the one active-job check:
the `active` file and the active node job's status.
Housekeeping never tries `execution.lock`.

A node job's `worker.lock` is held by the node job worker process, from
admission until the worker exits. Status reads `worker.lock` to distinguish
a running node job from an interrupted node job.
"Node busy" means another job is running, never lock
contention. A job's `assets.lock` is taken only under `admission.lock`, because
expiry removes it with the job directory; an upload holds it while streaming.
The bootstrap shell script follows the node lock rules.

A node job ends only through `node.finish`. The node job worker process calls
`node.finish` for the execution outcome, including a worker failure after
acquiring the node job's `worker.lock`.
A failed worker start also ends through `node.finish`.
Status calls `node.finish` when status finds an interrupted worker.

A node job ending `rebooting` is finished again as failed when
`systemctl reboot` is refused. Status finishes the node job as succeeded when
the node answers from a boot different from the recorded `RebootFrom`.
`node.finish` records the cause, then removes the node job's private input
and execution assets. Expiry of any node job directory removes the node job's
private files. `worker.log` keeps only what a crash writes.

Owed restores use three more locks. `owed_restores.lock` protects short record
reads and durable decisions, including final confirmation against expiry.
`change_activation.lock` serializes installation, native activation, and
restoration. A change's `restore_execution.lock` serializes
repeated restore invocations. Restoration takes `restore_execution.lock`,
makes a short record decision, releases `owed_restores.lock`, stops the
activation service, then takes `change_activation.lock`.

Installation acquires `change_activation.lock` before `owed_restores.lock`
and holds both locks through the file operation and sync.
Never wait for a native command or another lock while holding
`owed_restores.lock`. Restoration never takes a node job's execution or
admission lock. When acquiring `admission.lock` and `owed_restores.lock`,
acquire `admission.lock` first.
Waiting with `owed_restores.lock` held would delay confirmation and expiry
decisions.

A terminal node job result can acquire a later restore outcome. Status copies
the restore outcome from the durable restore record without changing the
node job's failure or finish time. History cannot discard an unresolved
restore or its node job. Both node runner installation paths refuse an
existing `mmx-owed-restore-*.service` unit, including a partially armed unit.
The owed restore service still needs the installed node runner executable.

## The controller machine is never rebooted

`engine.NodeMachine.RebootRefusal` is the one rule deciding whether a node is
the machine running the controller process. The boot IDs must match.
In addition, the machine IDs must match or the node must not be a container.
`engine.NodeMachine.RebootRefusal` runs on the
node, with the node's own readings (`facts.BootID`, `facts.MachineIDHash`,
`facts.RunsInAContainer`), and no other reboot decision compares these
values. The reachability confirmation's check for a change on the controller
machine is not a reboot decision: `controllerInventoryReachability` compares
the changed node's machine ID hash and boot ID with the controller machine's,
both on the controller process, because the inventory probes it compares
with run no node runner executable. The
controller process sends only `protocol.ControllerMachine`: the controller
machine's boot ID and the SHA-256 of the controller machine's machine ID.
The controller process reads the IDs once per process (`thisControllerMachine`)
and sends the readings at two points:

- `executeEntry` calls `Connection.RebootRefusal` for a fleet stage ending
  with a `reboot` task (`endsInReboot`), before preparing or delivering the
  node job.
- `config.Job.ControllerMachine` carries the readings so `Engine.reboot`
  asks `engine.NodeMachine.RebootRefusal` again after the reboot task's
  guards.

A refusal or an unanswered question fails the node job as `failed`, never as
uncertain. The pre-submission check fails before the node job is sent.
A node unable to obtain the readings required by
`engine.NodeMachine.RebootRefusal` returns an error.
`executeEntry` and `Engine.reboot` then fail with `engine.RebootNotChecked`.
No path permits reboot when the check cannot be completed.

`readControllerMachine` returns an error when a Linux controller machine
cannot read the boot ID. `executeEntry` then fails the node job with
`engine.RebootNotChecked` without asking the node.
Only a system that is not Linux sends an empty boot ID.
An unreadable machine ID stays empty on every system, because the boot ID
alone can decide.

## Persistence

- State files go through `internal/durable` (temporary write, fsync, rename,
  directory fsync). Every managed path is inspected and changed relative to an
  `os.Root` for the path's parent.
  Replace files and links only through `replaceAt`.
  Call the mutation boundary, check that the filesystem entry still matches
  the inspected entry, rename, then sync the directory.
  Files and links are removed through `removeAt`, which never removes a
  directory and syncs the parent after unlinking.
  A sync failure after replacement or removal reports
  `change: changed`: the filesystem entry has already changed.

  `openParent` pins a path's parent and inspects the path.
  A release directory (`unpack_archive`, `git`) is created only through
  `installStaged`: build in `.mmx-staging-<name>` within the pinned parent,
  then flush once with `sync`. Rename the staging directory into place only
  while the destination path is still absent, then sync the parent directory.

  Delete a directory tree only by renaming the directory to the staging name
  within the directory's pinned parent. Sync the parent, then delete the
  staging tree. The deletion rule covers `unpack_archive` with `state: absent`
  (a directory holding the `.mmx-archive` record) and `directory` deletion
  (`stageAndDelete`). For `directory` deletion, the next run deletes a staging
  tree left behind. `deletionTree` walks the tree
  an archive removal or directory deletion would delete, without following
  links, and fails on a mount point before anything is deleted.
  Renaming makes the original path disappear durably before the staging tree
  is deleted.
- Archive entries are untrusted. Before creation, `archiveCheck` alone checks
  entries in tar or zip archives. Tar may be plain or compressed with gzip,
  xz, bzip2, or zstd.
  Names and symlink targets must stay within the extraction directory.
  Nothing may be at or below a symlink. Hard links may target only files from
  earlier archive entries, and special files are forbidden.
  Extraction creates only checked entries within an `os.Root` on the
  staging directory. Decompressed output (an archive's declared file sizes, a
  download's `decompress`) must leave `assets.KeepFreePercent` of the output
  filesystem free (`assets.WritableSpace`).
- The controller fleet scheduler saves each entry through `entrySaver`.
  Save a new node job ID durably before submission.
  Save every finished or uncertain response durably before reporting the
  response.
  Running progress under the published job ID is only replaced
  atomically (`durable.Replace`), since the node can resend it after a crash.
  Progress writes are at least a second apart.
  A deferred progress write runs when the second ends.
  `finish` saves the final state durably and drops any
  pending progress.
- Node admission fsyncs only the saved request, whose bytes also identify a
  repeated submission.
  The `active` marker is replaced without fsync, because the marker only guards
  admission while the node runs. After a crash, status reports the saved
  request's node job as interrupted.
- The worker's `progress.jsonl` is the only append-only state file: one line
  per change, written without fsync. Readers stop at the first incomplete or
  undecodable line, so crash damage shortens the partial record instead of
  blocking status. `progress.jsonl` is a live view and an interrupted job's
  partial record, never recovery state.
- Persisted and peer JSON is decoded with `protocol.Decode`: unknown fields
  and trailing data are rejected.
- One controller process owns a run through `runs/<id>/control.lock`; it alone
  writes the run's status, selections, and entries. Another process writes
  only the stop file.
- The controller process saves a finished result, then downloads the result's
  full differences. A failed download is housekeeping.
  A run stays in history until no entry is uncertain and the run's retention
  has passed, unless the run is the latest normal run for the run's selection.
  Nodes remove a finished node job 72 hours after the node job's
  `result.json` was written. Nodes remove a never-admitted node job directory
  with the directory's uploads 72 hours after the directory was created.
- Persist only what recovery, resume, or display reads.

The file helpers for an [owed restore](design/execution.md#reachability-confirmation-and-owed-restore)
keep file bytes apart from recovery coordination. `PrepareFileChange` compares
and checks one candidate. `SavePreviousFile` saves the immediately preceding
bytes or absence.
`SaveProposedFile` saves the candidate's bytes as `SavePreviousFile` does,
only for an activation implementing `ChangeActivationWithSavedFiles` and a
change installing a file. The node hands the activation the paths of both
saved files: the previous file and the proposed file.

`Install` changes the destination.
`RestorePreviousFile` restores the saved previous bytes and metadata.
The prepared-file helpers reuse ordinary file inspection, candidate creation,
replacement, and removal. The prepared-file helpers recheck the inspected
entry and its contents before saving and installation.
Protected metadata changes also replace the file.
A dryrun creates no candidate or saved file.
The prepared-file helpers do not create a record, arm a timer, or activate
configuration. The calling owed restore service coordinates record creation,
timer arming, and activation.

`systemd_networkd_file` supplies compiled activation and restoration code,
with only device identities in its private restore data.
The `systemd_networkd_file` activation log check uses a journal cursor.
Restoration checks commands and deletions without rejecting warnings from the
previous configuration. The prepared-file boundary covers one module-owned
file, never general recovery of a node task block.

A `ChangeWithOwedRestore` without a prepared file carries module data and the
module's change subject, service ordering, and successful restore sentence.
The node stores these descriptions; it never selects text or unit ordering
by module name. File restoration fields occur only in file outcomes.
`firewalld_zone_membership` uses the same boundary without a prepared file.
Its empty destination reserves one membership obligation per node; its
private module data records both layers of each affected member. No previous
contents or file candidates are created. The existing locks, confirmations,
and durable phases still own acceptance and restoration. Its compiled
activation factory alone supplies firewalld commands; no command text is
read from the record.

`netplan_file` uses the prepared-file boundary.
The `netplan_file` private restore data holds
device names and generated network filenames, never command text or YAML.
`netplan_file` reuses the native settings reader and bounded restore startup
wait.

Netplan generation uses the ordinary private task directory
and the mutation boundary, because some Netplan versions regenerate live
output too. During dryrun, `netplan_file` never calls Netplan to generate files.
The copied candidate never enters the live tree.

Refuse changed device-creation settings for previously declared devices, even
when the devices are absent. Also refuse changed matches in retained generated
network files. Both changed device-creation settings and changed matches can
prevent a complete restore.
Native diagnostics omit echoed source and caret lines, including source from
other private YAML files.
Only links managed before apply, selected by removed generated files, and
subsequently reported unmanaged can have their global addresses flushed.

`ifupdown_interfaces_file` uses the prepared-file boundary.
The `ifupdown_interfaces_file` private restore data holds interface names only,
with the class to bring each interface up: `auto` or `allow-hotplug`.
The `ifupdown_interfaces_file` private restore data never holds stanza text
or commands.

The `ifupdown_interfaces_file` activation passes the saved previous and
proposed files to `ifdown -i`. Therefore, ifupdown reads the saved files to
choose commands, rather than taking command text from the owed restore record.
The `ifupdown_interfaces_file` parser decides only which interfaces changed.
Native checks go to `ifquery` and `ifup --no-act` through standard input, so
no candidate enters a directory that ifupdown reads.

## Transport

HTTPS and SSH call the same `node.Server` methods.
In the controller process, every node request goes through `Connection.call`.
Requests use HTTPS while the connection uses HTTPS.
Requests switch to verified SSH only after a reachability failure.
Never switch after an identity, authentication, or authorization rejection,
or an HTTP error answer. Trust comes
from OpenSSH's host-key check alone. The saved endpoint identity records what
verified SSH reported.
`connect` detects an outdated saved endpoint identity through a TLS pin error,
a different reported identity, or SSH reaching another machine.
`connect` then sets the node up again through verified SSH and saves the new
identity.
After setup and saving the new identity, an HTTPS identity error is final.

Resume and status pass the machine the node job ran on.
`connect` refuses a replacement machine (`replacedError`) before installing
anything.

`Connection.BootID` and `Connection.NodeID` say which machine a connection
reached and in which boot (`bootedMachine`): the first SSH command reads the
boot ID, and the node HTTPS API process reports the boot ID in
`endpoint.Info`. `Run.recordConnection` alone records which inventory name
reached each booted machine, for node jobs and for a fleet stage's facts.
A second inventory name on the same booted machine fails before anything is
sent to the node.

`artifact.checkVersion` alone compares a node runner executable's release
version with `release.Version`: `endpoint.Info.RunnerVersion` on the HTTPS
path, and `mmx-runner version` once per runner file and controller process
on the SSH path, in `connect`'s `open`. Development builds all report
`release.DefaultVersion` and match one another.

The controller process follows a running node job with
`Connection.Progress`. Each request carries the results and current task
already held by the controller process.
`node.Server.Progress` answers with only newer results once the results or
current task change, or after 5 seconds, waiting without a lock.
The node spaces running answers at least 200 ms after their request and
answers a finished job at once, so the controller process asks again
immediately. A
stop interrupts a waiting request.

`Status` stays a complete, immediate answer for reconciliation. After a
connection failure (unreachable HTTPS, lost SSH connection), an idempotent
operation repeats, at most three attempts in all.
Submission never repeats and instead inspects the submitted node job ID.
`sshSession.asRoot` alone adds sudo and privilege
to SSH commands. Native SSH runs without keyboard-interactive authentication.
Askpass answers only OpenSSH's password prompt naming the resolved target and
OpenSSH's passphrase prompt for an identity file resolved for the node.

Each JSON node request has one name in `protocol` (`protocol.RequestFacts`
and its siblings), used as both the HTTPS route `POST /v1/<name>` and the
`mmx-runner <name>` command over SSH. `node.Server.Answer` is the one table
of what the node does for each name: the node HTTPS API process and
`mmx-runner` pass it the request body unread, and it reads at most
`protocol.RequestLimit` bytes, so the two connections cannot apply
different limits. The controller process sends these requests through
`Connection.ask`, or `Connection.send` for the submit request, which is
never repeated. A new JSON node request adds a name and a table entry, not
an HTTPS route or an `mmx-runner` case.

`protocol.RequestLimit` bounds every JSON request and
`protocol.ResponseLimit` every JSON answer on SSH and HTTPS. Asset uploads
use `assets.Transfer`'s stall timeout and have no total deadline, on SSH and
HTTPS. The endpoint gives each read the idle interval instead of the request
deadline. A full-difference download, at most 8 MiB, uses the ordinary
30-second request deadline.

Administrator keys shared by all nodes, from the SSH authentication agent and
resolved identity files, load once per process.
Only a node's own managed key is read per node.

## Secrets

- Ordinary JSON never contains secret data: `value.Secret` and
  `protocol.Result` marshal to their safe forms. Only `value.Transport`
  carries private bytes, over authenticated connection or into root-private
  `/run` files.
- A node task handing a tool a file for as long as the task runs makes the
  task's private directory only through `Runtime.makePrivateTaskDirectory`
  (`modules/private_task_directory.go`).
  The private directory holds the logins of `git.go`, `docker.go`, `podman.go`,
  `mysql.go`, and `postgresql.go`, or the copy of a new file for a check run
  by an ordinary account (`unit.go`, `podman_quadlet.go`).
  Compose's dry run also uses a private scratch directory.

  The task's private directory is made in `/run/mmx/private_task_directories`.
  The node creates and checks the root directory through
  `node.privateTaskDirectoryRoot`: root-owned, mode 0711 like the parent
  `/run/mmx`, and on tmpfs.
  The node passes the root directory to the node task execution engine as
  `Engine.PrivateTaskDirectoryRoot`.
  A module reads the private-directory root from the module's `Runtime`,
  never from a package variable.

  For each node task, the private directory belongs to the account running
  the tool that reads the task's files, with mode 0700.
  Root writes every file before giving the private directory to the tool's
  account. Afterwards, root only removes the private directory.
  Root never follows a link the tool's account could add inside the private
  directory.

  Housekeeping (`sweepHistory`, under `admission.lock`) calls
  `removeLeftoverPrivateTaskDirectories` while no node job is active.
  The cleanup removes every directory in `/run/mmx/private_task_directories`,
  so directories left by a killed worker are gone before admission starts the
  next worker. No private task directory is removed
  by housekeeping while a node job runs.
- A task's result is secret when the task's scope trace saw a sensitive value.
  `Result.Safe` suppresses text for a secret result, except command
  diagnostics marked `OutputHoldsNoSecret`. Before command diagnostics leave
  `Engine.task`, `Result.WithoutSecretCommandOutput` replaces secret text
  collected from the node job's scope at `Engine.Run` and remembered by the
  task's scope trace, using `value.TextWithoutLoginsHandedOnlyToATool`.

  `value.SecretValueTexts` collects every secret value held by the node job
  or read by the task. Collection also includes forms without surrounding
  whitespace and individual lines without surrounding whitespace, skipping
  empty forms.
  `value.SecretValueTexts` includes keys
  of secret mappings and uses `value.EnvironmentText`, the command's own
  environment formatter, for secret lists and mappings.

  A failed file check passes the check's promise to the file task's
  `ReasonReadsNoSecret`.
  The file task and differences remain secret.
  `Result.Safe` delegates restore diagnostics to `OwedRestore.Safe`, also used
  when an interrupted node job
  reports a restore outcome outside its task results.
- A task's trace starts marked when the enclosing role call or node task block
  trace is marked (`Scope.Trace`). A read marks the reader's trace and all
  ancestor traces. In a role or node task block, tasks following a task
  reading a secret are secret too. The enclosing role call or node task block
  is also secret. Passing a value on is no read.

  `Scope.ResolveWithoutMarkingATrace` is the one way to resolve without a mark.
  Use `Scope.ResolveWithoutMarkingATrace` only when the resolved value enters
  nothing reported by a task.
  `config.ResolveRoleInputs` uses `Scope.ResolveWithoutMarkingATrace`.
  A role call with a secret for a role input hides nothing by itself.
  The role input remains secret in the role's scope, and a task reading the
  input marks the trace.
- `Result.Safe` keeps a secret result's reason when `Result.ReasonReadsNoSecret`
  guarantees that the reason holds no secret value (`hidesReason`).
  `Result.ReasonReadsNoSecret` travels to the controller process, which makes
  results safe again. `assert` sets `Result.ReasonReadsNoSecret` only for
  failure text containing no secret: a non-secret assertion message or the
  assertion's condition as written.
  An assertion message resolving from a secret becomes a secret value,
  including a hidden node task's result (`protocol.ResultValue`).

  The node task execution engine passes `Result.ReasonReadsNoSecret` on
  unchanged.
  `nodeTasks` returns a failure with a non-secret reason as
  `failureWhoseReasonReadsNoSecret`.
  `Engine.failure` sets `Result.ReasonReadsNoSecret` on the failing role call,
  node task block, or node task. The enclosing reason adds only MMX's own text
  and safe reasons from node tasks. A loop copies `Result.ReasonReadsNoSecret`
  from the failed item. The task's scope trace remains unchanged, so the
  result remains secret except for the reason guaranteed to contain no secret.
- Reading a login handed only to a module's tool leaves the trace unmarked,
  as `Scope.ResolveWithoutMarkingATrace` does.
  The login is a user name, password, or key delivered through the task's
  private directory or a local socket. The module puts the login nowhere else.

  The module declares the login input with
  `ArgumentList.LoginHandedOnlyToATool`.
  Resolve the declared login input only with `Scope.LoginHandedOnlyToATool`.
  `Scope.LoginHandedOnlyToATool` remembers secret login text in the task's
  trace. `config.resolveArgument` checks the login input's rule without
  marking the trace.

  A module handing a login only to a tool must report all additional login
  representations through `Scope.FormOfLoginHandedOnlyToATool`.
  Report every form the tool receives, stores, or sends.
  Docker and Podman use `resolveRegistryLogin`.
  Database modules compute the stored password form on the node while running.

  `Engine.execute` is the one place that replaces the
  secret login texts remembered by the task's trace
  (`Result.WithoutLoginsHandedOnlyToATool`), on what a
  module returns. Replacement precedes every use of the module result,
  including retry conditions, later node tasks, records, and node job errors.

  If the module cannot determine a secret representation printed or stored
  by the tool, keep the input an ordinary private value.
  A module whose tool can print part of a
  secret representation removes printed fragments before returning.
  Replacement of complete secret representations would leave partial
  fragments visible. `withoutMySQLStoredValue` handles fragments of the stored
  `mysql_user` password value. A server quotes at most 80 characters of a
  refused statement, which can end inside the stored value.
- A field forbidding literal values, such as `user.password_hash` or a
  repository's `password`, takes one whole reference to a named secret or role
  input.
  The module's check calls
  `modules.WholeSecretReference`, declares the field with
  `ArgumentList.WholeSecretReference`, and `checkModuleReferences` requires
  the referenced role input to be declared `secret: true`. The inventory's
  `ssh_password` and `sudo_password` take a named secret only
  (`ConnectionSecretName`), since no role surrounds them.
- Every template reference is found by one parse-tree walker.
  The walker rejects unknown template parse-tree node kinds.
  A `template` call reads the argument passed to the called body.
  The called body sees only the argument value.
  An `index` whose keys are literals or reads is the selector path the
  `index` names.
  An `index` key that is neither a literal nor a read reads the whole
  collection.

  Validation, secret tracing, and secret delivery all use the template
  parse-tree walker. A template reading the whole value scope fails validation.
- A selector never reads a named secret: `checkPath` rejects a reference
  such as `.vars.ports[.secrets.which_port]` when the project loads, because
  a node gets a secret only as a value, never as the key of a lookup.
- Secret delivery follows dynamic selectors the controller process can resolve
  exactly as the node will. Controller-resolvable values include the node's
  name, earlier outputs, fleet stage outcomes, inventory, fleet stage loop
  items, and variables no node task in the node job reassigns.
  If a controller-resolvable dynamic selector cannot resolve,
  nothing is delivered for the reference. The node fails the reference read
  that failed on the controller machine.
  Delivery continues into a named secret through static fields and
  controller-resolvable dynamic selectors.
  Delivery sends the union of secret fields selected by all references in
  the node job, omitting unselected fields.
  The node reads delivered fields through the original reference paths.

  The first selector depending on node-only
  values (facts, results, task loop items, role inputs, reassigned variables),
  directly or through a nested selector, ends the path.
  Delivery sends everything beneath the first node-only selector.
  Only delivery (`Scope.Within`) records which scope secret a delivered value
  replaces.
  Secret loading unlocks the whole named secret in each reference's static
  prefix.
  A run and resume load every named secret listed by
  `config.SecretNamesARunReads` for the run's inventory and limit.
  `config.SecretNamesARunReads` is the one answer to which secrets a run needs.
  `mmx secrets usage` also uses `config.SecretNamesARunReads`.
  `config.SecretNamesARunReads` excludes references the fleet stage walk never
  reads. Exclusions cover fleet stages omitted by `--only-fleet-stages` and
  fleet stages or `for_each_node` loops selecting no nodes before execution.
  The no-node calculation uses `config.SelectNodes`, narrowed by
  `config.Limit.Narrow`.
  Secret loading still includes references in `save_on_controller_machine`
  templates written by a fleet stage selecting no nodes.
  An unresolved selection keeps all secrets referenced by the fleet stage.

  Fleet stage loop items and completed outputs bind named secrets without
  fixing the secret values, so resume loads fresh values.
- Node-origin secrets include `read_file` with `content_is_secret`, `shell`
  with `output_is_secret`, and values computed from secrets on the node.
  `value.FromNode` records the origin as `value.NodeOrigin`.
  The node task execution engine applies `value.FromNode` once to each node
  task's result value and to fleet stage outputs.
  Saved history holds only `value.NodeOrigin`.
  `value.ReadAgain` is the one way resume obtains a secret originating on a
  node again. For a `read_file` origin, `readSecretsAgain` rereads files needed
  by unfinished work before the fleet stage walk starts.

  `secretsStillNeeded` follows the resume's planned walk through entry outputs,
  loop iteration outputs, loop items, and pending `save_on_controller_machine`
  writes, and errs toward reading.
  Rereads run outside the scheduler's lock with the run's context and a watcher
  that cancels rereads when the run receives a stop request.
  `Connection.ReadFile` uses the node's `ReadFile`, which reads files as the
  `read_file` module does. Rereads refuse a machine different from the machine
  where the producing node job ran.

  The fleet stage walker's `readAgain` looks up the content already obtained
  by `readSecretsAgain`. An unreadable file
  makes the value unavailable, and the consuming node job is `blocked`
  (`preparationFailure`). Every other origin stays unavailable and names its
  producer.
- A `save_on_controller_machine` path is public: a path that would hold a
  secret is refused during loading when `secretOutputs` identifies the output
  as secret.
- A value in a `save_on_controller_machine` path that may come from a node,
  any reference whose root is not `vars`, `inventory`, `node`, or a loop
  variable that `ProjectLoopVariables` lists (`declareLoopItems` lists a
  loop whose every reference reads `vars`, `inventory`, or such a loop
  variable, and every `for_each_node` loop), fills part of one file name.
  A run saved without the list trusts no loop item. `config.NodeValuesStayInOneFileName` refuses such a value
  with `/` or a NUL byte, and a rendered path with a `.` or `..` part, both
  where the run writes the file and where `mmx explain` shows it.
  The
  controller process always refuses the unreadable file secret when rendering
  the secret.
  A file that could not be written keeps its loop iteration unfinished
  (`sequenceOutcome`), so resume writes the file.
- On the node, text from `.outputs` with a control character other than a
  tab before its trailing line breaks never goes inside other text: the
  engine calls `Scope.GuardOutputLines`, `RoleScope` passes the guard on
  (`InheritOutputLines`), and `expand` and `Scope.Template` refuse a read
  whose value holds such text (`checkOutputLines`). The guard matches the
  text itself, so a role input, loop item, or variable that passes it on
  whole is guarded too. A whole reference passes the value unchanged. The
  controller process sets no guard: its `save_on_controller_machine`
  templates may collect multi-line outputs.
- A condition error never quotes a secret: `value.describe` names such an
  operand "a secret value".
- `value.Binary` is the one form of content that is not UTF-8 text.
  `Scalar` passes `value.Binary` whole.
  A reference inside other text and a template refuse `value.Binary`.
- Module results cannot contain the reserved record keys (`$mmx_secret`,
  `$mmx_truncated`, `$mmx_binary`). The restriction prevents a module from
  forging an internal record. For the same reason `value.ParsedJSON` reads
  JSON holding such a key as `NotJSON`, `config.ExportOutputs` refuses an
  output holding one (`value.ReservedKey`, which also looks inside a
  secret's data), and a read-file answer holds only text or `value.Binary`.
- A node job's outputs are data the node chose, even when they decode to
  secret records. `fleetStageWalker.publish` passes a node job's outputs
  through `trustNodeRecords` before anything loads them: a record keeps its
  name only when `wasSent` finds that the node job received that part of the
  named secret whole (`nodeJobSecrets` computes delivery again from the scope
  the node job was built from), and a `read_file` origin only when it names
  the node job's own fleet stage and node (`ownOrigins`, which
  `secretsStillNeeded` applies too). Any other record keeps the node's data
  with the node job as its origin, so a resume reports it unavailable.

## Task trees

`config.Task.Children` lists a task's nested task lists (role tasks and
node task block phases) and `config.TaskName` names a task.
Every static walker uses `config.Task.Children` and `config.TaskName`.
Add a new phase or child list to `config.Task.Children`.
`config.WalkFleetStages` visits fleet stages with their loop path.
Walkers carrying scope per level (`prepareFleetStages`, `Inspect`, the
controller's fleet stage walker) keep their own recursion.

## Task arguments

Each module's `Arguments` method is the one rule table for the module's
reference-capable arguments.
`config.taskArguments` gathers the arguments with the module's name.
Load applies each argument rule to literal values in every branch.
`config.InspectArguments` applies each argument rule to resolved values during
validation and explain.

`config.ResolveArguments` resolves public identifiers once as
literal data, applies the module's argument rules, and marks the task
`Resolved`.
The node task execution engine calls `config.ResolveArguments` before the
module's resource implementation runs. Then `InspectArguments` checks private
values.
Resource implementations receive resolved identifiers and never resolve or
check public identifiers again.
`ResolveArguments` cleans a resolved path, so `filledValueWithParentPart`
refuses a public identifier into which any reference fills text with a `..`
part: vars, role inputs, and loop items can all carry an output. A value
from another node then adds directories below the path a project writes but
never climbs out of it. A `..` written in the project's text stays. Modules
written in Starlark or as executables get their inputs as data and check a
path they build from an input themselves. Resource implementations resolve only private
values (content, templates, environment, module inputs) through the scope.

`Validate*` functions, called from each
module's `Prepare`, check structure only:
field combinations, states, and static fields. Put checks joining a static
field and a reference-capable argument in the argument's rule, chosen in the
module's `Arguments`. Resolved values then receive the argument's checks too.
A rule checking text with a parser uses `parsed`.
The optional `CheckSecretInputs` hook runs
during reference validation, when the loader knows which role inputs hold
secrets. The `CheckSecretInputs` callbacks identify secret reads and unresolved
whole-value fields. The module checks the access rule without resolving or
reading secret values.
`localSource` is the one check of a static controller-machine path:
`source`, `template`, and `signing_key_source`.
`validatePin` is the one check of a pinned URL and its SHA-256 (`download`,
`unpack_archive`, a repository's signing key).
A `download` loads as a `File` with a pinned URL (`Download.ModuleInputs`).
The task's `Module` keeps the name `download`.

An `unpack_archive`'s `url` or `source` freezes through the same `visitSource`
in `fileAssets`.
One URL and digest is fetched once per run, whichever task names the URL and
digest.
After `assets.Verify`, `assets.Reuse` takes an asset matching the pinned
SHA-256 digest from an earlier run's asset directory instead.
An unchecked or decompressed download is always fetched.
An unchecked download has no pinned digest. A decompressed asset is not the
published file that the digest identifies.
Role input defaults are
literal data; references come only through the caller's `inputs`.

A core module input whose type is not text (an integer, boolean, list, or
mapping) takes one whole-value reference through one path, never per module.
`checkTypesOf` accepts a whole-value reference in module inputs.
`Task.UnmarshalYAML` moves the reference to `Task.WholeValueReferences` and
leaves the type's zero value in the module input field.
`taskArguments` lists the reference first as a public argument.
The public argument's rule checks the non-text module input field's type
with the loader's `checkTypes`.

`ResolveArguments` puts each value into the module's inputs before it
resolves the module's other arguments. An argument below a field that a
whole value filled is never resolved again: the value is data, substituted
once. The argument rule checks the substituted data without resolving nested
text as references. Text resembling a reference remains text.

During project loading, the module's `Prepare` sees the zero value left for
each whole-value field. A preparation error naming a whole-value field is
deferred (`waitsForWholeValue`).
`checkModuleWithWholeValues` runs `Prepare` on a resolved copy wherever
inspection knows the values.

An input naming files read by `Prepare` on the controller machine must carry
the struct tag `mmx:"controller_machine_files"`
(`modules.ControllerMachineFiles`).
Inputs naming controller-machine files accept no whole-value reference.
Refuse a deferred `Prepare` error in a task naming controller-machine files,
because the failed preparation stopped before reading the files.
A value known only on the node gets the type check on the node.

## Module descriptions

Each core module's description sits in its own file, and the registry points
to the description. `config.descriptions` holds the node task forms that
are not modules
(`role`, `task_block`, `show`, `set_vars`, `task`). A description holds only
what validation cannot supply: summaries, field notes, examples, result
fields, and dryrun text.

Field names and types come from the structs strict
decoding reads (`yamlFields`). Allowed values come from shared lists checked
by `Validate*` functions. Retry defaults come from the registry.
`describe_test.go` fails until each registered module or field has an entry,
every example validates, and each field marked required is required by
validation.

Each core module's description names a topic from `modules.CoreModuleTopics`.
Topics group modules in the module-page index and `mmx describe-module` listing.
The registry lists core modules topic by topic, in the order of
`modules.CoreModuleTopics`.

The module's page source,
`internal/modules/<module>.md`, sits beside its Go file. `go generate
./docs/reference/modules` joins the module's description and Markdown page source into
`docs/reference/modules/<module>.md`, which the
package `mmx/docs/reference/modules` embeds for `mmx` alone; `mmx-runner` never imports
`mmx/docs/reference/modules`. Tests reading Markdown or source files live in
`docs/reference/modules/generator/`, because the Linux code suite runs test programs of
`./internal/...` and `./cmd/mmx` on guests without the source tree.
The [documentation map](documentation-map.md#module-pages) owns the
page rules.

## Source locations

YAML locations name files relative to the project directory.
Saved definitions and node jobs store each definition's file and base field
once (`Sources` JSON).
The CLI finds `mmx_project_settings.yml` in the current directory or its
parents. Named playbooks and inventories load from `playbooks/<name>.yml`
and `inventories/<name>.yml` at the project root.
Local source and template paths resolve from the project root, or from the
role directory inside a role.
State saved by the controller process lives in the project root's `.mmx/`.

## Validation follows execution

`config.Inspect` walks node tasks in the node task execution engine's order
and variable scopes, sharing rules through single functions.
`config.NodeVars` and `config.InventoryValues` build node scope values for
inspection and node jobs prepared by the controller process.
`config.NodeVars`, `config.InventoryValues`, `config.VariableOrigins`, and
the reference check's variable names all read
`config.varLayers`, the one value order (project, inventory, groups, node,
command line). `.inventory.nodes.<name>.vars` holds only node-specific
variable layers
(`nodeLayers`: groups, then node), since every job carries `.inventory` and the
shared layers are already in `.vars`.

`config.NodeGroups` is the one
membership lookup, and `everyNodeLayer` the one list of every node's per-node
layers, which a runtime-selected node and the command-line variable check
read. `config.AssignVars`
applies `set_vars` during inspection and execution and records each assigned
value's source location. `config.RoleScope` and
`config.ResolveRoleInputs` build role scopes. Node task block phases share the
node task block's scope. Cleanup has the node task block's status, and
`on_failure` is conditional.

A node task block's entry scope contains the variable values available before
any phase runs. Check values fixed before the phases in the entry scope.
A node task block resolves every public argument of tracked node tasks in the
entry scope. Tracked tasks run with exactly the values hashed at entry.
If a tracked task's entry-scope `when` is false and arguments cannot resolve,
leave the task out (`config.Task.WhenWasFalseAtBlockStart`).

The left-out tracked task fails if `when` becomes true when execution reaches
the task in written order. Validation leaves out a tracked node task whose
`when` is known locally to be false (`knownToBeFalse`).
Dryrun predicts service hashes from the tracked tasks that ran,
each in the task's scope, applying a file's `line` edits in turn.

`config.EntryNodeTasks` is the one list of what a node task block fixes as the
block starts: tracked changes and activate's services.
Tracked loops have items resolved in the entry scope.
The node task execution engine resolves exactly what `config.EntryNodeTasks`
lists.
Every remaining task, including loops, evaluates in written order.
A tracked loop runs over the items fixed at entry.

Validation compares tracked tasks' public arguments resolved at block entry
with arguments resolved after preceding tasks and assignments in written order.
Validation also rejects statically known duplicate tracked paths.
`config.TrackPath` is the one duplicate tracked-path rule, applied during
validation and again in the node task execution engine.
Several `line` and `edit_file` tasks may edit one file, which counts once.
Any remaining repeat is an error, except with a task whose `when` is false
as the node task block starts.
A file, link, or unit owns the whole path.
Validation counts a task only when its `when` is known locally to be
true.

The node task execution engine evaluates `when` in the node's entry scope
and counts an undecidable `when` condition as true.
The node task execution engine hashes a shared path as the task that may run
writes the path.

`engine/inspect_equivalence_test.go` runs fixtures through inspection and
dryrun.
Add a fixture to `engine/inspect_equivalence_test.go` for a new scoping rule.
Outputs and `set_vars` share `value.Scope.Evaluate`.
A predicate form becomes a boolean.
Anything else resolves as data.

`assert.that` is a condition like
`when`: `checkPredicate` checks its form, `inspectPolicies` its local values,
and the engine evaluates it with `Scope.Predicate`.
`Scope.Trace` copies share their
source's variables, results, and availability marks, so a mark made inside a
task reaches the caller of the task.

`config.SourceError` is the one form of
a value error. The error names the consuming YAML field.
When a value comes from elsewhere, the error also names where the value was
supplied.
Validation, the node task execution engine, and the controller's fleet stage
walker use `config.SourceError` for arguments, conditions, loops, selections,
and outputs.

## Selection and limits

`config.Select` resolves every selector. `config.SelectNodes` is the one
selection of a fleet stage (`nodes` minus `except_nodes`; a `for_each_node`
loop selects through `config.SelectNodes` too).
`config.SelectNodes` takes the fleet stage and alone applies
`skip_when_no_nodes_are_selected`. Without the setting, an empty selection is
an error. With the setting enabled, `config.SelectNodes` returns an empty node
list without error. `Inspect`, `Controller.New`, and the fleet stage walker
record the empty selection as `LeftOutBecauseItSelectsNoNodes`.

A fleet stage recorded as `LeftOutBecauseItSelectsNoNodes` marks nothing left
out, so the fleet stage's requirements and outputs stay readable.

`config.Limit.Narrow` is the one
intersection with `--limit`, used by `config.Inspect`, `Controller.New`,
`config.SecretNamesARunReads`, and the fleet stage walker.
`Project.KeepsFleetStage` is the one `--only-fleet-stages` test.
`config.Inspect` is the one local view of each fleet stage's selection
(`explain`, `nodes`, a limited `dryrun` or `run`).
`config.Inspect` is also the one up-front limit and `--only-fleet-stages` check.

`--var NAME=VALUE` sets only names declared in the chosen
playbook's `command_line_vars`; a declared name cannot also appear in any
`vars`. A command-line variable's value therefore comes from `--var` or the
variable's default.
Project-wide `validate` and inventory-only `nodes` reject `--var`.

Project-wide `validate` loads each playbook with
`Project.WholeProjectValidation`. A command-line variable without a default
is unknown, so `registerSources` leaves source fields reading the variable
unchecked.
Every command naming the playbook requires a command-line variable's value
when a source field reads the variable.

A run saves the run's limit and each selection's left-out nodes.
The saved project carries `--var` values (`Project.CommandLineValues`) and
`--only-fleet-stages` IDs (`Project.OnlyFleetStages`).
Resume reuses the saved limit, left-out nodes, `--var` values, and
`--only-fleet-stages` IDs.
Resume never takes a new limit, `--only-fleet-stages`, or `--var`.

A resumed node job starts the fleet stage's node tasks from the beginning,
using the saved playbook, inventory,
and file contents. Finished nodes are not rerun.
A left-out node's outputs and outcomes
are marked unavailable, so work reading them is blocked, naming the limit; a
fleet stage `--only-fleet-stages` leaves out gets no selection, and its
outputs and outcomes are marked unavailable, naming the option.

## Ad hoc commands and privilege

`mmx run-command` is an ordinary run.
`config.LoadRunCommand` builds the command's project
with one fleet stage
from the project's settings and inventory, and the CLI runs the command through
`Controller.New` and `Execute`. `mmx run-command` has no separate execution,
resume, secret, or history path.
Runs of `mmx run-command` record `controller.RunCommandEntryPoint` as the entry
point, which alone keeps command runs out of default run selection.
`Load` records the inventory name in `Inventory.Name`.
`Inventory.Path` holds the inventory's source file path.

Run identity includes the project, playbook name, and inventory name. Without a
run ID, `status`, `stop`, and `results` select the latest playbook run for the
selected inventory, across playbooks. `resume` selects the latest non-dryrun
playbook run for the selected inventory, across playbooks.
`status`, `stop`, `results`, and `resume` report the selected run and playbook.
A short or full run ID selects any retained run, including a `run-command` run.

Node tasks run as root. For `shell.run_as_user_account` and
`git.run_as_user_account`, `shellAccount` builds an account credential and
passes the credential to `runProcess`. The credential is the single mechanism
for running a child process as another user.
Every process a node task starts goes through `runProcess`.
`runProcess` starts the child process with umask 022 (`taskUmask`), as systemd
starts its services.
The node job worker process runs with umask 077 for its own files.

A checkout, a script's directory, or software installed for the whole node
must remain readable by the accounts using the installed files.
Code creating a file MMX means to keep private must set the file's mode
explicitly.
Resolve an account to IDs, a home, or a group name through Go's `os/user`.
In the cgo-free `mmx` and `mmx-runner` executables, `os/user` reads
`/etc/passwd` and `/etc/group`
(`shellAccount`, `metadataFor`, `authorized_keys`).
Only `user` and `group` management inspect accounts with `getent`.

## Output bounds

Every captured command stream, HTTP body, and module result goes through
`modules.capBuffer` (1 MiB, equal to `moduleio.MaxResultBytes`), which records
truncation; predicates reject truncated text, and an oversized module result
fails its task. `Engine.record` enforces one retained-result budget per job,
measured as status-answer JSON. `Engine.record` keeps full records while the
retained total fits within 16 MiB.
Then outcome-and-reason records state their omission.
The retained-result budget leaves room in the status answer for fleet stage
outputs and the facts snapshot.

Retries keep the first and latest nine tries,
with `tries_omitted` counting omitted attempts.
Evaluation reads actual results, never the retained records.

A failed start, restart, or reload of a unit gains the unit's journal lines
only through `UnitManager.JournalAfterFailedAction` (`modules/systemd.go`),
with a `JournalNow` reading taken before the action.
`UnitManager.JournalAfterFailedAction` keeps lines received since the pre-action
reading, bounds each line and the whole text, and has its own time limit.
`UnitManager.JournalAfterFailedAction` returns no text instead of an error, so
journal collection never changes whether a task fails.
The collected journal text is appended to `Reason`, which
`Result.Safe` hides for a secret result.

## Resources and dryrun

Resource code in `internal/modules` receives a small `Runtime`, never the
node task execution engine or saved-run state.
Resource code calls `Runtime.Mutation` before any managed change.
Dryrun refuses managed changes at `Runtime.Mutation`.
The primitives in `primitives.go` (`ReadFile`,
`InspectPath`, `LookUpAccount`, `RunCommand`, `HTTPRequest`, `SHA256`;
`WriteFile`, `MakeDirectory`, `MakeLink`, `RemovePath`,
`RunCommandThatChangesTheNode`) are the `Runtime` methods a module reads and
changes the node with; the changing ones are built on the `file`, `directory`,
and `symlink` code and on `Mutation`. During dryrun, `WriteFile`,
`MakeDirectory`, `MakeLink`, `RemovePath`, and `RunCommandThatChangesTheNode`
report predicted changes and change nothing.

A write that is MMX's own bookkeeping, such as a run-once marker or apt's
service-policy program, goes through the changing primitives with `RetainDiff`
cleared, so the write adds no difference to the task's result.

The only direct writes after `Mutation` are to kernel files that a rename
cannot replace: the running SELinux mode in `/sys`, and a setting under
`/proc/sys` (`sysctl_setting`'s `writeKernelSetting`). When something an earlier task may
still provide is missing, such as a package, account, or file, the resource
implementation returns `Runtime.notYet`.
The result is not checked in dryrun and failed in a run.

`accountFailure` applies the missing-prerequisite result to an unknown user
or group. `missingParent` handles a missing parent of a path a module would
create. Return a missing-prerequisite result unless an earlier dryrun node
task would create the parent directory.
`missingRunFromDirectory`
returns a missing-prerequisite failure when `run_from_directory` does not
exist, before starting a process in the directory, in a run or dryrun.
`missingRunFromDirectory` returns not checked instead
when an earlier node task of the dryrun would create the `run_from_directory`
directory.
A module deciding from a file an earlier dryrun node task would change also
returns `Runtime.notYet` (`Runtime.EarlierNodeTaskWouldChange`).
Examples include a lock file or a pip requirements file in a checkout.

`Engine.checkInADryrun` is the one place converting a failed check or missing
prerequisite to `not_checked` in a dryrun when an earlier node task would change
the node.
An earlier node task may belong to an earlier fleet stage on the current
node. The controller process passes earlier-fleet-stage predictions in
`config.Job.EarlierChangeInThisDryrun`.

Service records refuse dryrun writes.
Task policies (conditions, retries, timeouts, stopping) belong to the node task
execution engine.

After a node task changes managed paths, the node task execution engine passes
each path to the node job's package manager (`PathChanged`).
The package manager alone decides which paths invalidate the package manager's
state (apt: `/etc/apt/`).
An `apt_repository` task is the package manager's own (`Source`), which marks
the package manager's state stale when the task changes a source or key.

A node task block marks each service record with the node job before any
change and clears the mark once the block's `activate` phase succeeds.
A new service instance gets a fresh baseline unless the service record is
still marked and an input of the current node task block that the record holds
has changed since.

In the baseline decision, an input of a non-core module counts as changed,
because only the module's task can determine the input's state.
A start or restart loads everything on disk, so the start or restart saves
only the inputs of its own node task block in the record, and module
fingerprints, which have no baseline on disk.
A reload keeps all recorded inputs outside the current node task block.

For a module that is not core, the node task block saves
`unreportedFingerprint` in a record lacking the module's fingerprint before
the block's first write (`awaitFingerprints`).
If the module's task changes nothing, the node task block takes the reported
fingerprint as the baseline
(`takeFingerprintBaselines`).
After `activate` succeeds, `forgetUnreportedFingerprints` removes unresolved
placeholders for modules that changed nothing.

Service records and command records use `awaitFingerprints`,
`takeFingerprintBaselines`, and `forgetUnreportedFingerprints`.
`do_not_start_or_stop` removes the record of a unit that is stopped or does not
exist (`oweNothing`). When the unit starts, the unit loads the files then on
disk.
