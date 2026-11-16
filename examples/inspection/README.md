# Inspect a run

Set your nodes in `inventories/example.yml`, then:

```sh
mmx dryrun configure --output=summary --diff
mmx run configure
mmx results --diff=all
mmx results --node app01 --task app_config --diff=all
mmx results --json --diff=all
```

The template produces different contents for each node. Summary output keeps
the nodes' file changes separate. Matching outcomes appear once with their
count and node names. `show` values remain visible. `--output=nodes` lists
each node; the default `auto` uses that view through ten selected nodes and
summary above ten. The view is chosen once per command. A run whose node
selection depends on earlier results uses the inventory size as its initial
upper bound.

Bare `--diff` shows bounded previews. `--diff=all` reads complete retained
text differences from local history, including differences from previous
attempts. JSON always keeps individual results. `--json --diff=all` also
includes a `full_differences` object keyed by the SHA-256 references in the
individual JSON results. Saved results work after the controller restarts
and while the servers are unavailable. Saved results never compare today's
files or execute tasks.

Full differences are captured automatically for text inputs up to 1 MiB
each, within an 8 MiB budget per node job. Identical differences share one
copy. Larger files still install normally. Results explain any diagnostic
omission. Directory copies retain differences for their first ten change
examples. Secret-consuming tasks, private files, binary data, and deleted
contents are excluded before artifacts are stored. Stored differences are
for inspection, not file backups.

The existing `run_history_retention` setting owns the run's local
differences; the controller process downloads the differences when saving a
finished result. A difference that is not available locally (still running,
unconfirmed, never downloaded, missing, or corrupt) is shown as not
available locally, and JSON lists the unavailable difference in
`full_differences_unavailable`. Ordinary results and the saved task outcomes
remain available; nothing is rerun.
