# minibox

minibox runs untrusted jobs safely on Linux and spreads them across several machines.

## The problem

CI runners and build servers run code nobody on the team wrote: pull requests,
third-party build scripts, test suites that misbehave. One bad job can use all the
memory, fork-bomb the host, read another job's files, hang forever, or call out to the
internet.

Docker can isolate jobs, but a Docker daemon on every runner is heavy, runs as root, and
has a large attack surface. minibox is a small set of tools that give each job its own
box with real limits, and a coordinator that queues jobs and hands them to the right host.

## Features

**Isolation**

- Each job gets its own PID, mount, UTS, IPC, and network namespace
- Root inside a job is an unprivileged host user (uid 100000) with no capabilities
- A seccomp allow-list: a job can only make the system calls ordinary programs need, so
  `mount`, `ptrace`, `bpf`, and the rest fail
- `no_new_privs` is set, so setuid binaries can't gain power
- If the runner dies, its jobs die with it

**Filesystem**

- Run against a plain directory (`--rootfs`) or a named image (`--image`)
- Writes go to a copy-on-write layer that is deleted after the job, so the image stays clean
- Import an image from a tarball, pull one by URL with a SHA-256 check, or pull a
  registry image (`alpine:3.20`, `ghcr.io/org/app:v1`) from Docker Hub or any registry
  that allows anonymous pulls. Layer digests are checked and whiteouts are applied
- Host folders mounted in with `-v`. A folder owned by someone else is shown to the job
  as writable through an idmapped mount, without changing its owner on the host

**Limits**

- Memory (`--mem`), CPU share (`--cpus`), and process count (`--pids`) through cgroups v2
- Timeouts (`--timeout`). A timed-out job is recorded as `timed_out`

**Networking**

- `isolated` (default): loopback only
- `bridge`: an address on `minibox0` (`10.88.0.0/24`) with NAT to the internet
- `host`: shares the host's network, for trusted jobs only

**Jobs**

- Clean environment, with variables set explicitly (`-e`), and a working directory (`-w`)
- Output saved to a log file, plus a run record with status, exit code, and IP

**Cluster**

- Coordinator (`mbd`) serves HTTPS only. Each user has a token, and sees only their own jobs
- Each agent (`mbagent`) has its own token and can only act under its own name
- Agents on each host ask for work, run it, and report back
- A job held by an agent that stops polling is requeued for another agent
- Two agents can never be given the same job
- State lives in etcd, so the coordinator can restart without losing jobs, and an etcd
  cluster keeps working when one member fails. Losing quorum returns `503`
- `/metrics` in Prometheus format: job counts, online agents, slot use

**Tools**

- `mbctl` lists runs, shows live usage, prints logs, and cleans up old records

## Tech stack

| Area                   | Technology                                                         |
| ---------------------- | ------------------------------------------------------------------ |
| Runner                 | Rust, `nix`, `libc`, `seccompiler`, `serde`, `sha2`                |
| Coordinator and agents | Go 1.26, standard library, etcd client v3, TLS                     |
| State                  | etcd 3.4 or newer (leases, transactions)                           |
| Images                 | busybox and registry images, `tar`, `chown`, overlayfs, Docker Registry v2 API |
| Volumes                | Idmapped mounts (`mount` from util-linux 2.39+)                    |
| Networking             | Linux bridges and veth pairs, `iproute2`, `iptables` (NAT)         |
| Kernel features        | Namespaces, cgroups v2, seccomp BPF, capabilities, user namespaces |
| Scripts and tests      | Bash, `curl`, `openssl`, `etcdctl`, `ip netns`                     |
| Development            | WSL2 (Ubuntu) on Windows, cross-compiled Go binaries               |

## Parts

| Binary    | Language | Role                                                         |
| --------- | -------- | ------------------------------------------------------------ |
| `minibox` | Rust     | Runs one job in an isolated box                              |
| `mbd`     | Go       | Coordinator: queue, scheduling, requeue, metrics             |
| `mbagent` | Go       | Runs on each host. Asks for jobs, runs them, reports results |
| `mbctl`   | Go       | Local view of runs, logs, and cleanup                        |

An agent is the program on each worker machine. It is not an AI agent.

## Quick start

```bash
cargo build --release
(cd mbctl && go build -o ../bin/mbctl . \
  && go build -o ../bin/mbd ./cmd/mbd \
  && go build -o ../bin/mbagent ./cmd/mbagent)

# one job on one machine, from Docker Hub
sudo ./target/release/minibox image pull-registry alpine alpine:3.20
sudo ./target/release/minibox run --image alpine --mem 512 --timeout 600 -- sh -c 'cat /etc/alpine-release'
./bin/mbctl ps
```

Cluster (see `docs/multi-machine.md` for a full setup):

```bash
# coordinator, with etcd running and a TLS certificate
./bin/mbd --addr :8787 --tls-cert mbd.crt --tls-key mbd.key \
    --client-tokens clients.txt --agent-tokens agents.txt \
    --etcd http://etcd-1:2379,http://etcd-2:2379

# each worker host, as root, with its own token
sudo ./bin/mbagent --server https://coordinator:8787 --ca mbd.crt \
    --token "$HOST_A_TOKEN" --name host-a --slots 4 --minibox /usr/local/bin/minibox

# submit a job with your user token
curl --cacert mbd.crt -X POST https://coordinator:8787/v1/jobs \
  -H "Authorization: Bearer $USER_TOKEN" -H 'Content-Type: application/json' \
  -d '{"image":"ci","cmd":["sh","-c","make test"],"mem_mb":512,"timeout_secs":600}'
```

## Commands

```
minibox run [--image NAME | --rootfs DIR] [--net isolated|bridge|host]
            [--mem MB] [--cpus N] [--pids N] [--timeout SECS]
            [-v HOST:GUEST[:ro]] [-e KEY=VALUE] [-w DIR] -- CMD...
minibox image import NAME TARBALL
minibox image pull NAME URL [--sha256 HEX]
minibox image pull-registry NAME REF
minibox image ls

mbctl ps | prune | logs ID
mbd --addr ADDR --tls-cert FILE --tls-key FILE --client-tokens FILE --agent-tokens FILE \
    --etcd URLS [--etcd-ca FILE] [--prefix /mbd]
mbd token-hash                  # reads a token on stdin, prints the hash for a token file
kill -HUP <mbd pid>             # reload the token files without a restart
mbagent --server URL --ca FILE --token AGENT_TOKEN --name NAME [--slots N] [--minibox PATH]
```

API: `POST /v1/jobs`, `GET /v1/jobs`, `GET /v1/jobs/{id}`, `POST /v1/agents/poll`,
`POST /v1/jobs/{id}/report`, `GET /metrics` (no token).

## Requirements

- Linux (or WSL2), root, cgroups v2, user namespaces enabled
- Kernel 5.12+ and util-linux 2.39+ for volumes owned by other users
- `iproute2`, `iptables` for bridge networking
- `tar`, `chown`, `curl` for image commands
- `openssl` to make the coordinator's certificate
- etcd 3.4 or newer for the coordinator

## Testing

| Suite | What it checks |
| --- | --- |
| `cargo test --release` | Argument parsing, registry reference parsing, address allocation |
| `go test ./...` (with `ETCD_ENDPOINTS` set) | Store against a real etcd: atomic claims, requeue after a lease expires, owner filtering, restart, metrics |
| `tests/smoke.sh` | Isolation, limits, rootfs, networking, the allow-list, overlay images, volume ownership, logs, timeouts |
| `tests/registry.sh` | Pulls `alpine:3.20` from Docker Hub, runs it, checks the digest path |
| `tests/cluster.sh` | Coordinator and agent over HTTPS: auth, per-user isolation, TLS refusal, real jobs |
| `tests/multihost.sh` | A second host in its own network namespace, failover after a host dies |
| `tests/etcd-cluster.sh` | Three TLS etcd members: leader loss, quorum loss (503), recovery, durability |
| `tests/private-registry.sh` | Pulls from a local HTTPS registry that needs a login, refuses bad credentials, and rejects a corrupted layer |
| `tests/bench.sh` | Startup overhead per job |
| `tests/load.sh` | Simulated agents driving one coordinator: throughput, latency, and coordinator CPU at 10, 50, and 100 agents |

All scripts run as root in Linux or WSL2. The multi-host test simulates a second machine.
The etcd test runs all members on one computer.

