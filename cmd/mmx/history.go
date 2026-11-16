package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"time"

	"mmx/internal/controller"
)

func cleanupHistory(c controller.Controller, r *controller.Run) {
	policy := r.Project.HistoryRetention
	if r.EntryPoint == controller.RunCommandEntryPoint {
		// A run-command run keeps the retention its project had when it ran.
		if err := c.Prune(r, policy, time.Now()); err != nil {
			fmt.Fprintln(os.Stderr, "mmx: history cleanup:", err)
		}
		return
	}
	current, project, err := selection(r.EntryPoint, r.InventoryName)
	// The settings file of a moved project: its old path may be gone, or
	// exist without being readable by this account. Use the saved policy.
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, fs.ErrPermission) {
		err = nil
	}
	if err == nil && project.SettingsFile != "" && current.Selects(r) {
		policy = project.HistoryRetention
	}
	if err == nil {
		err = c.Prune(r, policy, time.Now())
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "mmx: history cleanup:", err)
	}
}
