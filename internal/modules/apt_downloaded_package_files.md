# `apt_downloaded_package_files`

apt keeps every package file it downloads, the `.deb` files, in its
archives directory, `/var/cache/apt/archives/`, after it installed them.
Over months of updates, downloaded package files can fill a small disk.
`apt_downloaded_package_files` deletes the downloaded package files with
`apt-get clean`. Installed packages stay as they are; apt downloads a
package file again when it needs one.

## Examples

### Delete the package files that apt downloaded

```yaml
- apt_downloaded_package_files:
    state: absent
```

On the node, MMX asks apt for the archives directory with `apt-config`.
MMX then lists package files in the archives directory and the archives
subdirectory `partial/`. apt keeps downloads that did not finish in
`partial/`. If MMX finds package files in the archives directory or
`partial/`, MMX runs `apt-get clean`.
MMX reads the archives directory and `partial/` again and reports which
package files are gone:

```text
changed — deleted 142 package files totaling 312.6 MiB (327784448 bytes) from /var/cache/apt/archives/
```

`value.deleted_package_files` holds the number of files and
`value.deleted_bytes` their bytes. A dryrun deletes nothing and reports the
files a run would delete. When neither the archives directory nor `partial/`
contains package files, the node task is `unchanged` and MMX skips
`apt-get clean`.

## How it works

- **What `apt-get clean` deletes.** `apt-get clean` deletes every file in the
  archives directory except apt's `lock`, and every file in `partial/`.
  `apt-get clean` also deletes apt's two caches of the package lists:
  `/var/cache/apt/pkgcache.bin` and `srcpkgcache.bin`.
  The next apt command builds the two package-list caches again in a few
  seconds. The package lists themselves, in `/var/lib/apt/lists/`, stay, so
  no `apt-get update` is needed afterwards.
- **apt's lock.** `apt-get clean` takes the locks of the archives directory
  and of the package lists, and fails at once while another apt holds one,
  such as apt's daily timer or an installation by hand. MMX then tries again
  every two seconds for up to `wait_for_apt_lock_at_most`, 15 minutes unless
  the task sets it, as [`apt_package`](apt_package.md) does. If an apt lock
  stays held, MMX fails the node task without another try and names the
  held lock and the process that holds it in the error.
- **The archives directory** is the one that apt's settings name
  (`Dir::Cache::archives`), `/var/cache/apt/archives/` unless a setting
  moves it.

## Errors and what they mean

| The reason holds | What the error means, and what to do |
|---|---|
| `another apt process still holds apt's lock after 15m0s: Could not get lock /var/cache/apt/archives/lock. It is held by process 2016 (apt-get)` | Another apt process held apt's lock for the whole wait. Run MMX again when that process ends; `ps -p 2016` on the node shows it. A node that needs longer can get a longer `wait_for_apt_lock_at_most`. |
| `apt-get is not installed` | The node does not install its packages with apt. A dryrun reports the node task not checked. |

## Tested on

**Distributions:** Debian 12 and 13, Ubuntu 24.04 and 26.04.

The suite `tests/integration/routine_fixes.py` downloads package files with
`apt-get download`, holds apt's lock for a moment while the node task runs,
and checks the dryrun, the run, and the archives directory on the node, on Debian 12 and 13 and Ubuntu 24.04 and 26.04. The
[validation record](../../docs/changing-mmx/validation.md) has the latest results.

## Why it works this way

- **`state: absent` is written.** The node task reads as what it does: the
  package files are absent afterwards. It is the only state.
- **apt-get does the deleting.** It is apt's own cleanup and knows which
  files are apt's; MMX decides the result from the directory before and
  after, not from what apt prints.
- **It waits for apt's lock** for the reason [`apt_package`](apt_package.md)
  does: apt's daily timer and cloud-init hold it just after a server first
  boots, and `apt-get clean` does not wait by itself.
