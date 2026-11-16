package assets

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

func TestReceiveVerifiesBeforePublicationAndRetransmits(t *testing.T) {
	data := []byte("exact bytes\x00\xff")
	ref, _ := Hash(context.Background(), bytes.NewReader(data))
	for _, body := range [][]byte{data[:3], append(append([]byte{}, data...), 0), bytes.Repeat([]byte("x"), len(data))} {
		dir := t.TempDir()
		if err := Receive(context.Background(), dir, ref, bytes.NewReader(body)); err == nil {
			t.Fatal("accepted invalid upload")
		}
		files, _ := os.ReadDir(dir)
		if len(files) != 0 {
			t.Fatal("left incomplete asset", files)
		}
		if err := Receive(context.Background(), dir, ref, bytes.NewReader(data)); err != nil {
			t.Fatal(err)
		}
		if err := Receive(context.Background(), dir, ref, bytes.NewReader(data)); err != nil {
			t.Fatal("whole retransmission", err)
		}
		files, _ = os.ReadDir(dir)
		if len(files) != 1 {
			t.Fatal(files)
		}
		ref.Path = filepath.Join(dir, ref.SHA256)
		if err := Verify(context.Background(), ref); err != nil {
			t.Fatal(err)
		}
	}
}
func TestAssetsAreRegularPrivateFilesAndDeduplicated(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "source")
	os.WriteFile(source, []byte("frozen"), 0644)
	saved := filepath.Join(dir, "assets")
	a, err := Snapshot(context.Background(), saved, source)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Snapshot(context.Background(), saved, source)
	if err != nil || a != b {
		t.Fatal(a, b, err)
	}
	os.WriteFile(source, []byte("edited"), 0644)
	if err := Verify(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(a.Path)
	if info.Mode().Perm() != 0600 {
		t.Fatal(info.Mode())
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(source, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Snapshot(context.Background(), saved, link); err == nil {
		t.Fatal("accepted symlink")
	}
	if _, err := Snapshot(context.Background(), saved, dir); err == nil {
		t.Fatal("accepted directory")
	}
	for _, ref := range []Ref{{SHA256: "../escape"}, {SHA256: a.SHA256, Size: -1}} {
		if err := Receive(context.Background(), saved, ref, strings.NewReader("")); err == nil {
			t.Fatal("accepted invalid reference")
		}
	}
}

type boundedReader struct {
	left int64
	max  int
}

func (r *boundedReader) Read(p []byte) (int, error) {
	if len(p) > r.max {
		return 0, errors.New("unbounded read")
	}
	if r.left == 0 {
		return 0, io.EOF
	}
	n := min(int64(len(p)), r.left)
	clear(p[:n])
	r.left -= n
	return int(n), nil
}
func TestStreamingBuffersAndCancellation(t *testing.T) {
	reader := &boundedReader{left: 64 * 1024 * 1024, max: 64 * 1024}
	if n, err := Copy(context.Background(), io.Discard, reader); err != nil || n != 64*1024*1024 {
		t.Fatal(n, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	dir := t.TempDir()
	ref, _ := Hash(context.Background(), strings.NewReader("bytes"))
	if err := Receive(ctx, dir, ref, strings.NewReader("bytes")); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	files, _ := os.ReadDir(dir)
	if len(files) != 0 {
		t.Fatal(files)
	}
}
func TestTransferProgressExtendsIdleDeadlineAndStallsCancel(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, reader, finish := Transfer(context.Background(), strings.NewReader(strings.Repeat("a", 10)), 100*time.Millisecond, nil)
		defer finish()
		for range 5 {
			time.Sleep(30 * time.Millisecond)
			if _, err := reader.Read(make([]byte, 1)); err != nil {
				t.Fatal(err)
			}
			synctest.Wait()
			if ctx.Err() != nil {
				t.Fatal("progressing transfer expired")
			}
		}
		time.Sleep(99 * time.Millisecond)
		synctest.Wait()
		if ctx.Err() != nil {
			t.Fatal("transfer expired before its renewed idle deadline")
		}
		time.Sleep(time.Millisecond)
		synctest.Wait()
		if ctx.Err() == nil || !strings.Contains(context.Cause(ctx).Error(), "stalled") {
			t.Fatal("stall did not cancel", context.Cause(ctx))
		}
	})
}

type interruptedReader struct{ sent bool }

func (r *interruptedReader) Read(p []byte) (int, error) {
	if r.sent {
		return 0, io.ErrUnexpectedEOF
	}
	r.sent = true
	return copy(p, "partial"), nil
}
func TestInterruptedUploadRemovesPartialFile(t *testing.T) {
	dir := t.TempDir()
	ref, _ := Hash(context.Background(), strings.NewReader("partial plus rest"))
	if err := Receive(context.Background(), dir, ref, &interruptedReader{}); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatal(err)
	}
	files, _ := os.ReadDir(dir)
	if len(files) != 0 {
		t.Fatal(files)
	}
}
