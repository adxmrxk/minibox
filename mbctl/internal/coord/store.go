// Package coord keeps the job queue and agent registry for the coordinator.
//
// State lives in etcd. Layout under the store's prefix (default /mbd):
//
//	seq                 counter that gives every job a number
//	jobs/<seq>          the job (JSON), in any state
//	queue/<seq>         present while a job is queued
//	assigned/<seq>      present while a job is assigned; the value is the agent's name
//	agents/<name>       present while the agent is alive; attached to a lease that
//	                    expires if the agent stops polling
//
// Claims and reports are transactions with compare-and-swap checks, so two agents can
// never be handed the same job, and a job held by a dead agent is requeued when its
// agent's lease expires.
package coord

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"mbctl/internal/jobs"

	clientv3 "go.etcd.io/etcd/client/v3"
)

// DefaultLeaseTTL is how long an agent can go without polling before it counts as gone.
const DefaultLeaseTTL = 45 * time.Second

// ListLimit caps how many jobs a single list request returns.
const ListLimit = 200

const opTimeout = 5 * time.Second

var (
	// ErrInvalid marks a request the caller got wrong, such as a job without an image.
	ErrInvalid = errors.New("invalid request")
	// ErrNotAssigned means a report came from an agent that does not hold the job.
	ErrNotAssigned = errors.New("job is not assigned to this agent")

	errSkip = errors.New("skip")
)

// Store is the coordinator's state.
type Store struct {
	cli    *clientv3.Client
	prefix string

	// Now is the clock used for timestamps. LeaseTTL is how long an agent can go silent.
	Now      func() time.Time
	LeaseTTL time.Duration

	mu     sync.Mutex
	leases map[string]clientv3.LeaseID
	// written remembers the slot and running counts last stored for each agent, so an idle
	// agent's poll only renews its lease and does not write to etcd.
	written map[string][2]int
}

type agentInfo struct {
	Slots    int   `json:"slots"`
	Running  int   `json:"running"`
	LastSeen int64 `json:"last_seen"`
}

// Open connects to etcd without TLS. prefix namespaces the keys, so several stores can share one cluster.
func Open(endpoints []string, prefix string) (*Store, error) {
	return OpenTLS(endpoints, prefix, nil)
}

// OpenTLS connects to etcd. Pass a tls.Config to trust the cluster's certificate authority.
// A nil config means plain HTTP.
func OpenTLS(endpoints []string, prefix string, tlsCfg *tls.Config) (*Store, error) {
	if len(endpoints) == 0 {
		return nil, errors.New("at least one etcd endpoint is required")
	}
	if !strings.HasPrefix(prefix, "/") {
		return nil, errors.New("prefix must start with /")
	}
	cli, err := clientv3.New(clientv3.Config{
		Endpoints:   endpoints,
		DialTimeout: opTimeout,
		TLS:         tlsCfg,
	})
	if err != nil {
		return nil, fmt.Errorf("connect to etcd: %w", err)
	}
	return &Store{
		cli:      cli,
		prefix:   strings.TrimSuffix(prefix, "/"),
		Now:      time.Now,
		LeaseTTL: DefaultLeaseTTL,
		leases:   map[string]clientv3.LeaseID{},
		written:  map[string][2]int{},
	}, nil
}

// Close releases the connection.
func (s *Store) Close() error { return s.cli.Close() }

func (s *Store) seqKey() string            { return s.prefix + "/seq" }
func (s *Store) jobsPrefix() string        { return s.prefix + "/jobs/" }
func (s *Store) queuePrefix() string       { return s.prefix + "/queue/" }
func (s *Store) assignedPrefix() string    { return s.prefix + "/assigned/" }
func (s *Store) agentsPrefix() string      { return s.prefix + "/agents/" }
func (s *Store) jobKey(seq int64) string   { return fmt.Sprintf("%s%020d", s.jobsPrefix(), seq) }
func (s *Store) queueKey(seq int64) string { return fmt.Sprintf("%s%020d", s.queuePrefix(), seq) }
func (s *Store) assignedKey(seq int64) string {
	return fmt.Sprintf("%s%020d", s.assignedPrefix(), seq)
}
func (s *Store) agentKey(name string) string { return s.agentsPrefix() + name }

func jobID(seq int64) string { return "job-" + strconv.FormatInt(seq, 10) }

func parseJobID(id string) (int64, error) {
	if !strings.HasPrefix(id, "job-") {
		return 0, fmt.Errorf("bad job id %q", id)
	}
	n, err := strconv.ParseInt(strings.TrimPrefix(id, "job-"), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("bad job id %q", id)
	}
	return n, nil
}

func seqFromKey(key string) (int64, error) {
	i := strings.LastIndex(key, "/")
	return strconv.ParseInt(key[i+1:], 10, 64)
}

func ctxTimeout() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), opTimeout)
}

// Submit validates a spec and queues it for owner.
func (s *Store) Submit(spec jobs.Spec, owner string) (jobs.Job, error) {
	if err := spec.Validate(); err != nil {
		return jobs.Job{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if strings.TrimSpace(owner) == "" {
		return jobs.Job{}, fmt.Errorf("%w: owner is required", ErrInvalid)
	}
	// Retry on contention: another coordinator may take the next number first.
	for attempt := 0; attempt < 50; attempt++ {
		ctx, cancel := ctxTimeout()
		resp, err := s.cli.Get(ctx, s.seqKey())
		cancel()
		if err != nil {
			return jobs.Job{}, err
		}
		var cur, rev int64
		if len(resp.Kvs) == 1 {
			cur, err = strconv.ParseInt(string(resp.Kvs[0].Value), 10, 64)
			if err != nil {
				return jobs.Job{}, fmt.Errorf("corrupt sequence counter: %w", err)
			}
			rev = resp.Kvs[0].ModRevision
		}
		next := cur + 1
		now := s.Now()
		job := jobs.Job{
			ID:        jobID(next),
			Owner:     owner,
			Spec:      spec,
			Status:    jobs.Queued,
			Submitted: now,
			Updated:   now,
		}
		raw, err := json.Marshal(job)
		if err != nil {
			return jobs.Job{}, err
		}

		ctx, cancel = ctxTimeout()
		txn, err := s.cli.Txn(ctx).
			If(clientv3.Compare(clientv3.ModRevision(s.seqKey()), "=", rev)).
			Then(
				clientv3.OpPut(s.seqKey(), strconv.FormatInt(next, 10)),
				clientv3.OpPut(s.jobKey(next), string(raw)),
				clientv3.OpPut(s.queueKey(next), ""),
			).Commit()
		cancel()
		if err != nil {
			return jobs.Job{}, err
		}
		if txn.Succeeded {
			return job, nil
		}
	}
	return jobs.Job{}, errors.New("could not allocate a job id: too much contention")
}

// Poll is called by an agent on every loop. It refreshes the agent's lease, requeues jobs
// whose agents have gone silent, and claims the oldest queued job if the agent has a free slot.
func (s *Store) Poll(name string, slots, running int) (*jobs.Job, error) {
	if strings.TrimSpace(name) == "" {
		return nil, fmt.Errorf("%w: agent name is required", ErrInvalid)
	}
	if err := s.refreshAgent(name, slots, running); err != nil {
		return nil, err
	}
	if err := s.requeueOrphans(); err != nil {
		return nil, err
	}
	if running >= slots {
		return nil, nil
	}
	return s.claimOldest(name)
}

// refreshAgent writes the agent's record under its lease, creating the lease on first use
// or after it has expired.
func (s *Store) refreshAgent(name string, slots, running int) error {
	s.mu.Lock()
	id, known := s.leases[name]
	last, wrote := s.written[name]
	s.mu.Unlock()

	ctx, cancel := ctxTimeout()
	defer cancel()
	if known {
		if _, err := s.cli.KeepAliveOnce(ctx, id); err != nil {
			known = false
		} else if wrote && last == [2]int{slots, running} {
			// Nothing changed and the lease is renewed, so there is nothing to write.
			return nil
		}
	}
	if !known {
		ttl := int64(s.LeaseTTL / time.Second)
		if ttl < 1 {
			ttl = 1
		}
		grant, err := s.cli.Grant(ctx, ttl)
		if err != nil {
			return err
		}
		id = grant.ID
		s.mu.Lock()
		s.leases[name] = id
		s.mu.Unlock()
	}

	raw, err := json.Marshal(agentInfo{Slots: slots, Running: running, LastSeen: s.Now().UnixNano()})
	if err != nil {
		return err
	}
	if _, err := s.cli.Put(ctx, s.agentKey(name), string(raw), clientv3.WithLease(id)); err != nil {
		return err
	}
	s.mu.Lock()
	s.written[name] = [2]int{slots, running}
	s.mu.Unlock()
	return nil
}

// requeueOrphans moves jobs back to the queue when the agent holding them no longer has a
// live lease.
func (s *Store) requeueOrphans() error {
	ctx, cancel := ctxTimeout()
	resp, err := s.cli.Get(ctx, s.assignedPrefix(), clientv3.WithPrefix())
	cancel()
	if err != nil {
		return err
	}
	for _, kv := range resp.Kvs {
		agent := string(kv.Value)
		ctx, cancel := ctxTimeout()
		alive, err := s.cli.Get(ctx, s.agentKey(agent), clientv3.WithCountOnly())
		cancel()
		if err != nil {
			return err
		}
		if alive.Count > 0 {
			continue
		}
		seq, err := seqFromKey(string(kv.Key))
		if err != nil {
			return err
		}
		_, err = s.updateJob(seq, func(j *jobs.Job) error {
			if j.Status != jobs.Assigned || j.Agent != agent {
				return errSkip
			}
			j.Status = jobs.Queued
			j.Agent = ""
			return nil
		}, func() ([]clientv3.Cmp, []clientv3.Op) {
			return []clientv3.Cmp{clientv3.Compare(clientv3.ModRevision(s.assignedKey(seq)), "=", kv.ModRevision)},
				[]clientv3.Op{clientv3.OpDelete(s.assignedKey(seq)), clientv3.OpPut(s.queueKey(seq), "")}
		})
		if err != nil && !errors.Is(err, errSkip) {
			return err
		}
	}
	return nil
}

// claimOldest hands the oldest queued job to an agent. It tries a few candidates in case
// another agent takes one first.
func (s *Store) claimOldest(name string) (*jobs.Job, error) {
	for attempt := 0; attempt < 20; attempt++ {
		ctx, cancel := ctxTimeout()
		resp, err := s.cli.Get(ctx, s.queuePrefix(), clientv3.WithPrefix(), clientv3.WithLimit(1))
		cancel()
		if err != nil {
			return nil, err
		}
		if len(resp.Kvs) == 0 {
			return nil, nil
		}
		kv := resp.Kvs[0]
		seq, err := seqFromKey(string(kv.Key))
		if err != nil {
			return nil, err
		}
		job, err := s.updateJob(seq, func(j *jobs.Job) error {
			if j.Status != jobs.Queued {
				return errSkip
			}
			j.Status = jobs.Assigned
			j.Agent = name
			return nil
		}, func() ([]clientv3.Cmp, []clientv3.Op) {
			return []clientv3.Cmp{clientv3.Compare(clientv3.ModRevision(s.queueKey(seq)), "=", kv.ModRevision)},
				[]clientv3.Op{clientv3.OpDelete(s.queueKey(seq)), clientv3.OpPut(s.assignedKey(seq), name)}
		})
		if errors.Is(err, errSkip) {
			continue
		}
		if err != nil {
			return nil, err
		}
		return &job, nil
	}
	return nil, nil
}

// Report records the result of a job. Only the agent the job is assigned to may report it.
func (s *Store) Report(id, name string, status jobs.Status, exitCode int, log string) error {
	if !status.Finished() {
		return fmt.Errorf("status %q is not a final status", status)
	}
	seq, err := parseJobID(id)
	if err != nil {
		return err
	}
	_, err = s.updateJob(seq, func(j *jobs.Job) error {
		if j.Status != jobs.Assigned || j.Agent != name {
			return ErrNotAssigned
		}
		ec := exitCode
		j.Status = status
		j.ExitCode = &ec
		j.Log = log
		return nil
	}, func() ([]clientv3.Cmp, []clientv3.Op) {
		return []clientv3.Cmp{clientv3.Compare(clientv3.Value(s.assignedKey(seq)), "=", name)},
			[]clientv3.Op{clientv3.OpDelete(s.assignedKey(seq))}
	})
	if errors.Is(err, errSkip) {
		return ErrNotAssigned
	}
	return err
}

// updateJob reads a job, applies mutate, and writes it back in one transaction. The write
// only happens if the job has not changed since it was read, and if the extra conditions
// hold. It returns the updated job.
func (s *Store) updateJob(
	seq int64,
	mutate func(*jobs.Job) error,
	extra func() ([]clientv3.Cmp, []clientv3.Op),
) (jobs.Job, error) {
	for attempt := 0; attempt < 50; attempt++ {
		ctx, cancel := ctxTimeout()
		resp, err := s.cli.Get(ctx, s.jobKey(seq))
		cancel()
		if err != nil {
			return jobs.Job{}, err
		}
		if len(resp.Kvs) == 0 {
			return jobs.Job{}, fmt.Errorf("no such job %s", jobID(seq))
		}
		kv := resp.Kvs[0]
		var j jobs.Job
		if err := json.Unmarshal(kv.Value, &j); err != nil {
			return jobs.Job{}, fmt.Errorf("corrupt job %s: %w", jobID(seq), err)
		}
		if err := mutate(&j); err != nil {
			return jobs.Job{}, err
		}
		j.Updated = s.Now()
		raw, err := json.Marshal(j)
		if err != nil {
			return jobs.Job{}, err
		}

		cmps, ops := extra()
		cmps = append(cmps, clientv3.Compare(clientv3.ModRevision(s.jobKey(seq)), "=", kv.ModRevision))
		ops = append(ops, clientv3.OpPut(s.jobKey(seq), string(raw)))

		ctx, cancel = ctxTimeout()
		txn, err := s.cli.Txn(ctx).If(cmps...).Then(ops...).Commit()
		cancel()
		if err != nil {
			return jobs.Job{}, err
		}
		if txn.Succeeded {
			return j, nil
		}
		// Something changed under us. Read again and retry.
	}
	return jobs.Job{}, fmt.Errorf("job %s: too much contention", jobID(seq))
}

// Get returns one job.
func (s *Store) Get(id string) (jobs.Job, bool) {
	seq, err := parseJobID(id)
	if err != nil {
		return jobs.Job{}, false
	}
	ctx, cancel := ctxTimeout()
	defer cancel()
	resp, err := s.cli.Get(ctx, s.jobKey(seq))
	if err != nil || len(resp.Kvs) == 0 {
		return jobs.Job{}, false
	}
	var j jobs.Job
	if err := json.Unmarshal(resp.Kvs[0].Value, &j); err != nil {
		return jobs.Job{}, false
	}
	return j, true
}

// List returns owner's jobs, newest first. It looks at the newest ListLimit jobs overall
// and keeps the ones owner submitted.
func (s *Store) List(owner string) ([]jobs.Job, error) {
	ctx, cancel := ctxTimeout()
	defer cancel()
	resp, err := s.cli.Get(ctx, s.jobsPrefix(),
		clientv3.WithPrefix(),
		clientv3.WithSort(clientv3.SortByKey, clientv3.SortDescend),
		clientv3.WithLimit(ListLimit))
	if err != nil {
		return nil, err
	}
	out := make([]jobs.Job, 0, len(resp.Kvs))
	for _, kv := range resp.Kvs {
		var j jobs.Job
		if err := json.Unmarshal(kv.Value, &j); err != nil {
			return nil, fmt.Errorf("corrupt job at %s: %w", kv.Key, err)
		}
		if j.Owner == owner {
			out = append(out, j)
		}
	}
	return out, nil
}

// Metrics renders the state in Prometheus text format. Counting jobs reads every job record,
// so scrape it at a sensible interval.
func (s *Store) Metrics() (string, error) {
	ctx, cancel := ctxTimeout()
	jobResp, err := s.cli.Get(ctx, s.jobsPrefix(), clientv3.WithPrefix())
	agentResp, err2 := s.cli.Get(ctx, s.agentsPrefix(), clientv3.WithPrefix())
	cancel()
	if err != nil {
		return "", err
	}
	if err2 != nil {
		return "", err2
	}

	counts := map[jobs.Status]int{}
	for _, kv := range jobResp.Kvs {
		var j jobs.Job
		if err := json.Unmarshal(kv.Value, &j); err != nil {
			continue
		}
		counts[j.Status]++
	}
	var online, slots, busy int
	for _, kv := range agentResp.Kvs {
		var a agentInfo
		if err := json.Unmarshal(kv.Value, &a); err != nil {
			continue
		}
		online++
		slots += a.Slots
		busy += a.Running
	}

	var b strings.Builder
	b.WriteString("# HELP minibox_jobs Number of jobs by status.\n# TYPE minibox_jobs gauge\n")
	for _, st := range []jobs.Status{jobs.Queued, jobs.Assigned, jobs.Succeeded, jobs.Failed, jobs.TimedOut} {
		fmt.Fprintf(&b, "minibox_jobs{status=%q} %d\n", string(st), counts[st])
	}
	b.WriteString("# HELP minibox_agents_online Agents with a live lease.\n# TYPE minibox_agents_online gauge\n")
	fmt.Fprintf(&b, "minibox_agents_online %d\n", online)
	b.WriteString("# HELP minibox_agent_slots Total job slots on online agents.\n# TYPE minibox_agent_slots gauge\n")
	fmt.Fprintf(&b, "minibox_agent_slots %d\n", slots)
	b.WriteString("# HELP minibox_agent_slots_busy Slots currently running a job.\n# TYPE minibox_agent_slots_busy gauge\n")
	fmt.Fprintf(&b, "minibox_agent_slots_busy %d\n", busy)
	return b.String(), nil
}
