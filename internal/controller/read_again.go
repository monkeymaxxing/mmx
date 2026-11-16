package controller

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"mmx/internal/config"
	"mmx/internal/protocol"
	"mmx/internal/value"
)

// readAgainLimit bounds connecting to a node and reading one file there.
const readAgainLimit = 5 * time.Minute

// nodeFileReads holds what a resume read again for read_file secrets, by
// origin. readSecretsAgain fills it before the walk starts; the walk and its
// node jobs only read it.
type nodeFileReads struct {
	reads map[value.NodeOrigin]nodeFileRead
}

type nodeFileRead struct {
	content any
	err     error
}

// readAgain gives the content read again for a read_file secret. An
// unreachable or replaced node, a missing file, or a stop made the value
// unavailable: the work that reads it is blocked with that reason.
func (w *fleetStageWalker) readAgain(origin value.NodeOrigin) (any, error) {
	read, ok := w.nodeFiles.reads[origin]
	if !ok {
		read.err = fmt.Errorf("the read_file secret was not read again before the resumed node tasks started")
	}
	if read.err != nil {
		return nil, &value.UnavailableError{
			Path:   fmt.Sprintf("secret from read_file task %s of fleet stage %s", origin.NodeTask, origin.FleetStage),
			Reason: fmt.Sprintf("mmx resume could not read %s on node %s again: %v", origin.ReadFilePath, origin.Node, read.err),
		}
	}
	return read.content, nil
}

// readSecretsAgain reads the file of each read_file secret that the saved
// run holds only by reference and that work still unfinished in this resume
// reads (secretsStillNeeded). Saved history keeps only the node and the path,
// so a resume connects to that node and reads the file again, before it
// starts any work, outside the scheduler's lock, all at once: one slow node
// holds back nothing else, and a secret nothing needs any more contacts no
// node. Each node must still be the machine the read_file task ran on.
// Ctrl-C (ctx) and mmx stop end the reads; their secrets stay unavailable for
// this attempt.
func (c Controller) readSecretsAgain(ctx context.Context, r *Run) *nodeFileReads {
	origins := secretsStillNeeded(r)
	// The machine each node job ran on, by fleet stage path and node: the
	// fleet stage of a loop iteration too.
	machines := map[[2]string]machine{}
	for _, en := range r.Entries {
		machines[[2]string{en.FleetStage, en.Node}] = machine{en.NodeID, en.Architecture}
	}
	reads := &nodeFileReads{reads: map[value.NodeOrigin]nodeFileRead{}}
	if len(origins) == 0 {
		return reads
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	watched := make(chan struct{})
	defer close(watched)
	go func() {
		ticker := time.NewTicker(200 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-watched:
				return
			case <-ticker.C:
				if stop, _ := c.stopRequested(r.ID); stop {
					cancel()
					return
				}
			}
		}
	}()
	var mu sync.Mutex
	var wg sync.WaitGroup
	for origin := range origins {
		wg.Add(1)
		go func() {
			defer wg.Done()
			content, err := c.readNodeFile(ctx, r, origin, machines[[2]string{origin.FleetStage, origin.Node}])
			if err != nil && ctx.Err() != nil {
				err = fmt.Errorf("stop requested before the file was read")
			}
			mu.Lock()
			reads.reads[origin] = nodeFileRead{content, err}
			mu.Unlock()
		}()
	}
	wg.Wait()
	return reads
}

func (c Controller) readNodeFile(ctx context.Context, r *Run, origin value.NodeOrigin, recorded machine) (any, error) {
	managedNode, ok := r.Inventory.Nodes[origin.Node]
	if !ok {
		return nil, fmt.Errorf("the saved inventory has no node %s", origin.Node)
	}
	ctx, cancel := context.WithTimeout(ctx, readAgainLimit)
	defer cancel()
	conn, err := c.connect(ctx, r, managedNode, "", recorded)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	return conn.ReadFile(ctx, origin.ReadFilePath)
}

// preparationFailure is the status of a node job that could not be prepared:
// blocked when a value it needs is unavailable, such as a read_file secret
// its node could not read again; failed otherwise.
func preparationFailure(err error) string {
	var unavailable *value.UnavailableError
	if errors.As(err, &unavailable) {
		return "blocked"
	}
	return "failed"
}

// secretsStillNeeded lists the saved read_file secrets that work still
// unfinished in this resume reads. It follows the walk the resume will make:
// a fleet stage with an entry that is not completed, or with no selection
// yet, reads everything its definition references; a loop iteration that is
// not completed reads its item, its enclosed fleet stages' references, and
// the loop's when and outputs; a completed fleet stage whose
// save_on_controller_machine files are still missing or failed reads the
// outputs those files need. A reference .outputs.<fleet stage>.<node>.<name>
// reaches only that node's output when the selectors are fixed, and the whole
// collection otherwise. Uncertain entries count as unfinished, and a
// condition that may skip a node task is not evaluated: such a secret is
// read.
func secretsStillNeeded(r *Run) map[value.NodeOrigin]bool {
	needed := map[value.NodeOrigin]bool{}
	var collect func(any)
	collect = func(v any) {
		switch x := v.(type) {
		case *value.Secret:
			if !x.Available && x.FromNode != nil && x.FromNode.ReadFilePath != "" {
				needed[*x.FromNode] = true
			}
		case map[string]any:
			for _, child := range x {
				collect(child)
			}
		case []any:
			for _, child := range x {
				collect(child)
			}
		}
	}
	// produced collects what a fleet stage published: on node key, or for a
	// loop on iteration key, its output name; "" means every one.
	produced := func(path, key, name string) {
		sel := r.Selections[path]
		if sel == nil || !sel.Resolved {
			return
		}
		pick := func(outputs map[string]any) {
			if name == "" {
				collect(outputs)
			} else {
				collect(outputs[name])
			}
		}
		if sel.Loop {
			for _, item := range sel.Items {
				if key == "" || item.Key == key {
					pick(sel.Iterations[item.Key].Outputs)
				}
			}
			return
		}
		for _, j := range r.selectionEntries(path) {
			if key == "" || r.Entries[j].Node == key {
				pick(r.Entries[j].Response.Outputs)
			}
		}
	}
	// reads collects what the references in input reach; paths maps each
	// fleet stage ID visible there to its path in the run.
	var readsPath func(string, map[string]string)
	readsPath = func(ref string, paths map[string]string) {
		parts, err := value.Path(ref)
		if err != nil {
			return
		}
		for _, part := range parts {
			if part.Dynamic != "" {
				readsPath(part.Dynamic, paths)
			}
		}
		if len(parts) < 2 || parts[0].Name != "outputs" {
			return
		}
		producer, ok := paths[parts[1].Name]
		if !ok {
			return
		}
		fixed := func(i int) string {
			if i < len(parts) && parts[i].Dynamic == "" && parts[i].Index == nil {
				return parts[i].Name
			}
			return ""
		}
		key, name := fixed(2), ""
		if key != "" {
			name = fixed(3)
		}
		produced(producer, key, name)
	}
	reads := func(input any, paths map[string]string) {
		for _, ref := range config.References(input) {
			readsPath(ref, paths)
		}
	}
	var walk func([]config.FleetStage, string, map[string]string) map[string]string
	walk = func(fleetStages []config.FleetStage, prefix string, parent map[string]string) map[string]string {
		paths := map[string]string{}
		for id, path := range parent {
			paths[id] = path
		}
		for _, s := range fleetStages {
			path := prefix + s.ID
			if prefix == "" && !r.Project.KeepsFleetStage(s.ID) {
				continue
			}
			sel := r.Selections[path]
			switch {
			case sel == nil || !sel.Resolved:
				// Not selected yet: all of it may run.
				reads(s, paths)
			case sel.Loop:
				for _, item := range sel.Items {
					if response, ok := sel.Iterations[item.Key]; ok && protocol.Completed(response.Status) {
						continue
					}
					collect(item.Value)
					inner := walk(s.FleetStages, path+"["+value.EscapeKey(item.Key)+"]/", paths)
					reads([]any{s.When, s.Outputs}, inner)
				}
			default:
				unfinished := false
				entries := r.selectionEntries(path)
				for _, j := range entries {
					unfinished = unfinished || !protocol.Completed(r.Entries[j].Response.Status)
				}
				if unfinished {
					reads(s, paths)
				}
				// Files still to write read this fleet stage's own outputs.
				for i, entry := range s.SaveOnControllerMachine {
					if entry.Output == "" {
						if !controllerMachineFileKept(sel, i, "") {
							reads(entry, paths)
							produced(path, "", "")
						}
						continue
					}
					for _, j := range entries {
						if node := r.Entries[j].Node; r.Entries[j].Response.Status == "succeeded" && !controllerMachineFileKept(sel, i, node) {
							reads(entry.Path, paths)
							produced(path, node, entry.Output)
						}
					}
				}
			}
			paths[s.ID] = path
		}
		return paths
	}
	walk(r.Project.FleetStages, "", map[string]string{})
	return needed
}
