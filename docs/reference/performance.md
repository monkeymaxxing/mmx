# Performance

This record holds current measured results. The
[performance validation plan](../changing-mmx/design/development.md#performance-validation) owns
the method and the remaining stages; rerun the affected measurements after a
change that touches the controller's per-node path and replace these results.

## Controller against simulated nodes

`tests/performance/controller_scale.py` runs the real `mmx run` against
`tests/performance/simnodes`, which serves one node HTTPS API per simulated node
with its own machine identity and a saved HTTPS identity, so the controller
takes its ordinary HTTPS path without SSH setup. The simulator answers at once
apart from each job's duration, so the numbers describe controller cost, not
node or network cost. Each node reports a real Debian 13 facts snapshot.

| Scenario | Job per node |
|---|---|
| short | 1 s, 10 results of 64 bytes |
| long | 20 s, 100 results of 2 KiB stdout, one result every 0.2 s |

Measured on a development build shortly before MMX 0.1.0, on an Apple M1 Max (10 cores, 32 GiB), macOS 26.6.2, Go 1.26.8,
`max_nodes_at_once: 100` unless noted. Transfer is bytes the nodes sent to the
controller.

| Scenario | Nodes | Wall | Controller CPU | Peak memory | Transfer | Saved history |
|---|---:|---:|---:|---:|---:|---:|
| short | 100 | 2.8 s | 2.0 s | 80 MiB | 1.8 MB | 1.3 MB |
| short | 1,000 | 20.3 s | 31.3 s | 246 MiB | 18.1 MB | 13.1 MB |
| short, concurrency 1,000 | 1,000 | 18.0 s | 41.6 s | 437 MiB | 18.1 MB | 13.1 MB |
| long | 100 | 22.8 s | 8.9 s | 403 MiB | 45.4 MB | 23.0 MB |
| long | 1,000 | 214.1 s | 112.6 s | 1,893 MiB | 454 MB | 230 MB |
| long, concurrency 1,000 | 1,000 | 42.1 s | 127.3 s | 2,941 MiB | 440 MB | 230 MB |

The controller's per-node cost follows from three current design choices:

- A runner file is read and hashed once per process, not once per node
  connection. Administrator keys load once per process too: asking a forwarded
  SSH agent for its keys once per node cost about 0.6 s per node with 100 nodes
  connecting at once.
- Only job IDs and finished responses are fsynced. Concurrent fsyncs are
  serialized by the filesystem, so fsyncing running progress made each
  1-second job take about 2.5 s here; progress is replaced without fsync, at
  most once per second per node, which halves the long scenario's CPU.
- Progress requests carry what the controller holds, and the node answers when
  a task starts or finishes, sending only newer results. The node spaces
  running answers 200 ms after their request and answers a finished job at
  once; spacing on the controller instead delayed every job that outlived its
  first request by up to 200 ms. Fixed-interval polling
  that repeats every retained result cost about 20 times the transfer in the
  long scenario.

## One node's run

Timing probes in a scratch copy of that build (never released) followed unchanged runs of
the fleet workload (below) from the macOS controller to the local Lima guest
(Debian 13 ARM64, 2 vCPU), idle and with six busy loops loading the guest's two
CPUs. Times are one node's path through the controller, averaged over repeated
runs; guest CPU is the whole guest's busy time per run.

One node's unchanged run takes 93 ms idle and 203 ms with the guest loaded,
and uses 43 ms of guest CPU. The parts, idle and loaded:

| Part | Idle | Loaded |
|---|---:|---:|
| The job (facts under 1 ms, eight tasks) | 14 ms | 58 ms |
| The one authorization, when the connection opens | 8 ms | 54 ms |
| Controller durable saves (three per node, on macOS) | 32 ms | 31 ms |
| Resolving the node's SSH settings with `ssh -G` | 8 ms | 7 ms |

JSON is not a cost: the 5 KB job encodes in 0.1 ms and the largest answer, 8 KB
and mostly facts, decodes in 0.5 ms, under 1% of the run together. The run
itself adds a record and two status saves, about 30 ms of fsync on macOS.

## Real nodes against Ansible

The driver for this comparison's private fleet is not part of this repository.
The workload files remain in `tests/performance/fleet/`; reproducing the
comparison needs an independently provisioned fleet and its own driver.
Both tools run on the same controller machine and apply the same
workload (`tests/performance/fleet`): a group, a system user, two directories, a
template, a configuration file whose change restarts the guest's service, and an
HTTP check of that service. Each run checks the reported changes and reads the
applied version back from a sample of guests.

Fleet: 1,000 Debian 13 AMD64 Incus containers (1 CPU each), 500 on each of two
Hetzner `ccx43` machines (16 vCPU, 61 GiB), reached through NetBird from a third
`ccx43`, the controller. MMX, the same build, with `max_nodes_at_once: 100` unless
noted and `runner_url` pointing at the controller's nginx origin, which each
Incus machine caches for its guests. Tuned Ansible is ansible-core 2.19.11 with
an inventory configured for the benchmark: 128 forks, pipelining, persistent SSH connections, no
fact gathering; its rows are from the same fleet shape and workload a day
earlier. Stock Ansible uses 5 forks, fact gathering, and no SSH tuning.
Controller CPU includes every child process. Network is all controller traffic.
Guest CPU is the busy time of both Incus machines, which carry all guest-side work.
The 100-node runs use the first 100 guests of the same fleet.

| Run | Nodes | Wall | Controller CPU | Network | Guest CPU |
|---|---:|---:|---:|---:|---:|
| MMX first contact | 100 | 5.6 s | 7.7 s | 9 MB | 93 s |
| MMX first contact | 1,000 | 83.9 s | 105.3 s | 216 MB | 1,088 s |
| MMX first apply | 100 | 2.0 s | 3.1 s | 10 MB | 20 s |
| MMX first apply | 1,000 | 32.9 s | 41.0 s | 232 MB | 380 s |
| MMX unchanged | 100 | 0.9 s | 2.5 s | 6 MB | 8 s |
| MMX unchanged | 1,000 | 14.4 s | 34.1 s | 199 MB | 176 s |
| MMX unchanged, `max_nodes_at_once: 500` | 1,000 | 11.3 s | 34.2 s | 208 MB | 141 s |
| MMX one change | 100 | 2.0 s | 3.0 s | 9 MB | 15 s |
| MMX one change | 1,000 | 22.0 s | 37.7 s | 207 MB | 220 s |
| Tuned Ansible first apply | 100 | 18.8 s | 53.9 s | 330 MB | 308 s |
| Tuned Ansible first apply | 1,000 | 132.1 s | 628.6 s | 3,401 MB | 3,228 s |
| Tuned Ansible unchanged | 100 | 15.7 s | 44.3 s | 273 MB | 254 s |
| Tuned Ansible unchanged | 1,000 | 108.0 s | 517.2 s | 2,799 MB | 2,686 s |
| Tuned Ansible one change | 100 | 18.5 s | 51.6 s | 330 MB | 299 s |
| Tuned Ansible one change | 1,000 | 128.9 s | 602.7 s | 3,394 MB | 3,175 s |
| Stock Ansible unchanged | 100 | 96.0 s | 136.0 s | 343 MB | 293 s |

After first contact, MMX applies the workload 9 to 17 times faster than tuned
Ansible at 100 nodes and 4 to 10 times faster at 1,000, with about 15 times less
controller CPU, guest CPU, and network traffic. First contact plus first apply
at 1,000 nodes, 117 s, is faster than Ansible's first apply alone. MMX's
controller peaked at 240 MiB at `max_nodes_at_once: 100` and 670 MiB at 1,000.
Raising `max_nodes_at_once` above 500 did not help: the unchanged run took 12.7
s at 250 and 11.3 s at both 500 and 1,000, about 90 node runs a second for the
two fleet machines. Each fleet machine runs 500 containers on one kernel, and
the containers queue there; separate servers do not share that queue.

First contact depends on where the runner comes from. Without `runner_url`,
1,000 concurrent 15 MB uploads (the runner before it was stripped and sent
compressed; now about 4 MB) from the controller through NetBird fell to about
3 MB/s in aggregate, against 45 MB/s for one stream, and first contact did not
finish in 40 minutes. With the mirror, the controller's traffic is about 230 KB
per node.

Each idle simulated node HTTPS API holds about 10 MiB of memory, 5 GiB for an Incus machine's 500 guests.
With those simulated APIs running, the Incus machines had about 7 GiB available, and a
128-fork Ansible run stalled on memory there until some guests' SSH banners
timed out; the Ansible runs above were measured with MMX removed from the guests.

## Known remaining costs

- Peak memory in the long scenario has not been profiled. Likely causes are
  holding every node's retained results and writing the whole run as one
  `--json` document at the end.
- At concurrency 1,000, the three durable saves per node still queue behind
  each other: the median 1-second job took 4.0 s.
- At 1,000 nodes the controller spends about 34 ms of CPU and exchanges about
  200 KB of network traffic per node, against 25 ms and 65 KB at 100 nodes.
  Where the extra goes has not been measured.
- First contact is the slowest run: about 8.4 s of a concurrency slot per node
  at 1,000 nodes against 5.6 s at 100. Setup's individual actions on each node have not been
  timed separately.
