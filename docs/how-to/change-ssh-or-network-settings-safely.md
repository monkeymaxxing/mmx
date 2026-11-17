# Change SSH or network settings safely

**Who acts:** you, or your coding agent, in your MMX project on your
controller machine: the computer where you run `mmx`.

**When to use this page:** you change a server's network settings, such
as its address, or sshd's settings, such as the SSH port or password
login, and you want to keep your access to the server, and MMX's.

**What this page does:** you give `web01` a fixed address in place of
DHCP, and see the server put the old network file back by itself after a
typo cuts the server off. Then you move SSH on `web01` and `web02` from port 22 to
port 2222 without a moment when MMX can't log in. The page also says what
MMX does not protect, and how to get back in when you are locked out.

**Terms:**

- A **node** is a server that MMX manages. A **playbook** lists **fleet
  stages**. Each fleet stage picks nodes, and each of those nodes runs the
  fleet stage's **node tasks**, in order.
- MMX connects to a node in two ways: **SSH**, through the server's sshd,
  and **HTTPS** on TCP port 7443, to MMX's own service on the node.
  [Connect to your servers](connect-to-your-servers.md) explains both.
- A **reachability confirmation** is a request that your running `mmx`
  command sends to the node over a new SSH or HTTPS connection, to prove
  that the node can still be reached.
- An **owed restore** is a record on the node that a network file must go
  back to its previous version unless reachability confirmations arrive in
  time. A systemd timer on the node carries out the restore, without your
  computer and without a connection.

Choose your route:

| You change | What MMX does | Route |
| --- | --- | --- |
| A network file of systemd-networkd, Netplan, NetworkManager, ifupdown, or WireGuard, with MMX's network modules | MMX checks new connections before and after the change. Without the confirmations, the node puts the old file back by itself. | [Change a network file](#change-a-network-file) |
| sshd's settings, such as the port, password login, or the accounts that may log in | No automatic restore. You keep a way in: add the new setting next to the old setting, check new connections, then remove the old setting. | [Change sshd's settings](#change-sshds-settings) |
| Firewall rules | `ufw` and `firewalld` refuse some rules that would close sshd's port. A firewall rule is not put back. | [What MMX does not protect](#what-mmx-does-not-protect) |
| You are already locked out, or MMX is | Wait for the restore, or use the server's console. | [Get back in](#get-back-in) |

## Before you start

- `mmx run` succeeds on the servers you change. The examples use `web01`
  at `203.0.113.11`, which runs Debian 13, and `web02` at `203.0.113.12`,
  which runs Ubuntu 24.04. Both are in the group `web_servers` in
  `inventories/example.yml`, and MMX logs in to both as `root`.
- You can open each server's console without SSH, such as the web console
  or rescue system of your hosting provider. When a wrong sshd setting or
  firewall rule shuts out both you and MMX, the console is the way back
  in. Find the console before you start, not after a lockout.
- Find the inputs of any module with `mmx describe-module NAME`, such as
  `mmx describe-module systemd_networkd_file`.

## Change a network file

MMX's network modules change one network file and protect the change with
an owed restore. A plain `file` node task that writes a network file gets
no such protection, so use the module for the server's network tool:

| The server's network tool | Module | Files |
| --- | --- | --- |
| systemd-networkd | [`systemd_networkd_file`](../reference/modules/systemd_networkd_file.md) | `/etc/systemd/network/<name>.network` or `.netdev` |
| Netplan, with the systemd-networkd renderer | [`netplan_file`](../reference/modules/netplan_file.md) | `/etc/netplan/<name>.yaml` |
| NetworkManager | [`networkmanager_connection_file`](../reference/modules/networkmanager_connection_file.md) | `/etc/NetworkManager/system-connections/<name>.nmconnection` |
| ifupdown, Debian's package | [`ifupdown_interfaces_file`](../reference/modules/ifupdown_interfaces_file.md) | `/etc/network/interfaces` or a file in `/etc/network/interfaces.d/` |
| WireGuard | [`wireguard_interface`](../reference/modules/wireguard_interface.md) | The interface's configuration file |

MMX does not install or switch a server's network tool. Two servers with
the same distribution can use different tools: a hosting provider's image
may use another tool than the distribution's installer.

1. **Find the file that configures the network card.** On `web01`, which
   uses systemd-networkd, ask `networkctl` for the main card, `eth0`:

   ```sh
   ssh root@203.0.113.11 networkctl status eth0
   ```

   ```text
   ● 4459: eth0
                      Link File: /usr/lib/systemd/network/99-default.link
                   Network File: /etc/systemd/network/10-cloud-init-eth0.network
                          State: routable (configured)
   ```

   **Check:** the output names a `Network File:` in `/etc/systemd/network/`.
   If `networkctl` is missing, or the card shows `unmanaged`, the server
   uses another tool. `ls /etc/netplan /etc/NetworkManager/system-connections /etc/network/interfaces`
   shows which tool's files exist.

2. **Write the whole file in a playbook.** `web01` gets its address from
   DHCP today. Create `playbooks/network_settings.yml`, which gives `web01`
   the same address as a fixed address:

   ```yaml
   # playbooks/network_settings.yml
   fleet_stages:
     - id: network_settings
       nodes: web01
       node_tasks:
         - id: main_network_card
           systemd_networkd_file:
             name: 10-cloud-init-eth0.network   # the file from step 1
             content: |
               [Match]
               Name=eth0

               [Network]
               Address=203.0.113.11/24
               Gateway=203.0.113.1
               DNS=203.0.113.1
             restore_unless_controller_confirms_within: 2m
   ```

   The module takes the file's whole text in the tool's own format.
   `restore_unless_controller_confirms_within: 2m` is how long the node
   waits for the confirmations. Without the confirmations, the node puts
   the old file back when the two minutes end. `2m` is the default. Choose
   a longer time when the network takes longer to come up, such as a bond
   that negotiates with a switch.

   **Check:** `mmx validate network_settings` prints `Valid: mmx_example`.

3. **Preview the change.**

   ```sh
   mmx dryrun network_settings --diff
   echo "exit code $?"
   ```

   ```text
     network_settings / web01 / main_network_card: succeeded, would_change — Would replace /etc/systemd/network/10-cloud-init-eth0.network and activate it. Whether systemd-networkd accepts the proposed network configuration change and whether the node remains reachable after activation were not checked.
     network_settings / web01 / main_network_card: would update "/etc/systemd/network/10-cloud-init-eth0.network"
       40 bytes, permissions_mode 0644, uid 998, gid 998 -> 89 bytes, permissions_mode 0644, uid 0, gid 0
       --- before
       +++ after
       @@ -2,5 +2,6 @@
        Name=eth0

        [Network]
       -DHCP=ipv4
       -
       +Address=203.0.113.11/24
       +Gateway=203.0.113.1
       +DNS=203.0.113.1
   exit code 2
   ```

   **Check:** exit code 2, and the difference shows your change. A dryrun
   changes nothing on the server, so a dryrun can't tell whether the server
   stays reachable. Only the run can.

4. **Run the change.**

   ```sh
   mmx run network_settings
   ```

   ```text
   network_settings / web01: succeeded
     network_settings / web01 / main_network_card: succeeded, changed
     network_settings / web01 / main_network_card: updated "/etc/systemd/network/10-cloud-init-eth0.network"
       40 bytes, permissions_mode 0644, uid 998, gid 998 -> 89 bytes, permissions_mode 0644, uid 0, gid 0
   Run ad684c47: succeeded
   ```

   The run took about three seconds. New SSH and HTTPS connections reached
   `web01` before and after the change, so the node kept the new file and
   removed the restore timer.

   **Check:** the run ends with `Run ...: succeeded`, and the server has
   the fixed address:

   ```sh
   ssh root@203.0.113.11 ip -4 -brief address show eth0
   ```

   ```text
   eth0@if4460      UP             203.0.113.11/24
   ```

   A second `mmx run network_settings` finds the same file and waits for
   nothing:

   ```text
     network_settings / web01 / main_network_card: succeeded, unchanged — Network configuration file /etc/systemd/network/10-cloud-init-eth0.network is unchanged; no activation or confirmation wait was needed.
   ```

5. **See the server put the old file back.** This step makes a mistake on
   purpose, so you know what a cut-off looks like. Skip it on a server
   that serves users: the server is unreachable for two minutes. In
   `playbooks/network_settings.yml`, change the address to `203.0.113.17`,
   a typo for `203.0.113.11`, and run:

   ```sh
   mmx run network_settings
   echo "exit code $?"
   ```

   ```text
   network_settings / web01: failed
     main_network_card: No reachability confirmation arrived over a new HTTPS connection or a new SSH connection within 2m. The previous file /etc/systemd/network/10-cloud-init-eth0.network and its running configuration are restored.
     network_settings / web01 / main_network_card: failed, changed — No reachability confirmation arrived over a new HTTPS connection or a new SSH connection within 2m. The previous file /etc/systemd/network/10-cloud-init-eth0.network and its running configuration are restored.
     network_settings / web01 / main_network_card: requested update "/etc/systemd/network/10-cloud-init-eth0.network"
       89 bytes, permissions_mode 0644, uid 0, gid 0 -> 89 bytes, permissions_mode 0644, uid 0, gid 0
     network_settings / web01 / file change 46975be97b78541d: /etc/systemd/network/10-cloud-init-eth0.network was restored to its previous state
       why the change was not kept: No reachability confirmation arrived over a new HTTPS connection or a new SSH connection within 2m.
   Run a3c6997b: failed
   exit code 1
   ```

   What happened on `web01`, seen from its console:

   | Time | `web01` |
   | --- | --- |
   | 0 s | The new file is in place, and `eth0` has the address `203.0.113.17`. New connections to `203.0.113.11` fail. |
   | 0 s to 2 min | The timer `mmx-owed-restore-46975be97b78541d.timer` waits. |
   | 2 min | The timer starts `mmx-owed-restore-46975be97b78541d.service`. The service puts the previous file back and reloads systemd-networkd. `eth0` has `203.0.113.11` again, and the timer is gone. |

   The run ended about two minutes after it started, and fails with
   `changed`: the network was changed and then put back.

   **Check:** `ssh root@203.0.113.11 true` works, and the file on the server
   is the one from step 4. Change the address back to `203.0.113.11`, and
   run again: the line says `unchanged`.

### What MMX checks, in order

For each network file that changes, MMX does these steps on the node
before the next node task starts:

1. Your `mmx` command opens new SSH and HTTPS connections to the node. Each
   kind of connection that works now must work after the change too. When
   neither kind works, the node task fails, and the file is not changed.
2. The node saves the old file and starts the restore timer. The timer
   runs out after `restore_unless_controller_confirms_within`.
3. The node writes the new file and activates the file with the tool's
   own command, such as `networkctl reload`. When the tool reports an
   error, the restore starts at once.
4. Your `mmx` command opens new SSH and HTTPS connections again. An open
   connection does not count, because a network change can keep open
   connections alive while new connections fail.
5. When every kind of connection from the first step confirms, the node
   keeps the new file and removes the timer. Otherwise the timer puts the
   old file back, the node activates the old file, and the node task fails.

The timer runs on the node, so the node restores an unconfirmed change
also when your computer sleeps, `mmx` ends, or you press Ctrl-C. The
node does not keep an unconfirmed change over a restart of the server: at
boot, the restore service puts the old file back at once. Run `mmx status RUN-ID` later
to collect the result: the lines are the same as in step 5. A node with
`connection: ssh_only` in the inventory needs only SSH confirmations.

A network change is protected one file at a time. When a playbook changes
two files and the second file cuts the server off, the node restores only
the second file. MMX does not follow a server to a new address:
confirmations go to the address in the inventory, so a change that removes
that address from the server is restored. To move a server to a new address, add the
new address next to the old address, change the inventory, then remove the old
address in a later run. When the server you change is also your controller machine, MMX also
checks that your controller machine can still reach the other nodes;
[the execution design](../changing-mmx/design/execution.md#a-change-on-the-controller-machine-itself)
describes the check.

## Change sshd's settings

A wrong sshd setting locks MMX out at the next new connection, and MMX
does not put an sshd file back by itself. MMX's SSH connection logs in
through sshd. MMX's HTTPS service on the node checks each new HTTPS
connection against sshd's current settings for accounts and keys, as
[Login rules that only OpenSSH can check](connect-to-your-servers.md#login-rules-that-only-openssh-can-check)
describes. So keep a way in at every step: open the new port next to the
old port, check new connections, and close the old port only then.

1. **Write sshd's settings in their own file, with both ports.** Create
   `playbooks/ssh_settings.yml`:

   ```yaml
   # playbooks/ssh_settings.yml
   fleet_stages:
     - id: ssh_settings
       nodes: web_servers
       node_tasks:
         - task_block:
             id: sshd_settings
             changes:
               - id: write_sshd_settings
                 file:
                   path: /etc/ssh/sshd_config.d/10-shop.conf
                   permissions_mode: "0644"
                   content: |
                     # Managed by MMX in playbooks/ssh_settings.yml
                     Port 22
                     Port 2222
                     PasswordAuthentication no
                   check_new_file:       # check the new file before it replaces the live one
                     shell:
                       script: sshd -t -f "$NEW_FILE"
                       dryrun: sshd -t -f "$NEW_FILE"
                       env:
                         NEW_FILE: "{{ .new_file.path }}"
             validate:
               - id: check_whole_sshd_configuration
                 shell:
                   script: sshd -t
                   dryrun: sshd -t
                   changed_when: false
             activate:
               # Ubuntu 24.04 and 26.04: ssh.socket holds the listening ports.
               - id: regenerate_ssh_socket_ports
                 when: {equals: ["{{ .node.distribution.id }}", ubuntu]}
                 shell:
                   script: systemctl daemon-reload
                   only_when_tracked_inputs_changed: true
               - id: restart_ssh_socket
                 when: {equals: ["{{ .node.distribution.id }}", ubuntu]}
                 systemd_service:
                   name: ssh.socket
                   state: running
               - id: reload_sshd
                 systemd_service:
                   name: ssh
                   state: running
                   refresh: reload
             verify:
               - id: check_new_port
                 wait_for:
                   tcp_port_accepts_connections: 2222
                   wait_at_most: 10s
   ```

   What each part does:

   - `/etc/ssh/sshd_config.d/10-shop.conf` is a file of its own, which
     Debian's and Ubuntu's `/etc/ssh/sshd_config` reads through its
     `Include` line. sshd uses the first value that it reads for most
     settings, and the name `10-shop.conf` sorts before files such as
     `50-cloud-init.conf` that some cloud images add. `Port` is different: each
     `Port` line adds a port.
   - `check_new_file` runs `sshd -t -f` on the new file alone, before the
     new file replaces the live file. A file that sshd rejects never
     replaces `/etc/ssh/sshd_config.d/10-shop.conf`, so sshd's next start,
     such as after a reboot, can't fail because of the rejected file.
     [Change a configuration and reload its service](change-a-configuration-and-reload-its-service.md#steps)
     explains `check_new_file`.
   - `validate` runs `sshd -t` on the whole configuration, with the new
     file in place. A file can pass `check_new_file` alone and still break
     the whole configuration, for example a file that ends inside a `Match`
     block. A failed `validate` stops the node task block before
     `activate`, so sshd is not reloaded. The new file stays on disk,
     though, so fix the file and run again before you restart sshd or the
     server.
   - `reload_sshd` reloads sshd only when the file differs from the file
     that sshd had when MMX last started or reloaded sshd. Debian's and
     Ubuntu's `ssh` service checks the whole configuration with `sshd -t`
     before the reload. A rejected configuration fails the reload, and the
     running sshd keeps its old settings.
   - On Ubuntu 24.04 and 26.04, `ssh.socket` opens the ports, not sshd. A
     systemd generator makes the socket's ports from sshd's `Port` lines,
     and the generator runs only at `systemctl daemon-reload`.
     `regenerate_ssh_socket_ports` runs the reload, and `restart_ssh_socket`
     then restarts `ssh.socket` with the new ports. A restart of `ssh` alone
     does not open a new port. [Run a command after a change](../reference/modules/shell.md#run-a-command-after-a-change)
     explains `only_when_tracked_inputs_changed`.
   - `check_new_port` checks on the node that sshd accepts connections on
     port 2222.

   **Check:** `mmx validate ssh_settings` prints `Valid: mmx_example`.

2. **Preview the change.**

   ```sh
   mmx dryrun ssh_settings
   echo "exit code $?"
   ```

   ```text
     ssh_settings / web02 / sshd_settings/changes/write_sshd_settings: succeeded, would_change
       check_new_file: succeeded
     ssh_settings / web02 / sshd_settings/changes/write_sshd_settings: would create "/etc/ssh/sshd_config.d/10-shop.conf"
       absent -> 91 bytes, permissions_mode 0644, uid 0, gid 0
     ssh_settings / web02 / sshd_settings/validate/check_whole_sshd_configuration: succeeded, unchanged
     ssh_settings / web02 / sshd_settings/activate/regenerate_ssh_socket_ports: succeeded, would_change — would run: tracked input /etc/ssh/sshd_config.d/10-shop.conf would change
     ssh_settings / web02 / sshd_settings/activate/restart_ssh_socket: succeeded, would_change — tracked input /etc/ssh/sshd_config.d/10-shop.conf changed after the service last started or reloaded
     ssh_settings / web02 / sshd_settings/activate/reload_sshd: succeeded, would_change — tracked input /etc/ssh/sshd_config.d/10-shop.conf changed after the service last started or reloaded
     ssh_settings / web02 / sshd_settings/verify/check_new_port: not_checked, unchanged — dial tcp 127.0.0.1:2222: connect: connection refused
     ssh_settings / web02 / sshd_settings: succeeded, would_change
   Some node tasks are not_checked, usually because what they check is not installed or created yet: an earlier node task does that during the run. This is normal on a new server. mmx run checks them for real; change the playbook only if the run fails.
   Run bdced5d8: succeeded
   exit code 3
   ```

   The lines for `web01` are the same, except that the two Ubuntu node
   tasks say `skipped, unchanged — when is false`.

   **Check:** `check_new_file: succeeded` on each server. Exit code 3 is
   expected: nothing listens on port 2222 before the run, so the dryrun
   can't check `check_new_port`. A rejected file fails here already, as
   [If something is different](#if-something-is-different) shows.

3. **Run the change.** The playbook turns password login off. Before you
   run it, check which key MMX logs in with:

   - If MMX logs in with your own key, go on.
   - If the server took a password at the first contact, MMX made its own
     key in `~/.ssh/mmx/` ([a server that takes only a password](connect-to-your-servers.md#a-server-that-takes-only-a-password)).
     MMX keeps that key working on the new port: at the first run after
     step 4, MMX logs in with it over SSH on port 2222, checks that port
     2222 reaches the same machine, and copies the key to the new port
     ([Other SSH settings](connect-to-your-servers.md#other-ssh-settings)).
     Port 2222 must therefore be open and its host key trusted before step 4,
     which this step's **Check** proves.

   ```sh
   mmx run ssh_settings
   ```

   ```text
     ssh_settings / web02 / sshd_settings/changes/write_sshd_settings: succeeded, changed
     ssh_settings / web02 / sshd_settings/changes/write_sshd_settings: created "/etc/ssh/sshd_config.d/10-shop.conf"
       absent -> 91 bytes, permissions_mode 0644, uid 0, gid 0
     ssh_settings / web02 / sshd_settings/validate/check_whole_sshd_configuration: succeeded, unchanged
     ssh_settings / web02 / sshd_settings/activate/regenerate_ssh_socket_ports: succeeded, changed — ran because tracked input /etc/ssh/sshd_config.d/10-shop.conf changed
     ssh_settings / web02 / sshd_settings/activate/restart_ssh_socket: succeeded, changed — tracked input /etc/ssh/sshd_config.d/10-shop.conf changed after the service last started or reloaded
     ssh_settings / web02 / sshd_settings/activate/reload_sshd: succeeded, changed — tracked input /etc/ssh/sshd_config.d/10-shop.conf changed after the service last started or reloaded
     ssh_settings / web02 / sshd_settings/verify/check_new_port: succeeded, unchanged — TCP port 127.0.0.1:2222 accepts connections
     ssh_settings / web02 / sshd_settings: succeeded, changed
   Run eea95472: succeeded
   ```

   **Check:** the run ends with `Run ...: succeeded`, and you can log in on
   the new port, from your computer:

   ```sh
   ssh -p 2222 root@203.0.113.11 true
   ssh -p 2222 root@203.0.113.12 true
   ```

   Both commands end without an error. Port 22 still works too.

   OpenSSH files a host key under the address and the port, but a key
   that your computer already trusts for port 22 also counts on port 2222
   when the server shows the same key: OpenSSH 10.0 asked no question for
   `ssh -p 2222` here, and `~/.ssh/known_hosts` got no new line. Both
   commands above need no answer then, and neither does step 4. If a
   command does ask about the host key, because your computer has no key
   for this server yet, compare the fingerprint and answer `yes`, as step 1
   of
   [Connect to your servers](connect-to-your-servers.md#a-server-you-log-in-to-with-a-key)
   shows, or read the key with `ssh-keyscan -p 2222 203.0.113.11`. MMX never
   accepts a host key, so step 4 fails with `Host key verification failed`
   until your computer trusts the server's key.

4. **Tell MMX the new port, and check SSH on the new port.** In
   `inventories/example.yml`, add `ssh_port: 2222` to each server:

   ```yaml
   # inventories/example.yml
   nodes:
     web01:
       address: 203.0.113.11
       ssh_user: root
       ssh_port: 2222
     web02:
       address: 203.0.113.12
       ssh_user: root
       ssh_port: 2222
   groups:
     web_servers:
       nodes: [web01, web02]
   ```

   Then run over SSH only:

   ```sh
   mmx run ssh_settings --connection ssh_only
   ```

   Reason: an ordinary run uses HTTPS, which does not go through sshd's
   port, so only `--connection ssh_only` proves the SSH port
   ([Other SSH settings](connect-to-your-servers.md#other-ssh-settings)).
   A node whose key MMX made itself needs the key from step 3 here.

   **Check:** the run ends with `Run ...: succeeded`, and every node task
   says `unchanged`. `mmx results --json` shows `"connection_used": "ssh"`
   for each server. The SSH port that MMX used is the node's `ssh_port` in
   the inventory, or 22 without one: `mmx nodes` does not print the port.

5. **Close the old port.** In `playbooks/ssh_settings.yml`, delete the line
   `Port 22`, then preview and run:

   ```sh
   mmx dryrun ssh_settings --diff
   mmx run ssh_settings
   ```

   The dryrun's difference removes `-Port 22`. The first dryrun or run over
   HTTPS after you changed `ssh_port` prints a line such as
   `203.0.113.12: setting up MMX through verified SSH` for each server.
   [Other SSH settings](connect-to-your-servers.md#other-ssh-settings)
   explains why.

   **Check:** the run ends with `Run ...: succeeded`, port 22 is closed,
   and `mmx run ssh_settings --connection ssh_only` succeeds:

   ```sh
   ssh root@203.0.113.11 true
   ```

   ```text
   ssh: connect to host 203.0.113.11 port 22: Connection refused
   ```

After step 5, `playbooks/ssh_settings.yml` reads:

```yaml
# playbooks/ssh_settings.yml
fleet_stages:
  - id: ssh_settings
    nodes: web_servers
    node_tasks:
      - task_block:
          id: sshd_settings
          changes:
            - id: write_sshd_settings
              file:
                path: /etc/ssh/sshd_config.d/10-shop.conf
                permissions_mode: "0644"
                content: |
                  # Managed by MMX in playbooks/ssh_settings.yml
                  Port 2222
                  PasswordAuthentication no
                check_new_file:
                  shell:
                    script: sshd -t -f "$NEW_FILE"
                    dryrun: sshd -t -f "$NEW_FILE"
                    env:
                      NEW_FILE: "{{ .new_file.path }}"
          validate:
            - id: check_whole_sshd_configuration
              shell:
                script: sshd -t
                dryrun: sshd -t
                changed_when: false
          activate:
            - id: regenerate_ssh_socket_ports
              when: {equals: ["{{ .node.distribution.id }}", ubuntu]}
              shell:
                script: systemctl daemon-reload
                only_when_tracked_inputs_changed: true
            - id: restart_ssh_socket
              when: {equals: ["{{ .node.distribution.id }}", ubuntu]}
              systemd_service:
                name: ssh.socket
                state: running
            - id: reload_sshd
              systemd_service:
                name: ssh
                state: running
                refresh: reload
          verify:
            - id: check_new_port
              wait_for:
                tcp_port_accepts_connections: 2222
                wait_at_most: 10s
```

### Settings that decide who may log in

`AllowUsers`, `AllowGroups`, `DenyUsers`, `DenyGroups`, and
`PermitRootLogin` decide which accounts sshd lets in. Each of these
settings must still let in the account in the inventory's `ssh_user`, or
MMX is locked out. The run that changes the setting still succeeds,
because the run's connections are already open. For example, with
`AllowUsers deploy` added while MMX logs in as `root`, the run that
changes the setting prints a warning on its housekeeping line:

```text
ssh_settings / web01: succeeded
  housekeeping: recording the last successful run on the node: Get "https://203.0.113.11:7443/v1/info": HTTPS 403: SSH account policy denies root
```

The next run fails:

```text
ssh_settings / web01: failed
  Get "https://203.0.113.11:7443/v1/info": HTTPS 403: SSH account policy denies root
```

To change such a setting safely:

1. Put the inventory's account in the allowed list or group first, in an
   earlier node task or an earlier run. For example, add `root` to
   `AllowUsers`, or add the account to the group in `AllowGroups` with
   a [`group`](../reference/modules/group.md) node task.
2. Run the change.
3. Run `mmx run ssh_settings`, then `mmx run ssh_settings --connection ssh_only`.
   Both runs use new connections, so both runs prove that MMX can still
   log in over HTTPS and over SSH.

`PasswordAuthentication no` keeps MMX working, because MMX logs in with a
key: your own key, or the key that MMX made at the first contact with a
server that took a password. MMX carries its own key to a new SSH port after
SSH shows the same machine, but not to a new address
([Other SSH settings](connect-to-your-servers.md#other-ssh-settings)). Before you turn password
login off, check that you can log in with a key yourself.

### On Fedora, Rocky Linux, and other Red Hat family servers

The sshd service is named `sshd`, not `ssh`, so use `name: sshd` in
`reload_sshd`. SELinux lets sshd listen only on ports of the type
`ssh_port_t`. Add a [`selinux_port`](../reference/modules/selinux_port.md) node task
for port 2222 with `selinux_type: ssh_port_t` before the node task block.
The `selinux_port` module needs the package `policycoreutils-python-utils`,
which [`dnf_package`](../reference/modules/dnf_package.md) installs.

A Fedora Server or Rocky Linux server usually runs firewalld with only the
`ssh` service open, so port 2222 stays closed and the `ssh -p 2222`
commands in step 3 time out. Open the port with a
[`firewalld`](../reference/modules/firewalld.md) node task before the node task
block. The zone is the zone of the server's network card;
`firewall-cmd --get-active-zones` on the server shows the zone:

```yaml
# Before the task_block. Written from the firewalld module page; not run
# on a real Red Hat family server.
- id: allow_new_ssh_port
  firewalld:
    zone: public
    rules:
      - {action: allow, port: 2222, protocol: tcp}
    rules_not_listed: keep
```

`rules_not_listed: keep` leaves the other rules of the zone as they are.
Keep the rule that allows the `ssh` service until step 5 has closed port 22.

## What MMX does not protect

- **sshd's files.** No module puts an sshd file back. The order of steps
  in [Change sshd's settings](#change-sshds-settings) is the protection.
- **Network settings that a module other than the network modules
  writes**, such as a `file`, `line`, or `shell` node task that edits
  `/etc/network/interfaces` or runs `ip`. Use the module from the table in
  [Change a network file](#change-a-network-file).
- **Firewall rules.** [`ufw`](../reference/modules/ufw.md) refuses a change after
  which no rule lets sshd's port in from the address that MMX's connection
  came from. [`firewalld`](../reference/modules/firewalld.md) refuses a change after
  which the zone no longer allows sshd's port, or rejects the port for every
  source. `firewalld` does not check which sources the allow rule names. A
  rule that passes the check is not put back. [`firewalld_zone_membership`](../reference/modules/firewalld_zone_membership.md),
  which moves a network card into another firewalld zone, is protected
  like a network file. Firewall rules from a `file` or `shell` node task,
  such as nftables rules, are not checked at all. A firewall that closes
  port 7443 does not lock MMX out: MMX uses SSH instead, as
  [A server whose port 7443 is closed](connect-to-your-servers.md#a-server-whose-port-7443-is-closed)
  says.
- **Several files together.** Each network file is confirmed and restored
  by itself, as [What MMX checks, in order](#what-mmx-checks-in-order) says.

## Get back in

| What happened | What to do |
| --- | --- |
| A network change cut the server off, and the run still waits | Wait. The node puts the old file back when `restore_unless_controller_confirms_within` ends, two minutes by default. The run then ends with `was restored to its previous state`. |
| `mmx` ended during a network change, such as after Ctrl-C | Wait for `restore_unless_controller_confirms_within`, two minutes by default. Then run `mmx status RUN-ID`, with the run ID that the run printed at its start, to collect the restore's result. |
| The run ends with `restore completion is unconfirmed`, and the server stays unreachable | The restore on the server failed. See [A restore that did not finish](#a-restore-that-did-not-finish). |
| An sshd setting shuts MMX and you out | Open the server's console. Fix the file, such as `/etc/ssh/sshd_config.d/10-shop.conf`, then run `sshd -t && systemctl reload ssh` there. On Ubuntu, a port fix also needs `systemctl daemon-reload && systemctl restart ssh.socket`. On the Red Hat family, the service is `sshd`. Fix the playbook the same way, and run the playbook. The next run reloads sshd again, because the file on disk differs from the file that MMX last reloaded sshd with: MMX does not see a reload that you run by hand. |
| A firewall rule shuts SSH out | Open the server's console, and remove the rule with `ufw` or `firewall-cmd`. Fix the playbook, and run the playbook. |
| `ssh: connect to host ... port 22: Connection refused` on a run with `--connection ssh_only`, while ordinary runs succeed | sshd moved to another port, and the inventory still names the old port. Set `ssh_port` in the inventory, as in [step 4](#change-sshds-settings). |

### A restore that did not finish

A restore can fail, such as when the network tool on the server is
broken. The server then stays cut off, so MMX can't learn what happened.
The node keeps the owed restore and the saved old file, and refuses a later
change of the same file until the restore succeeds. In this example,
`networkctl` was missing on `web01` when the restore ran. After about ten
minutes of trying to reach `web01`, the run ended with:

```text
network_settings / web01: unknown
  cannot collect node job 6ab0f40b9fa9e681b66b10a7b467fa3feeb71a78f1a10755bad9fa25c052b2af after file change 2323d0d805429de0; restore completion is unconfirmed; run mmx status 20261115T073956.152627178-79c3a64532ba51f4 to inspect it again: context deadline exceeded
  network_settings / web01 / file change 2323d0d805429de0: the restore of /etc/systemd/network/10-cloud-init-eth0.network has not started yet
    restore service on the node: mmx-owed-restore-2323d0d805429de0.service
Run 79c3a645: running
```

The line `has not started yet` is what MMX knew when MMX lost contact.
The line names the restore service, which you need on the server.

1. **Open the server's console,** because the server is not reachable over
   the network.

2. **Read why the restore failed.** On the console:

   ```sh
   journalctl -u mmx-owed-restore-2323d0d805429de0.service
   ```

   ```text
   mmx-runner[4610]: owed restore 2323d0d805429de0 failed: Reloading systemd-networkd: networkctl is not installed; install the package that provides it in an earlier node task: systemd (apt, pacman), systemd-networkd (dnf, zypper); correct the cause and run systemctl start mmx-owed-restore-2323d0d805429de0.service
   ```

   **Check:** `systemctl status mmx-owed-restore-2323d0d805429de0.service`
   says `failed`.

3. **Fix the cause** that the message names, such as a missing program.

4. **Start the restore again,** with the command from the message:

   ```sh
   systemctl start mmx-owed-restore-2323d0d805429de0.service
   ```

   **Check:** `systemctl status mmx-owed-restore-2323d0d805429de0.service`
   says `Unit mmx-owed-restore-2323d0d805429de0.service could not be found.`
   The node removes the restore's units after a restore that succeeded.
   `ssh root@203.0.113.11 true` works again from your computer.

5. **Collect the result** on your computer:

   ```sh
   mmx status 79c3a645
   ```

   ```text
   network_settings / web01: failed
     main_network_card: No reachability confirmation arrived over a new HTTPS connection or a new SSH connection within 2m. Restoring /etc/systemd/network/10-cloud-init-eth0.network failed: Reloading systemd-networkd: networkctl is not installed; install the package that provides it in an earlier node task: systemd (apt, pacman), systemd-networkd (dnf, zypper). The owed restore and saved previous file remain; the previous running configuration is not confirmed restored.
     ...
     network_settings / web01 / file change 2323d0d805429de0: /etc/systemd/network/10-cloud-init-eth0.network was restored to its previous state
       why the change was not kept: No reachability confirmation arrived over a new HTTPS connection or a new SSH connection within 2m.
   Run 79c3a645: failed
   ```

   **Check:** the last `file change` line says `was restored to its
   previous state`. The node task's reason keeps the first restore's
   failure, and the node task stays `failed`, because the change was not
   kept. Fix the playbook, and run the playbook again.

## If something is different

| What you see | What it means and what to do |
| --- | --- |
| `No reachability confirmation arrived over a new HTTPS connection or a new SSH connection within 2m. The previous file ... and its running configuration are restored.` | The new network file cut the server off, and the node put the old file back. Fix the file in the playbook, and run again. |
| `No new SSH or HTTPS connection confirmed node reachability before the file change; ... was not changed.` | MMX could not open a new connection to the server before the change, so the node changed nothing. Check that `mmx run` reaches the server, as [Connect to your servers](connect-to-your-servers.md) says. |
| `Cannot change ...: node job ... still owes a restore of this file. Restore service: mmx-owed-restore-....service.` | An earlier change of the same file has a restore that is not finished. Wait for the restore, or repair a failed restore as [A restore that did not finish](#a-restore-that-did-not-finish) says. |
| `the restore of ... has not started yet`, on a change that failed or that MMX could not confirm | The restore is waiting on the node. The next line, `restore service on the node`, names the systemd unit that runs it. See [A restore that did not finish](#a-restore-that-did-not-finish). |
| `check_new_file failed: exit status 255`, with sshd's message, such as `line 3: Bad configuration option: PasswordAuthentcation` | sshd rejected the new file, and the live file stays as it was. The message names the line. Fix the file in the playbook, and run again. A dryrun shows the same failure. |
| `housekeeping: recording the last successful run on the node: ... HTTPS 403: SSH account policy denies root` | An sshd setting no longer lets in the inventory's account. The next run fails. Fix the setting now, from the console if MMX can no longer log in, as [Settings that decide who may log in](#settings-that-decide-who-may-log-in) says. |
| `HTTPS 403: SSH account policy denies root` and the run fails | An sshd setting no longer lets in the inventory's account. See [Get back in](#get-back-in). |
| `check_new_port: not_checked, unchanged — dial tcp 127.0.0.1:2222: connect: connection refused` in a dryrun | Nothing listens on the new port before the run. Run the change. |
| `ssh -p 2222` times out, or `check_new_port` fails, on Fedora or Rocky Linux | firewalld keeps port 2222 closed. Add the `firewalld` node task from [On Fedora, Rocky Linux, and other Red Hat family servers](#on-fedora-rocky-linux-and-other-red-hat-family-servers). |
| `Host key verification failed` on the run of step 4 | Your computer does not trust the server's host key. Trust the key as step 3 says. |
| `check_whole_sshd_configuration` fails, while `check_new_file` passed | The new file breaks the combined configuration. The new file stays on disk, so fix the file in the playbook and run again before sshd or the server restarts. |
| `check_new_port: failed` in a run, on Ubuntu | `ssh.socket` did not open the new port. Check that `regenerate_ssh_socket_ports` and `restart_ssh_socket` ran. |

## Where to go next

- [Connect to your servers](connect-to-your-servers.md): SSH settings in
  the inventory, host keys, and a server whose port 7443 is closed.
- [Change a configuration and reload its service](change-a-configuration-and-reload-its-service.md):
  `check_new_file`, `validate`, and putting files back when a node task
  block fails.
- The network module pages, such as
  [`systemd_networkd_file`](../reference/modules/systemd_networkd_file.md) and
  [`netplan_file`](../reference/modules/netplan_file.md): every input, the tool's own
  checks, and every error.
- [Roll out a change gradually](roll-out-a-change-gradually.md): change one
  server first, before a risky change reaches the others.
