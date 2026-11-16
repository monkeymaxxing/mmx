package modules

import (
	"context"
	"crypto"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"time"

	"mmx/internal/protocol"
)

// What certbot_certificate needs of certbot itself, beside the certificate
// and its renewal file: running the certbot command, certbot's lock, the
// plugins that certbot can load, the certificate authority's answer in
// certbot's log, and the copies of keys that an older certbot keeps.

// runCertbot runs certbot with the fixed environment of a primitive's
// command, which certbot hands on to a certificate's script, and with
// standard input closed, so that a question ends certbot instead of waiting.
func runCertbot(ctx context.Context, program string, args ...string) protocol.Result {
	return runProcess(ctx, commandEnvironment, "/", nil, nil, program, args...).Result
}

// certbotOutput is what a failure says of a failed certbot: the end of what
// it printed on both of its outputs. certbot explains a refusal on standard
// error and lists the names that a challenge failed for on standard output.
// Neither holds a secret: certbot prints the path of a credentials file,
// never what is in it.
func certbotOutput(r protocol.Result) string {
	parts := []string{}
	for _, text := range []string{r.Stderr, r.Stdout} {
		if text = lastLines(strings.TrimSpace(text), 12); text != "" {
			parts = append(parts, text)
		}
	}
	if len(parts) == 0 {
		return r.Reason
	}
	return strings.Join(parts, "\n")
}

// The lock of certbot
//
// certbot works alone. Every certbot command that reads or changes
// certificates takes a lock on the file .certbot.lock in each of its three
// directories and removes the files when it ends: a POSIX record lock on
// the whole file (Python's fcntl.lockf). A second certbot does not wait; it
// fails at once with "Another instance of Certbot is already running." The
// renewal service of some distributions sleeps for minutes with the locks
// held.
//
// A task asks the kernel whether a process holds such a lock (F_GETLK),
// which takes no lock and so cannot disturb the certbot that runs. It waits
// until none is held and then starts its own certbot. Reading certbot's
// message instead would need a certbot that fails first.
var (
	// certbotLockWait is how long a task waits for another certbot to end.
	certbotLockWait = 10 * time.Minute
	// certbotLockPoll is the pause between two looks at the lock.
	certbotLockPoll = time.Second
)

// certbotLockHeld returns the lock file that another process holds a lock
// on, "" when no certbot runs.
var certbotLockHeld = func() (string, error) {
	for _, directory := range []string{certbotConfigurationDirectory, certbotWorkDirectory, certbotLogDirectory} {
		path := filepath.Join(directory, ".certbot.lock")
		f, err := os.Open(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		lock := syscall.Flock_t{Type: syscall.F_WRLCK}
		err = syscall.FcntlFlock(f.Fd(), syscall.F_GETLK, &lock)
		f.Close()
		if err != nil {
			return "", fmt.Errorf("%s: %w", path, err)
		}
		if lock.Type != syscall.F_UNLCK {
			return path, nil
		}
	}
	return "", nil
}

// waitUntilNoOtherCertbotRuns waits until no process holds certbot's lock:
// at most certbotLockWait, and never longer than the task's own time limit,
// which ends the context. It returns how long it waited: 0 when no other
// certbot ran at its first look.
func waitUntilNoOtherCertbotRuns(ctx context.Context) (time.Duration, error) {
	started := time.Now()
	const advice = "so the certbot_certificate node task did not start certbot. On some distributions, the certbot renewal service waits for several minutes before renewing certificates. Run the playbook again when the running certbot process exits"
	for look := 1; ; look++ {
		held, err := certbotLockHeld()
		if err != nil {
			return 0, fmt.Errorf("certbot_certificate: check whether another certbot process is running: %w", err)
		}
		waited := time.Since(started)
		switch {
		case held == "" && look == 1:
			return 0, nil
		case held == "":
			return waited, nil
		case waited >= certbotLockWait:
			return waited, fmt.Errorf("certbot_certificate: another certbot process still holds a lock after %s (lock file %s), %s", certbotLockWait, held, advice)
		}
		select {
		case <-ctx.Done():
			waited = time.Since(started)
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return waited, fmt.Errorf("certbot_certificate: the node task reached its time limit after waiting %s for another certbot process (lock file %s), %s", waited.Truncate(time.Second), held, advice)
			}
			return waited, fmt.Errorf("certbot_certificate: the node task was stopped while waiting for another certbot process holding the lock %s: %w", held, ctx.Err())
		case <-time.After(certbotLockPoll):
		}
	}
}

// waitedSentence says how long a task waited for another certbot, "" when
// it did not wait.
func waitedSentence(waited time.Duration) string {
	if waited < time.Second {
		return ""
	}
	return fmt.Sprintf(" The task waited %s until another certbot had ended.", waited.Truncate(time.Second))
}

// certbotPluginsOf lists the plugins that the certbot program can load.
// certbot finds a plugin as an entry point of the group certbot.plugins in
// its Python, so MMX asks the interpreter that the certbot program names in
// its first line for these entry points, as JSON. That reads only: the
// command "certbot plugins" would write certbot's log and make its
// directories. known is false when the program is no script, or the answer
// is not a list that holds certbot's own two plugins: then MMX does not
// know, and certbot itself reports a missing plugin.
var certbotPluginsOf = func(ctx context.Context, program string) (plugins []string, known bool) {
	interpreter := certbotInterpreter(program)
	if len(interpreter) == 0 {
		return nil, false
	}
	const listing = `import json, importlib.metadata as m
e = m.entry_points()
g = e.select(group="certbot.plugins") if hasattr(e, "select") else e.get("certbot.plugins", [])
print(json.dumps(sorted({p.name for p in g})))`
	// -B: Python writes no byte-code file, so that a dryrun writes nothing.
	r := runProcess(ctx, commandEnvironment, "/", nil, nil, interpreter[0], append(interpreter[1:], "-B", "-c", listing)...).Result
	if r.Status != "succeeded" || json.Unmarshal([]byte(r.Stdout), &plugins) != nil {
		return nil, false
	}
	if !slices.Contains(plugins, "webroot") || !slices.Contains(plugins, "standalone") {
		return nil, false
	}
	return plugins, true
}

// certbotInterpreter is the interpreter that the first line of the certbot
// program names, with its arguments: "#! /usr/bin/python3 -s" on the Red
// Hat family. It is nil for a program that is no script with an absolute
// interpreter, such as certbot from a snap.
func certbotInterpreter(program string) []string {
	f, err := os.Open(program)
	if err != nil {
		return nil
	}
	defer f.Close()
	start := make([]byte, 256)
	n, _ := io.ReadFull(f, start)
	line, _, _ := strings.Cut(string(start[:n]), "\n")
	rest, found := strings.CutPrefix(line, "#!")
	fields := strings.Fields(rest)
	if !found || len(fields) == 0 || !filepath.IsAbs(fields[0]) {
		return nil
	}
	return fields
}

// acmeProblem is an error answer of a certificate authority, as RFC 8555
// defines it.
type acmeProblem struct {
	Type   string `json:"type"`
	Detail string `json:"detail"`
	// HTTPStatus and RetryAfter come from the answer's first line and its
	// Retry-After header.
	HTTPStatus string `json:"-"`
	RetryAfter string `json:"-"`
}

// sentence is the problem in plain words.
func (p acmeProblem) sentence() string {
	text := p.Detail
	if text == "" {
		text = "the certificate authority gave no text"
	}
	text += " (error type " + p.Type
	if p.HTTPStatus != "" {
		text += ", HTTP status " + p.HTTPStatus
	}
	if p.RetryAfter != "" {
		text += "; the certificate authority says to ask again after: " + p.RetryAfter
	}
	return text + ")"
}

// certbotLogMark is where certbot's log file ended before a task started
// certbot.
type certbotLogMark struct {
	file os.FileInfo
}

// certbotLogLimit bounds how much of certbot's newest log file MMX reads.
const certbotLogLimit = 2 << 20

func certbotLogFile() string { return filepath.Join(certbotLogDirectory, "letsencrypt.log") }

func markCertbotLog() certbotLogMark {
	info, _ := os.Stat(certbotLogFile())
	return certbotLogMark{file: info}
}

// since reads what certbot wrote into its newest log file after the mark:
// the rest of the same file, or the whole file when certbot started a new
// one, and of that at most the last certbotLogLimit bytes. It returns ""
// when it cannot read the file.
func (mark certbotLogMark) since() string {
	f, err := os.Open(certbotLogFile())
	if err != nil {
		return ""
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return ""
	}
	start := int64(0)
	if mark.file != nil && os.SameFile(mark.file, info) && info.Size() >= mark.file.Size() {
		start = mark.file.Size()
	}
	if info.Size()-start > certbotLogLimit {
		start = info.Size() - certbotLogLimit
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return ""
	}
	data, err := io.ReadAll(io.LimitReader(f, certbotLogLimit))
	if err != nil {
		return ""
	}
	return string(data)
}

// A line that starts an entry of certbot's log, such as
// "2026-10-09 17:09:21,678:DEBUG:acme.client:Received response:".
var certbotLogEntry = regexp.MustCompile(`^\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2},\d{3}:[A-Z]+:[^:]+:(.*)$`)

// lastAuthorityErrorIn finds the certificate authority's error answer in a
// part of certbot's log. certbot ends with the same exit code for every
// refusal and prints the authority's reason differently in each version, or
// not at all, and its log holds each answer of the authority as it came:
//
//	2026-10-09 17:09:21,678:DEBUG:acme.client:Received response:
//	HTTP 400
//	Content-Type: application/problem+json; charset=utf-8
//	Retry-After: 600000
//
//	{"type": "urn:ietf:params:acme:error:rejectedIdentifier", "detail": "..."}
//
// Only the last answer in the log counts, and only when it is such an error
// document: an earlier one can be an error that certbot recovered from.
// The log is certbot's debug log, not an interface: for every other form
// found is false, and the caller reports what certbot printed.
func lastAuthorityErrorIn(log string) (problem acmeProblem, found bool) {
	var answer []string
	collecting := false
	for _, line := range strings.Split(log, "\n") {
		if entry := certbotLogEntry.FindStringSubmatch(line); entry != nil {
			collecting = strings.TrimSpace(entry[1]) == "Received response:"
			if collecting {
				answer = answer[:0]
			}
			continue
		}
		if collecting {
			answer = append(answer, strings.TrimSuffix(line, "\r"))
		}
	}
	if len(answer) == 0 {
		return problem, false
	}
	status, isStatus := strings.CutPrefix(answer[0], "HTTP ")
	body := -1
	contentType := ""
	for i, line := range answer[1:] {
		if strings.TrimSpace(line) == "" {
			body = i + 2
			break
		}
		name, text, isHeader := strings.Cut(line, ":")
		if !isHeader {
			return problem, false
		}
		switch strings.ToLower(strings.TrimSpace(name)) {
		case "content-type":
			contentType = strings.ToLower(strings.TrimSpace(text))
		case "retry-after":
			problem.RetryAfter = strings.TrimSpace(text)
		}
	}
	if !isStatus || body < 0 || !strings.HasPrefix(contentType, "application/problem+json") {
		return acmeProblem{}, false
	}
	retryAfter := problem.RetryAfter
	if err := json.Unmarshal([]byte(strings.Join(answer[body:], "\n")), &problem); err != nil || problem.Type == "" {
		return acmeProblem{}, false
	}
	problem.HTTPStatus, problem.RetryAfter = strings.TrimSpace(status), retryAfter
	return problem, true
}

// removeCopiesOfCertbotKeys removes what certbot older than 2.6 keeps of a
// certificate outside the certificate's own directories: a second copy of
// each of its private keys in keys/, and the certificate requests made with
// them in csr/. certbot delete leaves both there. The files are numbered
// across the whole node, so only the key says whose a file is: MMX removes
// the files that carry the public key of one of the certificate's own
// private keys. It returns how many files it removed. A newer certbot has
// no such directories, and nothing happens.
func removeCopiesOfCertbotKeys(name string) (int, error) {
	keys, requests := filepath.Join(certbotConfigurationDirectory, "keys"), filepath.Join(certbotConfigurationDirectory, "csr")
	copies, err := os.ReadDir(keys)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	own := map[string]bool{}
	archived, err := filepath.Glob(filepath.Join(certbotArchiveDirectory(name), "privkey*.pem"))
	if err != nil {
		return 0, err
	}
	for _, path := range archived {
		if public, readable := publicKeyOf(path); readable {
			own[public] = true
		}
	}
	removed := 0
	remove := func(directory string, entries []os.DirEntry) error {
		for _, entry := range entries {
			path := filepath.Join(directory, entry.Name())
			if !entry.Type().IsRegular() {
				continue
			}
			if public, readable := publicKeyOf(path); !readable || !own[public] {
				continue
			}
			if err := os.Remove(path); err != nil {
				return err
			}
			removed++
		}
		return nil
	}
	if err := remove(keys, copies); err != nil {
		return removed, err
	}
	if entries, err := os.ReadDir(requests); err == nil {
		if err := remove(requests, entries); err != nil {
			return removed, err
		}
	}
	return removed, nil
}

// publicKeyOf returns the public key that a PEM file of a private key or of
// a certificate request carries, in one form for both; readable is false
// for every other file. The private key itself goes nowhere.
func publicKeyOf(path string) (public string, readable bool) {
	data, err := readCertbotFile(path)
	if err != nil {
		return "", false
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return "", false
	}
	var key any
	switch block.Type {
	case "CERTIFICATE REQUEST", "NEW CERTIFICATE REQUEST":
		request, err := x509.ParseCertificateRequest(block.Bytes)
		if err != nil {
			return "", false
		}
		return string(request.RawSubjectPublicKeyInfo), true
	case "PRIVATE KEY":
		key, err = x509.ParsePKCS8PrivateKey(block.Bytes)
	case "RSA PRIVATE KEY":
		key, err = x509.ParsePKCS1PrivateKey(block.Bytes)
	case "EC PRIVATE KEY":
		key, err = x509.ParseECPrivateKey(block.Bytes)
	default:
		return "", false
	}
	signer, isKey := key.(crypto.Signer)
	if err != nil || !isKey {
		return "", false
	}
	encoded, err := x509.MarshalPKIXPublicKey(signer.Public())
	if err != nil {
		return "", false
	}
	return string(encoded), true
}
