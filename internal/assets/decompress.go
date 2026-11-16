package assets

import (
	"bytes"
	"compress/bzip2"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"syscall"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz"
)

// KeepFreePercent is the part of a filesystem that decompression and archive
// extraction leave available, so that a small compressed file cannot fill a
// disk: they refuse output that would leave less.
const KeepFreePercent = 5

// WritableSpace is how many bytes may still be written to the filesystem
// that holds path while KeepFreePercent of it stays available, as the Avail
// column of df counts it. It is never negative.
func WritableSpace(path string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	block := int64(st.Bsize)
	space := int64(st.Bavail)*block - int64(st.Blocks)*block*KeepFreePercent/100
	return max(space, 0), nil
}

// CompressionOf names the compression whose signature starts data: gzip,
// bzip2, xz, or zstd; or "" for none of them.
func CompressionOf(data []byte) string {
	for _, format := range []struct {
		name      string
		signature []byte
	}{
		{"gzip", []byte{0x1f, 0x8b}},
		{"bzip2", []byte("BZh")},
		{"xz", []byte{0xfd, '7', 'z', 'X', 'Z', 0x00}},
		{"zstd", []byte{0x28, 0xb5, 0x2f, 0xfd}},
	} {
		if bytes.HasPrefix(data, format.signature) {
			return format.name
		}
	}
	return ""
}

// Decompressor reads the contents of r, compressed with format: gzip, bzip2,
// xz, or zstd. Every decoder is pure Go, so neither the controller machine
// nor the node needs a compression tool.
func Decompressor(format string, r io.Reader) (io.ReadCloser, error) {
	switch format {
	case "gzip":
		return gzip.NewReader(r)
	case "bzip2":
		return io.NopCloser(bzip2.NewReader(r)), nil
	case "xz":
		x, err := xz.NewReader(r)
		if err != nil {
			return nil, err
		}
		return io.NopCloser(x), nil
	case "zstd":
		// The zstd tool itself refuses a window above 128 MiB unless told
		// otherwise; the same limit bounds the decoder's memory here.
		z, err := zstd.NewReader(r, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxWindow(128<<20))
		if err != nil {
			return nil, err
		}
		return z.IOReadCloser(), nil
	}
	return nil, fmt.Errorf("unsupported compression format %q; use gzip, bzip2, xz, or zstd", format)
}

// DownloadDecompressed fetches raw and checks the file as its project
// publishes it against digest, or saves it unchecked when digest is empty, as
// Download and DownloadUnchecked do. It then saves the decompressed contents
// as the asset; only that file stays in dir.
func DownloadDecompressed(ctx context.Context, dir, raw, digest, format string) (Ref, error) {
	if digest != "" {
		if err := ValidateDownload(raw, digest); err != nil {
			return Ref{}, err
		}
	}
	return downloadDecompressed(ctx, dir, raw, digest, format, http.DefaultTransport, StallTimeout)
}

func downloadDecompressed(ctx context.Context, dir, raw, digest, format string, transport http.RoundTripper, idle time.Duration) (Ref, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return Ref{}, err
	}
	compressedDir, err := os.MkdirTemp(dir, ".compressed-")
	if err != nil {
		return Ref{}, err
	}
	defer os.RemoveAll(compressedDir)
	compressed, err := download(ctx, compressedDir, raw, digest, transport, idle)
	if err != nil {
		return Ref{}, err
	}
	space, err := WritableSpace(dir)
	if err != nil {
		return Ref{}, err
	}
	return decompress(ctx, dir, compressed, format, space)
}

// decompress saves the decompressed contents of the asset compressed as a new
// asset in dir, refusing more than space bytes.
func decompress(ctx context.Context, dir string, compressed Ref, format string, space int64) (Ref, error) {
	file, err := Open(compressed.Path)
	if err != nil {
		return Ref{}, err
	}
	defer file.Close()
	contents, err := Decompressor(format, file)
	if err != nil {
		return Ref{}, fmt.Errorf("decompress %s: %w", format, err)
	}
	defer contents.Close()
	ref, err := write(ctx, dir, &spaceLimit{reader: contents, space: space, path: dir}, "", nil)
	if err != nil {
		return Ref{}, fmt.Errorf("decompress %s: %w", format, err)
	}
	return ref, nil
}

// spaceLimit fails a read that takes the output past space bytes.
type spaceLimit struct {
	reader io.Reader
	space  int64
	path   string
}

func (l *spaceLimit) Read(p []byte) (int, error) {
	n, err := l.reader.Read(p)
	l.space -= int64(n)
	if l.space < 0 {
		return 0, fmt.Errorf("cannot save the decompressed file at %s while keeping %d%% of the filesystem free", l.path, KeepFreePercent)
	}
	return n, err
}
