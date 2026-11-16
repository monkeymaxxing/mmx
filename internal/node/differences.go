package node

import (
	"fmt"
	"os"
	"path/filepath"

	"mmx/internal/assets"
	"mmx/internal/differences"
)

// Open while holding admission so expiry cannot remove the directory midway.
// Once open, the immutable file can be streamed even if another request expires it.
func (s Server) OpenDiff(id string, ref assets.Ref) (*os.File, error) {
	if !validID.MatchString(id) || ref.Validate() != nil || ref.Size > differences.Budget {
		return nil, fmt.Errorf("invalid full difference reference")
	}
	if err := s.Initialize(); err != nil {
		return nil, err
	}
	l, err := waitLock(filepath.Join(s.Root, "admission.lock"))
	if err != nil {
		return nil, err
	}
	defer unlock(l)
	f, err := assets.Open(filepath.Join(s.jobDir(id), "diffs", ref.SHA256))
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil || info.Size() != ref.Size {
		f.Close()
		return nil, fmt.Errorf("saved full difference size does not match the recorded size")
	}
	return f, nil
}
