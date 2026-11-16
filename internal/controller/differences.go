package controller

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"time"

	"mmx/internal/assets"
	"mmx/internal/endpoint"
)

func (c Controller) DiffDir(id string) string { return filepath.Join(c.runDir(id), "diffs") }

func (c *Connection) downloadDiff(ctx context.Context, id string, ref assets.Ref, dir string) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return c.call(ctx, true, func(d *endpoint.Client) error {
		return d.DownloadDiff(ctx, id, ref, dir)
	}, func(s *sshSession) error {
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()
		command, input := s.asRoot(fmt.Sprintf("%s diff %s %s %d", quote(s.Runner), quote(id), quote(ref.SHA256), ref.Size), nil)
		reader, writer := io.Pipe()
		done := make(chan error, 1)
		go func() {
			err := s.rawSSHTo(ctx, command, input, writer)
			writer.CloseWithError(err)
			done <- err
		}()
		err := assets.Receive(ctx, dir, ref, reader)
		reader.Close()
		if err != nil {
			cancel()
		}
		remoteErr := <-done
		if err != nil {
			return err
		}
		return remoteErr
	})
}
