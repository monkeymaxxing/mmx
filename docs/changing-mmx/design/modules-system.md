# MMX: system modules

Part of the [main plan](README.md). `docs/changing-mmx/design/modules-system.md` gives
an overview of the core modules that manage a node's system resources.
The scope includes:

- Accounts, SSH access, and the firewall.
- Scheduled jobs, the node's name and time zone, kernel settings and kernel
  modules, and the alternatives system.
- Swap, filesystems and mounts, SSH key pairs, access control lists, and
  capabilities.
- Cleanup of the systemd journal and of old files.

`docs/changing-mmx/design/modules-system.md` also records decisions that span the system
modules. Each module's page gives the module's behavior, examples, and
reasons. The [module index](../../reference/modules/README.md) lists every page.
[Modules](modules.md) owns the behavior every module shares: dryrun,
results, retries, the rule for
[a task that owns a list](modules.md#a-task-that-owns-a-list-says-what-happens-to-the-rest),
and how a node task block's tracked inputs cause a restart.

| Module | Manages |
|---|---|
| [`user`](../../reference/modules/user.md) | One local account and its supplementary groups. |
| [`group`](../../reference/modules/group.md) | One local group and its supplementary members. |
| [`authorized_keys`](../../reference/modules/authorized_keys.md) | One account's authorized SSH keys. |
| [`ufw`](../../reference/modules/ufw.md) | The ufw firewall: default policies, incoming, routed, and outgoing rules, and whether it is on. |
| [`firewalld`](../../reference/modules/firewalld.md) | The services, ports, and rich rules of one firewalld zone. |
| [`firewalld_zone_membership`](../../reference/modules/firewalld_zone_membership.md) | The interfaces and IP source networks assigned to one firewalld zone, with confirmed reachability and autonomous restoration. |
| [`firewalld_zone`](../../reference/modules/firewalld_zone.md) | Create or remove a zone, with an explicit global reload when needed. |
| [`selinux_boolean`](../../reference/modules/selinux_boolean.md) | One SELinux boolean, now and after a reboot. |
| [`selinux_file_context`](../../reference/modules/selinux_file_context.md) | The SELinux label rule for a path, and the labels of the files there. |
| [`selinux_port`](../../reference/modules/selinux_port.md) | The SELinux type of a network port. |
| [`selinux_mode`](../../reference/modules/selinux_mode.md) | Whether SELinux enforces or only reports. |
| [`systemd_scheduled_job`](../../reference/modules/systemd_scheduled_job.md) | A command that systemd runs on a schedule. |
| [`cron_job`](../../reference/modules/cron_job.md) | One cron entry in its own file under `/etc/cron.d/`. |
| [`hostname`](../../reference/modules/hostname.md) | The node's host name. |
| [`time_zone`](../../reference/modules/time_zone.md) | The node's time zone. |
| [`locale`](../../reference/modules/locale.md) | One locale that the node offers, and whether it is the system default. |
| [`sysctl_setting`](../../reference/modules/sysctl_setting.md) | One kernel setting, now and after a reboot. |
| [`kernel_module`](../../reference/modules/kernel_module.md) | One kernel module, loaded now and after a reboot, with its options. |
| [`update_alternatives`](../../reference/modules/update_alternatives.md) | Which installed program a generic command, such as `editor`, runs. |
| [`swap_file`](../../reference/modules/swap_file.md) | A swap file, active now and after a reboot. |
| [`create_filesystem_on_empty_device`](../../reference/modules/create_filesystem_on_empty_device.md) | A filesystem on a device that holds nothing yet. |
| [`fstab_mount`](../../reference/modules/fstab_mount.md) | One mount kept in `/etc/fstab`. |
| [`ssh_key_pair`](../../reference/modules/ssh_key_pair.md) | An SSH key pair for an account, created once. |
| [`file_access_control_list`](../../reference/modules/file_access_control_list.md) | The access control list of a file or directory. |
| [`linux_capability_on_program_file`](../../reference/modules/linux_capability_on_program_file.md) | The Linux capabilities of a program file. |
| [`systemd_journal_vacuum`](../../reference/modules/systemd_journal_vacuum.md) | Deletes archived files of the systemd journal with `journalctl --vacuum-size` and `--vacuum-time`. |
| [`delete_old_files`](../../reference/modules/delete_old_files.md) | Deletes the regular files of one directory whose names match a pattern and that are older than a duration. |

Each module is named for the tool it drives and exists on every distribution
that has the tool. A module whose tool is not installed fails with a message
that names the package to install first. A dryrun reports the task not
checked, because an earlier task may install the missing tool.

## Decisions

The decisions below are settled. Revisit a decision only with a concrete
workflow that the decision fails to support.

| Decision | Reason |
|---|---|
| `ufw` rules require `protocol: tcp`, `udp`, or `tcp_and_udp`. `remove` owns rule order within each kind, and `keep` appends new rules. | [Required settings](modules.md#a-setting-with-no-safe-guess-is-required) make the opened protocols visible. ufw uses the first matching rule, so order must follow an owned list. An SSH allowance stays installed during reordering. |
| Each `firewalld_zone_membership` list has its own policy, `interfaces_not_listed` or `source_networks_not_listed`. An omitted list stays unchanged. | An interface-only task must not delete sources; this follows [explicit deletion](modules.md#leaving-a-setting-out-never-deletes). |
| `firewalld_zone` identifies its target with `zone`. | The zone creation, rules, and membership modules name the same resource in the same way. |
| `firewalld` owns services, ports, and rich rules, but leaves the zone target unchanged. Its SSH safeguard applies to every zone, including zones without SSH traffic. | The target can admit additional traffic. A task must explicitly retain an SSH allowance under the existing safeguard. |
| File permissions, directory cleanup, archive unwrapping, and Git selectors follow the [file and release directory decisions](modules.md#files-and-release-directories). | Resource ownership and access choices for file permissions, directory cleanup, archive unwrapping, and Git selectors belong to the file modules. |
| A sudo rule, a log rotation rule, and an NFS export are `file` tasks with `check_new_file` or a `systemd_service` task. | The existing task and its check already do the job. |
| `user` requires `create_home_directory` with a present account; membership fields name primary and supplementary groups separately. | A service account and a person's account need different home choices; the [required-setting rule](modules.md#a-setting-with-no-safe-guess-is-required) keeps the choice visible. Membership names identify exactly which relationship changes. |
| `user` uses `only_when_account_has_no_password` and `also_delete_home_directory_and_mail`; `group` uses `supplementary_members`. | The names state when the password changes, what removal deletes, and which memberships the task manages. |
| `authorized_keys` preserves existing lines for listed keys without options under `keep` and `remove`. Different explicit options require `replace_options_of_listed_keys: true`. File inputs name the controller machine. | A public-key file without options must not discard access restrictions, and a written restriction must never be silently ignored. The `replace_options_of_listed_keys: true` override follows the [protective-choice rule](modules.md#protective-choices-come-in-three-levels). Path names say where files are read. |
| `ssh_key_pair` uses `key_comment_when_created`. `update_alternatives` uses `priority_when_registered`, `program_path`, and `generic_command_path`, with matching program-path result fields. | [Creation names](modules.md#a-setting-used-only-at-creation-says-so-in-its-name) state when a changed value takes effect. `program_path` and `generic_command_path` distinguish the program from its generic command. |
| `sysctl_setting` removes its boot file through `boot_setting_removed_live_value_kept`. | MMX cannot infer a replacement live value, so `boot_setting_removed_live_value_kept` names both effects: removal of the boot file and retention of the live value. |
| `kernel_module` without `options` keeps its saved options; `options: {}` deletes them. | The [omission rule](modules.md#leaving-a-setting-out-never-deletes) prevents a load-only task from deleting configuration. |
| `linux_capability_on_program_file` requires `capabilities_not_listed`; listed capabilities become permitted and effective. | The [list-ownership rule](modules.md#a-task-that-owns-a-list-says-what-happens-to-the-rest) makes removal explicit; `keep` retains unlisted capabilities' flags. |
| `swap_file` writes sizes as whole `MiB`, `GiB`, or `TiB` values. | Storage tasks use one size notation, including partition and LVM tasks. |
| `fstab_mount` requires `options` and `check_filesystem_at_boot`; its paths are `mount_source` and `mount_point`. | The [required-setting rule](modules.md#a-setting-with-no-safe-guess-is-required) keeps mount and boot choices visible. Boot checks use named choices instead of unexplained pass numbers; source means the mounted resource. |
| `file_access_control_list` explains that `setfacl -m` recalculates the mask. | Starting group mode bits do not prevent named entries from gaining their requested access. |
| `selinux_mode` refuses `disabled` and offers `permissive` to stop enforcement. | Enterprise Linux 9 ignores `SELINUX=disabled` as a way to disable SELinux; the kernel parameter `selinux=0` disables it. |


`systemd_unit` manages per-unit drop-ins, not systemd's dash-prefix directories
such as `app-.service.d`. An unsupported name fails clearly. Per-unit
drop-ins cover the same configuration as dash-prefix directories without
another directory naming form.

## Routine cleanup of disk space

A full disk is among the most common faults of a server.
The usual fixes are few: cleanup of the journal, old logs, container
images, and apt's downloaded package files.
Each cleanup has a core module instead of a `shell` recipe, so a dryrun
says what the cleanup would delete.
The module name and inputs also let a person or program reading the
playbook identify what the cleanup does.

[`systemd_journal_vacuum`](../../reference/modules/systemd_journal_vacuum.md) and
[`delete_old_files`](../../reference/modules/delete_old_files.md) belong in the system
modules design document.
[`docker_unused_images` and `podman_unused_images`](modules-applications.md#unused-images)
and [`apt_downloaded_package_files`](modules-packages.md#apts-downloaded-package-files)
belong to their tools' documents.

| Decision | Reason |
|---|---|
| `systemd_journal_vacuum` takes `keep_at_most_size` (whole MiB, GiB, or TiB, as `swap_file`'s `size`) and `keep_newer_than` (a duration with days, as `run_history_retention`). At least one of `keep_at_most_size` and `keep_newer_than` is required, and neither has a default. `systemd_journal_vacuum` runs `journalctl --vacuum-size` and `--vacuum-time` with both in one command. | journalctl applies journald's own rule, so MMX keeps no second rule that could delete differently. How much journal a node keeps depends on the node's purpose. |
| `systemd_journal_vacuum` reports the archived files that are gone afterwards and their bytes on disk, from the journal's directories before and after. A dryrun applies journald's rule to the journal's directories. A dryrun always selects empty files. Then the dryrun selects files oldest first while a directory's files exceed the size or a file's first entry is older than the age. The dryrun uses the earliest of the file name's and the file's own times. | journalctl's summary rounds the bytes. The prediction lets a dryrun say what a run deletes, so `mmx check-one-time-playbooks` sees whether a node still needs the cleanup. |
| `systemd_journal_vacuum` deletes only archived files. | journald writes to the active files. The `systemd_journal_vacuum` page says how to keep the journal small for good, with journald's own settings. |
| `delete_old_files` requires `directory`, `name_patterns`, `older_than`, and `include_subdirectories`. `delete_old_files` deletes regular files only, follows no symlink below the directory, and enters no mount point below the directory. As with deletions by the `directory` module, `delete_old_files` refuses a directory less than two levels deep. | A cleanup must delete only what lies in the selected directory. A link or a mount inside a log directory can lead to a data disk. Deleting files in subdirectories changes the cleanup's scope, so a `delete_old_files` task must set `include_subdirectories` explicitly. |
| `delete_old_files` opens the selected directory's path components in order from `/`, without following a link. When a path component is a symlink, `delete_old_files` fails and names the component. A failure caused by a symlink is a configuration error and is not tried again. | The node task may delete only below the directory path written in the playbook. A reader of the playbook, or a program that decides which fixes run without a person, sees the written path. Following a link on the path would make `delete_old_files` delete elsewhere. For example, `/var/log/postgresql` could point to `/var/lib/postgresql`. |
| `delete_old_files` compares `older_than` with the modification time, and matches each pattern on the file name alone, as `find -name` does. | The modification time of a log file is when it was last written; the access time changes with every read. A pattern without `/` cannot reach into another directory. |
| `delete_old_files` reports the files and bytes deleted and names up to ten deleted files. Deletion previews from the `directory` module also count and name deletions. | A dryrun of a cleanup must show what the cleanup would delete without a list as long as the directory. |

## Beyond the first release

- nftables as a module; the template-and-file pattern covers it.
- Disk encryption.
- Account expiry and password ageing.
