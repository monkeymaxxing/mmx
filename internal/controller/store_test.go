package controller

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"mmx/internal/config"
	"mmx/internal/protocol"
)

func TestProgressUpdatesOnlyTheSelectedEntry(t *testing.T) {
	c, r := fleet(t, config.FleetStage{ID: "setup", Nodes: "all"})
	if err := c.saveStatus(r); err != nil {
		t.Fatal(err)
	}
	first := r.Entries[0]
	first.JobID = "job-one"
	first.NodeJobAttempt = 2
	first.RunnerHash = "runner"
	first.PreviousNodeJobAttempts = []Attempt{{JobID: "prior", Response: protocol.Response{Status: "failed", Error: "original failure"}}}
	first.Response = protocol.Response{Status: "running"}
	if err := c.saveEntry(r.ID, first); err != nil {
		t.Fatal(err)
	}
	other := r.Entries[1]
	other.Response.Status = "succeeded"
	if err := c.saveEntry(r.ID, other); err != nil {
		t.Fatal(err)
	}
	unchanged := map[string]os.FileInfo{}
	for _, path := range []string{filepath.Join(c.runDir(r.ID), "run.json"), filepath.Join(c.runDir(r.ID), "status.json"), c.entryPath(r.ID, other)} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		unchanged[path] = info
	}
	first.Response = protocol.Response{Status: "failed", Results: []protocol.Result{{Task: "check", Status: "failed", Stdout: "captured output"}}}
	if err := c.saveEntry(r.ID, first); err != nil {
		t.Fatal(err)
	}
	for path, before := range unchanged {
		after, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if !os.SameFile(before, after) {
			t.Fatalf("one node's update rewrote %s", path)
		}
	}
	r.Status = "failed"
	if err := c.saveStatus(r); err != nil {
		t.Fatal(err)
	}
	saved, err := c.Load(r.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(saved.Entries[0], first) || !reflect.DeepEqual(saved.Entries[1], other) || saved.Entries[2].Response.Status != "pending" || saved.Status != "failed" {
		t.Fatalf("lost progress, attempts, or pending work: %+v", saved)
	}
}

type waitingJSON struct {
	started chan struct{}
	release chan struct{}
}

func (v waitingJSON) MarshalJSON() ([]byte, error) {
	close(v.started)
	select {
	case <-v.release:
		return []byte(`"saved"`), nil
	case <-time.After(5 * time.Second):
		return nil, fmt.Errorf("another node could not save while this record was being encoded")
	}
}

func TestNodesSaveProgressIndependently(t *testing.T) {
	c, r := fleet(t, config.FleetStage{ID: "setup", Nodes: "all"})
	waiting := waitingJSON{started: make(chan struct{}), release: make(chan struct{})}
	c.executeNode = func(ctx context.Context, _ *Run, _ config.FleetStage, en Entry, _ bool, save func(Entry) error) (Entry, error) {
		en.Response.Status = "running"
		if en.Node == "a" {
			en.Response.Results = []protocol.Result{{Task: "output", Value: waiting}}
			if err := save(en); err != nil {
				return en, err
			}
			en.Response.Results[0].Value = "saved"
		} else if en.Node == "b" {
			select {
			case <-waiting.started:
			case <-ctx.Done():
				return en, ctx.Err()
			}
			err := save(en)
			close(waiting.release)
			if err != nil {
				return en, err
			}
		}
		en.Response.Status = "succeeded"
		return en, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.Execute(ctx, r, false); err != nil {
		t.Fatal(err)
	}
	saved, err := c.Load(r.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Status != "succeeded" {
		t.Fatal(saved.Status)
	}
	for _, en := range saved.Entries {
		if en.Response.Status != "succeeded" {
			t.Fatal("concurrent progress was lost", en)
		}
	}
}

func BenchmarkSaveNodeProgress(b *testing.B) {
	for _, nodes := range []int{10, 1000} {
		b.Run(fmt.Sprint(nodes), func(b *testing.B) {
			c := Controller{State: b.TempDir()}
			inventory := config.Inventory{Nodes: map[string]config.Node{}}
			for i := 0; i < nodes; i++ {
				name := fmt.Sprintf("node%04d", i)
				inventory.Nodes[name] = config.Node{Address: name}
			}
			p := config.Project{Project: "benchmark", MaxNodesAtOnce: 100, FleetStages: []config.FleetStage{{ID: "setup", Nodes: "all"}}}
			r, err := c.New(context.Background(), p, inventory, nil, false)
			if err != nil {
				b.Fatal(err)
			}
			entry := r.Entries[0]
			entry.Response = protocol.Response{Status: "succeeded", Results: []protocol.Result{{Task: "example", Stdout: strings.Repeat("x", 1024)}}}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := c.saveEntry(r.ID, entry); err != nil {
					b.Fatal(err)
				}
			}
			b.StopTimer()
			info, err := os.Stat(c.entryPath(r.ID, entry))
			if err != nil {
				b.Fatal(err)
			}
			b.ReportMetric(float64(info.Size()), "bytes/save")
		})
	}
}

// A new job ID and every finished or uncertain response are saved durably;
// running progress under the published job only replaces the file.
func TestProgressOnlySavesRunningProgressOfPublishedJob(t *testing.T) {
	running := Entry{JobID: "job", Response: protocol.Response{ID: "job", Status: "running"}}
	for _, tt := range []struct {
		name      string
		entry     Entry
		published string
		want      bool
	}{
		{"publishes a new job", running, "", false},
		{"publishes a retry's new job", running, "earlier", false},
		{"running progress", running, "job", true},
		{"finished", Entry{JobID: "job", Response: protocol.Response{ID: "job", Status: "succeeded"}}, "job", false},
		{"uncertain", Entry{JobID: "job", Response: protocol.Response{ID: "job", Status: "unknown"}}, "job", false},
		{"not admitted", Entry{Response: protocol.Response{Status: "running"}}, "", false},
	} {
		if got := progressOnly(tt.entry, tt.published); got != tt.want {
			t.Errorf("%s: %v", tt.name, got)
		}
	}
}

// Unchanged bytes are not rewritten once durable, but progress saved only by
// replacement is written durably when the same bytes must become durable.
func TestEntrySaverSkipsOnlyDurableDuplicates(t *testing.T) {
	c, r := fleet(t, config.FleetStage{ID: "setup", Nodes: "a"})
	path := c.entryPath(r.ID, r.Entries[0])
	inode := func() uint64 {
		st, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		return st.Sys().(*syscall.Stat_t).Ino
	}
	save, _ := c.entrySaver(r.ID, r.Entries[0])
	published := Entry{FleetStage: "setup", Node: "a", JobID: "job", Response: protocol.Response{ID: "job", Status: "running"}}
	if err := save(published); err != nil {
		t.Fatal(err)
	}
	first := inode()
	if err := save(published); err != nil || inode() != first {
		t.Fatal("durable duplicate rewritten", err)
	}
	finished := published
	finished.Response.Status = "succeeded"
	if err := save(finished); err != nil || inode() == first {
		t.Fatal("finished response not saved", err)
	}
	saved, err := c.Load(r.ID, false)
	if err != nil || saved.Entries[0].Response.Status != "succeeded" {
		t.Fatal(err)
	}
}

// Progress-only writes feed other commands' live view at most once per
// liveInterval: newer progress is written when the interval ends, and finish
// writes the outcome at once and drops pending progress.
func TestEntrySaverSpacesProgressWrites(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c, r := fleet(t, config.FleetStage{ID: "setup", Nodes: "a"})
		save, finish := c.entrySaver(r.ID, Entry{FleetStage: "setup", Node: "a", JobID: "job"})
		saved := func() protocol.Response {
			t.Helper()
			run, err := c.Load(r.ID, false)
			if err != nil {
				t.Fatal(err)
			}
			return run.Entries[0].Response
		}
		progress := Entry{FleetStage: "setup", Node: "a", JobID: "job", Response: protocol.Response{ID: "job", Status: "running", CurrentTask: "one"}}
		for _, task := range []string{"one", "two"} {
			progress.Response.CurrentTask = task
			if err := save(progress); err != nil {
				t.Fatal(err)
			}
		}
		if got := saved().CurrentTask; got != "one" {
			t.Fatal("progress written within the interval", got)
		}
		time.Sleep(liveInterval)
		synctest.Wait()
		if got := saved().CurrentTask; got != "two" {
			t.Fatal("pending progress was not written", got)
		}
		progress.Response.CurrentTask = "three"
		if err := save(progress); err != nil {
			t.Fatal(err)
		}
		progress.Response.Status, progress.Response.CurrentTask = "succeeded", ""
		if err := finish(progress); err != nil || saved().Status != "succeeded" {
			t.Fatal("finished response not written", err)
		}
		time.Sleep(liveInterval)
		synctest.Wait()
		if got := saved(); got.Status != "succeeded" {
			t.Fatal("pending progress overwrote the outcome", got)
		}
	})
}
