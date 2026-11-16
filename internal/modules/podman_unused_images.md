# `podman_unused_images`

`podman_unused_images` removes the images of a Podman image store that no
container uses, running or stopped, with `podman image prune`. It frees
disk space on a node whose old images fill it. `remove` says which images
go: only those without a name (`dangling_images_only`), or every image that
no container uses (`every_image_no_container_uses`).

An image is dangling when it has no name left: a pull of a tag that moved to
a newer image takes the tag from the old image, and a build that reuses a
tag does the same. [`podman_image`](podman_image.md) pulls images and
removes none.

## Set up

Podman must be installed, as for [`podman_image`](podman_image.md#set-up).

## Examples

### Remove the images that lost their names

```yaml
- podman_unused_images:
    remove: dangling_images_only
```

On the node, MMX lists the images of the system's (root's) store that
`podman image prune` removes: those without a name that no container uses
(`podman images --filter dangling=true --filter containers=false`). Images
consist of layers, stored sets of files that images can share. When any
images qualify, MMX runs `podman image prune --force`, then lists the store
again. MMX reports the images now absent and the disk space freed by
removing image layers:

```text
changed — removed 2 images from the system's image store and freed 41.2 MiB (43200512 bytes): sha256:0e5c...; sha256:9a1f...
```

`value.removed_images` lists each image with its `image_id`, its `names`,
and its `size_bytes` as `podman images` reports it, and `value.freed_bytes`
holds the bytes freed. A dryrun removes nothing and reports the same for the
images a run would remove. When no image qualifies, the node task is
`unchanged`.

### Every image that no container uses, in an account's store

```yaml
- podman_unused_images:
    remove: every_image_no_container_uses
    in_image_store_of_user_account: shop
```

This removes, from the image store of the account `shop`, every image that
no container of the account uses (`podman image prune --all`), also images
with a name, such as `registry.example.com/shop/web:2.3` after the
containers moved to `2.4`.

**A deploy that keeps the previous image for a quick rollback loses it.**
`every_image_no_container_uses` removes the previous version as soon as no
container uses it; a rollback must then pull it again. Use
`dangling_images_only` on such a node, or remove the images only after the
new version has proved itself.

## How it works

- **Which images.** Those that `podman image prune` removes: images that no
  container uses, whether it runs or not, and that are not in a read-only
  store; with `dangling_images_only` only those without a name. A dryrun
  lists them with `podman images` and the same filters that prune applies.
- **What a run reports** comes from the store before and after the prune,
  not from what prune prints. A build on the node with cached steps leaves
  unnamed parent images behind. A parent image supplies the base for
  another image. Podman's prune removes an unnamed parent image along with
  the image built from the parent. The run reports the removed parent
  image, while the dryrun lists only the image built from the parent.
- **The bytes freed.** Images share layers: an old version of an application
  and the new one often share the base system's layers. So `freed_bytes`
  counts only the layers that no image which stays uses: MMX reads the
  layers of every image from `podman image inspect`, measures the layers of
  the removed images that no other image uses on disk, and after the prune
  counts those that are gone. A dryrun predicts the number of bytes the run
  frees. With a storage driver other than overlay, Podman names no layer
  directories, and `freed_bytes` is `null`.
- **An account's store.** `in_image_store_of_user_account` works as for
  `podman_image`: Podman runs as the account, with the account's runtime
  directory, and MMX turns lingering on for the account when it is off
  ([an account's image store](podman_image.md#an-accounts-image-store-and-lingering)).

## Errors and what they mean

| The reason holds | What it means, and what to do |
|---|---|
| `podman_unused_images.remove is required` | Validation requires `remove`; there is no default. |
| `the program podman is not installed` | Install Podman in an earlier node task. A dryrun reports the node task as `not_checked`. |
| `podman image prune --force: ...` | Podman failed. The node task result's `reason` includes Podman's standard error and the removal summary, including any images removed before Podman failed. |

## Tested on

**Distributions:** Debian 12 and 13, Ubuntu 24.04 and 26.04, Rocky Linux 9, AlmaLinux 10.

The suite `tests/integration/podman_containers.py` builds images with
shared layers in the system's store and in an account's, leaves one image
in use by a stopped container, and compares the dryrun, the run, and
`podman images` on the node with both choices, on Debian 12 and 13, Ubuntu 24.04 and 26.04, Rocky Linux 9, and AlmaLinux 10. The
[validation record](../../docs/changing-mmx/validation.md) has the latest results.

## Why it works this way

- **`remove` has no default.** Whether named images go decides whether a
  rollback image survives, so the node task says it.
- **Podman decides which images go.** MMX lists qualifying images with
  Podman's own filters and removes qualifying images with Podman's own
  prune. Podman's listing filters and prune agree about keeping an image
  that a stopped container uses.
- **`freed_bytes` counts layers, not images.** The sum of the removed
  images' sizes would count every shared layer, which stays on disk.
