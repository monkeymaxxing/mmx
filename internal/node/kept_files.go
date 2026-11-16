package node

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"mmx/internal/assets"
	"mmx/internal/protocol"
)

// keptFileRetention is how long a node keeps a file that no run has used.
const keptFileRetention = 30 * 24 * time.Hour

// keptFilesDir keeps, by their SHA-256, the files of modules that are not
// core and the files from a web address that node tasks install or unpack,
// so a later run sends a node only the files it lacks.
func (s Server) keptFilesDir() string { return filepath.Join(s.Root, "kept_files") }

// UseKeptFiles links each named file the node keeps, after checking its
// SHA-256, into the job's files, and answers with the files the controller
// must send. Like Upload, it runs before admission, under the job's asset
// lock.
func (s Server) UseKeptFiles(request protocol.KeptFilesRequest) (protocol.KeptFilesResponse, error) {
	response := protocol.KeptFilesResponse{FilesToSend: []string{}}
	if !validID.MatchString(request.JobID) {
		return response, fmt.Errorf("invalid node job ID")
	}
	for _, ref := range request.Files {
		if err := ref.Validate(); err != nil {
			return response, err
		}
	}
	if err := s.Initialize(); err != nil {
		return response, err
	}
	admission, err := waitLock(filepath.Join(s.Root, "admission.lock"))
	if err != nil {
		return response, err
	}
	assetLock, err := s.assetLock(request.JobID)
	unlock(admission)
	if err != nil {
		return response, err
	}
	defer unlock(assetLock)
	if _, err := os.Stat(filepath.Join(s.jobDir(request.JobID), "request.json")); !os.IsNotExist(err) {
		if err != nil {
			return response, err
		}
		return response, fmt.Errorf("cannot add kept files to an admitted job")
	}
	if err := os.MkdirAll(s.assetDir(request.JobID), 0700); err != nil {
		return response, err
	}
	for _, ref := range request.Files {
		if !s.useKeptFile(request.JobID, ref) {
			response.FilesToSend = append(response.FilesToSend, ref.SHA256)
		}
	}
	return response, nil
}

// useKeptFile reports whether the job now has the file from the store. A
// kept file whose bytes no longer match is removed and sent again.
func (s Server) useKeptFile(id string, ref assets.Ref) bool {
	kept := ref
	kept.Path = filepath.Join(s.keptFilesDir(), ref.SHA256)
	if err := assets.Verify(context.Background(), kept); err != nil {
		if !os.IsNotExist(err) {
			os.Remove(kept.Path)
		}
		return false
	}
	if err := os.Link(kept.Path, filepath.Join(s.assetDir(id), ref.SHA256)); err != nil && !os.IsExist(err) {
		return false
	}
	// The time of last use decides when the store removes the file.
	now := s.timeNow()
	return os.Chtimes(kept.Path, now, now) == nil
}

// keepFiles adds the verified files of an admitted job that the job names to
// keep to the store. The store only saves sending: a file it fails to keep
// is sent next time.
func (s Server) keepFiles(refs []assets.Ref, id string) {
	if os.MkdirAll(s.keptFilesDir(), 0700) != nil {
		return
	}
	now := s.timeNow()
	for _, ref := range refs {
		if ref.Validate() != nil {
			continue
		}
		kept := filepath.Join(s.keptFilesDir(), ref.SHA256)
		if err := os.Link(filepath.Join(s.assetDir(id), ref.SHA256), kept); err == nil || os.IsExist(err) {
			// Failure to refresh the cache timestamp only permits an earlier cache eviction.
			_ = os.Chtimes(kept, now, now)
		}
	}
}

// removeUnusedKeptFiles removes the kept files no run has used for
// keptFileRetention.
func (s Server) removeUnusedKeptFiles() []string {
	entries, err := os.ReadDir(s.keptFilesDir())
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return []string{"kept file cleanup: " + err.Error()}
	}
	var warnings []string
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() || s.timeNow().Sub(info.ModTime()) < keptFileRetention {
			continue
		}
		if err := os.Remove(filepath.Join(s.keptFilesDir(), entry.Name())); err != nil && !os.IsNotExist(err) {
			warnings = append(warnings, "kept file cleanup: "+err.Error())
		}
	}
	return warnings
}
