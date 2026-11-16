# See the effective configuration

Set the address in `inventories/example.yml`, then run:

```sh
mmx validate
mmx explain configure
mmx explain configure --limit app --json
mmx dryrun configure
mmx run configure
mmx dryrun configure
```

The role receives the project port and the node's greeting. Explain shows
the project port and node greeting values, the values' YAML locations,
inherited timing, and the ordered tasks. Command output is labelled as
runtime data. Explain does not connect to the node.

The first dryrun proposes a file change. After a successful run, the next
dryrun finds the file unchanged. The [workflow exit-code
table](../../docs/changing-mmx/design/workflow.md#exit-results) defines the
proposed-change and unchanged outcomes, incomplete inspection, failures,
command errors, and Ctrl-C.

The shell task demonstrates a read-only dryrun, output matching for an
additional failure condition, and a later task conditioned on its output.
`contains` matches literal text; `matches` uses Go regular expressions.
`contains` and `matches` require complete string values. Referencing output
that was truncated at the output size limit fails the condition rather than
comparing only part of the output.
