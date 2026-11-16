# Security updates on Debian and Ubuntu

This is the Debian and Ubuntu way to apply security updates: the
distribution's `unattended-upgrade` tool, from the package
`unattended-upgrades`. MMX installs and configures `unattended-upgrade` with
existing modules. A node is a Linux server that MMX manages; run `mmx` on
your controller machine, such as your laptop.

Set each node's address in `inventories/example.yml`, the inventory that
lists the nodes and their SSH accounts. Review the two choices below, then
run the setup playbook, the file that lists what each node does:

```sh
mmx dryrun set_up_automatic_security_updates
mmx run set_up_automatic_security_updates
mmx run set_up_automatic_security_updates
```

On each node, in order, the playbook:

1. installs `unattended-upgrades` with `apt_package`;
2. writes `/etc/apt/apt.conf.d/20auto-upgrades` to request a daily refresh
   of package lists and a daily unattended upgrade;
3. writes `/etc/apt/apt.conf.d/52unattended-upgrades-local` with two
   commented choices: no automatic reboot, and removal of automatically
   installed dependencies that nothing needs anymore;
4. enables and starts `apt-daily.timer` and `apt-daily-upgrade.timer` with
   `systemd_service`.

The second setup run changes nothing. The timers choose when the daily
work starts; it does not run at the same clock time on every node.

## Choices to review

**Allowed origins stay with the distribution.** The file
`/etc/apt/apt.conf.d/50unattended-upgrades` selects which package sources
may supply updates. The example leaves
`/etc/apt/apt.conf.d/50unattended-upgrades` untouched and uses the
distribution's defaults for security updates. The distribution's defaults
also allow the base release origin, so they do not promise that every
upgraded package is a security fix. Existing local changes and other apt
configuration files can broaden the origins further. Review the node's
allowed origins if you need a strict security-only policy. See the upstream
defaults for
[Debian](https://github.com/mvo5/unattended-upgrades/blob/master/data/50unattended-upgrades.Debian)
and
[Ubuntu](https://github.com/mvo5/unattended-upgrades/blob/master/data/50unattended-upgrades.Ubuntu).

**No automatic reboot.** `Automatic-Reboot "false"` leaves reboot timing
to you. A kernel update needs a reboot to use the new kernel. Neither
playbook reboots a node. The [`reboot` page](../../docs/reference/modules/reboot.md)
shows how to reboot nodes in order and wait for them to return.

**Remove unused dependencies.** `Remove-Unused-Dependencies "true"` removes
automatically installed packages that nothing needs anymore. If you want to
keep unused automatically installed dependencies, set
`Remove-Unused-Dependencies` to `"false"` in the setup playbook.

## Apply security updates now

After setup, use the second playbook when you cannot wait for the timer:

```sh
mmx dryrun apply_security_updates_now
mmx run apply_security_updates_now
```

The fleet stage, which selects nodes and their work, uses
`max_nodes_at_once: "100%"`: every selected node can work at once.
On each node, the `shell` script refreshes package lists with `apt-get update`
and runs `unattended-upgrade` with the installed configuration.

The script compares the list of installed packages and their versions before
and after the upgrade (`dpkg-query --show`). The script exits with 2 when
the list changed: a package was upgraded, added, or removed. MMX accepts 0
and 2 as success and reports changed only for 2. A refresh of the package
lists alone does not report changed. A command that fails fails the task.

Dryrun runs only `unattended-upgrade --dry-run`, the tool's own simulation.
The simulation does not refresh package lists or upgrade packages, so
`unattended-upgrade --dry-run` uses the node's current package lists. MMX
reports unchanged on success because the simulation changes no package.
Inspect the tool's output and logs for the proposed work. See the [tool's
manual](https://manpages.debian.org/trixie/unattended-upgrades/unattended-upgrade.8.en.html)
and the [`shell` page on its `dryrun`
command](../../docs/reference/modules/shell.md#the-dryrun-command).

## Other distributions

On the Red Hat family, use `dnf_package` with `upgrade_all_packages_within_their_version_rules: true` and
`only_security_updates: true`. The package sources must provide security
advisories; see [Apply only security updates](../../docs/reference/modules/dnf_package.md#apply-only-security-updates).
