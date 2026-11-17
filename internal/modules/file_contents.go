package modules

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"mmx/internal/assets"
	"mmx/internal/value"
)

// FileContents keeps literal files on disk. Templates are parsed/rendered on the
// node using the task's scope; their output never becomes a retained asset.
type FileContents struct {
	io.ReadSeeker
	Size     int64
	expected string
	close    func() error
}

func (c *FileContents) Close() error {
	if c.close != nil {
		return c.close()
	}
	return nil
}
func (c *FileContents) Digest(ctx context.Context) (string, error) {
	ref, err := assets.Hash(ctx, c)
	if err != nil {
		return "", err
	}
	if ref.Size != c.Size || (c.expected != "" && c.expected != ref.SHA256) {
		return "", fmt.Errorf("file contents changed in size or failed SHA-256 verification")
	}
	_, err = c.Seek(0, io.SeekStart)
	return ref.SHA256, err
}
func OpenFileContents(f File, s value.Scope) (*FileContents, error) {
	if len(f.ContentFromNodePaths) > 0 {
		return openNodePaths(f.ContentFromNodePaths)
	}
	if f.Source != "" || f.URL != "" {
		if f.Asset == nil {
			return nil, fmt.Errorf("file source has no saved copy of the source contents")
		}
		if err := f.Asset.Validate(); err != nil {
			return nil, err
		}
		file, err := assets.Open(f.Asset.Path)
		if err != nil {
			return nil, err
		}
		return &FileContents{ReadSeeker: file, Size: f.Asset.Size, expected: f.Asset.SHA256, close: file.Close}, nil
	}
	var text string
	var err error
	if f.Literal != nil {
		text = *f.Literal
	} else if f.Content != nil {
		text, err = s.Text(*f.Content)
	} else {
		text, err = s.Template(f.TemplateText)
	}
	if err != nil {
		return nil, err
	}
	return &FileContents{ReadSeeker: strings.NewReader(text), Size: int64(len(text))}, nil
}

// openNodePaths opens files on the node, following links that root owns
// (openNodeFile), as one content: their bytes in order. Each path must resolve to a regular file; a missing one
// returns an error for which errors.Is(err, os.ErrNotExist) holds.
func openNodePaths(paths []string) (*FileContents, error) {
	var parts nodeFiles
	closeAll := func() error {
		var err error
		for _, file := range parts.files {
			err = errors.Join(err, file.Close())
		}
		return err
	}
	for _, path := range paths {
		file, err := openNodeFile(path)
		if err != nil {
			// These are read-only input files, so closing them cannot lose managed writes.
			_ = closeAll()
			return nil, fmt.Errorf("content_from_node_paths: %w", err)
		}
		info, err := file.Stat()
		if err == nil && !info.Mode().IsRegular() {
			err = fmt.Errorf("content_from_node_paths: %s is not a regular file", path)
		}
		if err != nil {
			file.Close()
			// These are read-only input files, so closing them cannot lose managed writes.
			_ = closeAll()
			return nil, err
		}
		parts.files, parts.sizes = append(parts.files, file), append(parts.sizes, info.Size()) //nolint:nilaway // The preceding error branch returns on failed Stat; successful Stat supplies non-nil file information.
		//nolint:nilaway // The preceding error branch excludes failed Stat results.
		parts.size += info.Size()
	}
	return &FileContents{ReadSeeker: io.NewSectionReader(parts, 0, parts.size), Size: parts.size, close: closeAll}, nil
}

// nodeFiles reads open files as one sequence of bytes, each up to the size it
// had when it was opened.
type nodeFiles struct {
	files []*os.File
	sizes []int64
	size  int64
}

func (c nodeFiles) ReadAt(p []byte, off int64) (int, error) {
	n := 0
	for i, file := range c.files {
		if off >= c.sizes[i] {
			off -= c.sizes[i]
			continue
		}
		want := int(min(int64(len(p)-n), c.sizes[i]-off))
		m, err := file.ReadAt(p[n:n+want], off)
		n += m
		if m < want {
			if err == nil || err == io.EOF {
				err = fmt.Errorf("%s changed while MMX read it", file.Name())
			}
			return n, err
		}
		if n == len(p) {
			return n, nil
		}
		off = 0
	}
	return n, io.EOF
}
