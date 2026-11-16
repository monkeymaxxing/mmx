package node

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"mmx/internal/durable"
	"mmx/internal/protocol"
)

func (s Server) timeNow() time.Time {
	if s.now != nil {
		return s.now().UTC()
	}
	return time.Now().UTC()
}

// finish is the one way an admitted job ends: its worker's outcome, a failed
// start, or an interrupted worker found by status. It records the terminal
// result, then removes what only a running job needs: its private input and
// execution assets. A later restore outcome can refresh the stored result; a
// rebooting result changes when its reboot is refused or completes.
// Volatile secret exports and housekeeping are
// added only while answering a request.
func (s Server) finish(r protocol.Response) (protocol.Response, error) {
	var err error
	r, err = s.attachOwedRestores(r, false)
	if err != nil {
		return r, err
	}
	now := s.timeNow()
	r.Finished = &now
	r.Housekeeping = nil
	if err := durable.JSON(filepath.Join(s.jobDir(r.ID), "result.json"), r); err != nil {
		return r, err
	}
	if err := s.removePrivate(r.ID, "input"); err != nil {
		r.Housekeeping = append(r.Housekeeping, "removing private job input: "+err.Error())
	}
	if err := os.RemoveAll(s.assetDir(r.ID)); err != nil {
		r.Housekeeping = append(r.Housekeeping, "removing job assets: "+err.Error())
	}
	saved, err := s.result(r.ID)
	saved.Housekeeping = append(saved.Housekeeping, r.Housekeeping...)
	return saved, err
}

// A controller hears that a job reboots from a progress or status answer, and
// a node that reboots first can end the connection that would have carried
// it. So the worker waits for an answer saying rebooting to go out, then
// rebootAnnounceGrace for it to arrive, and reboots; without a controller it
// reboots after rebootAnnounceWait.
const (
	rebootAnnounceWait  = 5 * time.Second
	rebootAnnounceGrace = time.Second
)

func (s Server) awaitRebootAnnounced(id string) {
	wait, grace := s.announceWait, s.announceGrace
	if wait == 0 {
		wait, grace = rebootAnnounceWait, rebootAnnounceGrace
	}
	mark := filepath.Join(s.jobDir(id), "reboot-announced")
	for deadline := time.Now().Add(wait); time.Now().Before(deadline); time.Sleep(progressPoll) {
		if _, err := os.Stat(mark); err == nil {
			time.Sleep(grace)
			return
		}
	}
}

// requestReboot is a rebooting job's last act, once its result is durable.
// A refused request fails the reboot task, its last result.
func (s Server) requestReboot(r protocol.Response) error {
	request := s.reboot
	if request == nil {
		request = systemReboot
	}
	s.awaitRebootAnnounced(r.ID)
	err := request()
	if err == nil {
		return nil
	}
	task := &r.Results[len(r.Results)-1]
	task.Status, task.Change, task.Reason, task.Value = "failed", "unchanged", err.Error(), nil
	r.Status, r.Error, r.RebootFrom, r.Change = "failed", task.Task+": "+task.Reason, "", "unchanged"
	for _, result := range r.Results {
		r.Change = protocol.CombineChange(r.Change, result.Change)
	}
	_, err = s.finish(r)
	return err
}

func systemReboot() error {
	if out, err := exec.Command("systemctl", "reboot").CombinedOutput(); err != nil {
		return fmt.Errorf("systemctl reboot: %v: %s", err, bytes.TrimSpace(out))
	}
	return nil
}

// rebooted answers for a job that requested a reboot: rebooting while its node
// runs on the boot it left, and once the node runs on another boot, the reboot
// task's success, recorded through finish.
func (s Server) rebooted(r protocol.Response) (protocol.Response, error) {
	if r.Status != "rebooting" {
		return r, nil
	}
	boot, err := s.machine.CurrentBootID()
	if err != nil || boot == r.RebootFrom {
		return r, err
	}
	task := &r.Results[len(r.Results)-1]
	task.Status, task.Reason = "succeeded", ""
	task.Value = map[string]any{"boot_id_before": r.RebootFrom, "boot_id_after": boot}
	r.Status, r.RebootFrom = "succeeded", ""
	return s.finish(r)
}
func (s Server) result(id string) (protocol.Response, error) {
	r, err := s.storedResult(id)
	if err != nil {
		return r, err
	}
	return s.privateOutputs(r)
}
func (s Server) storedResult(id string) (protocol.Response, error) {
	var r protocol.Response
	b, err := os.ReadFile(filepath.Join(s.jobDir(id), "result.json"))
	if err != nil {
		return r, err
	}
	if err := protocol.Decode(b, &r); err != nil {
		return r, err
	}
	// A rebooting job has ended on the node; its reboot is its last result.
	rebooting := r.Status == "rebooting" && r.RebootFrom != "" && len(r.Results) > 0
	if r.ID != id || !(protocol.Terminal(r.Status) || rebooting) || r.Finished == nil {
		return r, fmt.Errorf("invalid terminal result for job %s", id)
	}
	return r, nil
}

// Called under admission.lock, which status/stop readers also hold. It defers
// while the active job runs and never takes execution.lock. Admission calls
// it first, so the private task directories that a killed worker left, with
// the logins in them, are gone before the next job starts, and a status or
// stop request removes them sooner.
func (s Server) sweepHistory(preserve string) []string {
	if id, err := s.activeJob(); err != nil {
		return []string{"node history cleanup: " + err.Error()}
	} else if id != "" {
		return nil
	}
	var warnings []string
	if err := s.removeLeftoverPrivateTaskDirectories(); err != nil {
		warnings = append(warnings, "removing the private task directories that an interrupted node job left: "+err.Error())
	}
	entries, err := os.ReadDir(filepath.Join(s.Root, "jobs"))
	if err != nil {
		return append(warnings, "node history cleanup: "+err.Error())
	}
	warnings = append(warnings, s.removeUnusedKeptFiles()...)
	// A renamed directory is already eligible; retry any unfinished deletion.
	expired, err := os.ReadDir(filepath.Join(s.Root, "expired"))
	if err != nil && !os.IsNotExist(err) {
		warnings = append(warnings, "node history cleanup: "+err.Error())
	}
	for _, entry := range expired {
		if entry.IsDir() && validID.MatchString(entry.Name()) {
			if err := s.removeExpired(entry.Name()); err != nil {
				warnings = append(warnings, fmt.Sprintf("node history cleanup for job %s: %v", entry.Name(), err))
			}
		}
	}
	for _, entry := range entries {
		id := entry.Name()
		if !entry.IsDir() || !validID.MatchString(id) || id == preserve {
			continue
		}
		if err := s.expireJob(id); err != nil {
			warnings = append(warnings, fmt.Sprintf("node history cleanup for job %s: %v", id, err))
		}
	}
	return warnings
}

// A finished job's result expires hostRetention after it was written. The
// controller saves a result as soon as it sees it; the node copy only lets a
// disconnected controller recover it with mmx status or mmx resume.
func (s Server) expireJob(id string) error {
	info, err := os.Stat(filepath.Join(s.jobDir(id), "result.json"))
	if os.IsNotExist(err) {
		return s.expireUnadmitted(id)
	}
	if err != nil || s.timeNow().Sub(info.ModTime()) < nodeRetention {
		return err
	}
	result, err := s.storedResult(id)
	if err != nil {
		return err
	}
	if _, err := s.attachOwedRestores(result, true); err != nil {
		return err
	}
	canExpire, err := s.expireOwedRestores(id)
	if err != nil || !canExpire {
		return err
	}
	// Uploads release admission.lock while streaming. Do not unlink their lock.
	assets, err := s.assetLock(id)
	if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
		return nil
	}
	if err != nil {
		return err
	}
	defer unlock(assets)
	// Rename before deleting: a partial deletion can be retried without needing
	// to re-read a request that was already removed.
	expired := filepath.Join(s.Root, "expired")
	if err := os.MkdirAll(expired, 0700); err != nil {
		return err
	}
	if err := os.Rename(s.jobDir(id), filepath.Join(expired, id)); err != nil {
		return err
	}
	return s.removeExpired(id)
}

// A never-admitted job directory holds a stop marker, which refuses a late
// admission of that job, or the asset lock of uploads that were never
// submitted. It and those uploads are kept for hostRetention. Admission cannot
// race this removal; an upload still holding the asset lock defers it. Receive
// removes partial files on ordinary failure; this removes an interrupted
// upload's.
func (s Server) expireUnadmitted(id string) error {
	if _, err := os.Stat(filepath.Join(s.jobDir(id), "request.json")); !os.IsNotExist(err) {
		return err
	}
	info, err := os.Stat(s.jobDir(id))
	if err != nil || s.timeNow().Sub(info.ModTime()) < nodeRetention {
		return err
	}
	assets, err := s.assetLock(id)
	if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
		return nil
	}
	if err != nil {
		return err
	}
	defer unlock(assets)
	// A runner stopped during admission can leave the private input behind.
	if err := s.removePrivate(id, "input"); err != nil {
		return err
	}
	if err := os.RemoveAll(s.assetDir(id)); err != nil {
		return err
	}
	return os.RemoveAll(s.jobDir(id))
}
func (s Server) removeExpired(id string) error {
	removeAll := s.removeAll
	if removeAll == nil {
		removeAll = os.RemoveAll
	}
	for _, kind := range []string{"input", "outputs"} {
		if err := s.removePrivate(id, kind); err != nil {
			return err
		}
	}
	if err := os.Remove(s.hiddenDetailsPath(id)); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := removeAll(s.assetDir(id)); err != nil {
		return err
	}
	return removeAll(filepath.Join(s.Root, "expired", id))
}
