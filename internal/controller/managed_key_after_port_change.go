package controller

import (
	"bytes"
	"context"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"mmx/internal/access"
	"mmx/internal/config"
	"mmx/internal/durable"
	"mmx/internal/endpoint"
	"mmx/internal/protocol"

	"golang.org/x/crypto/ssh"
)

// adoptManagedKeyAfterPortChange gives MMX's own key, made for this node at
// another SSH port, to the node's current name. It does nothing unless the
// current name has no key yet and the node uses MMX's own key.
//
// Why the port stays in the name at all: two servers behind one address can
// differ only by port, and each needs its own saved node identity (over
// HTTPS the address is the same too). Why a key may still move: a key is
// adopted only after verified SSH, with OpenSSH's host-key check for the new
// port, reaches a machine whose ID equals the machine ID in this project's
// saved node identity for the old name. A different server on the old port
// never matches. The key files are copied, never linked, because removing one
// name must not remove another name's key. Any failure adopts nothing, and the
// caller behaves as it did before this function existed.
func (c Controller) adoptManagedKeyAfterPortChange(ctx context.Context, run *Run, h config.Node, settings sshSettings, keyPath string, credentials *access.Credentials) {
	if h.IdentityFile != "" {
		return
	}
	if _, err := os.Stat(keyPath); err == nil || !os.IsNotExist(err) {
		return
	}
	keyDir := filepath.Dir(keyPath)
	entries, err := os.ReadDir(keyDir)
	if err != nil {
		return
	}
	present := map[string]bool{}
	for _, entry := range entries {
		if runnerHash.MatchString(entry.Name()) {
			present[entry.Name()] = true
		}
	}
	if len(present) == 0 {
		return
	}
	for port := 1; port <= 65535; port++ {
		old := settings
		old.Port = strconv.Itoa(port)
		destination := nodeDestination(old)
		if old.Port == settings.Port || !present[destination] {
			continue
		}
		var saved endpoint.Info
		raw, err := os.ReadFile(filepath.Join(c.State, "nodes", destination+".json"))
		if err != nil || protocol.Decode(raw, &saved) != nil || !runnerHash.MatchString(saved.NodeID) {
			continue
		}
		oldKey := filepath.Join(keyDir, destination)
		if c.machineIDWithKey(ctx, h, run, credentials, settings, oldKey) != saved.NodeID {
			continue
		}
		if copyKeyFiles(oldKey, keyPath) != nil {
			return
		}
		if credentials.Notify != nil {
			credentials.Notify(h.Address + ": MMX's own SSH key for SSH port " + old.Port + " now also serves SSH port " + settings.Port + ": verified SSH at the new port reached the same machine as the saved node identity; key: " + keyPath)
		}
		return
	}
}

// machineIDWithKey opens one fresh verified SSH connection that offers only
// the key at keyPath, never asks for a password, and returns the node ID
// (the SHA-256 of the machine ID, as probeInventorySSH computes it); it
// returns "" when the connection or the key fails.
func (c Controller) machineIDWithKey(ctx context.Context, h config.Node, run *Run, credentials *access.Credentials, settings sshSettings, keyPath string) string {
	session, err := newSSHSession(h, credentials, run.Project.Secrets, keyPath, nil, settings.User+"@"+net.JoinHostPort(settings.Hostname, settings.Port))
	if err != nil {
		return ""
	}
	defer session.Close()
	nodeID, _, err := session.probeMachine(ctx, h.Address)
	if err != nil {
		return ""
	}
	return nodeID
}

// copyKeyFiles copies a private key and its public key to another name in the
// same directory, with the same modes.
func copyKeyFiles(from, to string) error {
	for _, suffix := range []string{"", ".pub"} {
		data, err := os.ReadFile(from + suffix)
		if err != nil {
			return err
		}
		if err := durable.Write(to+suffix, data, 0600); err != nil {
			return err
		}
	}
	return nil
}

// removeCopiesOfManagedKey removes every other file set in the key directory
// that holds the same public key as the key at keyPath, and the project's
// saved node identity under each copy's name, so removing MMX from a node
// leaves nothing that adoption after an SSH port change made or kept.
func removeCopiesOfManagedKey(keyPath, nodesDir string, key ssh.PublicKey) (removed []string, err error) {
	entries, err := os.ReadDir(filepath.Dir(keyPath))
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		name := strings.TrimSuffix(entry.Name(), ".pub")
		if name == entry.Name() || !runnerHash.MatchString(name) {
			continue
		}
		path := filepath.Join(filepath.Dir(keyPath), name)
		raw, err := os.ReadFile(path + ".pub")
		if err != nil {
			continue
		}
		other, _, _, _, err := ssh.ParseAuthorizedKey(raw)
		if err != nil || !bytes.Equal(other.Marshal(), key.Marshal()) {
			continue
		}
		for _, file := range []string{filepath.Join(nodesDir, name+".json"), path, path + ".pub", path + ".lock"} {
			switch err := os.Remove(file); {
			case err == nil:
				removed = append(removed, "controller machine: removed "+file)
			case !os.IsNotExist(err):
				return removed, err
			}
		}
	}
	return removed, nil
}
