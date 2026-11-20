# Audit and measured improvements

Each finding below was reproduced before it was fixed. The before and after numbers come
from the test or benchmark named in each row, run on the same machine. Everything was run
on one computer (Intel i5-12400, WSL2 Ubuntu, etcd 3.4 on one node).

## Security findings

| # | Finding | Before | After | How it was measured |
| --- | --- | --- | --- | --- |
| S1 | Image names were joined onto the image store with no check, so `../../` let a job use any host directory as its root. A job read `/etc/secret.txt` from a directory outside the store. | Job ran and read the host file | Refused: "bad image name" | `audit-before.sh` attack script; names are also checked by the coordinator on submit |
| S2 | A layer could create a symlink to a host directory, and a later whiteout file deleted files through it. | Host file deleted (test fails) | Whiteouts refuse any path that passes through a symlink (test passes) | `whiteouts_never_touch_files_outside_the_image` unit test |
| S3 | Job logs and run records were created with mode 644, so any local user could read job output, which can contain secrets. | `644` | `600` | `stat` on the files written by a job |
| S4 | Token files stored the tokens themselves. | Raw tokens on disk | SHA-256 hashes only; raw token files are refused at startup | cluster test "a file with raw tokens is refused" |

Two suspected issues were tested and did not reproduce. GNU tar refused to write through a
symlink on tarball import, so that path was blocked before any change. The registry path is
covered by S2.

## Correctness and reliability findings

| # | Finding | Before | After | How it was measured |
| --- | --- | --- | --- | --- |
| R1 | A finished job whose result report hit a transient coordinator error was never recorded. It stayed "assigned" forever. | 33 of 100 finished jobs never recorded | 0 of 100 | `TestReportsSurviveTransientCoordinatorErrors`: the coordinator rejects the first report for every third job |
| R2 | A job log over etcd's value limit made the whole result write fail, so the job was stuck. | 2 MB log refused (`ResourceExhausted`) | Stored as the newest 64 KB | `TestOversizedLogIsCappedAndRecorded` |
| R3 | `GET /v1/jobs` for one user only checked the newest 200 jobs across all users, so an older job of that user could disappear from their list. | 0 of 1 old job visible | 1 of 1 | `TestListFindsAnOldJobEvenAfterManyNewerJobsByOthers` |

## Performance findings

| # | Finding | Before | After | How it was measured |
| --- | --- | --- | --- | --- |
| P1 | Every agent poll checked each assigned job's agent with its own etcd round trip, so a poll cost one round trip per assigned job. | 129.6 ms per poll with 500 assigned jobs | 3.33 ms per poll (about 39x faster) | `BenchmarkPollWithAssigned` |
| P2 | Idle agents rewrote their record to etcd on every poll, even when nothing had changed. | Write on every poll | Write only when slots or running counts change | Code change; the poll benchmark above includes it |

## Control-plane load test

`tests/load.sh` runs simulated agents (`mbload`) against one coordinator on the same machine
as etcd. Each run submits 2000 jobs; the agents finish each job instantly, so the numbers
measure the coordinator and etcd, not sandbox start-up. Latency is submit to finish.
Every run finished all 2000 jobs with 0 errors.

| Agents | Before (block IDs and long poll) | After (poll throttles) | Notes |
| --- | --- | --- | --- |
| 10 | 76.8 jobs/s, p50 13.2 s | 85.4 jobs/s, p50 12.4 s | one run; not repeated |
| 50 | 71.4 jobs/s, p50 13.2 s | 54.1 to 66.0 jobs/s, p50 about 18 s | 4 runs; lower than before |
| 100 | 58.9 jobs/s, p50 16.6 s | 48.8 to 50.6 jobs/s, p50 about 19 s | 4 runs; lower than before |

Before the block-ID fix, the same runs had about 45 errors each (submit contention on the
shared counter). Block IDs removed those errors. Coordinator CPU per run was 14 s at 10
agents and 45 to 65 s at 100 agents after the change, and peak memory was 29 to 46 MB.

A CPU profile at 100 agents (`tests/load.sh` with `PPROF_OUT`) showed the cost is in the
poll path. Every new job wakes every waiting agent, and each woken poll makes several etcd
reads. The two throttles cut some of that work, which helped at 10 agents. At 50 and 100
agents the herd is still the main cost, so throughput falls as agents are added. The
batched-submit change was tried and removed because it did not help.


## Test suite after the fixes

All suites were rerun on the final code (WSL2 Ubuntu, Go and Rust release builds).

| Suite | Result |
| --- | --- |
| Rust unit tests | 12 of 12 |
| Go tests against etcd | all packages pass |
| Smoke (isolation, limits, overlay, volumes, hardening) | 50 of 50 |
| Registry (Docker Hub alpine) | 5 of 5 |
| Private registry (login, digest check) | 6 of 6 |
| Cluster (HTTPS, auth, users, rotation) | 17 of 17 |
| Multi-host (simulated second host, failover) | 6 of 6 |
| Three-node TLS etcd failover | 10 of 10 |
