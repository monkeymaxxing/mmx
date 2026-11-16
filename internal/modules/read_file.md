# `read_file`

`read_file` reads one file on the node, so that a later node task, or through
a fleet stage output another node, can use its content. It changes nothing on
the node. To copy a file from the node to the controller machine, use
`read_file` and a fleet stage's
[`save_on_controller_machine`](../../docs/how-to/use-a-result-from-one-server-on-another.md#save-outputs-to-files-on-your-controller-machine);
MMX has no separate `fetch` module.

## Examples

### Pass a cluster's join token to the other servers

A k3s cluster's first server writes a join token that the other servers need:

```yaml
# playbooks/cluster.yml
fleet_stages:
  - id: start_first_server
    nodes: server01
    node_tasks:
      - id: join_token
        read_file:
          path: /var/lib/rancher/k3s/server/token
          content_is_secret: true
    outputs:
      join_token: "{{ .node_tasks.join_token.content }}"
```

On the node `server01`, the node task reads the token file. The node task's
result field `content` holds the file's text, and the fleet stage passes it
on as its output `join_token`. Both fields are required:
`content_is_secret` makes every such node task say whether the content may
appear in output and saved results. With `true`, the content is a secret that
started on a node;
[Secrets that start on a node](../../docs/reference/secrets.md#secrets-that-start-on-a-node)
shows how the token reaches nodes joining the cluster and how `mmx resume`
handles the secret content.

## How it works

`read_file` changes nothing and reports unchanged, so it also runs during
`mmx dryrun`.

- `path` is an absolute path of a regular file of at most 1 MiB. Links are
  followed. A directory, a device, or a larger file fails the node task.
- A missing file fails the node task. During dryrun, the `read_file` node
  task reports `not_checked`, because an earlier node task of the run may
  still create the missing file.
- A file that is not UTF-8 text gives binary content. Binary content can only
  be passed on whole: as a file's `content: "{{ .node_tasks.key.content }}"`,
  an output, a role input, or a file saved on the controller machine. A
  reference to it inside other text, or in a template, fails and says so,
  because text cannot hold it.
- `read_file` tries once unless the node task sets `retry`. To wait for a
  file that a program creates a little later, put a `wait_for` with
  `path_exists` before the `read_file` node task.

## Tested on

**Distributions:** Debian 12 and 13, Ubuntu 24.04 and 26.04, Arch Linux, openSUSE Leap 16.1, Rocky Linux 9, AlmaLinux 10, Fedora 45.

The base suite covers this module (`task_secrets.py`) and runs on every tested distribution.

## Why it works this way

- **The content is limited to 1 MiB.** It travels inside the node job and is
  saved with the playbook run; large files belong in streamed file transfers.
