package modules

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"unicode"
	"unicode/utf8"

	"mmx/internal/differences"
	"mmx/internal/protocol"
)

func fileState(info os.FileInfo) *protocol.FileState {
	if info == nil {
		return nil
	}
	st := info.Sys().(*syscall.Stat_t)
	return &protocol.FileState{Kind: "file", Size: info.Size(), Mode: fmt.Sprintf("%04o", nativePermissions(info.Mode())), UID: int(st.Uid), GID: int(st.Gid)}
}

func fileChange(path string, before os.FileInfo, metadata fileMetadata, size int64, hasContents bool) *protocol.FileChange {
	old := fileState(before)
	if !hasContents && before != nil {
		size = before.Size()
	}
	next := &protocol.FileState{Kind: "file", Size: size, Mode: fmt.Sprintf("%04o", nativePermissions(metadata.mode)), UID: metadata.uid, GID: metadata.gid}
	action := "update"
	if before == nil {
		action = "create"
	}
	return &protocol.FileChange{Path: path, Action: action, Before: old, After: next}
}

// One context hunk is sufficient for ordinary configuration previews. Bound both
// work and retained output; large/binary inputs keep the structured summary.
// A node file is private when its mode or a directory above it keeps other
// accounts out.
func (run Runtime) textDiff(change *protocol.FileChange, old, next []byte) {
	if privateMode(change) || !othersCanTraverseTo(change.Path) {
		hidePrivateText(change)
		return
	}
	run.shownTextDiff(change, old, next)
}

// privateMode reports a file that other accounts could not read before the
// change, or cannot read after it, by its mode.
func privateMode(change *protocol.FileChange) bool {
	return (change.Before != nil && !worldReadable(change.Before)) || !worldReadable(change.After)
}

func hidePrivateText(change *protocol.FileChange) {
	change.DiffOmitted = "contents hidden for a private file"
	change.FullDiffOmitted = change.DiffOmitted
}

// shownTextDiff fills the difference of old and next, texts that may be
// shown. A module whose private file it can show without its secrets, such
// as wireguard_interface's file without the private key, gives it those
// texts.
func (run Runtime) shownTextDiff(change *protocol.FileChange, old, next []byte) {
	largePreview := len(old) > 64*1024 || len(next) > 64*1024
	if largePreview {
		change.DiffOmitted = "text preview omitted: file exceeds 64 KiB"
	}
	if len(old) > differences.InputLimit || len(next) > differences.InputLimit {
		change.FullDiffOmitted = "full difference omitted: file exceeds 1 MiB"
		return
	}
	isText := func(b []byte) bool {
		if !utf8.Valid(b) {
			return false
		}
		for _, r := range string(b) {
			if unicode.IsControl(r) && r != '\n' && r != '\t' {
				return false
			}
		}
		return true
	}
	if !isText(old) || !isText(next) {
		change.DiffOmitted = "binary contents omitted"
		change.FullDiffOmitted = change.DiffOmitted
		return
	}
	if bytes.Equal(old, next) {
		return
	}
	lines := func(b []byte) []string {
		if len(b) == 0 {
			return nil
		}
		return strings.SplitAfter(strings.TrimSuffix(string(b), "\n"), "\n")
	}
	// Preserve final-newline differences in line comparisons.
	a, b := lines(old), lines(next)
	if len(a) > 0 && bytes.HasSuffix(old, []byte("\n")) {
		a[len(a)-1] += "\n"
	}
	if len(b) > 0 && bytes.HasSuffix(next, []byte("\n")) {
		b[len(b)-1] += "\n"
	}
	prefix := 0
	for prefix < len(a) && prefix < len(b) && a[prefix] == b[prefix] {
		prefix++
	}
	suffix := 0
	for suffix < len(a)-prefix && suffix < len(b)-prefix && a[len(a)-1-suffix] == b[len(b)-1-suffix] {
		suffix++
	}
	start := max(0, prefix-3)
	aEnd, bEnd := min(len(a), len(a)-suffix+3), min(len(b), len(b)-suffix+3)
	lineStart := func(count int) int {
		if count == 0 {
			return start
		}
		return start + 1
	}
	var out strings.Builder
	fmt.Fprintf(&out, "--- before\n+++ after\n@@ -%d,%d +%d,%d @@\n", lineStart(aEnd-start), aEnd-start, lineStart(bEnd-start), bEnd-start)
	var full strings.Builder
	full.WriteString(out.String())
	count := 3
	truncated := false
	emit := func(marker string, line string) {
		text := marker + line
		if !strings.HasSuffix(line, "\n") {
			text += "\n\\ No newline at end of file\n"
		}
		full.WriteString(text)
		if truncated {
			return
		}
		n := strings.Count(text, "\n")
		if out.Len()+len(text) > 16*1024 || count+n > 200 {
			truncated = true
			return
		}
		out.WriteString(text)
		count += n
	}
	for _, line := range a[start:prefix] {
		emit(" ", line)
	}
	for _, line := range a[prefix : len(a)-suffix] {
		emit("-", line)
	}
	for _, line := range b[prefix : len(b)-suffix] {
		emit("+", line)
	}
	for _, line := range a[len(a)-suffix : aEnd] {
		emit(" ", line)
	}
	if run.RetainDiff != nil {
		change.FullDiff, change.FullDiffOmitted = run.RetainDiff(full.String())
	} else {
		change.FullDiffOmitted = "full difference storage unavailable"
	}
	if largePreview {
		return
	}
	change.Diff = out.String()
	if truncated {
		change.DiffOmitted = "text preview truncated at 16 KiB or 200 lines"
	}
}

func worldReadable(s *protocol.FileState) bool {
	if s == nil {
		return false
	}
	mode, err := strconv.ParseUint(s.Mode, 8, 12)
	return err == nil && mode&0004 != 0
}

// othersCanTraverseTo reports whether other accounts can pass through every
// existing directory above path (the execute bit for others). A file below
// one that they cannot pass, such as a 0644 file in a 0700 home directory, is
// private whatever its own mode. A directory that does not exist yet holds no
// earlier text; one that cannot be inspected counts as private.
func othersCanTraverseTo(path string) bool {
	for directory := filepath.Dir(path); ; directory = filepath.Dir(directory) {
		info, err := os.Stat(directory)
		switch {
		case err == nil && info.Mode().Perm()&0001 == 0:
			return false
		case err != nil && !errors.Is(err, fs.ErrNotExist):
			return false
		}
		if directory == filepath.Dir(directory) {
			return true
		}
	}
}

// TextDiff fills a file change's text difference preview as a file task's
// result shows it, without storing a full difference. The controller process
// uses it for the files save_on_controller_machine writes on the controller
// machine. Only the file's mode makes such a file private: the project
// usually sits in a private home directory, and the files hold the fleet's
// outputs, which the run shows anyway.
func TextDiff(change *protocol.FileChange, old, next []byte) {
	if privateMode(change) {
		hidePrivateText(change)
		return
	}
	Runtime{}.shownTextDiff(change, old, next)
}
