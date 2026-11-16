// Package differences owns bounded diagnostic artifacts, separate from inputs.
package differences

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"mmx/internal/assets"
	"mmx/internal/protocol"
)

const InputLimit = 1024 * 1024
const Budget = 8 * 1024 * 1024

func Visit(r *protocol.Result, visit func(*protocol.FileChange)) {
	if r.FileChange != nil {
		visit(r.FileChange)
	}
	if r.DirectoryChange != nil {
		for i := range r.DirectoryChange.Examples {
			visit(&r.DirectoryChange.Examples[i])
		}
	}
	for i := range r.Tries {
		Visit(&r.Tries[i], visit)
	}
	for i := range r.Items {
		Visit(&r.Items[i], visit)
	}
	if r.CheckNewFile != nil {
		Visit(r.CheckNewFile, visit)
	}
}

func Refs(response protocol.Response) ([]assets.Ref, error) {
	seen := map[string]int64{}
	var refs []assets.Ref
	var total int64
	var failure error
	for _, r := range response.Results {
		r = r.Safe()
		Visit(&r, func(c *protocol.FileChange) {
			if c.FullDiff == nil || failure != nil {
				return
			}
			ref := *c.FullDiff
			if err := ref.Validate(); err != nil {
				failure = err
				return
			}
			if size, ok := seen[ref.SHA256]; ok {
				if size != ref.Size {
					failure = fmt.Errorf("full differences with the same SHA-256 hash report different sizes")
				}
				return
			}
			total += ref.Size
			if ref.Size > Budget || total > Budget {
				failure = fmt.Errorf("full differences exceed the 8 MiB limit for one node job")
				return
			}
			seen[ref.SHA256] = ref.Size
			refs = append(refs, ref)
		})
	}
	return refs, failure
}

// A task stages at most the job's remaining budget in memory. Nothing reaches
// disk until all its predicates, validators and retries have finished using
// secrets. The store is used by one sequential node worker.
type Store struct {
	Dir      string
	used     int
	retained map[string]bool
}
type Capture struct {
	store   *Store
	pending map[string]string
	size    int
}

func (s *Store) Task() *Capture { return &Capture{store: s, pending: map[string]string{}} }
func (c *Capture) Add(text string) (*assets.Ref, string) {
	if c.store.Dir == "" {
		return nil, "full difference storage unavailable"
	}
	ref := assets.Ref{SHA256: fmt.Sprintf("%x", sha256.Sum256([]byte(text))), Size: int64(len(text))}
	if c.store.retained[ref.SHA256] {
		return &ref, ""
	}
	if _, ok := c.pending[ref.SHA256]; ok {
		return &ref, ""
	}
	if c.store.used+c.size+len(text) > Budget {
		return nil, "full difference omitted: adding the difference would exceed the 8 MiB limit for one node job's full differences"
	}
	c.pending[ref.SHA256] = text
	c.size += len(text)
	return &ref, ""
}
func (c *Capture) Finish(result protocol.Result) protocol.Result {
	if result.Secret {
		Visit(&result, func(change *protocol.FileChange) {
			change.FullDiff = nil
			change.FullDiffOmitted = "contents suppressed for a task using secrets"
		})
		return result
	}
	if c.store.retained == nil {
		c.store.retained = map[string]bool{}
	}
	Visit(&result, func(change *protocol.FileChange) {
		ref := change.FullDiff
		if ref == nil || c.store.retained[ref.SHA256] {
			return
		}
		text, ok := c.pending[ref.SHA256]
		if !ok {
			return
		}
		if err := assets.Receive(context.Background(), c.store.Dir, *ref, strings.NewReader(text)); err != nil {
			change.FullDiff = nil
			change.FullDiffOmitted = "full difference could not be saved: " + err.Error()
			return
		}
		c.store.retained[ref.SHA256] = true
		c.store.used += len(text)
	})
	return result
}

func Local(dir string, ref assets.Ref) assets.Ref {
	ref.Path = filepath.Join(dir, ref.SHA256)
	return ref
}

// Verify the bounded immutable file before returning a descriptor for display.
func Open(ref assets.Ref) (*os.File, error) {
	if ref.Validate() != nil || ref.Size > Budget {
		return nil, fmt.Errorf("invalid full difference reference")
	}
	f, err := assets.Open(ref.Path)
	if err != nil {
		return nil, err
	}
	actual, err := assets.Hash(context.Background(), io.LimitReader(f, ref.Size+1))
	if err == nil && (actual.SHA256 != ref.SHA256 || actual.Size != ref.Size) {
		err = fmt.Errorf("full difference size or SHA-256 mismatch")
	}
	if err == nil {
		_, err = f.Seek(0, io.SeekStart)
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}
