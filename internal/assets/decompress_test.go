package assets

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz"
)

// restic is the text of the bzip2 fixture below, which Python's bz2 module
// compressed: Go has no bzip2 writer.
var restic = strings.Repeat("#!/bin/sh\necho restic\n", 100)

const bzipped = "QlpoOTFBWSZTWQCEeLAAAPnRgAAQaACaYZwAIACQKYAAClVGj1PRo1MEWSLRFgi+EWCLRFki0RaIvpF2ReEWiLZFsi/EXhFsi6IuiLJF/F3JFOFCQAIR4sA="

func compressed(t *testing.T, format string) []byte {
	t.Helper()
	var b bytes.Buffer
	var w io.WriteCloser
	var err error
	switch format {
	case "bzip2":
		data, err := base64.StdEncoding.DecodeString(bzipped)
		if err != nil {
			t.Fatal(err)
		}
		return data
	case "gzip":
		w = gzip.NewWriter(&b)
	case "xz":
		w, err = xz.NewWriter(&b)
	case "zstd":
		w, err = zstd.NewWriter(&b)
	}
	if err == nil {
		_, err = io.WriteString(w, restic)
	}
	if err == nil {
		err = w.Close()
	}
	if err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestDecompressorReadsEveryFormatAndRecognizesIt(t *testing.T) {
	for _, format := range []string{"gzip", "bzip2", "xz", "zstd"} {
		data := compressed(t, format)
		if got := CompressionOf(data); got != format {
			t.Fatalf("CompressionOf %s data: %q", format, got)
		}
		r, err := Decompressor(format, bytes.NewReader(data))
		if err != nil {
			t.Fatal(format, err)
		}
		got, err := io.ReadAll(r)
		r.Close()
		if err != nil || string(got) != restic {
			t.Fatal(format, len(got), err)
		}
		// The wrong format is an error, never other bytes.
		other := "gzip"
		if format == "gzip" {
			other = "zstd"
		}
		if r, err := Decompressor(other, bytes.NewReader(data)); err == nil {
			if _, err := io.ReadAll(r); err == nil {
				t.Fatal(format, "read as", other)
			}
		}
	}
	if got := CompressionOf([]byte("#!/bin/sh\n")); got != "" {
		t.Fatal(got)
	}
	if _, err := Decompressor("lz4", strings.NewReader("")); err == nil {
		t.Fatal("accepted an unknown compression")
	}
}

// The controller machine checks the file as published, decompresses it, and
// keeps only the decompressed file as the asset.
func TestDownloadDecompressedChecksThePublishedFile(t *testing.T) {
	published := compressed(t, "xz")
	publishedDigest, _ := Hash(context.Background(), bytes.NewReader(published))
	installed, _ := Hash(context.Background(), strings.NewReader(restic))
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(published)
	}))
	defer server.Close()
	get := func(dir, digest, format string) (Ref, error) {
		return downloadDecompressed(context.Background(), dir, server.URL+"/restic.xz", digest, format, server.Client().Transport, StallTimeout)
	}
	for _, digest := range []string{publishedDigest.SHA256, ""} {
		dir := t.TempDir()
		ref, err := get(dir, digest, "xz")
		if err != nil || ref.SHA256 != installed.SHA256 || ref.Size != int64(len(restic)) || Verify(context.Background(), ref) != nil {
			t.Fatal(ref, err)
		}
		if found, _ := os.ReadDir(dir); len(found) != 1 || found[0].Name() != installed.SHA256 {
			t.Fatal("kept more than the decompressed file", found)
		}
	}
	for _, tc := range []struct{ digest, format, message string }{
		{installed.SHA256, "xz", "SHA-256 mismatch"},
		{publishedDigest.SHA256, "gzip", "decompress gzip"},
	} {
		dir := t.TempDir()
		if _, err := get(dir, tc.digest, tc.format); err == nil || !strings.Contains(err.Error(), tc.message) {
			t.Fatal(tc, err)
		}
		if found, _ := os.ReadDir(dir); len(found) != 0 {
			t.Fatal("left files after a failure", found)
		}
	}
}

// A decompressed file may not take the filesystem past the part that stays
// free, so a small compressed file cannot fill a disk.
func TestDecompressRefusesMoreThanTheSpaceLeft(t *testing.T) {
	source := t.TempDir() + "/restic.gz"
	if err := os.WriteFile(source, compressed(t, "gzip"), 0600); err != nil {
		t.Fatal(err)
	}
	ref, err := Snapshot(context.Background(), t.TempDir(), source)
	if err != nil {
		t.Fatal(err)
	}
	for space, fits := range map[int64]bool{int64(len(restic)): true, int64(len(restic)) - 1: false} {
		dir := t.TempDir()
		_, err := decompress(context.Background(), dir, ref, "gzip", space)
		if fits != (err == nil) || (!fits && !strings.Contains(err.Error(), "cannot save the decompressed file at "+dir+" while keeping 5% of the filesystem free")) {
			t.Fatal(space, err)
		}
		if found, _ := os.ReadDir(dir); !fits && len(found) != 0 {
			t.Fatal("published a partial file", found)
		}
	}
	space, err := WritableSpace(t.TempDir())
	if err != nil || space < 0 {
		t.Fatal(space, err)
	}
}

// TestDecompressorRefusesCraftedZstdHuffmanWeights reads a downloaded file
// whose zstd blocks give Huffman weights an FSE table of accuracy 15
// (GO-2026-6865). The second block's single count overflowed the decoder's
// int16 and decoded with the first block's table, which panicked on the
// decoder's own goroutine, where no caller can recover; the run on the node
// then ended. The input is upstream's regression input for the advisory.
func TestDecompressorRefusesCraftedZstdHuffmanWeights(t *testing.T) {
	oneFrame := slices.Concat(
		[]byte{0x28, 0xb5, 0x2f, 0xfd, 0x00, 0x38, 0x74, 0x06, 0x00, 0x42, 0xb8, 0x32, 0x0d, 0x1a, 0x00, 0x00, 0x00, 0x90, 0xaf, 0xfa, 0xff, 0x03, 0x00, 0x80, 0xff, 0x7f},
		bytes.Repeat([]byte{0xa5, 0x94, 0x52, 0x4a, 0x29}, 37),
		[]byte{0xa5, 0x94, 0x12, 0x00, 0xad, 0x56, 0x03, 0xee, 0xff, 0xdf, 0xb3, 0x1a, 0x1c, 0x1a, 0x00, 0xe0, 0xff, 0x1f, 0x0f, 0xb1, 0x9c, 0x5a, 0xcf, 0x4d, 0xe4, 0x1d, 0x7b, 0xab, 0x99, 0x5e, 0x01, 0x51, 0xa4, 0x73, 0xd8, 0xb1, 0xf3, 0x39, 0x4b, 0x67, 0xf6, 0xab, 0x1a, 0xab, 0x1a, 0xab, 0x1a},
		bytes.Repeat([]byte{0x8c, 0x31, 0xc6, 0x18, 0x63}, 1365),
		[]byte{0x8c, 0x31},
		bytes.Repeat([]byte{0x63, 0x8c, 0x31, 0xc6, 0x18}, 1365),
		[]byte{0x63, 0x2c},
		bytes.Repeat([]byte{0x31, 0xc6, 0x18, 0x63, 0x8c}, 1365),
		[]byte{0x31, 0x46},
		bytes.Repeat([]byte{0x31, 0xc6, 0x18, 0x63, 0x8c}, 1365),
		[]byte{0x31, 0x06, 0x00},
	)
	// The same blocks, the first marked last, each in its own frame.
	twoFrames := slices.Concat(oneFrame[:6], []byte{0x75, 0x06, 0x00}, oneFrame[9:215], oneFrame[:6], oneFrame[215:])
	for name, in := range map[string][]byte{"one frame": oneFrame, "two frames": twoFrames} {
		r, err := Decompressor("zstd", bytes.NewReader(in))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.ReadAll(r); err == nil {
			t.Errorf("%s: want a read error", name)
		}
		r.Close()
	}
}
