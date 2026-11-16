// Package assets stores immutable file contents separately from job instructions.
package assets

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"syscall"
)

var digestPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

// Path is a local binding, never an instruction supplied by the peer.
type Ref struct {
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
	Path   string `json:"-"`
}

func (r Ref) Validate() error {
	if !digestPattern.MatchString(r.SHA256) || r.Size < 0 || r.Size == math.MaxInt64 {
		return fmt.Errorf("invalid asset digest or size")
	}
	return nil
}
func Open(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		f.Close()
		return nil, fmt.Errorf("asset must be a regular file: %s", path)
	}
	return f, nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

// Copy never dispatches a reader's whole-file WriteTo implementation.
func Copy(ctx context.Context, w io.Writer, r io.Reader) (int64, error) {
	return io.CopyBuffer(w, contextReader{ctx, r}, make([]byte, 64*1024))
}
func Hash(ctx context.Context, r io.Reader) (Ref, error) {
	h := sha256.New()
	n, err := Copy(ctx, h, r)
	return Ref{SHA256: hex.EncodeToString(h.Sum(nil)), Size: n}, err
}
func Verify(ctx context.Context, ref Ref) error {
	if err := ref.Validate(); err != nil {
		return err
	}
	f, err := Open(ref.Path)
	if err != nil {
		return err
	}
	defer f.Close()
	actual, err := Hash(ctx, f)
	if err != nil {
		return err
	}
	if actual.SHA256 != ref.SHA256 || actual.Size != ref.Size {
		return fmt.Errorf("asset %s: size or SHA-256 mismatch", ref.SHA256)
	}
	return nil
}

// Snapshot copies and hashes the same stream; only a completed private file is
// published under its content digest. Identical files share one retained copy.
func Snapshot(ctx context.Context, dir, path string) (Ref, error) {
	input, err := Open(path)
	if err != nil {
		return Ref{}, err
	}
	defer input.Close()
	return write(ctx, dir, input, "", nil)
}
func Receive(ctx context.Context, dir string, ref Ref, input io.Reader) error {
	if err := ref.Validate(); err != nil {
		return err
	}
	_, err := write(ctx, dir, io.LimitReader(input, ref.Size+1), ref.SHA256, &ref.Size)
	return err
}
func write(ctx context.Context, dir string, input io.Reader, digest string, size *int64) (Ref, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return Ref{}, err
	}
	f, err := os.CreateTemp(dir, ".upload-")
	if err != nil {
		return Ref{}, err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	h := sha256.New()
	n, err := Copy(ctx, io.MultiWriter(f, h), input)
	ref := Ref{SHA256: hex.EncodeToString(h.Sum(nil)), Size: n}
	if err != nil {
		return Ref{}, err
	}
	if (size != nil && n != *size) || (digest != "" && ref.SHA256 != digest) {
		return Ref{}, fmt.Errorf("asset %s: size or SHA-256 mismatch", digest)
	}
	if err := f.Sync(); err != nil {
		return Ref{}, err
	}
	if err := f.Close(); err != nil {
		return Ref{}, err
	}
	ref.Path = filepath.Join(dir, ref.SHA256)
	// A hard link publishes without replacing an existing asset, even if callers
	// race. Existing bytes must match before they can be reused.
	if err := os.Link(f.Name(), ref.Path); err != nil {
		if !os.IsExist(err) {
			return Ref{}, err
		}
		if err := Verify(ctx, ref); err != nil {
			return Ref{}, err
		}
	}
	return ref, nil
}
