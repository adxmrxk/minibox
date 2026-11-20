// mbd is the minibox coordinator. It takes jobs over HTTPS, queues them, and
// hands them to agents (mbagent) that have free slots.
//
//	mbd --addr :8787 --tls-cert server.crt --tls-key server.key \
//	    --client-tokens clients.txt --agent-tokens agents.txt --etcd https://etcd:2379
//
//	mbd token-hash            # reads a token on stdin, prints the hash to store in a file
//
// Token files hold one "name sha256-of-token" pair per line. Raw tokens never sit on the
// server. A name may appear more than once, so a user can hold two tokens during a rotation.
// Send SIGHUP to mbd to reload both files without restarting.
package main

import (
	"bufio"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	_ "net/http/pprof" // registers /debug/pprof on DefaultServeMux, served only with --pprof
	"os"
	"os/signal"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"mbctl/internal/coord"
	"mbctl/internal/jobs"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "token-hash" {
		tokenHash()
		return
	}

	addr := flag.String("addr", ":8787", "address to listen on")
	clientFile := flag.String("client-tokens", "", "file of 'user sha256' lines, one per user token")
	agentFile := flag.String("agent-tokens", "", "file of 'name sha256' lines, one per agent token")
	etcd := flag.String("etcd", "http://127.0.0.1:2379", "comma-separated etcd endpoints")
	prefix := flag.String("prefix", "/mbd", "etcd key prefix for this coordinator's state")
	tlsCert := flag.String("tls-cert", "", "TLS certificate file (PEM)")
	tlsKey := flag.String("tls-key", "", "TLS private key file (PEM)")
	insecure := flag.Bool("insecure-http", false, "serve plain HTTP. For local testing only; tokens are sent unencrypted")
	etcdCA := flag.String("etcd-ca", "", "CA file (PEM) for etcd's TLS certificate. Set it when the etcd endpoints are https")
	pprofAddr := flag.String("pprof", "", "optional address for Go profiling endpoints, e.g. 127.0.0.1:6060. Off by default")
	flag.Parse()

	if *pprofAddr != "" {
		go func() {
			log.Printf("mbd profiling endpoints on %s", *pprofAddr)
			log.Print(http.ListenAndServe(*pprofAddr, http.DefaultServeMux))
		}()
	}

	if (*tlsCert == "") != (*tlsKey == "") {
		log.Fatal("mbd: --tls-cert and --tls-key must be given together")
	}
	if *tlsCert == "" && !*insecure {
		log.Fatal("mbd: TLS is required. Pass --tls-cert and --tls-key, or --insecure-http for local testing")
	}
	if *clientFile == "" {
		log.Fatal("mbd: --client-tokens is required")
	}

	a := &auth{clientPath: *clientFile, agentPath: *agentFile}
	if err := a.reload(); err != nil {
		log.Fatalf("mbd: %v", err)
	}
	if len(a.clients) == 0 {
		log.Fatal("mbd: no client tokens loaded")
	}
	if len(a.agents) == 0 {
		log.Print("mbd: no agent tokens loaded, so no agent can connect (see --agent-tokens)")
	}
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	go func() {
		for range hup {
			if err := a.reload(); err != nil {
				log.Printf("mbd: reload failed, keeping the old tokens: %v", err)
				continue
			}
			log.Printf("mbd: reloaded tokens (%d user token(s), %d agent token(s))", len(a.clients), len(a.agents))
		}
	}()

	var etcdTLS *tls.Config
	if *etcdCA != "" {
		pem, err := os.ReadFile(*etcdCA)
		if err != nil {
			log.Fatalf("mbd: read etcd CA: %v", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			log.Fatal("mbd: etcd CA file has no valid PEM certificates")
		}
		etcdTLS = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
	store, err := coord.OpenTLS(strings.Split(*etcd, ","), *prefix, etcdTLS)
	if err != nil {
		log.Fatalf("mbd: %v", err)
	}
	defer store.Close()

	srv := &server{store: store}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/jobs", a.client(srv.submit))
	mux.HandleFunc("GET /v1/jobs", a.client(srv.list))
	mux.HandleFunc("GET /v1/jobs/{id}", a.client(srv.get))
	mux.HandleFunc("POST /v1/agents/poll", a.agent(srv.pollHandler(a)))
	mux.HandleFunc("POST /v1/jobs/{id}/report", a.agent(srv.reportHandler(a)))
	mux.HandleFunc("GET /metrics", srv.metrics)

	log.Printf("mbd listening on %s (tls=%t), state in etcd at %s (prefix %s)", *addr, *tlsCert != "", *etcd, *prefix)
	if *tlsCert != "" {
		log.Fatal(http.ListenAndServeTLS(*addr, *tlsCert, *tlsKey, mux))
	}
	log.Fatal(http.ListenAndServe(*addr, mux))
}

// tokenHash reads one token on stdin and prints its SHA-256 in hex.
func tokenHash() {
	sc := bufio.NewScanner(os.Stdin)
	if !sc.Scan() {
		fmt.Fprintln(os.Stderr, "mbd token-hash: give the token on stdin")
		os.Exit(2)
	}
	token := strings.TrimSpace(sc.Text())
	if token == "" {
		fmt.Fprintln(os.Stderr, "mbd token-hash: the token is empty")
		os.Exit(2)
	}
	sum := sha256.Sum256([]byte(token))
	fmt.Println(hex.EncodeToString(sum[:]))
}

// validName is the set of names a user or agent may have. Names become etcd key segments, so
// they must not contain '/'.
var validName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// loadTokens reads "name hash" lines into a map from hash to name. Blank lines and lines
// starting with # are skipped. A raw token in place of a hash is rejected.
func loadTokens(path, kind string) (map[string]string, error) {
	out := map[string]string{}
	if path == "" {
		return out, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s tokens: %w", kind, err)
	}
	for i, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			return nil, fmt.Errorf("%s tokens line %d: want 'name sha256'", kind, i+1)
		}
		if !validName.MatchString(fields[0]) {
			return nil, fmt.Errorf("%s tokens line %d: name may only use letters, digits, '.', '_' and '-'", kind, i+1)
		}
		if len(fields[1]) != 64 {
			return nil, fmt.Errorf("%s tokens line %d: store the SHA-256 of the token, not the token (run: mbd token-hash)", kind, i+1)
		}
		if _, err := hex.DecodeString(fields[1]); err != nil {
			return nil, fmt.Errorf("%s tokens line %d: hash is not hex", kind, i+1)
		}
		out[strings.ToLower(fields[1])] = fields[0]
	}
	return out, nil
}

type auth struct {
	mu         sync.RWMutex
	clientPath string
	agentPath  string
	clients    map[string]string // hash -> user
	agents     map[string]string // hash -> agent name
}

// reload reads both token files. It only swaps the new maps in if both files are valid.
func (a *auth) reload() error {
	clients, err := loadTokens(a.clientPath, "client")
	if err != nil {
		return err
	}
	agents, err := loadTokens(a.agentPath, "agent")
	if err != nil {
		return err
	}
	a.mu.Lock()
	a.clients, a.agents = clients, agents
	a.mu.Unlock()
	return nil
}

func bearer(r *http.Request) []byte {
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, "Bearer ") {
		return nil
	}
	return []byte(strings.TrimPrefix(h, "Bearer "))
}

// lookup hashes the presented token and finds its name. It checks every entry, so the
// timing does not reveal which one matched.
func lookup(known map[string]string, tok []byte) (string, bool) {
	if len(tok) == 0 {
		return "", false
	}
	sum := sha256.Sum256(tok)
	want := []byte(hex.EncodeToString(sum[:]))
	name, found := "", false
	for h, n := range known {
		if subtle.ConstantTimeCompare(want, []byte(h)) == 1 {
			name, found = n, true
		}
	}
	return name, found
}

// client lets through requests from a known user and passes the user's name to the handler.
func (a *auth) client(next func(http.ResponseWriter, *http.Request, string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		a.mu.RLock()
		user, ok := lookup(a.clients, bearer(r))
		a.mu.RUnlock()
		if !ok {
			writeError(w, http.StatusUnauthorized, "missing or unknown client token")
			return
		}
		next(w, r, user)
	}
}

// agent lets through requests from a known agent. The handler still checks that the name
// in the body matches the token's agent.
func (a *auth) agent(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		a.mu.RLock()
		_, ok := lookup(a.agents, bearer(r))
		a.mu.RUnlock()
		if !ok {
			writeError(w, http.StatusUnauthorized, "missing or unknown agent token")
			return
		}
		next(w, r)
	}
}

// agentName returns the agent a request's token belongs to.
func (a *auth) agentName(r *http.Request) (string, bool) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return lookup(a.agents, bearer(r))
}

type server struct {
	store *coord.Store
}

func (s *server) submit(w http.ResponseWriter, r *http.Request, user string) {
	var spec jobs.Spec
	if err := decode(w, r, &spec); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	job, err := s.store.Submit(spec, user)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, job)
}

func (s *server) list(w http.ResponseWriter, r *http.Request, user string) {
	list, err := s.store.List(user)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// get returns a job only to the user who submitted it. Other users get a 404, so they
// can't tell whether someone else's job exists.
func (s *server) get(w http.ResponseWriter, r *http.Request, user string) {
	job, ok := s.store.Get(r.PathValue("id"))
	if !ok || job.Owner != user {
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
		if name, ok := a.agentName(r); !ok || name != req.Name {
			writeError(w, http.StatusForbidden, "this token does not belong to that agent")
			return
		}
		job, err := s.store.PollWait(req.Name, req.Slots, req.Running, pollWait)
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

// pollWait is how long an agent's poll is held open waiting for work. It must stay below the
// agents' HTTP timeout.
const pollWait = 20 * time.Second

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
		if name, ok := a.agentName(r); !ok || name != req.Agent {
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
