// mbd is the minibox coordinator. It takes jobs over HTTP, queues them, and
// hands them to agents (mbagent) that have free slots.
//
//	mbd --addr :8787 --token CLIENT_SECRET --agent-tokens agents.txt --etcd http://127.0.0.1:2379
//
// agents.txt holds one "name token" pair per line. Each agent gets its own token, and it can
// only poll and report as the name that token belongs to.
package main

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"

	"mbctl/internal/coord"
	"mbctl/internal/jobs"
)

func main() {
	addr := flag.String("addr", ":8787", "address to listen on")
	clientToken := flag.String("token", os.Getenv("MBD_TOKEN"), "token for clients that submit and read jobs (or set MBD_TOKEN)")
	agentFile := flag.String("agent-tokens", "", "file of 'name token' lines, one per agent")
	etcd := flag.String("etcd", "http://127.0.0.1:2379", "comma-separated etcd endpoints")
	prefix := flag.String("prefix", "/mbd", "etcd key prefix for this coordinator's state")
	flag.Parse()

	if *clientToken == "" {
		log.Fatal("mbd: a client token is required (--token or MBD_TOKEN)")
	}
	agents, err := loadAgentTokens(*agentFile)
	if err != nil {
		log.Fatalf("mbd: %v", err)
	}
	if len(agents) == 0 {
		log.Print("mbd: no agent tokens loaded, so no agent can connect (see --agent-tokens)")
	}

	store, err := coord.Open(strings.Split(*etcd, ","), *prefix)
	if err != nil {
		log.Fatalf("mbd: %v", err)
	}
	defer store.Close()

	a := &auth{clientTok: []byte(*clientToken), agents: agents}
	srv := &server{store: store}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/jobs", a.client(srv.submit))
	mux.HandleFunc("GET /v1/jobs", a.client(srv.list))
	mux.HandleFunc("GET /v1/jobs/{id}", a.client(srv.get))
	mux.HandleFunc("POST /v1/agents/poll", a.agent(srv.pollHandler(a)))
	mux.HandleFunc("POST /v1/jobs/{id}/report", a.agent(srv.reportHandler(a)))
	mux.HandleFunc("GET /metrics", srv.metrics)

	log.Printf("mbd listening on %s, state in etcd at %s (prefix %s), %d agent(s) allowed", *addr, *etcd, *prefix, len(agents))
	log.Fatal(http.ListenAndServe(*addr, mux))
}

// loadAgentTokens reads "name token" lines. Blank lines and lines starting with # are skipped.
func loadAgentTokens(path string) (map[string]string, error) {
	out := map[string]string{}
	if path == "" {
		return out, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read agent tokens: %w", err)
	}
	for i, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			return nil, fmt.Errorf("agent tokens line %d: want 'name token'", i+1)
		}
		out[fields[1]] = fields[0]
	}
	return out, nil
}

type auth struct {
	clientTok []byte
	agents    map[string]string // token -> agent name
}

func bearer(r *http.Request) []byte {
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, "Bearer ") {
		return nil
	}
	return []byte(strings.TrimPrefix(h, "Bearer "))
}

// client lets through requests that carry the client token.
func (a *auth) client(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if len(a.clientTok) == 0 || subtle.ConstantTimeCompare(bearer(r), a.clientTok) != 1 {
			writeError(w, http.StatusUnauthorized, "missing or wrong client token")
			return
		}
		next(w, r)
	}
}

// agent lets through requests from a known agent. The handler still checks that the name
// in the body matches the token's agent (see agentName).
func (a *auth) agent(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := a.lookup(bearer(r)); !ok {
			writeError(w, http.StatusUnauthorized, "missing or unknown agent token")
			return
		}
		next(w, r)
	}
}

// lookup finds the agent a token belongs to. It checks every token, so the timing does not
// reveal which one matched.
func (a *auth) lookup(tok []byte) (string, bool) {
	if len(tok) == 0 {
		return "", false
	}
	name, found := "", false
	for known, n := range a.agents {
		if subtle.ConstantTimeCompare(tok, []byte(known)) == 1 {
			name, found = n, true
		}
	}
	return name, found
}

type server struct {
	store *coord.Store
}

// agentMatches checks that the agent named in a request is the agent its token belongs to.
func (s *server) agentMatches(a *auth, r *http.Request, named string) bool {
	name, ok := a.lookup(bearer(r))
	return ok && name == named
}

func (s *server) submit(w http.ResponseWriter, r *http.Request) {
	var spec jobs.Spec
	if err := decode(w, r, &spec); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	job, err := s.store.Submit(spec)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, job)
}

func (s *server) list(w http.ResponseWriter, r *http.Request) {
	jobs, err := s.store.List()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, jobs)
}

func (s *server) get(w http.ResponseWriter, r *http.Request) {
	job, ok := s.store.Get(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "no such job")
		return
	}
	writeJSON(w, http.StatusOK, job)
}

type pollRequest struct {
	Name    string `json:"name"`
	Slots   int    `json:"slots"`
	Running int    `json:"running"`
}

func (s *server) pollHandler(a *auth) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req pollRequest
		if err := decode(w, r, &req); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if !s.agentMatches(a, r, req.Name) {
			writeError(w, http.StatusForbidden, "this token does not belong to that agent")
			return
		}
		job, err := s.store.Poll(req.Name, req.Slots, req.Running)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		if job == nil {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		writeJSON(w, http.StatusOK, job)
	}
}

type reportRequest struct {
	Agent    string `json:"agent"`
	Status   string `json:"status"`
	ExitCode int    `json:"exit_code"`
	Log      string `json:"log"`
}

func (s *server) reportHandler(a *auth) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req reportRequest
		if err := decode(w, r, &req); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if !s.agentMatches(a, r, req.Agent) {
			writeError(w, http.StatusForbidden, "this token does not belong to that agent")
			return
		}
		err := s.store.Report(r.PathValue("id"), req.Agent, jobs.Status(req.Status), req.ExitCode, req.Log)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func (s *server) metrics(w http.ResponseWriter, r *http.Request) {
	text, err := s.store.Metrics()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	_, _ = w.Write([]byte(text))
}

func decode(w http.ResponseWriter, r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return errors.New("invalid JSON: " + err.Error())
	}
	return nil
}

// writeStoreError picks the status code from what went wrong: the caller's mistake (400),
// a report for a job it doesn't hold (409), or a storage problem such as a lost etcd
// quorum (503). Clients can retry the last one.
func writeStoreError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, coord.ErrInvalid):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, coord.ErrNotAssigned):
		writeError(w, http.StatusConflict, err.Error())
	default:
		writeError(w, http.StatusServiceUnavailable, "coordinator storage unavailable: "+err.Error())
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": strings.TrimSpace(msg)})
}
