# Size configuration from node facts

This small service demonstrates configuration based on the managed server's CPU
and RAM. Set your node in `inventories/example.yml`, then run:

```sh
mmx validate
mmx dryrun sized_service
mmx run sized_service
mmx results --json
```

The template sets one worker per online logical CPU and allocates 25% of total
RAM, in whole MiB, to a cache. Change `cache_percent` in the role call to choose
another percentage. `percent` accepts nonnegative integers and percentages from
0 to 100 and rounds down.

MMX collects `.node` facts automatically once per node job, including
dryrun. A later run or resume reads the node facts again. After resizing the
server, run MMX again. MMX renders the template with the new capacity and
reloads the service if the service's configuration changed. An unrelated
fact change does not cause a reload. The demo service copies its
configuration to `loaded` on start/reload and records each action in
`/srv/mmx-facts-demo/actions` so starts and reloads can be inspected.

Saved results include the snapshot, collection time, and any fields that could
not be read. Human results identify differences from the preceding observation for
that node within the same run, including earlier resume attempts. A new run
starts a new baseline; MMX maintains no separate facts database.

The [facts
catalog](../../docs/changing-mmx/design/configuration.md#facts-and-results-are-observations-not-defaults)
lists kernel, boot, OS, capacity, network, and mount fields. The [Linux
acceptance suite](../../tests/integration/node_facts.py) independently
checks the catalog's facts and resizes the disposable test VM to verify
actual service behavior.
