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

**Filesystem**

- Run against a plain directory (`--rootfs`) or a named image (`--image`)
- Writes go to a copy-on-write layer that is deleted after the job, so the image stays clean
- Import an image from a tarball, or pull one by URL with a SHA-256 check

**Limits**

- Memory (`--mem`), CPU share (`--cpus`), and process count (`--pids`) through cgroups v2
- Timeouts (`--timeout`). A timed-out job is recorded as `timed_out`

**Networking**

- `isolated` (default): loopback only
- `bridge`: an address on `minibox0` (`10.88.0.0/24`) with NAT to the internet
- `host`: shares the host's network, for trusted jobs only

**Jobs**

- Host folders mounted in, read-only with `:ro` (`-v`)
- Clean environment, with variables set explicitly (`-e`), and a working directory (`-w`)
- Output saved to a log file, plus a run record with status, exit code, and IP

**Cluster**

- Coordinator (`mbd`) takes jobs over HTTP. Clients use one token. Each agent has its own
  token, and can only act under its own name
- Agents (`mbagent`) on each host ask for work, run it, and report back
- A job held by an agent that stops polling is requeued for another agent
- Two agents can never be given the same job
- State lives in etcd, so the coordinator can restart without losing jobs, and an etcd
  cluster keeps working when one member fails
- `/metrics` in Prometheus format: job counts, online agents, slot use

**Tools**

- `mbctl` lists runs, shows live usage, prints logs, and cleans up old records

## Tech stack

| Area                   | Technology                                                         |
| ---------------------- | ------------------------------------------------------------------ |
| Runner                 | Rust, `nix`, `libc`, `seccompiler`, `serde`, `sha2`                |
| Coordinator and agents | Go 1.26, standard library, etcd client v3                          |
| State                  | etcd 3.4 or newer (leases, watches, transactions)                  |
| Images                 | busybox rootfs, `tar`, `chown`, overlayfs                          |
| Networking             | Linux bridges and veth pairs, `iproute2`, `iptables` (NAT)         |
| Kernel features        | Namespaces, cgroups v2, seccomp BPF, capabilities, user namespaces |
| Scripts and tests      | Bash, `curl`, `etcdctl`, `ip netns`                                |
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

# one job on one machine
sudo ./target/release/minibox image import ci ./ci.tar
sudo ./target/release/minibox run --image ci --mem 512 --timeout 600 -- sh build.sh
./bin/mbctl ps
```

Cluster:

```bash
# coordinator, with etcd running. agents.txt has one "name token" line per host.
./bin/mbd --addr :8787 --token "$CLIENT_TOKEN" --agent-tokens agents.txt \
    --etcd http://etcd-1:2379,http://etcd-2:2379

# each worker host, as root, with its own token
sudo ./bin/mbagent --server http://coordinator:8787 --token "$HOST_A_TOKEN" \
    --name host-a --slots 4 --minibox /usr/local/bin/minibox

# submit a job
curl -X POST http://coordinator:8787/v1/jobs -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"image":"ci","cmd":["sh","-c","make test"],"mem_mb":512,"timeout_secs":600}'
# (use the client token in the Authorization header)
```

## Commands

```
minibox run [--image NAME | --rootfs DIR] [--net isolated|bridge|host]
            [--mem MB] [--cpus N] [--pids N] [--timeout SECS]
            [-v HOST:GUEST[:ro]] [-e KEY=VALUE] [-w DIR] -- CMD...
minibox image import NAME TARBALL | pull NAME URL [--sha256 HEX] | ls

mbctl ps | prune | logs ID
mbd --addr ADDR --token CLIENT_TOKEN --agent-tokens FILE --etcd URLS [--prefix /mbd]
mbagent --server URL --token AGENT_TOKEN --name NAME [--slots N] [--minibox PATH]
```

API: `POST /v1/jobs`, `GET /v1/jobs`, `GET /v1/jobs/{id}`, `POST /v1/agents/poll`,
`POST /v1/jobs/{id}/report`, `GET /metrics`.

## Requirements

- Linux (or WSL2), root, cgroups v2, user namespaces enabled
- `iproute2`, `iptables`, `tar`, `curl`, `busybox-static` for tests
- etcd 3.4 or newer for the coordinator

## Limits

- Images come from tarballs or URLs. There is no registry protocol yet, so you can't pull
  from Docker Hub or a private registry.
- A job that writes to a host folder needs that folder writable by uid 100000.
- The seccomp allow-list uses x86_64 system call names. It is built and tested on x86_64
  only.
- Each job write is an etcd round trip, so throughput is tens of job cycles per second.
- Clients share one token. Agents have their own, but there are no per-user accounts.
- Tested on one computer, with simulated hosts and a Windows and WSL pair. Not yet on
  separate physical machines.
