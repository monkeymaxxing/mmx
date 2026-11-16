// Command simnodes serves many simulated MMX nodes, one TLS listener each, so
// the real controller can be measured at fleet scale without the nodes' own
// cost. Requests are answered, not authorized: the controller still signs each
// one, and only its cost is being measured.
package main

import (
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"mmx/internal/endpoint"
	"mmx/internal/facts"
	"mmx/internal/protocol"
	"mmx/internal/release"
)

type job struct{ started time.Time }

type stats struct {
	Requests map[string]int64 `json:"requests"`
	BytesIn  int64            `json:"bytes_in"`
	BytesOut int64            `json:"bytes_out"`
	// Timeline is each node's first info request, submit request, and finished
	// answer, in seconds since the first request any node received.
	Timeline map[string][3]float64 `json:"timeline"`
}

type sim struct {
	identity                   *endpoint.Identity
	runnerHash, arch           string
	duration                   time.Duration
	nodeTasks, stdout, changed int
	facts                      facts.Snapshot
	mu                         sync.Mutex
	jobs                       map[string]job
	first                      time.Time
	timeline                   map[string][3]float64
	requests                   map[string]*atomic.Int64
	bytesIn, bytesOut          atomic.Int64
}

func nodeID(name string) string {
	sum := sha256.Sum256([]byte("simnode/" + name))
	return hex.EncodeToString(sum[:])
}

// response is the job's state after the elapsed time: one more result per
// equal slice of the duration, then success with every result.
func (s *sim) response(id string, j job) protocol.Response {
	done := s.nodeTasks
	if s.duration > 0 {
		done = int(float64(s.nodeTasks) * float64(time.Since(j.started)) / float64(s.duration))
	}
	r := protocol.Response{ID: id, Status: "running", Facts: &s.facts}
	if done >= s.nodeTasks {
		done = s.nodeTasks
		now := time.Now().UTC()
		r.Status, r.Finished = "succeeded", &now
		r.Change = "unchanged"
		if s.changed > 0 {
			r.Change = "changed"
		}
	} else {
		r.CurrentTask = fmt.Sprintf("configure/task_%d", done+1)
	}
	for i := 0; i < done; i++ {
		change := "unchanged"
		if i < s.changed {
			change = "changed"
		}
		r.Results = append(r.Results, protocol.Result{Task: fmt.Sprintf("configure/task_%d", i+1), Status: "succeeded", Change: change,
			Stdout: strings.Repeat("x", s.stdout)})
	}
	return r
}

// mark records the first time node name reached a timeline event.
func (s *sim) mark(name string, event int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.first.IsZero() {
		s.first = time.Now()
	}
	t := s.timeline[name]
	if t[event] == 0 {
		t[event] = time.Since(s.first).Seconds()
		s.timeline[name] = t
	}
}

func (s *sim) count(kind string) {
	s.mu.Lock()
	c, ok := s.requests[kind]
	if !ok {
		c = &atomic.Int64{}
		s.requests[kind] = c
	}
	s.mu.Unlock()
	c.Add(1)
}

// node is one simulated machine's endpoint.
type managedNode struct {
	*sim
	name string
}

func (h managedNode) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s, name, path := h.sim, h.name, r.URL.Path
	if path == "/stats" {
		out := stats{Requests: map[string]int64{}, BytesIn: s.bytesIn.Load(), BytesOut: s.bytesOut.Load(), Timeline: map[string][3]float64{}}
		s.mu.Lock()
		for name, t := range s.timeline {
			out.Timeline[name] = t
		}
		for kind, c := range s.requests {
			out.Requests[kind] = c.Load()
		}
		s.mu.Unlock()
		_ = json.NewEncoder(w).Encode(out)
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	s.bytesIn.Add(int64(len(body)))
	var result any
	switch {
	case path == "/v1/session":
		s.count("session")
		result = map[string]string{"status": "authenticated"}
	case path == "/v1/info":
		s.count("info")
		s.mark(name, 0)
		result = endpoint.Info{NodeID: nodeID(name), Architecture: s.arch, RunnerHash: s.runnerHash, TLSPin: s.identity.Pin, BootID: "51515151-0000-4000-8000-000000000000", RunnerVersion: release.Version}
	case path == "/v1/submit":
		s.count("submit")
		s.mark(name, 1)
		var submitted struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(body, &submitted); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		s.mu.Lock()
		if _, exists := s.jobs[submitted.ID]; !exists {
			s.jobs[submitted.ID] = job{started: time.Now()}
		}
		j := s.jobs[submitted.ID]
		s.mu.Unlock()
		r := s.response(submitted.ID, j)
		if r.Status != "running" {
			s.mark(name, 2)
		}
		result = r.Transport()
	case strings.HasPrefix(path, "/v1/status/"), strings.HasPrefix(path, "/v1/stop/"):
		s.count("status")
		id := path[strings.LastIndex(path, "/")+1:]
		s.mu.Lock()
		j, exists := s.jobs[id]
		s.mu.Unlock()
		if !exists {
			result = protocol.Response{ID: id, Status: "not_found"}.Transport()
		} else {
			result = s.response(id, j).Transport()
		}
	case path == "/v1/progress":
		// Like node.Server.Progress: answer once the job has moved past the
		// request, or after 5 seconds, with only the newer results.
		s.count("progress")
		var request protocol.ProgressRequest
		if err := json.Unmarshal(body, &request); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		s.mu.Lock()
		j, exists := s.jobs[request.ID]
		s.mu.Unlock()
		if !exists {
			result = protocol.Response{ID: request.ID, Status: "not_found"}.Transport()
			break
		}
		deadline := time.Now().Add(5 * time.Second)
		r := s.response(request.ID, j)
		for r.Status == "running" && len(r.Results) == request.Results && r.CurrentTask == request.CurrentTask && time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond)
			r = s.response(request.ID, j)
		}
		if r.Status != "running" {
			s.mark(name, 2)
		}
		if r.Status == "running" && request.Results > 0 && request.Results <= len(r.Results) {
			r.Results, r.ResultsAfter, r.Facts = r.Results[request.Results:], request.Results, nil
		}
		result = r.Transport()
	default:
		http.Error(w, "unsupported simulated endpoint "+path, 404)
		return
	}
	out, err := json.Marshal(result)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	s.bytesOut.Add(int64(len(out)) + 1)
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(append(out, '\n'))
}

func main() {
	nodes := flag.Int("nodes", 10, "simulated nodes, named sim-00000 upward")
	state := flag.String("state", "", "directory for the TLS identity and identity.json")
	runner := flag.String("runner", "", "runner artifact the simulated nodes report as installed")
	arch := flag.String("arch", "arm64", "architecture the simulated nodes report")
	factsFile := flag.String("facts", "", "facts snapshot JSON every node reports")
	duration := flag.Duration("duration", time.Second, "time each job runs")
	nodeTasks := flag.Int("tasks", 10, "results each job records")
	stdout := flag.Int("stdout", 64, "stdout bytes in each result")
	changed := flag.Int("changed", 0, "leading results that report a change")
	flag.Parse()
	identity, err := endpoint.LoadIdentity(*state)
	if err != nil {
		log.Fatal(err)
	}
	binary, err := os.ReadFile(*runner)
	if err != nil {
		log.Fatal(err)
	}
	sum := sha256.Sum256(binary)
	s := &sim{identity: identity, runnerHash: hex.EncodeToString(sum[:]), arch: *arch, duration: *duration, nodeTasks: *nodeTasks, stdout: *stdout, changed: *changed,
		jobs: map[string]job{}, requests: map[string]*atomic.Int64{}, timeline: map[string][3]float64{}}
	raw, err := os.ReadFile(*factsFile)
	if err != nil {
		log.Fatal(err)
	}
	if err := json.Unmarshal(raw, &s.facts); err != nil {
		log.Fatal(err)
	}
	addresses := map[string]string{}
	failed := make(chan error, 1)
	for i := 0; i < *nodes; i++ {
		name := fmt.Sprintf("sim-%05d", i)
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			log.Fatal(err)
		}
		addresses[name] = listener.Addr().String()
		server := &http.Server{Handler: managedNode{s, name}, TLSConfig: &tls.Config{MinVersion: tls.VersionTLS13, GetCertificate: identity.GetCertificate}, ErrorLog: log.New(io.Discard, "", 0)}
		go func() { failed <- server.ServeTLS(listener, "", "") }()
	}
	info, _ := json.Marshal(map[string]any{"addresses": addresses, "tls_pin": identity.Pin, "runner_hash": s.runnerHash, "architecture": *arch})
	if err := os.WriteFile(*state+"/identity.json", info, 0600); err != nil {
		log.Fatal(err)
	}
	log.Fatal(<-failed)
}
