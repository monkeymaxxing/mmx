# `valid_page`

An introduction with links to [`file`](file.md),
[the playbook file reference](../../docs/reference/playbook-file.md#node-task-retries-and-time-limits),
and [the code](symlink.go), and a code span `[not a link](../../docs/shared_rules.md)`.

## Set up

Install nothing.

## Examples

```yaml
## A comment, not a section, inside a code fence
- symlink:
    path: /srv/app/current
    target: /srv/app/releases/1.2.3
```

## Tested on

**Distributions:** Debian 12 and 13, Ubuntu 24.04 and 26.04.

A suite.

## Why it works this way

A reason.
