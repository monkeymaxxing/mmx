package controller

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"sync/atomic"
	"time"

	"mmx/internal/assets"
	"mmx/internal/config"
	"mmx/internal/endpoint"
)

type TransferProgress struct {
	Bytes     int64 `json:"bytes"`
	Total     int64 `json:"total"`
	Files     int   `json:"files"`
	Completed int   `json:"completed"`
}

func (c *Connection) upload(ctx context.Context, id string, ref assets.Ref, progress func(int64) error) error {
	input, err := assets.Open(ref.Path)
	if err != nil {
		return err
	}
	defer input.Close()
	// Uploads have no execution side effect. A repeat resends the complete file
	// with the same immutable description.
	return c.call(ctx, true, func(d *endpoint.Client) error {
		return d.Upload(ctx, id, ref, input, progress)
	}, func(s *sshSession) error {
		if _, err := input.Seek(0, io.SeekStart); err != nil {
			return err
		}
		transfer, reader, finish := assets.Transfer(ctx, input, assets.StallTimeout, progress)
		defer finish()
		var result struct {
			Status string `json:"status"`
		}
		err := s.runner(transfer, fmt.Sprintf("upload %s %s %d", quote(id), quote(ref.SHA256), ref.Size), reader, &result)
		if transfer.Err() != nil {
			return context.Cause(transfer)
		}
		if err == nil && result.Status != "uploaded" {
			err = fmt.Errorf("node did not confirm the file upload")
		}
		return err
	})
}
func (c Controller) deliverAssets(ctx context.Context, r *Run, job config.Job, en *Entry, checkpoint func(Entry) error, conn *Connection) error {
	refs, err := config.RequiredAssets(job.NodeTasks, job.Modules)
	if err != nil {
		return err
	}
	// A node keeps module files and files from a web address from earlier
	// runs; send only those it lacks.
	if len(job.KeptFiles) > 0 {
		toSend, err := conn.UseKeptFiles(ctx, job.ID, job.KeptFiles)
		if err != nil {
			return fmt.Errorf("ask node %s which job files are already available: %w; the node job has not started", en.Node, err)
		}
		kept := map[string]bool{}
		for _, ref := range job.KeptFiles {
			kept[ref.SHA256] = !toSend[ref.SHA256]
		}
		refs = slices.DeleteFunc(refs, func(ref assets.Ref) bool { return kept[ref.SHA256] })
	}
	if len(refs) == 0 {
		return nil
	}
	progress := TransferProgress{Files: len(refs)}
	for _, ref := range refs {
		progress.Total += ref.Size
	}
	report := func() error { copy := progress; en.Transfer = &copy; return checkpoint(*en) }
	if err := report(); err != nil {
		return err
	}
	var done int64
	for _, ref := range refs {
		ref.Path = filepath.Join(c.runDir(r.ID), "assets", ref.SHA256)
		uploadContext, cancel := context.WithCancel(ctx)
		var count atomic.Int64
		result := make(chan error, 1)
		go func() {
			result <- conn.upload(uploadContext, job.ID, ref, func(n int64) error { count.Store(n); return nil })
		}()
		ticker := time.NewTicker(time.Second)
		var transferErr error
	wait:
		for {
			select {
			case transferErr = <-result:
				break wait
			case <-ticker.C:
				progress.Bytes = done + count.Load()
				if stop, err := c.stopRequested(r.ID); err != nil {
					transferErr = err
				} else if stop {
					transferErr = context.Canceled
				}
				if transferErr == nil {
					transferErr = report()
				}
				if transferErr != nil {
					cancel()
					//lint:ignore S1005 The existing transfer failure determines the result; explicitly discard the shutdown error for errcheck.
					_ = <-result
					break wait
				}
			}
		}
		ticker.Stop()
		cancel()
		if transferErr != nil {
			return fmt.Errorf("file transfer to node %s, asset %s: %w; the node job has not started", en.Node, ref.SHA256, transferErr)
		}
		done += ref.Size
		progress.Bytes = done
		progress.Completed++
		if err := report(); err != nil {
			return err
		}
	}
	en.Transfer = nil
	en.Transport, en.ConnectionFallbackReason = conn.ConnectionUsed(), conn.FallbackReason
	return checkpoint(*en)
}
