// Package node persists admission and runs each job in a detached worker
// process. The controller never owns a process pipe that must stay open for
// admitted work to finish.
package node

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
	"unicode"

	"mmx/internal/config"
	"mmx/internal/durable"
	"mmx/internal/engine"
	"mmx/internal/facts"
	"mmx/internal/modules"
	"mmx/internal/protocol"
)

var validID = regexp.MustCompile(`^[a-f0-9]{64}$`)

type Server struct {
	unitDirectory     string // tests replace /etc/systemd/system
	systemctl         func(context.Context, ...string) (string, error)
	activationFactory func(string, string, json.RawMessage) (modules.ChangeActivation, error)
	extendRestoreWait func() error  // tests replace the systemd notification
	restoreWait       time.Duration // tests shorten the worker's restore-service allowance

	now          func() time.Time
	removeAll    func(string) error
	machine      engine.NodeMachine // tests replace the node's boot ID and machine ID
	reboot       func() error       // and its reboot request
	progressWait time.Duration
	spacing      time.Duration
	// Tests shorten how long a reboot waits to be announced.
	announceWait, announceGrace time.Duration
	doNotManage                 string // tests replace DoNotManagePath
	RuntimeRoot                 string
	Root                        string
	Binary                      string
}

// Install shares the admission and execution boundaries with ordinary jobs.
// With no active job, execution.lock can only be held by a finished worker
// removing its files, so waiting for it is brief.
func (s Server) Install(change func() error) error {
	if err := s.Initialize(); err != nil {
		return err
	}
	admission, err := waitLock(filepath.Join(s.Root, "admission.lock"))
	if err != nil {
		return err
	}
	defer unlock(admission)
	if id, err := s.activeJob(); err != nil {
		return err
	} else if id != "" {
		return fmt.Errorf("node busy with job %s%s", id, s.startedBy(id))
	}
	if err := s.refuseRunnerReplacement(); err != nil {
		return err
	}
	execution, err := waitLock(filepath.Join(s.Root, "execution.lock"))
	if err != nil {
		return err
	}
	defer unlock(execution)
	return change()
}

// activeJob is the one check for a running worker, used under admission.lock:
// the admitted job stays active until its terminal result. It also covers a
// started worker that does not hold execution.lock yet, and a rebooting job's
// worker, which keeps execution.lock until its reboot request returns: a job
// admitted then would run its node tasks while the node shuts down.
func (s Server) activeJob() (string, error) {
	active, err := os.ReadFile(filepath.Join(s.Root, "active"))
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	r, err := s.status(string(active))
	if err != nil {
		return "", err
	}
	busy := r.Status == "running"
	if r.Status == "rebooting" {
		busy, err = s.workerRunning(r.ID)
	}
	if err != nil || !busy {
		return "", err
	}
	return r.ID, nil
}

// startedByText is "account@host" for the busy message, with only the
// characters of ordinary account and host names. It is empty when the
// controller process sent neither, as an older controller process does.
func startedByText(j config.Job) string {
	clean := func(v string) string {
		return strings.Map(func(r rune) rune {
			if r < 128 && (unicode.IsLetter(r) || unicode.IsDigit(r) || strings.ContainsRune("._-\\$", r)) {
				return r
			}
			return -1
		}, v)
	}
	account, host := clean(j.StartedByAccount), clean(j.StartedFromHostName)
	switch {
	case account != "" && host != "":
		return account + "@" + host
	default:
		return account + host
	}
}

// startedBy is the busy message's ", started by account@host", or nothing
// when the job's record has no such file.
func (s Server) startedBy(id string) string {
	text, err := os.ReadFile(filepath.Join(s.jobDir(id), "started_by"))
	if err != nil || len(text) == 0 {
		return ""
	}
	return ", started by " + string(text)
}

func (s Server) jobDir(id string) string { return filepath.Join(s.Root, "jobs", id) }
func (s Server) Initialize() error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("mmx-runner must run as root")
	}
	for _, path := range []string{s.Root, filepath.Join(s.Root, "jobs")} {
		if err := os.MkdirAll(path, 0700); err != nil {
			return err
		}
		st, err := os.Lstat(path)
		if err != nil {
			return err
		}
		owner, ok := st.Sys().(*syscall.Stat_t)
		if !st.IsDir() || st.Mode().Perm() != 0700 || !ok || owner.Uid != 0 {
			return fmt.Errorf("mmx-runner state directory must belong to root and have mode 0700: %s", path)
		}
	}
	return nil
}
func lock(path string) (*os.File, error)     { return fileLock(path, syscall.LOCK_EX|syscall.LOCK_NB) }
func waitLock(path string) (*os.File, error) { return fileLock(path, syscall.LOCK_EX) }
func fileLock(path string, flags int) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(f.Fd()), flags); err != nil {
		f.Close()
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	return f, nil
}
func unlock(f *os.File) { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); _ = f.Close() }

// Submit takes connection details only from the node-side SSH or HTTPS
// entry point. The controller-supplied job cannot set connection details.
func (s Server) Submit(raw []byte, connection modules.ControllerConnection) (response protocol.Response, err error) {
	var j config.Job
	if err := protocol.Decode(raw, &j); err != nil {
		return protocol.Response{}, err
	}
	if !validID.MatchString(j.ID) || j.Protocol != protocol.Version || !modules.Name.MatchString(j.Project) || !config.ValidFleetStagePath(j.FleetStage) {
		return protocol.Response{}, fmt.Errorf("invalid node job ID, project name, fleet stage path, or protocol version")
	}
	if err := s.Initialize(); err != nil {
		return protocol.Response{}, err
	}
	l, err := waitLock(filepath.Join(s.Root, "admission.lock"))
	if err != nil {
		return protocol.Response{}, err
	}
	defer unlock(l)
	warnings := s.sweepHistory(j.ID)
	defer func() { response.Housekeeping = append(response.Housekeeping, warnings...) }()

	saved, err := json.Marshal(j)
	if err != nil {
		return protocol.Response{}, err
	}
	dir := s.jobDir(j.ID)
	// A saved request is replaced atomically, so it is absent or whole; its
	// bytes identify a repeated submission.
	if old, err := os.ReadFile(filepath.Join(dir, "request.json")); err == nil {
		if !bytes.Equal(old, saved) {
			return protocol.Response{}, fmt.Errorf("node job ID already belongs to a different request")
		}
		return s.status(j.ID)
	} else if !errors.Is(err, os.ErrNotExist) {
		return protocol.Response{}, err
	}
	assetLock, err := s.assetLock(j.ID)
	if err != nil {
		return protocol.Response{}, err
	}
	defer unlock(assetLock)
	if err := s.verifyAssets(j.NodeTasks, j.Modules, j.ID); err != nil {
		return protocol.Response{}, err
	}
	s.keepFiles(j.KeptFiles, j.ID)
	if id, err := s.activeJob(); err != nil {
		return protocol.Response{}, err
	} else if id != "" {
		return protocol.Response{}, fmt.Errorf("node busy with job %s%s", id, s.startedBy(id))
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return protocol.Response{}, err
	}
	if j.PrivateInputs {
		if err := s.writePrivate(j.ID, "input", raw); err != nil {
			return protocol.Response{}, err
		}
	}
	// Keep node-observed connection data outside the controller's request. A
	// repeated submission returns above and cannot replace this address.
	for _, address := range []*string{&connection.SourceAddress, &connection.NodeLocalAddress} {
		parsed, err := netip.ParseAddr(*address)
		*address = ""
		if err == nil {
			*address = parsed.Unmap().String()
		}
	}
	if err := durable.JSON(filepath.Join(dir, "controller_connection.json"), connection); err != nil {
		return protocol.Response{}, err
	}
	if err := durable.Write(filepath.Join(dir, "started_by"), []byte(startedByText(j)), 0600); err != nil {
		return protocol.Response{}, err
	}
	if err := durable.Write(filepath.Join(dir, "request.json"), saved, 0600); err != nil {
		return protocol.Response{}, err
	}
	// active only guards admission while the node runs: after a crash no worker
	// is running, and status reports the saved request's job as interrupted.
	if err := durable.Replace(filepath.Join(s.Root, "active"), []byte(j.ID), 0600); err != nil {
		return protocol.Response{}, err
	}
	if err := s.startWorker(j.ID); err != nil {
		return s.finish(protocol.Response{ID: j.ID, Status: "failed", Error: fmt.Sprintf("could not start node job: %v", err)})
	}
	return protocol.Response{ID: j.ID, Status: "running"}, nil
}

// workerEnv is every worker's whole environment, whichever transport admitted
// it, as a login of root gives a service: PATH, LANG, USER, and the LOGNAME and
// HOME of root. Every command a task runs as root inherits it; a task's own
// env and run_as_user_account replace entries after that.
var workerEnv = append([]string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C.UTF-8", "USER=root"}, modules.RootLoginEnvironment()...)

// startWorker starts the job's worker in a session of its own, detached from
// the request that admitted it: the HTTPS request, the SSH session, or the
// endpoint itself can end without stopping it. The endpoint's unit restarts
// only its main process. The worker inherits the job's worker.lock, locked
// here before it starts, and holds it until it exits however it exits, so a
// held lock means the job is running, with no gap while the worker starts.
func (s Server) startWorker(id string) error {
	held, err := lock(filepath.Join(s.jobDir(id), "worker.lock"))
	if err != nil {
		return err
	}
	// Closing this descriptor leaves the worker's copy holding the lock.
	defer held.Close()
	output, err := os.OpenFile(filepath.Join(s.jobDir(id), "worker.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	defer output.Close()
	c := exec.Command(s.Binary, "worker", id)
	c.Env, c.Dir = workerEnv, "/"
	c.Stdout, c.Stderr = output, output
	c.ExtraFiles = []*os.File{held}
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := c.Start(); err != nil {
		return err
	}
	go func() { _ = c.Wait() }()
	return nil
}

// workerRunning reports whether the job's worker holds its worker.lock.
func (s Server) workerRunning(id string) (bool, error) {
	f, err := os.OpenFile(filepath.Join(s.jobDir(id), "worker.lock"), os.O_RDWR, 0)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer f.Close()
	err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return true, nil
	}
	return false, err
}

// Admission holds this lock until the worker holds worker.lock. A status query
// must not finalize a saved request before its worker starts.
func (s Server) Status(id string) (protocol.Response, error) { return s.inspect(id, false) }

// A stop marker also covers a stop arriving just before admission; sweeps keep
// a never-admitted one for hostRetention. Reads and writes share the admission
// lock with expiry.
func (s Server) Stop(id string) (protocol.Response, error) { return s.inspect(id, true) }

// progressWait bounds one progress request. The controller HTTPS client waits
// 15 seconds for response headers, which also covers the final inspection, so
// the wait stays well below that.
const progressWait = 5 * time.Second

// progressSpacing is the least time a running answer waits after its request
// arrives, so a job whose node tasks finish quickly is not asked about many times
// a second. progressPoll is how often a waiting request looks for a change.
const (
	progressSpacing = 200 * time.Millisecond
	progressPoll    = 20 * time.Millisecond
)

// Progress follows a running job past what the controller holds. A job that is
// not running, including one that finishes while the request waits, answers at
// once. A running job answers once a result or the current task differs
// from the request and progressSpacing has passed, or after progressWait. A
// running answer omits the results the controller already has. Waiting holds no lock: it watches the progress journal and result file.
func (s Server) Progress(request protocol.ProgressRequest) (protocol.Response, error) {
	if !validID.MatchString(request.ID) {
		return protocol.Response{}, fmt.Errorf("invalid node job ID")
	}
	wait := s.progressWait
	if wait == 0 {
		wait = progressWait
	}
	spacing := s.spacing
	if spacing == 0 {
		spacing = progressSpacing
	}
	started := time.Now()
	deadline, spaced := started.Add(wait), started.Add(spacing)
	for {
		before := s.jobMark(request.ID)
		r, err := s.inspect(request.ID, false)
		moved := len(r.Results) != request.Results || r.CurrentTask != request.CurrentTask || !samePendingConfirmation(r.PendingReachabilityConfirmation, request.KnownReachabilityConfirmation)
		if now := time.Now(); err != nil || r.Status != "running" || !now.Before(deadline) || (moved && !now.Before(spaced)) {
			if r.Status == "running" && request.Results > 0 && request.Results <= len(r.Results) {
				r.Results, r.ResultsAfter, r.Facts = r.Results[request.Results:], request.Results, nil
			}
			return r, err
		}
		// Until the answer is due, only a written result ends a moved wait.
		for time.Now().Before(deadline) {
			mark := s.jobMark(request.ID)
			if mark[1] != before[1] || (moved && !time.Now().Before(spaced)) || (!moved && mark != before) {
				break
			}
			time.Sleep(progressPoll)
		}
	}
}

func sameFile(f *os.File, path string) bool {
	a, err := f.Stat()
	if err != nil {
		return false
	}
	b, err := os.Stat(path)
	return err == nil && os.SameFile(a, b)
}

// jobMark changes when the worker writes progress or finishes, or when an
// independent request or service updates one of the job's restore records.
func (s Server) jobMark(id string) [3]string {
	mark := [3]string{"", "", s.owedRestoreMark(id)}
	if st, err := os.Stat(filepath.Join(s.jobDir(id), "progress.jsonl")); err == nil {
		mark[0] = fmt.Sprint(st.Size())
	}
	if st, err := os.Stat(filepath.Join(s.jobDir(id), "result.json")); err == nil {
		mark[1] = fmt.Sprint(st.Size())
	}
	return mark
}

func (s Server) inspect(id string, stop bool) (protocol.Response, error) {
	if !validID.MatchString(id) {
		return protocol.Response{}, fmt.Errorf("invalid node job ID")
	}
	if stop {
		if err := s.Initialize(); err != nil {
			return protocol.Response{}, err
		}
	}
	l, err := waitLock(filepath.Join(s.Root, "admission.lock"))
	if err != nil {
		return protocol.Response{}, err
	}
	defer unlock(l)
	warnings := s.sweepHistory(id)
	if stop {
		if err := os.MkdirAll(s.jobDir(id), 0700); err != nil {
			return protocol.Response{}, err
		}
		if err := durable.Write(filepath.Join(s.jobDir(id), "stop"), nil, 0600); err != nil {
			return protocol.Response{}, err
		}
	}
	r, err := s.status(id)
	if err != nil {
		return r, err
	}
	if r.Status == "rebooting" {
		// The worker reboots once this answer can reach the controller. A
		// mark that cannot be written only delays the reboot by
		// rebootAnnounceWait.
		_ = os.WriteFile(filepath.Join(s.jobDir(id), "reboot-announced"), nil, 0600)
	}
	r.StopRequested, err = s.stopRequested(id)
	if err == nil && r.StopRequested && r.Status == "not_found" {
		r.Status, r.Error = "stopped", ""
	}
	r.Housekeeping = append(r.Housekeeping, warnings...)
	return r, err
}

func (s Server) stopRequested(id string) (bool, error) {
	if !validID.MatchString(id) {
		return false, fmt.Errorf("invalid node job ID")
	}
	_, err := os.Stat(filepath.Join(s.jobDir(id), "stop"))
	if os.IsNotExist(err) {
		return false, nil
	}
	return err == nil, err
}

// The worker appends one line per change to progress.jsonl: facts once, each
// newly retained result, and the task it starts. Lines are not synced; a live read can meet a partial final line and a
// crash can leave torn or zeroed blocks, so readers stop at the first incomplete
// or undecodable line. The journal is a live view and partial record of an
// interrupted job, never recovery state; damage must not block reconciliation. It holds the retained results plus one
// short line per started task.
type progressLine struct {
	Facts  *facts.Snapshot  `json:"facts,omitempty"`
	Result *protocol.Result `json:"result,omitempty"`
	Task   string           `json:"current_task,omitempty"`
}

type progressJournal struct {
	file    *os.File
	facts   bool
	written int
}

func openJournal(path string) (*progressJournal, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY|os.O_APPEND, 0600)
	return &progressJournal{file: f}, err
}
func (p *progressJournal) starting(task string) error { return p.append(progressLine{Task: task}) }

// The engine only appends results.
func (p *progressJournal) report(o protocol.Response) error {
	var lines []progressLine
	if o.Facts != nil && !p.facts {
		lines, p.facts = append(lines, progressLine{Facts: o.Facts}), true
	}
	for _, r := range o.Results[p.written:] {
		lines = append(lines, progressLine{Result: &r})
	}
	p.written = len(o.Results)
	return p.append(lines...)
}
func (p *progressJournal) append(lines ...progressLine) error {
	var b []byte
	for _, l := range lines {
		line, err := json.Marshal(l)
		if err != nil {
			return err
		}
		b = append(append(b, line...), '\n')
	}
	_, err := p.file.Write(b)
	return err
}

// progress folds the journal into a running answer. The current task is the
// one named by the last line, so a recorded result clears it.
func (s Server) progress(id string) (protocol.Response, error) {
	r := protocol.Response{ID: id, Status: "running"}
	f, err := os.Open(filepath.Join(s.jobDir(id), "progress.jsonl"))
	if os.IsNotExist(err) {
		return r, nil
	}
	if err != nil {
		return r, err
	}
	defer f.Close()
	in := bufio.NewReader(f)
	for {
		line, err := in.ReadBytes('\n')
		if err == io.EOF {
			return r, nil
		}
		if err != nil {
			return r, err
		}
		var l progressLine
		if protocol.Decode(line, &l) != nil {
			return r, nil
		}
		if l.Facts != nil {
			r.Facts = l.Facts
		}
		if l.Result != nil {
			r.Results = append(r.Results, *l.Result)
		}
		r.CurrentTask = l.Task
	}
}

func (s Server) status(id string) (response protocol.Response, returnedError error) {
	defer func() {
		if returnedError == nil && response.Status != "not_found" {
			response, returnedError = s.attachOwedRestores(response, true)
		}
	}()
	if !validID.MatchString(id) {
		return protocol.Response{}, fmt.Errorf("invalid node job ID")
	}
	r, err := s.result(id)
	if err == nil {
		return s.rebooted(r)
	}
	if !os.IsNotExist(err) {
		return r, err
	}
	if _, err := os.Stat(filepath.Join(s.jobDir(id), "request.json")); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return protocol.Response{ID: id, Status: "not_found", Error: "node history is unavailable for job " + id + "; missing history does not mean this job did not run"}, nil
		}
		return protocol.Response{}, err
	}
	running, err := s.workerRunning(id)
	if err != nil {
		return protocol.Response{}, fmt.Errorf("cannot inspect the node job worker process: %w", err)
	}
	if running {
		return s.progress(id)
	}
	// Completion may have raced the worker.lock check.
	r, err = s.result(id)
	if err == nil {
		return s.rebooted(r)
	}
	if !os.IsNotExist(err) {
		return r, err
	}
	r = protocol.Response{ID: id, Status: "failed", Error: "node job interrupted before recording completion; mmx resume or the next mmx run inspects the actual resource and service state"}
	prior, err := s.progress(id)
	if err != nil {
		return r, err
	}
	r.Results, r.Facts = prior.Results, prior.Facts
	return s.finish(r)
}

// Worker runs an admitted job. held is the job's worker.lock as admission
// passed it to the started worker; with none, the worker waits for the lock.
// Once it holds the lock, every way the job ends goes through finish, which
// records the cause; worker.log keeps only what a crash writes.
func (s Server) Worker(id string, held *os.File) error {
	if !validID.MatchString(id) {
		return fmt.Errorf("invalid node job ID")
	}
	if err := s.Initialize(); err != nil {
		return err
	}
	syscall.Umask(0o077)
	path := filepath.Join(s.jobDir(id), "worker.lock")
	if held != nil {
		if !sameFile(held, path) {
			return fmt.Errorf("worker lock does not belong to job %s", id)
		}
		// Task processes, including daemons a task leaves running, must not
		// keep the job looking alive.
		syscall.CloseOnExec(int(held.Fd()))
		defer held.Close()
	} else {
		own, err := waitLock(path)
		if err != nil {
			return err
		}
		defer unlock(own)
	}
	// Only runner installation or the previous worker's cleanup can hold it.
	l, err := waitLock(filepath.Join(s.Root, "execution.lock"))
	if err == nil {
		defer unlock(l)
	}
	// A repeated start finds its job already ended.
	if _, statErr := os.Stat(filepath.Join(s.jobDir(id), "result.json")); statErr == nil {
		return nil
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return statErr
	}
	r := protocol.Response{ID: id}
	if err == nil {
		r, err = s.work(id)
	}
	if err != nil {
		r = protocol.Response{ID: id, Status: "failed", Error: err.Error(), Change: r.Change, Results: r.Results, Facts: r.Facts}
	}
	if r, err = s.finish(r); err == nil && r.Status == "rebooting" {
		err = s.requestReboot(r)
	}
	return err
}

// work runs the job; the worker holds the execution lock. An error ends the
// job as failed, keeping any results it returns.
func (s Server) work(id string) (protocol.Response, error) {
	dir := s.jobDir(id)
	b, err := os.ReadFile(filepath.Join(dir, "request.json"))
	if err != nil {
		return protocol.Response{}, err
	}
	var j config.Job
	if err = protocol.Decode(b, &j); err != nil {
		return protocol.Response{}, err
	}
	// The refusal holds for every job a run, a resume, or run-command sends,
	// whichever connection delivered it, and comes before the first node task.
	refusal := s.managementRefusal()
	if refusal != "" && !j.Dryrun {
		return protocol.Response{ID: id, Status: "failed", Error: refusal}, nil
	}
	if j.PrivateInputs {
		b, err := os.ReadFile(s.privatePath(id, "input"))
		if err != nil {
			return protocol.Response{}, fmt.Errorf("private node job inputs are unavailable; mmx resume sends the inputs again")
		}
		if err := protocol.Decode(b, &j); err != nil {
			return protocol.Response{}, fmt.Errorf("invalid private node job input")
		}
		if err := os.Remove(s.privatePath(id, "input")); err != nil {
			return protocol.Response{}, err
		}
	}
	if err := config.BindAssets(j.NodeTasks, j.Modules, s.assetDir(id)); err != nil {
		return protocol.Response{}, err
	}
	if err := stageModules(context.Background(), j.Modules, s.assetDir(id)); err != nil {
		return protocol.Response{}, err
	}
	journal, err := openJournal(filepath.Join(dir, "progress.jsonl"))
	if err != nil {
		return protocol.Response{}, err
	}
	defer journal.file.Close()
	// The engine keeps its service records in the runner's volatile
	// directory, which the node makes with its mode before the first task.
	if _, err := s.runtimeDir(); err != nil {
		return protocol.Response{}, err
	}
	// A record without connection data leaves the source unknown. The
	// detached worker's environment cannot identify the admitting connection.
	var connection modules.ControllerConnection
	connectionBytes, err := os.ReadFile(filepath.Join(dir, "controller_connection.json"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return protocol.Response{}, err
	}
	if err == nil {
		if err := protocol.Decode(connectionBytes, &connection); err != nil {
			return protocol.Response{}, err
		}
	}
	e := engine.Engine{ControllerConnection: connection, RuntimeRoot: s.RuntimeRoot, PrivateTaskDirectoryRoot: s.privateTaskDirectoryRoot, StateRoot: s.Root, ModuleHelper: s.Binary, DiffDir: filepath.Join(s.jobDir(id), "diffs"), Machine: s.machine}
	e.CheckFileDestination = s.CheckChangeAllowed
	e.KeepHiddenDetails = func(r protocol.Result) (string, error) { return s.keepHiddenDetails(id, r) }
	e.OwedRestoresForTask = func(taskPath, moduleName string, secret func() bool, runtime modules.Runtime) modules.OwedRestoreService {
		return s.changeService(id, taskPath, moduleName, j.ReachabilityConfirmationConnectionKinds, secret, runtime)
	}
	e.StopRequested = func() (bool, error) { return s.stopRequested(id) }
	e.Starting, e.Report = journal.starting, journal.report
	r := e.Run(context.Background(), j)
	// A dryrun inspects the node as usual and fails as the run would.
	if refusal != "" && r.Status != "stopped" {
		message := "a run would be refused: " + refusal
		if r.Error != "" {
			message += "; the dryrun itself also failed: " + r.Error
		}
		r.Status, r.Error, r.Outputs = "failed", message, nil
	}
	if err := s.savePrivateOutputs(r); err != nil {
		return r, fmt.Errorf("saving private fleet stage outputs: %w", err)
	}
	return r, nil
}

// DoNotManagePath is the file an administrator creates on a node, by hand, to
// keep MMX from changing it while they work on it. MMX only reads it.
const DoNotManagePath = "/etc/mmx/do_not_manage"

// doNotManageTextLimit bounds how much of the file a refusal reads and shows:
// enough for a few lines saying who works on the node and until when.
const doNotManageTextLimit = 1024

// managementRefusal says why this node refuses jobs that could change it, with
// the text of the do_not_manage file, or returns "" when the file does not
// exist.
func (s Server) managementRefusal() string {
	path := s.doNotManage
	if path == "" {
		path = DoNotManagePath
	}
	// Any entry at the path refuses, also a symbolic link whose target is gone.
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
		return ""
	}
	link := err == nil && info.Mode()&os.ModeSymlink != 0 //nolint:nilaway // Lstat returns non-nil info when err == nil and the conjunction checks err first.
	if link {
		info, err = os.Stat(path)
	}
	said := ""
	switch {
	case link && err != nil:
		said = "it is a symbolic link whose target cannot be read: " + err.Error()
	case err != nil:
		said = "it could not be read: " + err.Error()
	case !info.Mode().IsRegular():
		said = "it is not a regular file, so MMX does not read it"
	default:
		said, err = doNotManageText(path)
		if err != nil {
			said = "it could not be read: " + err.Error()
		}
	}
	return fmt.Sprintf("this node refuses management because %s exists; %s. MMX never creates or removes this file: whoever created it removes it when MMX may manage the node again", path, said)
}

// doNotManageText reads the start of the file as one line of plain text.
func doNotManageText(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, doNotManageTextLimit+1))
	if err != nil {
		return "", err
	}
	shortened := len(b) > doNotManageTextLimit
	if shortened {
		b = b[:doNotManageTextLimit]
	}
	var lines []string
	for _, line := range strings.Split(strings.ToValidUTF8(string(b), "?"), "\n") {
		line = strings.TrimSpace(strings.Map(func(r rune) rune {
			if unicode.IsControl(r) {
				return ' '
			}
			return r
		}, line))
		if line != "" {
			lines = append(lines, line)
		}
	}
	if len(lines) == 0 {
		return "the file is empty", nil
	}
	text := fmt.Sprintf("the file says %q", strings.Join(lines, " / "))
	if shortened {
		text += fmt.Sprintf(" (only its first %d bytes are shown)", doNotManageTextLimit)
	}
	return text, nil
}
