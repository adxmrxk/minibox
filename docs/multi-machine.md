# Running minibox across real machines

The test scripts simulate a second host with a network namespace. This guide covers
running the same setup on separate computers. It has not been run on real hardware yet,
so treat each step as something to check, not a guarantee.

## What you need

- Three Linux machines for etcd (or one etcd node if you accept a single point of failure)
- One coordinator machine (can be one of the etcd machines)
- One or more worker machines that run jobs
- All machines on the same network, able to reach each other on the ports below
- Root on each worker, cgroups v2, user namespaces enabled, util-linux 2.39+ for volumes owned by other users

Ports: etcd 2379 (client) and 2380 (peer), coordinator 8787 (HTTPS), workers need outbound 8787 and 443.

## 1. etcd cluster

On each etcd machine, start one member. Replace the names and addresses with your own.

```bash
etcd --name e1 --data-dir /var/lib/etcd \
  --listen-client-urls http://10.0.0.11:2379 --advertise-client-urls http://10.0.0.11:2379 \
  --listen-peer-urls http://10.0.0.11:2380 --initial-advertise-peer-urls http://10.0.0.11:2380 \
  --initial-cluster e1=http://10.0.0.11:2380,e2=http://10.0.0.12:2380,e3=http://10.0.0.13:2380 \
  --initial-cluster-token minibox --initial-cluster-state new
```

Check it:

```bash
ETCDCTL_API=3 etcdctl --endpoints=http://10.0.0.11:2379,http://10.0.0.12:2379,http://10.0.0.13:2379 endpoint health
ETCDCTL_API=3 etcdctl --endpoints=http://10.0.0.11:2379 member list
```

## 2. Certificates

The coordinator serves HTTPS. Make a certificate whose names match the address workers will use.

```bash
openssl req -x509 -newkey rsa:2048 -nodes -days 365 \
  -keyout mbd.key -out mbd.crt -subj "/CN=mbd" \
  -addext "subjectAltName=IP:10.0.0.20,DNS:mbd.internal"
```

Copy `mbd.crt` to every worker. It is the CA file the agents trust.

## 3. Tokens

Generate each token with `openssl rand -hex 24`. Give it to its user or worker, and store only
its hash in the token file:

```bash
TOKEN=$(openssl rand -hex 24)
echo "$TOKEN" | mbd token-hash        # prints the line to put in the file
```

`clients.txt` holds one `user hash` line per person who submits jobs.
`agents.txt` holds one `name hash` line per worker.
To rotate, add a second line for the same name, send `kill -HUP` to `mbd`, switch the user
to the new token, remove the old line, and send `kill -HUP` again.

## 4. Coordinator

```bash
mbd --addr :8787 --tls-cert mbd.crt --tls-key mbd.key \
    --client-tokens clients.txt --agent-tokens agents.txt \
    --etcd http://10.0.0.11:2379,http://10.0.0.12:2379,http://10.0.0.13:2379
```

Check it answers over TLS from another machine:

```bash
curl --cacert mbd.crt https://10.0.0.20:8787/metrics
```

## 5. Workers

On each worker, as root, with `minibox` and the image store set up:

```bash
mbagent --server https://10.0.0.20:8787 --ca mbd.crt \
        --token <that worker's token> --name worker-1 --slots 4 \
        --minibox /usr/local/bin/minibox
```

## 6. Checks

Run these to confirm each part works across the network:

- Submit a job from a client machine and watch it finish: `GET /v1/jobs/{id}` shows `succeeded`.
- Stop one etcd member. Jobs should still be accepted and run.
- Stop one worker mid-job. After about 45 seconds its job should run on another worker.
- Stop two etcd members. Submits should return `503` until one comes back.
- Try a worker token against a client endpoint. It should get `401`.

## Known gaps for real machines

- Clock skew between machines affects lease timing. Keep the clocks in sync with NTP.
- Firewalls must allow the ports above.
- Nothing here has been timed on real hardware yet. Measure before sizing a fleet.

The etcd example above uses plain HTTP. For TLS, give every member the same CA and
certificate, and use `https://` in the endpoints:

```bash
etcd ... --cert-file etcd.crt --key-file etcd.key \
         --peer-cert-file etcd.crt --peer-key-file etcd.key --peer-trusted-ca-file ca.crt
```

Do not add `--trusted-ca-file` on the client port unless you want every client to present a
certificate. Then point `mbd` at the CA with `--etcd-ca ca.crt`.
