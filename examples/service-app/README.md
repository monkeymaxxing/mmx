# A managed application service

Set the server address and administrator in `inventories/example.yml`, then run:

```sh
mmx dryrun worker
mmx run worker
mmx run worker
```

The role creates a system group and non-login service account, then creates
the service account's working directory. The role installs a small Bash
worker and configuration, then validates and installs the worker's systemd
unit. The role enables and starts the worker, then checks what the worker
loaded. The worker is a small fixture for the service lifecycle, not an HTTP
server.

Change `message` to exercise a configuration reload. Change `label` to
change the unit's environment: MMX reloads the systemd manager and restarts
the worker, even though ordinary configuration refresh is `reload`. An
unchanged rerun does not reload the systemd manager or restart the worker.
`/srv/mmx-worker/loaded` shows the active label/message.

A fresh dryrun reports ownership depending on the new account and unit validation
as not yet checked. It creates no account, directory, or service. The first apply
validates the unit after installing its prerequisites.

For explicit IDs, account removal, and service drop-ins, see the
[account-and-unit example](../account-and-unit/README.md). Directory copies and
recursive permissions are described on the [`directory` page](../../docs/reference/modules/directory.md).
