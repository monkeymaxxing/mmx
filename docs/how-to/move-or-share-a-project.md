# Move or share a project

**Who acts:** you, or your coding agent, on each controller machine: the
computer where you run `mmx`. In the route for a teammate, each step names
who acts, you or your teammate. The examples call the teammate Barbara.

**When to use this page:** you move your MMX project to another controller
machine, such as a new laptop or a CI runner. Or a teammate starts to run
the same project from the teammate's own computer.

**What this page does:** you learn which files go into Git and which stay
on each controller machine. Then you give a second controller machine what
Git does not carry: the vault passphrase, SSH access to each server, and
the servers' SSH host keys. The page also says what the first run from a
new controller machine shows, what happens when two people run at once,
and what to remove from an old machine.

**Terms:**

- A **node** is a Linux server that MMX manages. The **inventory**, a file
  such as `inventories/example.yml`, lists the nodes.
- A **playbook** is a file in `playbooks/` that lists **fleet stages**.
  Each fleet stage picks nodes, and each of those nodes runs the fleet
  stage's **node tasks**, in order.
- A **node job** is the work of one fleet stage on one node. A node runs
  one node job at a time.
- The **vault** is the encrypted file `secrets_vault.age` in the project.
  The **vault passphrase** opens the vault. [Keep secrets](keep-secrets.md)
  explains the vault and the vault passphrase.
- The **SSH host key** is the key that a server shows to prove the
  server's identity. Your computer keeps trusted host keys in
  `~/.ssh/known_hosts`.

## What MMX keeps on a controller machine

Git carries the project's files. Everything else in this table stays on
the controller machine that made the file. So a second controller machine
needs a copy of each file, or makes a new one.

| What | Where | In Git | A controller machine without it |
| --- | --- | --- | --- |
| The project: `mmx_project_settings.yml`, `inventories/`, `playbooks/`, roles, `files/`, `one_time_playbooks/`, and the vault `secrets_vault.age` | The project directory | Yes | Has no project. Clone the project. |
| Saved runs | `.mmx/runs/` in the project | No | `mmx results`, `mmx status`, and `mmx resume` find no run that another controller machine made. |
| What MMX learned about each node at the first contact | `.mmx/nodes/` in the project | No | The first run sets up MMX on each node again, over SSH. The setup finds MMX of the same release installed and keeps MMX as it is. |
| The vault passphrase | A file outside the project, such as `~/.config/mmx/shop-vault-passphrase`, or the environment variable `MMX_VAULT_PASSWORD` | No | Each command that opens the vault asks for the passphrase at a terminal, and fails without a terminal. |
| Your SSH key | `~/.ssh/`, or your SSH agent | No | MMX logs in only to a server that has the public half of one of this controller machine's keys, or that takes a password. |
| MMX's own SSH keys, which MMX makes only after logging in to a server with a password | `~/.ssh/mmx/` | No | MMX needs the server's password at the first contact, and then makes a new key. |
| Trusted SSH host keys | `~/.ssh/known_hosts` | No, unless you keep a `known_hosts` file in the project | MMX refuses to connect until the controller machine trusts each server's host key. |

[The .mmx directory](../reference/project-settings.md#the-mmx-directory)
says what `.mmx/` holds and what happens if you delete a part. A project
made without `mmx init` needs the line `/.mmx/` in `.gitignore`.

## Choose your route

| Your situation | Route |
| --- | --- |
| A teammate will also run the project | [Share the project with a teammate](#share-the-project-with-a-teammate) |
| You move to a new laptop, and stop using the old laptop | [Move the project to a new controller machine](#move-the-project-to-a-new-controller-machine) |
| A CI job will run the project | [Run the project from CI](#run-the-project-from-ci) |
| Two people may run the project at the same time | [Two runs at the same time](#two-runs-at-the-same-time) |

## Before you start

- The project is in a Git repository that each controller machine can
  clone, such as a repository on your Git server.
- `mmx run` works from your controller machine, as in
  [Get started](../get-started.md). The examples use the project `shop`,
  with `web01` at `203.0.113.11` and `web02` at `203.0.113.12` in the group
  `web_servers`, and the playbook `write_shop_settings` from
  [Keep secrets](keep-secrets.md#create-the-vault-and-use-a-secret).
- The vault passphrase is in your team's password manager. Nobody can open
  the vault without the passphrase.

## Share the project with a teammate

Barbara gets the project from Git. Barbara's SSH key reaches the servers
through a playbook that you run, so the list of keys is in Git too.

1. **You: put your public key and an access playbook in the project.**

   ```sh
   mkdir -p files/team_ssh_keys
   cp ~/.ssh/id_ed25519.pub files/team_ssh_keys/you.pub
   ```

   Create `playbooks/give_team_ssh_access.yml`:

   ```yaml
   # playbooks/give_team_ssh_access.yml
   fleet_stages:
     - id: give_team_ssh_access
       nodes: web_servers
       node_tasks:
         - id: add_team_keys
           authorized_keys:
             for_user_account: root                             # the account in ssh_user
             key_directories_on_controller_machine: [files/team_ssh_keys]
             keys_not_listed: keep                              # keep keys that are not in the directory
   ```

   The [`authorized_keys`](../reference/modules/authorized_keys.md) node task adds
   each key from the `.pub` files in `files/team_ssh_keys/` to root's
   `~/.ssh/authorized_keys` on each server. `keys_not_listed: keep` leaves
   the other keys in root's `~/.ssh/authorized_keys` alone.

   **Check:** `mmx validate` prints `Valid: mmx_example`. If the directory
   `files/team_ssh_keys` is missing, the message ends with `no such file or
   directory`.

2. **You: commit and push the project.**

   ```sh
   git check-ignore .mmx
   git add .
   git commit -m "Shop project"
   git push
   ```

   **Check:** `git check-ignore .mmx` prints `.mmx`. If the command prints
   nothing, add the line `/.mmx/` to `.gitignore` before `git add`. After
   the push, `git status --short` prints nothing.

3. **Barbara: install `mmx` of the same release as yours.** Follow step 1
   of [Get started](../get-started.md), with the same `version`.

   Reason: a server runs the `mmx-runner` of one release at a time. An
   `mmx` of another release installs the `mmx-runner` of its own release,
   so the two releases set up MMX again at each other's runs. To move a
   team to a newer release, see [Upgrade MMX](upgrade-mmx.md#a-team-or-ci-on-different-releases).

   **Check:** `mmx version` prints the same line on your computer and on
   Barbara's computer, such as
   `mmx 0.1.0 (protocol 1)`.

4. **Barbara: clone the project.** Use your repository's address:

   ```sh
   git clone git@git.example.com:team/shop.git shop
   cd shop
   ```

   **Check:** `mmx validate` prints `Valid: mmx_example`. The clone has no
   `.mmx/` directory yet.

5. **Barbara: add Barbara's public key to the project, and push.**

   ```sh
   cp ~/.ssh/id_ed25519.pub files/team_ssh_keys/barbara.pub
   git add files/team_ssh_keys/barbara.pub
   git commit -m "Add Barbara's SSH key"
   git push
   ```

   A public key may be shared. Never put a private key, such as
   `~/.ssh/id_ed25519` without `.pub`, in the project.

   **Check:** `git log --oneline -1` shows `Add Barbara's SSH key`.

6. **You: pull, and add the keys to the servers.**

   ```sh
   git pull
   mmx run give_team_ssh_access
   ```

   ```text
     give_team_ssh_access / web01 / add_team_keys: succeeded, changed
       {"added":["ssh-ed25519 SHA256:r6t0W7VuI3e5IPc/RZ7U0xbp139nmFA+faGdY5clV7o barbara@laptop"]}
     give_team_ssh_access / web01 / add_team_keys: updated "/root/.ssh/authorized_keys"
       103 bytes, permissions_mode 0600, uid 0, gid 0 -> 199 bytes, permissions_mode 0600, uid 0, gid 0
   ```

   `you.pub` is already on the servers, so only Barbara's key is in
   `added`.

   **Check:** the run ends with `Run ...: succeeded`, and `added` lists
   Barbara's key on each server.

7. **Barbara: save the vault passphrase in a file.** Copy the passphrase
   from the password manager, then:

   ```sh
   mkdir -p ~/.config/mmx
   (umask 077 && cat > ~/.config/mmx/shop-vault-passphrase)
   export MMX_VAULT_PASSWORD_FILE="$HOME/.config/mmx/shop-vault-passphrase"
   ```

   `cat` waits for input: paste the passphrase, press Enter, then Ctrl-D.
   [Run without a terminal](keep-secrets.md#run-without-a-terminal)
   explains the file, the `export` line, and how a coding agent, which
   cannot paste into `cat`, gets the passphrase.

   **Check:** `mmx secrets list-keys` prints the secret keys, such as
   `database_password`. `cannot decrypt vault: wrong passphrase or invalid
   age file` means that the file holds another passphrase.

8. **Barbara: trust each server's SSH host key.** First, you print the
   fingerprint that your computer trusts, for each server. The output has
   a line for each key type, and the `ED25519` line is the one to compare:

   ```text
   $ ssh-keygen -lF 203.0.113.11
   # Host 203.0.113.11 found: line 1
   203.0.113.11 ED25519 SHA256:jQfjdsm0J4HLMSJi9v9gZ3CfTWmYRT6xhF6svDQM/6g
   ```

   Barbara then logs in once to each server, compares the `ED25519`
   fingerprint with yours, and answers `yes`:

   ```sh
   ssh root@203.0.113.11 true
   ssh root@203.0.113.12 true
   ```

   Reason: MMX connects only to a server whose host key the controller
   machine already trusts, and never asks to accept a new key.
   [Connect to your servers](connect-to-your-servers.md#a-server-you-log-in-to-with-a-key)
   shows the same step for one server.

   A coding agent, which cannot answer the question, prints each
   fingerprint first and compares the fingerprint with yours:

   ```text
   $ ssh-keyscan -t ed25519 203.0.113.11 2>/dev/null | ssh-keygen -lf -
   256 SHA256:jQfjdsm0J4HLMSJi9v9gZ3CfTWmYRT6xhF6svDQM/6g 203.0.113.11 (ED25519)
   ```

   Then the agent logs in with
   `ssh -o StrictHostKeyChecking=accept-new root@203.0.113.11 true`.

   **Check:** `ssh root@203.0.113.11 true` runs without a question and
   without a password. A password question means that step 6 has not added
   Barbara's key yet.

9. **Barbara: preview the playbook.**

   ```sh
   mmx dryrun write_shop_settings
   echo "exit code $?"
   ```

   The first command from Barbara's copy sets up MMX on each server, over
   SSH, because Barbara's `.mmx/` is new:

   ```text
   Run 67600deb
   Project: mmx_example   Playbook: write_shop_settings
   Inventory: example (2 nodes)   Selected: 2 nodes
   Output: nodes (change with --output=nodes or --output=summary)
   Progress: 2 not started
   203.0.113.12: setting up MMX through verified SSH
   203.0.113.11: setting up MMX through verified SSH
   web01: 2 node tasks unchanged. 0 failed.
   web02: 2 node tasks unchanged. 0 failed.
   write_shop_settings / web01: succeeded
   write_shop_settings / web02: succeeded
     write_shop_settings / web01 / create_settings_directory: succeeded, unchanged
     write_shop_settings / web01 / create_settings_directory: directory "/etc/shop"
       checked 1 directories and 0 files; 0 metadata updates
     write_shop_settings / web01 / write_database_settings: succeeded, unchanged
     write_shop_settings / web02 / create_settings_directory: succeeded, unchanged
     write_shop_settings / web02 / create_settings_directory: directory "/etc/shop"
       checked 1 directories and 0 files; 0 metadata updates
     write_shop_settings / web02 / write_database_settings: succeeded, unchanged
   Run 67600deb: succeeded
   2 finished
   Saved: /home/barbara/shop/.mmx/runs/20261115T061216.229012078-67600deb9ed4cc8f
   exit code 0
   ```

   The setup finds MMX of the same release, installed by your runs. So the
   setup does not install `mmx-runner` again and does not restart MMX's
   service on the server. Barbara's `.mmx/nodes/` then holds what MMX learned about
   each server. Your own runs go on as before. MMX made no new SSH key,
   because Barbara's own key logged in.

   **Check:** the exit code is 0, because your runs already made the
   changes. A second `mmx dryrun write_shop_settings` prints no `setting
   up` line, because MMX then uses HTTPS.

10. **Barbara: run the playbook.**

    ```sh
    mmx run write_shop_settings
    ```

    **Check:** the run ends with `Run ...: succeeded`.

### A server that MMX logs in to with a password

Some servers take a password, such as an account `deploy` with
`ssh_password` and `sudo_password` from the vault, as
[Passwords for automation](connect-to-your-servers.md#passwords-for-automation)
shows. MMX then logs in with the password at the first contact and makes
an SSH key of its own in `~/.ssh/mmx/` on that controller machine, as
[A server that takes only a password](connect-to-your-servers.md#a-server-that-takes-only-a-password)
explains. Your key stays on your computer. Barbara's first run reads the
password from the same secret and makes Barbara's own key:

```text
203.0.113.12: setting up MMX through verified SSH
203.0.113.12: establishing ordinary SSH key access; private key: /home/barbara/.ssh/mmx/4293afe0a4f8a8a665b8c7c32a85700941cbcbcf132c6b7be503e3801a273291
```

The file name of the key is the same on each controller machine, because
MMX names the key after the server's address, port, and account. The key
inside the file is different. The account's `~/.ssh/authorized_keys` on
the server then holds your key and Barbara's key.

### Saved runs stay with the controller machine that made them

Barbara's `mmx results`, `mmx status`, and `mmx resume` see only the runs
in Barbara's `.mmx/runs/`, as [Stop, resume, and read a run](stop-resume-and-read-a-run.md#which-run-a-command-picks)
explains. A run ID from your computer gives:

```text
$ mmx resume 538820d9
mmx: no matching saved run
```

When your run stopped or failed and you are away, Barbara starts a new run
with `mmx run write_shop_settings`. A new run reads the current project
files and does the work that is still missing on each server.

## Two runs at the same time

Each node runs one node job at a time. When your node job still runs on
`web01`, Barbara's run fails on `web01` and goes on with the other servers.
Here, your `mmx run-command --nodes web01 -- sleep 40` was running:

```text
$ mmx run write_shop_settings
Run e5f4148c
Project: mmx_example   Playbook: write_shop_settings
Inventory: example (2 nodes)   Selected: 2 nodes
Output: nodes (change with --output=nodes or --output=summary)
Progress: 2 not started
web01: 1 failed (fleet stage write_shop_settings).
web02: 2 node tasks unchanged. 0 failed.
write_shop_settings / web01: failed
  HTTPS 409: node busy with job 4282e260406da847c71b551731af4833f817b7d49ef58d549bbdd2426220dc3e, started by sam@sams-laptop
write_shop_settings / web02: succeeded
  write_shop_settings / web02 / create_settings_directory: succeeded, unchanged
  write_shop_settings / web02 / create_settings_directory: directory "/etc/shop"
    checked 1 directories and 0 files; 0 metadata updates
  write_shop_settings / web02 / write_database_settings: succeeded, unchanged
Run e5f4148c: failed
1 finished · 1 failed
Saved: /home/barbara/shop/.mmx/runs/20261115T061238.901342242-e5f4148cd2116a11
Inspect:                mmx results e5f4148c
Resume unfinished work: mmx resume e5f4148c
Start a new run:        mmx run write_shop_settings
```

Your node job is not touched, and Barbara's run changed nothing on
`web01`. MMX does not wait for the other node job. After your node job
ends, Barbara runs `mmx resume e5f4148c`, which runs the missing work on
`web01`:

```text
write_shop_settings / web01: succeeded
...
Run e5f4148c: succeeded
```

MMX does not say who started the other node job. Agree in your team who
runs which playbook, or ask in your team's chat before a long run.

The first run from a new copy of the project sets up MMX over SSH. The
setup also fails on a server where a node job runs, and does not wait:

```text
write_shop_settings / web01: failed
  automatic setup of mmx-runner, MMX's HTTPS service on this node: SSH 203.0.113.11: exit status 1: node busy with job d9a9aeccf334e2af3559b1f95044dff612c0f75cc7bd15e2622104da9c3a14c1, started by sam@sams-laptop
```

Run the command again after the other node job ends.

## Move the project to a new controller machine

A move copies `.mmx/` from the old laptop, so the saved runs that MMX
still keeps come along ([how long MMX keeps runs](stop-resume-and-read-a-run.md#where-runs-are-kept-and-for-how-long)),
and the first run on the new laptop needs no setup over SSH. Finish or
resume each stopped or failed run on the old laptop first, or start a new
`mmx run` from the new laptop after the move.

1. **New laptop: install `mmx` of the same release as on the old laptop.**
   Follow step 1 of [Get started](../get-started.md).

   **Check:** `mmx version` prints the same line on the old laptop and on
   the new laptop.

2. **New laptop: clone the project, and copy `.mmx/` from the old
   laptop.**

   ```sh
   git clone git@git.example.com:team/shop.git shop
   rsync -a old-laptop:shop/.mmx/ shop/.mmx/
   ```

   `old-laptop` is the old laptop's SSH host name, and `shop` is the
   project's directory in your home directory there. Any copy that keeps
   the files works, such as a backup that you restore. Without `.mmx/`,
   the move still works: the first run then sets up MMX on each server
   over SSH, as in [step 9 of the teammate route](#share-the-project-with-a-teammate).

   **Check:** `mmx results 538820d9`, with the run ID of a recent run on
   the old laptop, shows the run.

   When the project's path differs on the new laptop, such as another
   home directory, give a run ID to `mmx results`, `mmx status`, and
   `mmx resume`: [Which run a command picks](stop-resume-and-read-a-run.md#which-run-a-command-picks)
   says why.

3. **New laptop: save the vault passphrase in a file.**

   ```sh
   mkdir -p ~/.config/mmx
   (umask 077 && cat > ~/.config/mmx/shop-vault-passphrase)
   export MMX_VAULT_PASSWORD_FILE="$HOME/.config/mmx/shop-vault-passphrase"
   ```

   Paste the passphrase from your password manager, press Enter, then
   Ctrl-D. [Run without a terminal](keep-secrets.md#run-without-a-terminal)
   explains the file and the `export` line.

   **Check:** `mmx secrets list-keys` prints the secret keys.

4. **New laptop: give the new laptop SSH access to the servers.** Choose
   one:

   - **Copy your SSH key** from the old laptop: `~/.ssh/id_ed25519` and
     `~/.ssh/id_ed25519.pub`, with mode `0600` on the private key. The
     servers change nothing.
   - **Make a new key** on the new laptop with `ssh-keygen -t ed25519`.
     Put the new public key in `files/team_ssh_keys/`, such as
     `you-new-laptop.pub`, and run `mmx run give_team_ssh_access` from the
     old laptop, as in steps 5 and 6 of
     [the teammate route](#share-the-project-with-a-teammate).

   If the old laptop has `~/.ssh/mmx/`, copy that directory too, with
   `rsync -a old-laptop:.ssh/mmx/ ~/.ssh/mmx/`. Then a server that took a
   password at the first contact takes the copied key instead.

   **Check:** `ssh root@203.0.113.11 true` runs without a password.

5. **New laptop: trust each server's SSH host key.** Copy
   `~/.ssh/known_hosts` from the old laptop, or log in once to each server
   and compare the fingerprint, as in step 8 of
   [the teammate route](#share-the-project-with-a-teammate).

   **Check:** `ssh root@203.0.113.11 true` runs without a question about
   the host key.

6. **New laptop: preview the playbook.**

   ```sh
   mmx dryrun write_shop_settings --output=summary
   echo "exit code $?"
   ```

   ```text
   Run d2c0edee
   Project: mmx_example   Playbook: write_shop_settings
   Inventory: example (2 nodes)   Selected: 2 nodes
   Output: summary (change with --output=nodes or --output=summary)
   Progress: 2 not started
   2 nodes [web01, web02]: 2 node tasks unchanged. 0 failed.
   write_shop_settings / 2 nodes [web01, web02]: succeeded
     write_shop_settings / 2 nodes [web01, web02] / create_settings_directory: succeeded, unchanged
     write_shop_settings / 2 nodes [web01, web02] / create_settings_directory: directory "/etc/shop"
       checked 1 directories and 0 files; 0 metadata updates
     write_shop_settings / 2 nodes [web01, web02] / write_database_settings: succeeded, unchanged
   Run d2c0edee: succeeded
   2 finished
   Saved: /home/you/shop/.mmx/runs/20261115T061437.569088463-d2c0edee6e44cc85
   exit code 0
   ```

   **Check:** the exit code is 0, and the output has no `setting up` line,
   because the copied `.mmx/nodes/` lets MMX use HTTPS at once.

7. **New laptop: remove the old laptop's key from the servers.** Skip this
   step when you copied your SSH key in step 4. Move the old key's file out
   of `files/team_ssh_keys/`, so that `give_team_ssh_access` never adds the
   old key again:

   ```sh
   mkdir -p files/retired_ssh_keys
   git mv files/team_ssh_keys/you.pub files/retired_ssh_keys/old-laptop.pub
   ```

   Create `playbooks/remove_retired_ssh_keys.yml`:

   ```yaml
   # playbooks/remove_retired_ssh_keys.yml
   fleet_stages:
     - id: remove_retired_ssh_keys
       nodes: web_servers
       node_tasks:
         - id: remove_retired_keys
           authorized_keys:
             for_user_account: root
             key_directories_on_controller_machine: [files/retired_ssh_keys]
             state: absent                    # remove the keys in these files
   ```

   Run the playbook, then commit the moved key file and the new playbook:

   ```sh
   mmx run remove_retired_ssh_keys
   git add files playbooks/remove_retired_ssh_keys.yml
   git commit -m "Remove the old laptop's SSH key"
   git push
   ```

   ```text
     remove_retired_ssh_keys / web01 / remove_retired_keys: succeeded, changed
       {"removed":["ssh-ed25519 SHA256:q54rjHeH2VBNfVjplmGqwokbM0v8B+N9q9nehoHWtBk you@old-laptop"]}
     remove_retired_ssh_keys / web01 / remove_retired_keys: updated "/root/.ssh/authorized_keys"
       192 bytes, permissions_mode 0600, uid 0, gid 0 -> 96 bytes, permissions_mode 0600, uid 0, gid 0
   ```

   Skip this when you copied `~/.ssh/mmx/` in step 4: the new laptop uses
   the same keys, because the key file is named by the server's address,
   port, and account. Otherwise, remove each key in the old laptop's
   `~/.ssh/mmx/*.pub` with an `authorized_keys` node task whose
   `for_user_account` is the account that MMX logged in to with a password,
   such as `deploy`.

   **Check:** from the old laptop, `mmx dryrun write_shop_settings` now
   fails with `HTTPS 403: key is not authorized for unrestricted MMX access
   as root`, and `ssh -o BatchMode=yes root@203.0.113.11 true` prints
   `Permission denied`.

8. **Old laptop: delete what MMX and the project left.**

   ```sh
   rm -rf ~/shop ~/.ssh/mmx ~/.config/mmx/shop-vault-passphrase
   ```

   Also remove the `export MMX_VAULT_PASSWORD_FILE=...` line from the
   shell's startup file. Delete the old SSH key only when nothing else uses
   the key.

   **Check:** `ls ~/shop ~/.ssh/mmx` prints `No such file or directory`
   for `~/shop` and for `~/.ssh/mmx`.

## Run the project from CI

A CI runner is a controller machine without a terminal, and a CI job
usually starts with a fresh clone. The job needs the same three things as
a teammate, from the CI system's secret storage:

- **The vault passphrase,** in the environment variable
  `MMX_VAULT_PASSWORD`.
  [Run without a terminal](keep-secrets.md#run-without-a-terminal) shows
  the sources that MMX reads.
- **An SSH private key** for the job. Put the key's public half in
  `files/team_ssh_keys/`, such as `ci.pub`, and run `give_team_ssh_access`
  as in [step 6 of the teammate route](#share-the-project-with-a-teammate).
- **The servers' SSH host keys,** written to `~/.ssh/known_hosts` from a
  secret. Or keep a `known_hosts` file in the project and name the file
  with `ssh_known_hosts` on each node, as
  [Other SSH settings](connect-to-your-servers.md#other-ssh-settings)
  shows.

The job runs with `--non-interactive`, so a missing passphrase or password
fails the run instead of waiting for an answer:

```sh
mmx run write_shop_settings --non-interactive --output=summary
```

```text
Run 63eb5da9
Project: mmx_example   Playbook: write_shop_settings
Inventory: example (2 nodes)   Selected: 2 nodes
Output: summary (change with --output=nodes or --output=summary)
Progress: 2 not started
203.0.113.12: setting up MMX through verified SSH
203.0.113.11: setting up MMX through verified SSH
2 nodes [web01, web02]: 2 node tasks unchanged. 0 failed.
write_shop_settings / 2 nodes [web01, web02]: succeeded
  write_shop_settings / 2 nodes [web01, web02] / create_settings_directory: succeeded, unchanged
  write_shop_settings / 2 nodes [web01, web02] / create_settings_directory: directory "/etc/shop"
    checked 1 directories and 0 files; 0 metadata updates
  write_shop_settings / 2 nodes [web01, web02] / write_database_settings: succeeded, unchanged
Run 63eb5da9: succeeded
2 finished
Saved: /home/you/ci/shop/.mmx/runs/20261115T061548.175318241-63eb5da95e566a75
```

The `setting up` lines appear in each job whose clone has no `.mmx/`.
Each setup uses SSH, so a server whose account needs a `sudo` password gets
the password in each such job, from a secret as
[Passwords for automation](connect-to-your-servers.md#passwords-for-automation)
shows. The saved run goes away with the job's directory. To finish the
work of a failed job, start a new job, which runs `mmx run` again.

## If something is different

| What you see | What it means and what to do |
| --- | --- |
| `mmx: vault passphrase required; use --vault-password-file, MMX_VAULT_PASSWORD_FILE, or MMX_VAULT_PASSWORD (prompts are disabled)` | This controller machine has no vault passphrase, and MMX may not ask. Save the passphrase as in step 7 of [the teammate route](#share-the-project-with-a-teammate). |
| `No ED25519 host key is known for 203.0.113.11 and you have requested strict checking.` and `Host key verification failed; trust the server's current host key in known_hosts first, as for any SSH tool` | This controller machine does not trust the server's host key yet. Do step 8 of [the teammate route](#share-the-project-with-a-teammate). |
| `SSH password for root@203.0.113.11:22 is required; configure an environment variable for the credential or use an SSH key or ssh-agent (prompts are disabled)` | The server does not have this controller machine's SSH key. Add the key with `give_team_ssh_access`, as in steps 5 and 6 of [the teammate route](#share-the-project-with-a-teammate). |
| `key directory files/team_ssh_keys: open /home/you/shop/files/team_ssh_keys: no such file or directory` | The directory that `key_directories_on_controller_machine` names is missing. Create the directory with at least one `.pub` file. |
| `HTTPS 409: node busy with job 4282e260..., started by sam@sams-laptop` | Another run's node job is working on that server, from this or another controller machine. The message ends with the account and the controller machine that started that node job (an older node job record names only the job ID). Wait until the other node job ends, then run `mmx resume` with your run's ID. See [Two runs at the same time](#two-runs-at-the-same-time). |
| `automatic setup of mmx-runner, MMX's HTTPS service on this node: ... node busy with job d9a9aecc...` | The first run from this copy of the project met another run's node job. Run the command again after the other node job ends. |
| `mmx: no matching saved run`, for a run ID from another controller machine | Saved runs stay with the controller machine that made them. Start a new `mmx run`. After a move with `.mmx/` copied, give the run ID: commands without a run ID pick only runs made at the project's current path. |
| `HTTPS 403: key is not authorized for unrestricted MMX access as this account; check current SSH keys or use connection: ssh_only for other SSH authorization mechanisms; journalctl -u mmx-https-api.service on the node gives the reason` | The server no longer has this controller machine's key, such as after step 7 of [the move route](#move-the-project-to-a-new-controller-machine). Use the controller machine whose key the server has. |
| A run from a new controller machine needs the `sudo` password, although you copied `~/.ssh/mmx/` | The first run without `.mmx/nodes/` sets up MMX over SSH, and SSH uses `sudo`. Give the password for that run, or copy `.mmx/` too, as in step 2 of [the move route](#move-the-project-to-a-new-controller-machine). |
| Each run prints `setting up MMX through verified SSH` again | The controller machines run different MMX releases, so each `mmx` installs the `mmx-runner` of its own release. Install the same release everywhere: `mmx version` prints the release. |

## Where to go next

- [Keep secrets](keep-secrets.md): the vault, the passphrase, and changing
  the passphrase when someone leaves the team.
- [Connect to your servers](connect-to-your-servers.md): host keys,
  passwords, and MMX's own SSH keys.
- [Stop, resume, and read a run](stop-resume-and-read-a-run.md): saved runs,
  `mmx resume`, and how long MMX keeps runs.
- [`authorized_keys`](../reference/modules/authorized_keys.md): every input of the
  module that manages the team's keys.
