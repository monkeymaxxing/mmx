# Accounts and service drop-ins

Set the node in `inventories/example.yml`. Choose the account IDs in
`mmx_project_settings.yml`, then run:

```sh
mmx dryrun account_service
mmx run account_service
mmx run account_service
```

The example creates a service account with explicit IDs and runs a small
demonstration service under the service account. IDs select new accounts or
check existing ones; MMX reports a mismatch instead of renumbering accounts
or reassigning files.

The `20-limits.conf` drop-in sets the service's memory limit and
environment. Change `memory_max` or `profile` and run again. MMX validates
the combined unit, reloads systemd's definitions, and restarts the service.
An unchanged rerun skips the reload and restart. Other drop-ins stay
untouched.

To decommission the example:

```sh
mmx dryrun remove
mmx run remove
```

The removal playbook stops and disables the service explicitly. Then the
removal playbook removes the service's named unit files, followed by the
account and group. Unit removal alone does not stop or disable a service.
Account removal preserves home, mail, and other files, including
`/srv/mmx-account-demo`; native `userdel` policy may also remove an empty
same-named group. Native tools reject an account or group still in use.
