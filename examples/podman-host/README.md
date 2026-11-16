# A server that runs containers with Podman

Set the server address in `inventories/example.yml` and the registry that the
server pulls its images from, then run:

```sh
mmx dryrun configure
mmx run configure
mmx run configure
```

On the node, in order:

1. MMX installs Podman and skopeo from the distribution's own packages, and
   on Debian and Ubuntu the programs that Podman's package only recommends.
2. MMX writes a drop-in file for your registry under
   `/etc/containers/registries.conf.d/`. Podman has no daemon to configure
   or start. Podman's settings are files under `/etc/containers/`.
3. MMX installs a systemd timer that deletes, every Sunday at 04:00, the
   images in the system's image store that no container uses and that are
   older than seven days. MMX deletes no image by itself.
4. MMX creates the ordinary account `shop` for containers that run without
   root, and gives that account the same cleanup timer in the account's own
   systemd, because each account has its own image store.

The second run changes nothing.

The example [deploy-with-podman-quadlet](../deploy-with-podman-quadlet/)
deploys a container to a server that this example prepared.

**Why skopeo.** A `podman_image` task with `pull_image: always` follows a tag
that its publisher moves. A dryrun of that task asks the registry which image
the tag names now, without downloading the image. Podman has no command for
that question; skopeo has, and it reads the same registry settings and logins
as Podman. Without skopeo the dryrun reports the task `not_checked` and says
to install skopeo. A run needs only Podman: it pulls.

**Why more packages on Debian and Ubuntu.** There, Podman's package
requires only what a container of root needs. The programs for pods, for the
containers of an ordinary account, and for Podman's own networks are
"recommended" packages. apt installs recommended packages by default, but
many server images and many administrators turn that off
(`APT::Install-Recommends "false"`). On such a server `apt install podman`
succeeds, and later a pod does not start
(`exec: "catatonit": executable file not found`) and an ordinary account
cannot run a container (`exec: "newuidmap": executable file not found`). So
the playbook names each of these packages, and the server is the same with
either apt setting:

| Package | What needs it |
|---|---|
| `catatonit` | Pods. |
| `uidmap` | Containers of an ordinary account. |
| `slirp4netns`, `passt` | The network of an ordinary account's containers: `slirp4netns` with Podman 4 and older, `passt` with Podman 5. |
| `dbus-user-session` | Lets an ordinary account's Podman hand its containers to the account's own systemd. Without `dbus-user-session`, every Podman command of the account warns and falls back to another way. |
| `netavark`, `aardvark-dns` | Podman's networks for containers, and the names of the containers on one network. Without `netavark` and `aardvark-dns`, Podman 4 falls back to older network programs, which Podman 5 no longer has. |
| `nftables` | The firewall program that `netavark` runs on Debian 13. |

On the Red Hat family, Podman's package requires all of these programs
itself, so the playbook names only `podman` and `skopeo` there.

**Why an ordinary account, not a system account.** Podman runs an account's
containers with user IDs from the account's own ranges of sub-user and
sub-group IDs (lines in `/etc/subuid` and `/etc/subgid`). The system's account
tools add those lines when they create an ordinary account, and add none for a
system account.

**On a node with SELinux and `selinux_file_context` tasks,** as in the
deployment example, also install the package `policycoreutils-python-utils`
on the Red Hat family; it has the `semanage` program that changes the
node's SELinux label rules.

## Distributions

The example works on Debian 12 and 13, Ubuntu 24.04 and 26.04, Rocky Linux 9,
and AlmaLinux 10.

Debian 12 ships a Podman older than 4.4, which has no Quadlet files; the
Podman host example works on Debian 12, and the deployment example does not.

For every field, see the
[`podman_quadlet`](../../docs/reference/modules/podman_quadlet.md) and
[`podman_image`](../../docs/reference/modules/podman_image.md) pages.
