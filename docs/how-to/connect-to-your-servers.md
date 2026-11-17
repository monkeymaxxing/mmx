# Connect to your servers

**Who acts:** you, or your coding agent, in your MMX project on your
controller machine: the computer where you run `mmx`.

**When to use this page:** you add a server to your inventory and want
MMX to reach it, or MMX cannot reach a server that MMX reached before.

**What this page does:** you prepare each kind of server for its first
contact with MMX: a server you log in to with an SSH key, a server that
takes only a password, a server behind a jump host, a server whose port
7443 is closed, and a server that was replaced. Each route ends with a run
that works.

**Terms:**

- A **node** is a Linux server that MMX manages. The **inventory**, a file
  such as `inventories/example.yml`, lists the nodes and how MMX logs in to
  each node. [Describe your servers](describe-your-servers.md) explains the
  inventory.
- The **SSH host key** is the key that a server shows to prove the
  server's identity. Your computer keeps the trusted host keys in
  `~/.ssh/known_hosts`.
- MMX connects to a node in two ways. The **first contact** uses SSH: MMX
  logs in with OpenSSH, the same `ssh` you use, installs `mmx-runner`,
  MMX's program for the node, and starts `mmx-runner` as the systemd service
  `mmx-https-api.service`. Later runs use **HTTPS** on TCP port 7443, to
  that service. If port 7443 can't be reached, MMX uses SSH instead and
  says so.

## Choose your route

| Your server | Route |
| --- | --- |
| You log in as `root`, or as an account with `sudo`, with an SSH key | [A server you log in to with a key](#a-server-you-log-in-to-with-a-key) |
| The server takes only a password, for login or for `sudo` | [A server that takes only a password](#a-server-that-takes-only-a-password) |
| Your computer reaches the server only through another server | [A server behind a jump host](#a-server-behind-a-jump-host) |
| A firewall closes port 7443 between your computer and the server | [A server whose port 7443 is closed](#a-server-whose-port-7443-is-closed) |
| You rebuilt or replaced a server at the same address | [A server that was replaced](#a-server-that-was-replaced) |
| The server uses another SSH port, a separate key, or its own SSH settings | [Other SSH settings](#other-ssh-settings) |

## Before you start

- You have an MMX project, made with `mmx init` as in
  [Get started](../get-started.md).
- The server runs Linux with systemd.
  [Limits on each distribution](../reference/limits-on-each-distribution.md) lists
  the tested distributions.
- The account you log in as is `root`, or an account that can run any
  command with `sudo`, because MMX installs software and changes system
  files.

The examples use `web01` at `203.0.113.11` and `web02` at `203.0.113.12`.
Put in your own servers' addresses and accounts.

## A server you log in to with a key

1. **Trust the server's SSH host key.** Log in once with `ssh`. Compare the
   fingerprint with the one your hosting provider shows, then answer
   `yes`:

   ```text
   $ ssh root@203.0.113.11 true
   The authenticity of host '203.0.113.11 (203.0.113.11)' can't be established.
   ED25519 key fingerprint is SHA256:h5lx2n/P+pChd8mhml+9Z4ZT620iYilDhwjS7Md5wTI.
   This key is not known by any other names.
   Are you sure you want to continue connecting (yes/no/[fingerprint])? yes
   Warning: Permanently added '203.0.113.11' (ED25519) to the list of known hosts.
   ```

   Reason: MMX connects only to a server whose host key your computer
   already trusts, and never asks you to accept a new key. The host key is
   the only proof that MMX reached the right server.

   A coding agent, which cannot answer the question, first prints the
   server's fingerprint with
   `ssh-keyscan -t ed25519 203.0.113.11 | ssh-keygen -lf -`, compares it
   with the hosting provider's, and then logs in with
   `ssh -o StrictHostKeyChecking=accept-new root@203.0.113.11 true`.

   **Check:** `ssh root@203.0.113.11 true` runs again without a question.

2. **Add the server to the inventory.**

   ```yaml
   # inventories/example.yml
   nodes:
     web01:
       address: 203.0.113.11
       ssh_user: root          # or an account that can run any command with sudo
   ```

   MMX uses your SSH key files and your SSH agent, as `ssh` does.

   **Check:** `mmx nodes` lists `web01   203.0.113.11   root`.

3. **Run a playbook on the server.**

   ```sh
   mmx run example --limit web01
   ```

   The first run sets up MMX's HTTPS service on the server over SSH,
   then runs the playbook. It says so before it changes anything:

   ```text
   203.0.113.11: setting up MMX's HTTPS service on web01 (mmx-https-api.service, TCP port 7443 on every network address). Only SSH keys that can already log in to web01 can use the service. For no port, set connection: ssh_only on web01 in the inventory and run mmx remove-mmx-from-node web01 --inventory example
   ```

   A dryrun before this run works over SSH and starts no service.

   **Check:** the run ends with `Run ...: succeeded`. The next run on
   `web01` prints no `setting up` line, because the run uses HTTPS.
   `mmx results --json` shows `"connection_used": "https"` for `web01`.

## A server that takes only a password

MMX asks for the SSH password and the `sudo` password at the first
contact. At the first run, MMX creates an SSH key on your computer, in `~/.ssh/mmx/`,
and adds the key's public half to the account's
`~/.ssh/authorized_keys` on the server. Later runs over HTTPS need no
password, because they sign in with that key. A run that uses SSH, such as
when port 7443 is closed, still needs the `sudo` password when the
account's `sudo` asks for one.

The example account is `deploy` on `web02`, with a login password and a
`sudo` password.

1. **Trust the server's SSH host key.** Log in once with `ssh` and answer
   `yes` after you compare the fingerprint, as in
   [the key route](#a-server-you-log-in-to-with-a-key). Type the password
   when `ssh` asks.

   **Check:** `ssh deploy@203.0.113.12 true` asks for the password and no
   question about the host key.

2. **Add the server to the inventory.**

   ```yaml
   # inventories/example.yml
   nodes:
     web02:
       address: 203.0.113.12
       ssh_user: deploy
   ```

   **Check:** `mmx nodes` lists `web02   203.0.113.12   deploy`.

3. **Make the first run from a terminal, and type both passwords.**

   ```sh
   mmx run example --limit web02
   ```

   ```text
   SSH password for deploy@203.0.113.12:22:
   sudo password for deploy@203.0.113.12:22:
   203.0.113.12: setting up MMX's HTTPS service on web02 (mmx-https-api.service, TCP port 7443 on every network address). Only SSH keys that can already log in to web02 can use the service. For no port, set connection: ssh_only on web02 in the inventory and run mmx remove-mmx-from-node web02 --inventory example
   Progress: 1 running
     welcome / web02: connecting
   203.0.113.12: establishing ordinary SSH key access; private key: /home/you/.ssh/mmx/c4566d72437fccc6921b14be1a0d1da5082acc193af229b34a2518d8299a619c
   ```

   MMX asks each question only when the server needs the answer. An
   account with a key and with `sudo` that needs no password gets no
   question. A dryrun before this first run asks for both passwords too,
   on every dryrun, and creates no key: the key comes with the run that
   sets up MMX's HTTPS service.

   **Check:** `ls ~/.ssh/mmx/` shows the new key, and the account's
   `~/.ssh/authorized_keys` on the server holds the key's public half, on a
   line that ends with the comment `mmx managed key`. MMX
   creates one key for each server address, port, and account.
   When you later change a server's `ssh_port`, MMX copies its key to the
   new port's name, but only after SSH shows that the new port reaches the
   same machine as before. A changed address is different: the server then
   needs your own key or a password.

4. **Run again without a terminal.**

   ```sh
   mmx run example --limit web02 --non-interactive
   ```

   `--non-interactive` makes a question that MMX would ask into an error,
   so a run in a script fails instead of waiting.

   **Check:** the run succeeds and asks nothing.

### Passwords for automation

A CI job or a coding agent has no terminal to type into. Give the
passwords to MMX from environment variables instead:

1. **Name the environment variables in the project settings.**

   ```yaml
   # mmx_project_settings.yml
   project_name: mmx_example
   default_inventory: example
   secrets_from_env:
     deploy_ssh_password: MMX_DEPLOY_SSH_PASSWORD     # a secret name and an environment variable you choose
     deploy_sudo_password: MMX_DEPLOY_SUDO_PASSWORD
   ```

2. **Refer to the secrets on the node.**

   ```yaml
   # inventories/example.yml
   nodes:
     web02:
       address: 203.0.113.12
       ssh_user: deploy
       ssh_password: "{{ .secrets.deploy_ssh_password }}"
       sudo_password: "{{ .secrets.deploy_sudo_password }}"
   ```

   Each value must be one whole reference, as shown, with nothing before
   or after the reference. MMX keeps the passwords in memory only. The
   passwords are never saved with the run, sent in a node job, or shown in
   results.

3. **Run with the environment variables set.**

   ```sh
   MMX_DEPLOY_SSH_PASSWORD='...' MMX_DEPLOY_SUDO_PASSWORD='...' mmx run example --non-interactive
   ```

   **Check:** the run succeeds. MMX reads an environment variable only when
   a connection needs the password. A run over HTTPS needs neither, so the
   variables can be missing. A run over SSH, such as with
   `--connection ssh_only`, needs the `sudo` password every time when the
   account's `sudo` asks for one, because each SSH request uses `sudo`.
   `root`, or an account whose `sudo` needs no password, is never asked.

A secret without an environment variable comes from the project's
encrypted vault, unlocked as for any other secret.
[Keep secrets](keep-secrets.md) explains the
vault.

SSH passwords go through OpenSSH's own password prompt. MMX turns off
keyboard-interactive login, where the prompts come from the server, so a
server that asks only through keyboard-interactive login gets no password
from MMX.

## A server behind a jump host

A jump host, also called a bastion, is a server that you log in to on the
way to servers that your computer can't reach directly. OpenSSH's
`ProxyJump` setting goes through the jump host. MMX uses `ProxyJump` from
an SSH configuration file that you name on the node.

The example jump host is `bastion01` at `203.0.113.30`, and the server
behind the jump host is `db01` at the private address `10.0.1.21`.

1. **Trust the jump host's SSH host key.** Log in to the jump host once
   with `ssh` and answer `yes` after you compare the fingerprint.

   **Check:** `ssh root@203.0.113.30 true` runs without a question.

2. **Write an SSH configuration file in the project.** Create
   `ssh/jump_host_config`:

   ```text
   # ssh/jump_host_config
   Host bastion01
     HostName 203.0.113.30
     User root
     IdentityFile ~/.ssh/id_ed25519
     IdentitiesOnly yes
     StrictHostKeyChecking yes

   Host 10.0.1.21
     ProxyJump bastion01
   ```

   MMX passes the file to `ssh` with `-F`, so OpenSSH reads
   `ssh/jump_host_config` instead of `~/.ssh/config`. Write every setting
   that the jump host needs, such as the jump host's key file, in the
   `Host bastion01` entry.

3. **Trust the server's SSH host key, through the jump host.**

   ```text
   $ ssh -F ssh/jump_host_config root@10.0.1.21 true
   The authenticity of host '10.0.1.21 (<no hostip for proxy command>)' can't be established.
   ED25519 key fingerprint is SHA256:q0xf1ZWwIOTJZ+8jq59ll8vt17I54T+qvD7Tj8ENrEg.
   This key is not known by any other names.
   Are you sure you want to continue connecting (yes/no/[fingerprint])? yes
   Warning: Permanently added '10.0.1.21' (ED25519) to the list of known hosts.
   ```

   **Check:** the same command runs again without a question.

4. **Add the server to the inventory.**

   ```yaml
   # inventories/example.yml
   nodes:
     db01:
       address: 10.0.1.21
       ssh_user: root
       ssh_config: ssh/jump_host_config   # a path from the project directory
       connection: ssh_only
   ```

   Reason for `connection: ssh_only`: the HTTPS connection on port 7443
   does not go through the jump host, so MMX uses SSH for every request.

   **Check:** `mmx run example --limit db01` succeeds, with no notice about
   port 7443. `mmx results --json` shows `"connection_used": "ssh"`.

The jump host needs no MMX: the jump host only passes the SSH connection
on. List the jump host in the inventory only when MMX also manages the
jump host. The node's `ssh_identity_file` and `ssh_known_hosts` apply to
the node, not to the jump host.

## A server whose port 7443 is closed

When MMX can't reach port 7443, MMX does the same work over SSH and prints
a notice. The run goes on, and the results are the same. You can keep the
fallback, open the port, or tell MMX to use SSH, so that MMX no longer
waits for port 7443 and prints no notice.

1. **Read the notice.** The notice says what MMX checked over SSH:

   ```text
   203.0.113.11: MMX could not connect to https://203.0.113.11:7443 (connection timed out). mmx-runner, MMX's HTTPS service on this node, is running. MMX is using SSH for this node. To use HTTPS, check that the controller machine can reach 203.0.113.11 on TCP port 7443: the network between them may be down, or a firewall may block the port. To make SSH the stated choice instead, set connection: ssh_only on the node in the inventory, or pass --connection ssh_only when this controller machine cannot reach the node's HTTPS address.
   ```

   MMX checks whether the HTTPS service runs, and whether ufw or
   firewalld is active on the server without a rule for port 7443. The
   notice names only causes that MMX checked. Here the service runs, so
   the block is in a firewall or in the network between your computer and
   the server. MMX never opens a firewall port itself.

   **Check:** `mmx results --json` shows `"connection_used": "ssh_fallback"`,
   and the same notice in `connection_fallback_reason`.

2. **Choose one of these:**

   - **Use HTTPS:** allow TCP port 7443 from your computer to the server,
     in the server's firewall and in your hosting provider's firewall.
   - **Use SSH for this server always:** add `connection: ssh_only` to the
     node in the inventory.

     ```yaml
     # inventories/example.yml
     nodes:
       web01:
         address: 203.0.113.11
         ssh_user: root
         connection: ssh_only
     ```

   - **Use SSH from this computer only,** such as a laptop on a network
     that blocks port 7443, without a change to the shared inventory:

     ```sh
     mmx run example --connection ssh_only
     ```

     `--connection` works with `dryrun`, `run`, `run-command`,
     `check-one-time-playbooks`, and `complete-one-time-playbook`, and
     `ssh_only` is its only value.

   **Check:** the next run prints no notice. With HTTPS, `mmx results
   --json` shows `"connection_used": "https"`; with either SSH choice,
   `mmx results --json` shows `"connection_used": "ssh"`.

`mmx resume` refuses `--connection`
([What a resume refuses](stop-resume-and-read-a-run.md#what-a-resume-refuses)).
To change the connection, start a new `mmx run`.

## A server that was replaced

A rebuilt or replaced server has a new SSH host key, even at the same
address. OpenSSH refuses the new key, so MMX refuses too, until you trust
the new key.

1. **Make sure the change of host key is yours.** A changed key also
   appears when someone else answers at the server's address. Continue
   only if you rebuilt or replaced the server.

   **Check:** MMX and `ssh` both say `REMOTE HOST IDENTIFICATION HAS
   CHANGED!` and show the new key's fingerprint. Compare the fingerprint
   with the one your hosting provider shows for the new server.

2. **Remove the old host key, and trust the new one.**

   ```sh
   ssh-keygen -R 203.0.113.11
   ssh root@203.0.113.11 true
   ```

   Answer `yes` after you compare the fingerprint.

   **Check:** `ssh root@203.0.113.11 true` runs again without a question.

3. **Start a new run.**

   ```sh
   mmx run example
   ```

   MMX finds a different machine at the address, sets up MMX on the new
   machine, and says so:

   ```text
   203.0.113.11: setting up MMX's HTTPS service on web01 (mmx-https-api.service, TCP port 7443 on every network address). Only SSH keys that can already log in to web01 can use the service. For no port, set connection: ssh_only on web01 in the inventory and run mmx remove-mmx-from-node web01 --inventory example
   Progress: 1 running
     welcome / web01: connecting
   203.0.113.11: node address now reaches a different machine with a trusted SSH host key; saved node identity updated
   ```

   **Check:** the run ends with `Run ...: succeeded`.

A run from before the replacement can't be resumed on the new machine,
because the run's saved progress describes the old machine. `mmx resume`
fails the replaced node, here in a run of the playbook `check_ready`:

```text
check_ready / web01: failed
  node was replaced since this run (machine identity changed); its recorded work belongs to the previous machine; use mmx run
```

Start a new `mmx run` instead.

## Other SSH settings

A node can carry the same SSH settings that you would give `ssh` on the
command line. Set the SSH settings on the node in the inventory:

```yaml
# inventories/example.yml
nodes:
  web03:
    address: 203.0.113.13
    ssh_user: root
    ssh_port: 2222                                 # the server's SSH port
    ssh_identity_file: /home/you/.ssh/web03_ed25519  # the private key for this server
    ssh_known_hosts: ssh/known_hosts               # a known_hosts file for this server
```

Each setting does what the same option does for `ssh`. The reference
lists them with their `ssh` equivalents:
[`ssh_port`](../reference/inventory-file.md#ssh_port),
[`ssh_identity_file`](../reference/inventory-file.md#ssh_identity_file),
[`ssh_known_hosts`](../reference/inventory-file.md#ssh_known_hosts), and
[`ssh_config`](../reference/inventory-file.md#ssh_config).

MMX files what it knows about a server under the server's address, SSH
port, and account. That includes the key that MMX made in `~/.ssh/mmx/` for
[a server that takes only a password](#a-server-that-takes-only-a-password).
After you change `ssh_port` on such a node, the first run looks for that key
under the old port, logs in with it over SSH on the new port, and copies it
to the new port's name only when the server there is the same machine as
before. MMX then prints a line such as
`203.0.113.12: MMX's own SSH key for SSH port 22 now also serves SSH port 2222: ...`.
Two different servers behind one address, told apart only by the port, never
share a key this way. After a changed address, MMX does not look for the
old key: such a node needs your own key through `ssh_identity_file`, or a
password login. The first
dryrun or run over HTTPS after the change prints a line such as
`203.0.113.12: setting up MMX through verified SSH`, because MMX checks the
server once more over SSH on the new port. Later runs don't print the line.

An ordinary run uses HTTPS, which does not go through sshd's port. A wrong
`ssh_port` therefore shows only on a run with `--connection ssh_only`, as
in [A server whose port 7443 is closed](#a-server-whose-port-7443-is-closed).

How a path such as `~/.ssh/web03_ed25519` or `ssh/known_hosts` is read
is in [Paths on the controller machine](../reference/inventory-file.md#paths-on-the-controller-machine).

**Check:** `mmx run example --limit web03` succeeds, and `ssh -p 2222 -i
/home/you/.ssh/web03_ed25519 -o UserKnownHostsFile=ssh/known_hosts
root@203.0.113.13 true` runs without a question.

### A Host name from your SSH configuration

`address` can be a `Host` name from `~/.ssh/config`. MMX then uses that
entry's settings, as `ssh` does:

```text
# ~/.ssh/config
Host shop-web01
  HostName 203.0.113.11
  User root
```

```yaml
# inventories/example.yml
nodes:
  web01:
    address: shop-web01
```

Without `ssh_user` in the inventory, the `User` line chooses the account,
and `mmx nodes` shows `-` in the account column. HTTPS goes to the
`HostName`, here `https://203.0.113.11:7443`.

**Check:** `ssh shop-web01 true` runs without a question, and `mmx run
example --limit web01` succeeds.

### HTTPS through a port mapping

When the server's port 7443 is reachable at another address or port, such
as through a NAT port mapping, set `https_address` on the node to that
origin, such as `https_address: https://203.0.113.5:17443`. MMX then
connects to that origin instead of the node's address on port 7443.

### Runner downloads for a large fleet

At the first contact, MMX sends `mmx-runner`, about 7 MB compressed, over
each server's SSH connection. For many servers, the servers can download
`mmx-runner` from a web server near them instead.

1. **Put each `mmx-runner` file on the web server, named by its SHA-256
   hash.** The files are in the directory where you installed `mmx`:

   ```sh
   mkdir -p mirror/mmx
   for arch in amd64 arm64; do
     hash=$(sha256sum ~/.local/lib/mmx/mmx-runner-linux-$arch | cut -d' ' -f1)
     cp ~/.local/lib/mmx/mmx-runner-linux-$arch mirror/mmx/mmx-runner-$hash
   done
   ```

   On a Mac, use `shasum -a 256` instead of `sha256sum`. Serve the
   `mirror` directory with any web server.

2. **Name the web server on each node.**

   ```yaml
   # inventories/example.yml
   nodes:
     web04:
       address: 203.0.113.14
       ssh_user: root
       mmx_runner_download_url: http://203.0.113.40:18473/mmx
   ```

   The server downloads the file with `curl`, so `curl` must be installed
   on the server. MMX sends the expected hash over verified SSH and
   installs only a file with that hash, so plain HTTP is safe here.

   **Check:** the first run on `web04` succeeds, and the web server's log
   shows the download, such as
   `"GET /mmx/mmx-runner-8bf9a38b... HTTP/1.1" 200`.

A missing or damaged download fails the setup of that server, and the
message names the URL. A download that stalls for 30 seconds on each of
three tries fails too. MMX does not then send the file over SSH.

### Login rules that only OpenSSH can check

At each HTTPS connection, the MMX service on the server reads sshd's
effective settings. It checks your key against the account's
`authorized_keys` files, sshd's user and group allow and deny lists, and
sshd's key rules, and checks that the account is `root` or can run any
command with `sudo`. When sshd uses a rule that the service can't follow,
such as `ForceCommand`, `ChrootDirectory`, a revocation list, or
`UseDNS`, the service refuses the connection with an error that names
`connection: ssh_only`. A connection whose key the account does not list
gets one fixed refusal that says nothing about the server's accounts, and
`journalctl -u mmx-https-api.service` on the server shows the reason. The service does not see keys that come only from
`AuthorizedKeysCommand` or from a certificate authority, and it does not
run PAM. Use `connection: ssh_only` when your servers rely on such login
rules, so that OpenSSH checks the rules at every connection:

- SSH certificate authorities, or keys that come only from
  `AuthorizedKeysCommand`
- multifactor login, DNS-based authorization, or revocation lists
- complex allow and deny patterns, or keys restricted to one command
- key options other than `from` and the `restrict` and `no-*` flags
- chroots, `sudo` rules that deny some commands, or unusual login shells
- custom PAM rules for login or sessions

When you remove your key from the server, the next HTTPS connection is
refused. A connection that is already open keeps its access until the
connection closes, and node jobs that already started keep running.

## If something is different

| What you see | What it means and what to do |
| --- | --- |
| `No ED25519 host key is known for 203.0.113.11 and you have requested strict checking.` and `Host key verification failed; trust the server's current host key in known_hosts first, as for any SSH tool` | Your computer does not trust the server's host key yet. Do step 1 of your route. |
| `WARNING: REMOTE HOST IDENTIFICATION HAS CHANGED!` | The server shows another host key than before. See [A server that was replaced](#a-server-that-was-replaced). |
| `ssh: connect to host 203.0.113.11 port 22: Connection timed out` | Your computer can't reach the server's SSH port. Check the address, the port, and any firewall. A server behind a jump host needs [the jump host route](#a-server-behind-a-jump-host). |
| `SSH password for deploy@203.0.113.12:22 is required; configure an environment variable for the credential or use an SSH key or ssh-agent (prompts are disabled)` | The server needs a password, and MMX may not ask, because of `--non-interactive`, `--json`, or no terminal. Run once from a terminal, or see [Passwords for automation](#passwords-for-automation). |
| `sudo password for deploy@203.0.113.12:22 is required; ...` on a run with `--connection ssh_only` | Each SSH request uses `sudo`, so an SSH run needs the `sudo` password every time the account's `sudo` asks for one. Set `sudo_password` from a secret, or let the run use HTTPS. |
| `secret deploy_sudo_password requires a nonempty environment variable MMX_DEPLOY_SUDO_PASSWORD` | The run needs a password whose environment variable is not set. Set the variable. |
| `MMX could not connect to https://203.0.113.11:7443 (connection timed out).` | Port 7443 is closed. MMX used SSH, and the run went on. See [A server whose port 7443 is closed](#a-server-whose-port-7443-is-closed). |
| `HTTPS 403: key is not authorized for unrestricted MMX access as this account; check current SSH keys or use connection: ssh_only for other SSH authorization mechanisms; journalctl -u mmx-https-api.service on the node gives the reason` | The MMX service on the server did not find your key in the account's `authorized_keys`, or could not read the account or that file, such as a file that another account can write. Check that the key MMX uses is in that file; the journal line names the reason. |
| `HTTPS 403: key is not authorized for unrestricted MMX access as root: its line in authorized_keys has options MMX does not accept (command, from); remove them, or use connection: ssh_only for other SSH authorization mechanisms` | Your key's line in `authorized_keys` limits it, for example with `command="..."` or a `from=` that does not match. MMX's HTTPS service gives full access, so it refuses a limited key. Remove those options, or use `connection: ssh_only`. |
| `automatic setup of the node runner executable from http://203.0.113.40:18473/wrong/mmx-runner-8bf9a38b...: ... curl: (22) The requested URL returned error: 404` | The server could not download `mmx-runner` from `mmx_runner_download_url`. Check the URL and the file names on the web server. |
| `node was replaced since this run (machine identity changed); its recorded work belongs to the previous machine; use mmx run` | You resumed a run from before the server was replaced. Start a new `mmx run`. |
| `mmx: resume uses the recorded run's connection choice and accepts no --connection; start a new mmx run <playbook> --connection ssh_only for SSH` | `mmx resume` keeps the run's connection choice. Start a new run with `--connection ssh_only`. |
| `account deploy does not have unrestricted root sudo access` | The account must be `root`, or able to run any command with `sudo`. Change the account's `sudo` rule, or use another account. |
| `administrator account deploy has a locked password, and sshd refuses a locked account unless UsePAM is yes` | The account's password is locked (`!` in `/etc/shadow`), and the server's sshd does not use PAM, so sshd refuses the account's SSH keys too. Run `usermod -p '*' deploy` for an account with keys and no password, or give it a password. |

[When something fails](../when-something-fails.md#connecting-to-a-server-and-setting-it-up)
lists the other failures at the first contact, such as a login script that
prints text, a server without systemd, and two inventory names that reach
one machine.

## Where to go next

- [Describe your servers](describe-your-servers.md): nodes, groups,
  variables, and an inventory for each environment.
- [Preview and run changes](preview-and-run-changes.md): run a playbook on
  the servers you can now reach.
- [Change sshd's settings on a node that MMX manages](change-ssh-or-network-settings-safely.md):
  change SSH on a server without locking MMX out.
