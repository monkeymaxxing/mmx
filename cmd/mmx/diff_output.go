package main

import (
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"

	"mmx/internal/assets"
	"mmx/internal/controller"
	"mmx/internal/differences"
	"mmx/internal/protocol"
)

// refs lists the artifacts the run's results name, in their local location.
func (p *presentation) refs(r *controller.Run) ([]assets.Ref, error) {
	var refs []assets.Ref
	seen := map[string]bool{}
	for _, en := range r.OrderedEntries() {
		responses := []protocol.Response{en.Response}
		for _, a := range en.PreviousNodeJobAttempts {
			responses = append(responses, a.Response)
		}
		for _, response := range responses {
			items, err := differences.Refs(response)
			if err != nil {
				return nil, err
			}
			for _, ref := range items {
				if !seen[ref.SHA256] {
					seen[ref.SHA256] = true
					refs = append(refs, differences.Local(filepath.Join(p.runDir(r), "diffs"), ref))
				}
			}
		}
	}
	return refs, nil
}

// verifyDiffs notes artifacts that are not available locally: still running,
// unconfirmed, or never downloaded.
func (p *presentation) verifyDiffs(r *controller.Run) error {
	refs, err := p.refs(r)
	if err != nil {
		return err
	}
	p.unavailable = map[string]bool{}
	for _, ref := range refs {
		f, err := differences.Open(ref)
		if err != nil {
			p.unavailable[ref.SHA256] = true
			continue
		}
		f.Close()
	}
	return nil
}
func (p *presentation) resultDiffs(w io.Writer, r *controller.Run, result protocol.Result, location string) error {
	// A full difference holds lines of files on nodes.
	w = terminalSafe(w)
	var failure error
	// Loop children also have their own flat, contextual task records.
	result.Items = nil
	seen := map[string]bool{}
	differences.Visit(&result, func(change *protocol.FileChange) {
		if failure != nil {
			return
		}
		if change.FullDiff == nil {
			if change.FullDiffOmitted != "" {
				fmt.Fprintf(w, "    %s: %s\n", change.Path, change.FullDiffOmitted)
			}
			return
		}
		ref := change.FullDiff
		key := change.Path + "\x00" + ref.SHA256
		if seen[key] {
			return
		}
		seen[key] = true
		if p.unavailable[ref.SHA256] {
			fmt.Fprintf(w, "    %s / %s: full difference for %q is not available locally\n", location, result.Task, change.Path)
			return
		}
		f, err := differences.Open(differences.Local(filepath.Join(p.runDir(r), "diffs"), *ref))
		if err != nil {
			failure = err
			return
		}
		defer f.Close()
		fmt.Fprintf(w, "    %s / %s: full difference for %q\n", location, result.Task, change.Path)
		failure = copyEscaped(w, io.LimitReader(f, ref.Size))
	})
	return failure
}

// Encode one artifact at a time instead of building a fleet's full differences
// in memory. Ordinary JSON always retains the individual structured records.
func (p *presentation) encodeRun(w io.Writer, view map[string]any, r *controller.Run) error {
	if p.diff != "all" {
		e := json.NewEncoder(w)
		e.SetIndent("", "  ")
		return e.Encode(view)
	}
	b, err := json.Marshal(view)
	if err != nil {
		return err
	}
	if _, err = w.Write(b[:len(b)-1]); err != nil {
		return err
	}
	if _, err = io.WriteString(w, ",\"full_differences\":{"); err != nil {
		return err
	}
	refs, err := p.refs(r)
	if err != nil {
		return err
	}
	var unavailable []string
	written := 0
	for _, ref := range refs {
		if p.unavailable[ref.SHA256] {
			unavailable = append(unavailable, ref.SHA256)
			continue
		}
		f, err := differences.Open(ref)
		if err != nil {
			return err
		}
		data, err := io.ReadAll(io.LimitReader(f, ref.Size))
		f.Close()
		if err != nil {
			return err
		}
		key, _ := json.Marshal(ref.SHA256)
		text, _ := json.Marshal(string(data))
		if written > 0 {
			if _, err = io.WriteString(w, ","); err != nil {
				return err
			}
		}
		written++
		if _, err = fmt.Fprintf(w, "%s:%s", key, text); err != nil {
			return err
		}
	}
	if _, err = io.WriteString(w, "}"); err != nil {
		return err
	}
	if len(unavailable) > 0 {
		b, _ := json.Marshal(unavailable)
		if _, err = fmt.Fprintf(w, ",\"full_differences_unavailable\":%s", b); err != nil {
			return err
		}
	}
	_, err = io.WriteString(w, "}\n")
	return err
}
