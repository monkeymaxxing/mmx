# A configuration directory and its service

Set the server address and administrator in `inventories/example.yml`, then run:

```sh
mmx dryrun copy_config --diff
mmx run copy_config
mmx run copy_config
```

MMX copies `conf.d/` into `/etc/mmx-config-demo/`, validates its `name=value`
lines, and starts a small demonstration service. Each start or reload records
the loaded configuration in `/srv/mmx-config-demo/loaded` and appends one line
to `/srv/mmx-config-demo/actions`.

Change either local configuration file and run again. The service reloads
once. An unchanged rerun leaves the service alone. If validation fails after
copying, the service keeps its previous configuration. Fix the source and
use `mmx run copy_config`. Use `mmx resume` when you want the failed run's
saved files and instructions.

The copy includes dotfiles and empty directories. The directory copy
preserves unrelated files on the server. Removing a local source file does
not delete its installed copy; declare `file: {path:
/etc/mmx-config-demo/old.conf, state: absent}` for deletion. Use individual
`file.template` tasks for files that need rendered values.
