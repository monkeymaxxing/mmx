# Upgrade MMX

**Who acts:** you, or your coding agent, on each controller machine: the
computer where you run `mmx`, such as your laptop or a CI runner. You do
nothing on your servers by hand: the first run after the upgrade updates
MMX there.

**When to use this page:** a new MMX release is out and you want it, or you
want to go back to an earlier release.

**What this page does:** you read what changed, replace the `mmx` that you
installed with the new release, and check your project with it. The page
then says what the first run after an upgrade does on each server, what
happens to saved runs and `mmx resume`, what a team or a CI job on different
releases sees, and how to go back to an earlier release.

**Terms:**

- A **node** is a Linux server that MMX manages. A **playbook** lists the
  work that each node does. The examples use a project in the directory `shop`, whose
  `project_name` is `mmx_example`, the servers `web01` at `203.0.113.11` and
  `web02` at `203.0.113.12`, and the playbook `set_up_web_servers`.
- A **release** is one version of MMX, such as 0.1.1. A **release archive**
  holds three things of one release: `mmx`, `mmx-runner`, and the
  official extra modules, which are modules that MMX ships as scripts
  beside `mmx`.
- **`mmx-runner`** is MMX's program on each node. MMX installs it on the
  node, and runs it as the systemd service `mmx-https-api.service`, which
  `mmx` reaches over HTTPS on port 7443.
- A **node job** is the work of one fleet stage of a playbook on one
  node. `mmx-runner` carries out the node job on the node.
- A **run** is one `mmx run`, `mmx dryrun`, or `mmx run-command`, with
  everything that later `mmx resume` commands added to it. MMX saves each
  run in the project's `.mmx/runs/` directory on your computer.

Choose your route:

| Your situation | Go to |
| --- | --- |
| You want a newer release | [Steps](#steps) |
| A run stopped or failed before you upgraded, and you want to finish it | [Finish a run that an earlier release started](#finish-a-run-that-an-earlier-release-started) |
| Teammates or a CI job run the same project | [A team or CI on different releases](#a-team-or-ci-on-different-releases) |
| You want to go back to an earlier release | [Go back to an earlier release](#go-back-to-an-earlier-release) |
| You want to know whether a release needs edits to your project | [What a version number tells you](#what-a-version-number-tells-you) |

## Before you start

- You installed `mmx` as [Get started](../get-started.md#steps) step 1
  says: the files of the release in `~/.local/lib/mmx`, and the link
  `~/.local/bin/mmx` to `~/.local/lib/mmx/mmx`. `mmx version` prints the
  release you have, such as `mmx 0.1.0 (protocol 1)`.
- If a run stopped or failed and you want to finish it with `mmx resume`,
  finish it now. A run that an earlier release started can be resumed only
  with that release; see
  [Finish a run that an earlier release started](#finish-a-run-that-an-earlier-release-started).
- No run of the project is working on a server right now, from your
  computer or from a teammate's or a CI job. The first run after the
  upgrade replaces `mmx-runner` on each server, and a server where a node
  job is working refuses the new `mmx-runner` until the node job ends.
- No change of a network file is waiting for a restore on a server. A server
  that still owes a restore refuses the new `mmx-runner` until the restore
  is resolved; see
  [A restore that did not finish](change-ssh-or-network-settings-safely.md#a-restore-that-did-not-finish).

## Steps

1. **Read the release notes of the new release.** Each release has a
   page at
   [github.com/monkeymaxxing/mmx/releases](https://github.com/monkeymaxxing/mmx/releases),
   such as `v0.1.1`, with its notes and its files. MMX has no separate
   changelog file.

   **Check:** you know whether the new release is a patch release or a
   minor release ([What a version number tells you](#what-a-version-number-tells-you)).
   For a minor release, note each incompatible change that the notes list,
   and the edit that it needs in your project. You make the edits in
   step 4.

2. **Download the release archive for your computer and its checksum
   file, and check the download.** In an empty directory, set the new
   release and your platform:

   ```sh
   version=0.1.1
   platform=macos-arm64      # or macos-amd64 (Intel Mac), linux-amd64, linux-arm64
   ```

   Then run the `release=` line, the two `curl` lines, and the checksum
   command from step 1 of [Get started](../get-started.md#steps). Download
   with `curl`, not a web browser, on a Mac.

   **Check:** the checksum command prints `OK` after the archive's name,
   such as `mmx-0.1.1-linux-amd64.tar.gz: OK`. If it prints `FAILED`, the
   download is damaged: download the archive again.

3. **Replace the installed release with the new one, and keep the earlier
   one beside it.**

   ```sh
   tar -xzf "mmx-$version-$platform.tar.gz"
   rm -rf ~/.local/lib/mmx-previous
   mv ~/.local/lib/mmx ~/.local/lib/mmx-previous
   mv "mmx-$version-$platform" ~/.local/lib/mmx
   mmx version
   ```

   - Replace the whole directory, not only `mmx`: `mmx` refuses to work
     with an `mmx-runner` or official extra modules of another release, and
     finds both beside itself.
   - `~/.local/lib/mmx-previous` keeps the earlier release, so you can
     still finish a run that the earlier release started, or go back. The
     `rm -rf` line removes the release that an upgrade before this one
     kept there; without it, `mv` would move the directory into the old
     one instead of renaming it.
   - The link `~/.local/bin/mmx` points to `~/.local/lib/mmx/mmx`, so the
     link stays as it is.

   **Check:** `mmx version` prints the new release, such as
   `mmx 0.1.1 (protocol 1)`. `protocol 1` numbers the format of the
   messages between `mmx` and `mmx-runner`; you don't need to compare it,
   because `mmx` checks the release of each `mmx-runner` itself.

4. **Check the project with the new release.** In the project directory:

   ```sh
   mmx validate
   ```

   `mmx validate` reads the project files on your computer, including
   every playbook, and contacts no server.

   **Check:** the command prints `Valid: mmx_example` and exits with code 0.
   Otherwise it names the file, the line, and the field that is wrong. Make
   the edit that the release notes give for that change, and run
   `mmx validate` again.

5. **Preview your playbook.** This first command that works on your
   servers also updates MMX on each server that the playbook selects:

   ```sh
   mmx dryrun set_up_web_servers
   echo "exit code $?"
   ```

   ```text
   Run 62c82c34
   Project: mmx_example   Playbook: set_up_web_servers
   Inventory: example (2 nodes)   Selected: 2 nodes
   Output: nodes (change with --output=nodes or --output=summary)
   Progress: 2 not started
   203.0.113.11: setting up MMX through verified SSH
   Progress: 1 finished · 1 running
     set_up_nginx / web02: connecting
   203.0.113.12: setting up MMX through verified SSH
   Progress: 1 finished · 1 running
     set_up_nginx / web02: running
   set_up_nginx / web01: succeeded
   set_up_nginx / web02: succeeded
     set_up_nginx / web01 / install_nginx: succeeded, unchanged
     set_up_nginx / web01 / write_home_page: succeeded, unchanged
     set_up_nginx / web02 / install_nginx: succeeded, unchanged
     set_up_nginx / web02 / write_home_page: succeeded, unchanged
   Run 62c82c34: succeeded
   2 finished
   Saved: /home/you/shop/.mmx/runs/20261115T073831.828974051-62c82c3438ad0bc0
   exit code 0
   ```

   `setting up MMX through verified SSH` means that MMX logged in to that
   server over SSH and put the new `mmx-runner` in place.
   [What the first run after an upgrade does](#what-the-first-run-after-an-upgrade-does-on-each-server)
   explains it. A second dryrun prints no `setting up` lines.

   The setup logs in over SSH, so MMX may ask for the SSH password, or for
   the `sudo` password when the login account needs one. A coding agent
   can't answer a question: it runs the command with `--non-interactive`,
   which fails instead of asking, and gives MMX the password as
   [Passwords for automation](connect-to-your-servers.md#passwords-for-automation)
   shows.

   **Check:** each server that still had the earlier release has a
   `setting up` line, except a server with `connection: ssh_only` or a run
   with `--connection ssh_only`, which print no such line. The run ends with
   `Run ...: succeeded`. Exit code 0 means nothing would change, 2 means
   something would change, and 3 means the dryrun could not check some
   work: read the results, as
   [Preview and run changes](preview-and-run-changes.md#what-the-results-mean)
   explains, before you run the playbook. If a server fails, find its
   message in [If something is different](#if-something-is-different).

6. **If others run the project too, upgrade their controller machines as
   well:** your teammates' computers, and the release that your CI job
   installs. Reason:
   [A team or CI on different releases](#a-team-or-ci-on-different-releases).

   **Check:** `mmx version` prints the same release on each controller
   machine.

## What the first run after an upgrade does on each server

The first `mmx dryrun`, `mmx run`, or `mmx resume` after an upgrade that
does work on a server prints `setting up MMX through verified SSH` for that
server, except a server with `connection: ssh_only` or a run with
`--connection ssh_only`: there MMX replaces `mmx-runner` over SSH without
that line. The setup then goes on:

- MMX logs in over SSH, after OpenSSH checks the server's SSH host key.
- MMX copies the new `mmx-runner` to the server, into its own directory
  under `/opt/mmx/bin/`, named by the program's SHA-256 hash. The server
  keeps the new `mmx-runner` only if its SHA-256 hash matches the hash that
  `mmx` sent.
- MMX restarts `mmx-https-api.service` with the new `mmx-runner`. From then
  on, `mmx` reaches the server over HTTPS again when port 7443 is open, and
  later runs print no `setting up` line.
- The earlier release's `mmx-runner` stays in its own directory under
  `/opt/mmx/bin/`.
- A server with `connection: ssh_only` has no HTTPS service. MMX installs
  the new `mmx-runner` over SSH in the same way.
- A server with `mmx_runner_download_url` in the inventory downloads the
  new `mmx-runner` from your web server. Copy the new release's
  `mmx-runner` files there first, as
  [Runner downloads for a large fleet](connect-to-your-servers.md#runner-downloads-for-a-large-fleet)
  step 1 shows. MMX does not send the file over SSH instead.

`mmx` works only with an `mmx-runner` of its own release. It asks each
`mmx-runner` for its release before it sends any work, and a server whose
`mmx-runner` reports another release fails before any change.

## Saved runs and `mmx resume` across an upgrade

The saved runs in `.mmx/runs/` stay where they are. `mmx results` and
`mmx status` show a run that the earlier release made, as long as the new
release can read the saved run.
[If something is different](#if-something-is-different) says what to do
when it can't.

### Finish a run that an earlier release started

Finish or stop a playbook run before you install a new release.
`mmx resume` refuses a run that another release started. It checks the
release before any work, names the release that started the run, and
contacts no node. Here a run that 0.1.0 started stopped while `web01` was
working, before `web02` started. After the upgrade to 0.1.1:

```sh
mmx resume 4ae99527
echo "exit code $?"
```

```text
mmx: this playbook run was started by MMX 0.1.0, but this mmx program is version 0.1.1. A run finished by two MMX releases can leave a node's last-successful record skipped. Resume the run with mmx of release 0.1.0 (such as the previous install, if you kept it), or start a new run with mmx run
exit code 4
```

The refusal is there because a run that two releases finish can leave a
server's record of
[when the playbook last succeeded](stop-resume-and-read-a-run.md#when-a-playbook-last-succeeded-on-each-server)
unwritten, and nothing would tell you. A refused run stays as it was.

Finish the work in one of two ways:

- **Resume with the release that started the run.** Step 3 kept it in
  `~/.local/lib/mmx-previous`:

  ```sh
  ~/.local/lib/mmx-previous/mmx resume 4ae99527
  echo "exit code $?"
  ```

  ```text
  Run 4ae99527
  ...
  203.0.113.12: setting up MMX through verified SSH
  ...
  set_up_nginx / web01: succeeded
  set_up_nginx / web02: succeeded
  ...
  Run 4ae99527: succeeded
  2 finished
  exit code 0
  ```

  The resume uses the files that the run saved, as any resume does
  ([Resume a run](stop-resume-and-read-a-run.md#resume-a-run)), so choose
  this way when you changed the project files since the run and want the
  run's own files. A resume with the earlier release may put the earlier
  `mmx-runner` back on a server. Your next run with the new release then
  puts the new one in place again, with a `setting up` line.
- **Start a new run with the new release:** `mmx run set_up_web_servers`.
  The new run uses the project files as they are now, and works on every
  server again, also on servers that had finished. Most node tasks then say
  `unchanged`; a `shell` node task without a guard runs its command again.

## A team or CI on different releases

Each server runs the `mmx-runner` of the release whose `mmx` connected
last, so controller machines on different releases replace `mmx-runner` at
each other's runs
([Move or share a project](move-or-share-a-project.md#share-the-project-with-a-teammate),
step 3, explains why).

So upgrade every controller machine that runs the project together:

- **Teammates:** each teammate does the [steps](#steps) on the teammate's
  own computer.
- **CI:** a CI job that installs `mmx` at its start names the release,
  such as `version=0.1.1` as in step 2. Change that line when you upgrade
  your own computer.
  [Run the project from CI](move-or-share-a-project.md#run-the-project-from-ci)
  covers the rest of a CI job.

Saved runs stay with the controller machine that made them, so an upgrade
on one computer does not change the saved runs on another.

## Go back to an earlier release

Do the [steps](#steps) with the earlier release's version, such as
`version=0.1.0`. The first run afterwards puts the earlier `mmx-runner`
back on each server, with a `setting up MMX through verified SSH` line, as
after an upgrade.

- A project file that uses something that only the later release has may
  not validate on the earlier release. Step 4 shows it. Undo that edit, for
  example with Git.
- The earlier release can't finish work that the later release began on
  a server, as in
  [Finish a run that an earlier release started](#finish-a-run-that-an-earlier-release-started).
  Start a new run.
- The earlier release may be unable to read a saved run that the later
  release wrote. [If something is different](#if-something-is-different)
  says what to do.

## What a version number tells you

MMX numbers its releases by [semantic versioning](https://semver.org/).
The first release is 0.1.0. The version number of a new release says
whether the upgrade can need an edit to your project:

- **A patch release, such as 0.1.1 or 0.1.2, only fixes faults.** A
  project that works on the earlier release works unchanged.
- **A minor release, such as 0.2.0 or 0.3.0, may make incompatible
  changes.** Its release notes list each incompatible change, with the
  edit that a project needs. New commands, modules, and fields also come in
  minor releases.

A change is incompatible when, after it, a project file that was valid on
the earlier release no longer validates or does something else, or when a
command, a flag, or a result field has a new name or is gone. The project
files are the project settings, the playbooks, the inventories, the roles,
the project's own modules, and the secrets vault.

MMX's version stays below 1.0.0 while real use may still show that a name
or a file format should change. From 1.0.0 on, only a new major version,
such as 2.0.0, may make an incompatible change.

## If something is different

| What you see | What it means and what to do |
| --- | --- |
| `mmx: the official extra modules in /home/you/.local/lib/mmx/official_extra_modules were shipped with MMX 0.1.0, but the mmx program is version 0.1.1; reinstall the mmx program and official_extra_modules/ from a single MMX release` | Only `mmx` was replaced, not the whole directory. Do step 3 again with the whole directory from the archive. |
| `the node runner executable /home/you/.local/lib/mmx/mmx-runner-linux-amd64 reports MMX version 0.1.0, but this mmx program is version 0.1.1. Install mmx together with the mmx-runner files of the same MMX release; a release archive holds both` | The `mmx-runner` files beside `mmx` come from another release. Do step 3 again with the whole directory from the archive. |
| `mmx: this playbook run was started by MMX 0.1.0, but this mmx program is version 0.1.1. ...` after `mmx resume` | The run was started by another release, and `mmx resume` changed nothing. See [Finish a run that an earlier release started](#finish-a-run-that-an-earlier-release-started). |
| `mmx: history cleanup: MMX cannot read this saved run, which another version of MMX may have written: /home/you/shop/.mmx/runs/... (...). History cleanup removes nothing while a saved run cannot be read, because that run may use a cached runner binary. Remove that run directory, then run again.` | The release you now have can't read that saved run, and so removes no expired runs. The command itself goes on. Remove the run directory that the message names, such as with `rm -rf`, if you no longer need that run. To keep it, read it first with the release that wrote it, such as `~/.local/lib/mmx-previous/mmx results` and the run ID. |
| `automatic setup of the node runner executable from http://203.0.113.40:18473/mmx/mmx-runner-98c1d9...: ... curl: (22) The requested URL returned error: 404` | The server downloads `mmx-runner` from your web server, which does not have the new release's files yet. Copy them there, as [Runner downloads for a large fleet](connect-to-your-servers.md#runner-downloads-for-a-large-fleet) step 1 shows, and run again. |
| `node busy with job ...` while MMX sets up a server | A node job of another run is working on that server. See [Move or share a project](move-or-share-a-project.md#if-something-is-different). |
| `cannot replace the node runner executable while owed restore unit ... exists; resolve the owed restore first` | The server still owes a restore of a network file. Resolve the restore, as [A restore that did not finish](change-ssh-or-network-settings-safely.md#a-restore-that-did-not-finish) says, and run again. |
| Each run prints `setting up MMX through verified SSH` again | Another controller machine runs the project with another release. See [Move or share a project](move-or-share-a-project.md#if-something-is-different). |
| A run after the upgrade asks for the SSH or `sudo` password, or fails with `(prompts are disabled)` | The setup of the new `mmx-runner` logs in over SSH. Give the password, or set it up for automation as [Passwords for automation](connect-to-your-servers.md#passwords-for-automation) shows. |

## Where to go next

- [Get started](../get-started.md): the first install of `mmx`.
- [Stop, resume, and read a run](stop-resume-and-read-a-run.md): saved
  runs, `mmx resume`, and how long MMX keeps runs.
- [Move or share a project](move-or-share-a-project.md): what each
  controller machine keeps, and running the project from CI.
- [Connect to your servers](connect-to-your-servers.md): passwords, HTTPS
  on port 7443, and runner downloads for a large fleet.
