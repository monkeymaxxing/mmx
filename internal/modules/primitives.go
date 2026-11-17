package modules

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"unicode/utf8"

	"mmx/internal/protocol"
	"mmx/internal/value"
)

// The primitives are the functions through which a module reads and changes
// the node. A Starlark module calls them under their snake_case names
// (read_file, write_file, ...); a core module calls these methods or the
// module code they are built on. A changing primitive never changes the node
// during a dryrun: it inspects the node and reports what it would change.
// Every primitive reports a failure as Error text instead of stopping its
// caller, so the module decides what the failure means.

// primitiveFileLimit is the most read_file returns and an HTTP response body
// holds.
const primitiveFileLimit = 1 << 20

// FileText is what read_file returns.
type FileText struct {
	Content string
	Error   string
}

// PathInfo is what inspect_path returns. Type is file, directory, link, or
// other; SHA256 is set for a file.
type PathInfo struct {
	Exists          bool
	Type            string
	Owner           string
	Group           string
	PermissionsMode string
	LinkTarget      string
	SHA256          string
	Error           string
}

// AccountInfo is what look_up_account returns.
type AccountInfo struct {
	Exists  bool
	UserID  int
	GroupID int
	Home    string
	Shell   string
	Groups  []string
	Error   string
}

// CommandOutput is what run_command and run_command_that_changes_the_node
// return. Ran says that the program started: it is false when a dryrun did
// not run a command that changes the node, and when the program could not
// start.
type CommandOutput struct {
	Ran            bool
	ExitCode       int
	StandardOutput string
	StandardError  string
	Error          string
	// ProgramNotFound: the program is not on the node, and Error says so.
	ProgramNotFound bool
}

// HTTPResponse is what http_request returns. Sent says that the whole request
// reached the connection, also when its answer then failed; it is false when
// a dryrun did not send a request that changes something.
type HTTPResponse struct {
	Sent           bool
	HTTPStatusCode int
	Headers        map[string]string
	Body           string
	Error          string
}

// PathChange is what a changing primitive on a path returns: whether the call
// changed the node, or in a dryrun whether it would.
type PathChange struct {
	Changed bool
	// Diff is the difference of a file's content, when it changes.
	Diff  string
	Error string
}

// ReadFile reads a text file of at most 1 MiB.
func (run Runtime) ReadFile(path string) FileText {
	if !filepath.IsAbs(path) {
		return FileText{Error: "the path must be absolute"}
	}
	f, err := os.Open(path)
	if err != nil {
		return FileText{Error: err.Error()}
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, primitiveFileLimit+1))
	switch {
	case err != nil:
		return FileText{Error: err.Error()}
	case len(b) > primitiveFileLimit:
		return FileText{Error: path + " holds more than 1 MiB"}
	case !utf8.Valid(b):
		return FileText{Error: path + " is not a UTF-8 text file"}
	}
	return FileText{Content: string(b)}
}

// InspectPath looks at a path without following a final link.
func (run Runtime) InspectPath(path string) PathInfo {
	if !filepath.IsAbs(path) {
		return PathInfo{Error: "the path must be absolute"}
	}
	st, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return PathInfo{}
	}
	if err != nil {
		return PathInfo{Error: err.Error()}
	}
	info := PathInfo{Exists: true, Type: "other", PermissionsMode: fmt.Sprintf("%04o", st.Mode().Perm())}
	if sys, ok := st.Sys().(*syscall.Stat_t); ok {
		info.Owner, info.Group = userAccountName(sys.Uid), groupName(sys.Gid)
	}
	switch {
	case st.Mode().IsRegular():
		info.Type = "file"
		f, err := os.Open(path)
		if err != nil {
			info.Error = err.Error()
			return info
		}
		defer f.Close()
		h := sha256.New()
		if _, err := io.Copy(h, f); err != nil {
			info.Error = err.Error()
			return info
		}
		info.SHA256 = hex.EncodeToString(h.Sum(nil))
	case st.IsDir():
		info.Type = "directory"
	case st.Mode()&os.ModeSymlink != 0:
		info.Type = "link"
		if info.LinkTarget, err = os.Readlink(path); err != nil {
			info.Error = err.Error()
		}
	}
	return info
}

func userAccountName(id uint32) string {
	if u, err := user.LookupId(strconv.Itoa(int(id))); err == nil {
		return u.Username
	}
	return strconv.Itoa(int(id))
}

func groupName(id uint32) string {
	if g, err := user.LookupGroupId(strconv.Itoa(int(id))); err == nil {
		return g.Name
	}
	return strconv.Itoa(int(id))
}

// LookUpAccount reads an account and the groups it belongs to.
func (run Runtime) LookUpAccount(name string) AccountInfo {
	u, err := user.Lookup(name)
	var unknown user.UnknownUserError
	if errors.As(err, &unknown) {
		return AccountInfo{}
	}
	if err != nil {
		return AccountInfo{Error: err.Error()}
	}
	info := AccountInfo{Exists: true, Home: u.HomeDir}
	info.UserID, _ = strconv.Atoi(u.Uid)
	info.GroupID, _ = strconv.Atoi(u.Gid)
	if fields, err := account(context.Background(), "passwd", name); err == nil && len(fields) >= 7 {
		info.Shell = fields[6]
	}
	ids, err := u.GroupIds()
	if err != nil {
		info.Error = err.Error()
		return info
	}
	for _, id := range ids {
		n, _ := strconv.Atoi(id)
		info.Groups = append(info.Groups, groupName(uint32(n)))
	}
	return info
}

// SHA256 hashes text and returns the hash in hexadecimal.
func (run Runtime) SHA256(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// commandEnvironment is the fixed environment of a command that MMX runs for
// a module, as a shell task's without an account.
var commandEnvironment = map[string]string{"PATH": "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", "LANG": "C.UTF-8", "USER": "root"}

// RootLoginEnvironment is LOGNAME and root's home directory as HOME. The
// node job worker process has them in its environment, so every command that
// a core module runs as root inherits them, and a tool finds root's settings.
func RootLoginEnvironment() []string {
	home := "/root"
	if root, err := user.Lookup("root"); err == nil && root.HomeDir != "" {
		home = root.HomeDir
	}
	return []string{"LOGNAME=root", "HOME=" + home}
}

// primitiveCommandEnvironment is the whole environment of a command that a
// primitive runs: commandEnvironment, LOGNAME, and root's home directory as
// HOME. The node job worker process itself has no HOME, and a tool that
// keeps its settings in the home directory, such as a client program, must
// find root's.
func primitiveCommandEnvironment() []string {
	environment := RootLoginEnvironment()
	for name, value := range commandEnvironment {
		environment = append(environment, name+"="+value)
	}
	return environment
}

// EarlierNodeTaskWouldChange reports, in a dryrun, that an earlier node task
// of the node job would create or change path: the path itself, a directory
// that holds it, or something inside it. A module that reads a file which
// another node task writes asks this: what the file holds now is then not
// what a run would find. It is false in a run, where earlier node tasks have
// done their work.
func (run Runtime) EarlierNodeTaskWouldChange(path string) bool {
	return run.Dryrun && run.PreviewPaths.WouldChange(path)
}

// ProgramLocation is what find_program returns.
type ProgramLocation struct {
	Found bool
	Path  string
	Error string
}

// FindProgram looks a program up as a primitive's command would find it: by
// its name on the node job worker process's PATH, or by its path. A module
// asks before its first command, so that a missing tool is a missing
// prerequisite and not a failed command.
func (run Runtime) FindProgram(ctx context.Context, name string) ProgramLocation {
	if name == "" {
		return ProgramLocation{Error: "the program needs a name"}
	}
	path, err := lookPath(ctx, name)
	if errors.Is(err, exec.ErrNotFound) || errors.Is(err, os.ErrNotExist) {
		return ProgramLocation{}
	}
	if err != nil {
		return ProgramLocation{Error: err.Error()}
	}
	return ProgramLocation{Found: true, Path: path}
}

// RunCommand runs a program that only reads, as root, with no shell. MMX
// cannot see what a program does, so it runs this also during a dryrun; the
// module keeps such programs read only.
func (run Runtime) RunCommand(ctx context.Context, arguments []string, standardInput string) CommandOutput {
	return runPrimitiveCommand(ctx, arguments, standardInput)
}

// RunCommandThatChangesTheNode runs a program that changes the node, as root,
// with no shell. During a dryrun it does not run, and Ran is false.
func (run Runtime) RunCommandThatChangesTheNode(ctx context.Context, arguments []string, standardInput string) CommandOutput {
	if len(arguments) == 0 {
		return CommandOutput{Error: "the command needs a program to run"}
	}
	if run.Dryrun {
		return CommandOutput{}
	}
	if err := run.Mutation("command " + arguments[0]); err != nil {
		return CommandOutput{Error: err.Error()}
	}
	return runPrimitiveCommand(ctx, arguments, standardInput)
}

// RunCommandThatRefreshesAPackageIndex runs a program that downloads a
// package manager's package index, such as pacman -Sy, as root, with no
// shell. The index is a cache of what the package servers offer: refreshing it
// changes no package, so it is no change of the task, as apt_package's index
// refresh is not. During a dryrun it does not run, and Ran is false: a dryrun
// refreshes nothing.
func (run Runtime) RunCommandThatRefreshesAPackageIndex(ctx context.Context, arguments []string) CommandOutput {
	if len(arguments) == 0 {
		return CommandOutput{Error: "the command needs a program to run"}
	}
	if run.Dryrun {
		return CommandOutput{}
	}
	if err := run.Mutation("package index " + arguments[0]); err != nil {
		return CommandOutput{Error: err.Error()}
	}
	return runPrimitiveCommand(ctx, arguments, "")
}

func runPrimitiveCommand(ctx context.Context, arguments []string, standardInput string) CommandOutput {
	if len(arguments) == 0 {
		return CommandOutput{Error: "the command needs a program to run"}
	}
	p := runProcessWithOnlyThisEnvironment(ctx, primitiveCommandEnvironment(), "/", strings.NewReader(standardInput), nil, arguments[0], arguments[1:]...)
	out := CommandOutput{Ran: p.Started, ExitCode: p.Result.ExitCode, StandardOutput: p.Result.Stdout, StandardError: p.Result.Stderr, ProgramNotFound: p.ProgramNotFound}
	// A read failure fails the command whatever its exit code: the output
	// is not all that the program wrote.
	if p.Result.ExitCode < 0 || p.OutputReadFailure != nil {
		out.Error = p.Result.Reason
	}
	return out
}

// HTTPRequest sends one HTTP or HTTPS request. It follows no redirect,
// always verifies an HTTPS certificate, and never writes the address into its
// error, because an address can hold a secret. A request with a method other
// than GET and HEAD changes something: a dryrun does not send it.
func (run Runtime) HTTPRequest(ctx context.Context, method, address string, headers map[string]string, body, certificateAuthorityFile string) HTTPResponse {
	method = strings.ToUpper(method)
	reads := method == http.MethodGet || method == http.MethodHead
	parsed, err := url.Parse(address)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return HTTPResponse{Error: "the HTTP request URL must be a valid http:// or https:// URL with a host name"}
	}
	if !reads {
		if run.Dryrun {
			return HTTPResponse{}
		}
		if err := run.Mutation("http request to " + parsed.Hostname()); err != nil {
			return HTTPResponse{Error: err.Error()}
		}
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if certificateAuthorityFile != "" {
		pem, err := os.ReadFile(certificateAuthorityFile)
		if err != nil {
			return HTTPResponse{Error: "read the HTTP request certificate authority file: " + err.Error()}
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return HTTPResponse{Error: "the HTTP request certificate authority file contains no PEM certificate"}
		}
		transport.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
	client := &http.Client{Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	request, err := http.NewRequestWithContext(ctx, method, address, bytes.NewReader([]byte(body)))
	if err != nil {
		return HTTPResponse{Error: "the request could not be made for " + parsed.Hostname()}
	}
	for k, v := range headers {
		request.Header.Set(k, v)
	}
	// A request that reached the connection may have changed something,
	// even when its answer fails, such as at the node task's time limit.
	var sent atomic.Bool
	request = request.WithContext(httptrace.WithClientTrace(request.Context(), &httptrace.ClientTrace{
		WroteRequest: func(written httptrace.WroteRequestInfo) {
			if written.Err == nil {
				sent.Store(true)
			}
		},
	}))
	response, err := client.Do(request)
	if err != nil {
		return HTTPResponse{Sent: sent.Load(), Error: "the request to " + parsed.Hostname() + " failed: " + withoutAddress(err)}
	}
	defer response.Body.Close()
	b, err := io.ReadAll(io.LimitReader(response.Body, primitiveFileLimit+1))
	out := HTTPResponse{Sent: true, HTTPStatusCode: response.StatusCode, Headers: map[string]string{}}
	for k := range response.Header {
		out.Headers[strings.ToLower(k)] = response.Header.Get(k)
	}
	switch {
	case err != nil:
		out.Error = "reading the answer of " + parsed.Hostname() + " failed: " + withoutAddress(err)
	case len(b) > primitiveFileLimit:
		out.Error = "the answer of " + parsed.Hostname() + " holds more than 1 MiB"
	default:
		out.Body = string(b)
	}
	return out
}

// withoutAddress is an error's text without the URL that Go's HTTP client
// writes into it.
func withoutAddress(err error) string {
	var u *url.Error
	if errors.As(err, &u) {
		return u.Err.Error()
	}
	return err.Error()
}

// WriteFile makes path hold content with that owner, group, and mode,
// through the file module: a new file beside the old one, renamed into place,
// with the SELinux label of its path.
func (run Runtime) WriteFile(ctx context.Context, path, content, owner, group, permissionsMode string) PathChange {
	if !filepath.IsAbs(path) {
		return PathChange{Error: "the path must be absolute"}
	}
	f := File{Path: filepath.Clean(path), Literal: &content, Owner: Account(owner), Group: Account(group), Mode: permissionsMode}
	if err := ValidateFile(f); err != nil {
		return PathChange{Error: err.Error()}
	}
	return pathChange(run.File(ctx, f, value.Scope{}))
}

// MakeDirectory makes path a directory with that owner, group, and mode,
// through the directory module; the parent must exist.
func (run Runtime) MakeDirectory(ctx context.Context, path, owner, group, permissionsMode string) PathChange {
	if !filepath.IsAbs(path) {
		return PathChange{Error: "the path must be absolute"}
	}
	d := Directory{Path: filepath.Clean(path), Owner: Account(owner), Group: Account(group), Mode: permissionsMode}
	if err := ValidateDirectory(d); err != nil {
		return PathChange{Error: err.Error()}
	}
	return pathChange(run.Directory(ctx, d, value.Scope{}))
}

// MakeLink makes path a symbolic link to target, through the symlink module.
func (run Runtime) MakeLink(ctx context.Context, path, target string) PathChange {
	if !filepath.IsAbs(path) {
		return PathChange{Error: "the path must be absolute"}
	}
	l := Symlink{Path: filepath.Clean(path), Target: target}
	if err := ValidateSymlink(l); err != nil {
		return PathChange{Error: err.Error()}
	}
	return pathChange(run.Symlink(ctx, l))
}

// RemovePath removes a file, a link, or an empty directory.
func (run Runtime) RemovePath(ctx context.Context, path string) PathChange {
	if !filepath.IsAbs(path) || filepath.Clean(path) == "/" {
		return PathChange{Error: "the path must be absolute and not /"}
	}
	st, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return PathChange{}
	}
	if err != nil {
		return PathChange{Error: err.Error()}
	}
	if st.IsDir() {
		entries, err := os.ReadDir(path)
		if err != nil {
			return PathChange{Error: err.Error()}
		}
		if len(entries) > 0 {
			return PathChange{Error: path + " is a directory that is not empty"}
		}
	}
	if run.Dryrun {
		return PathChange{Changed: true}
	}
	if err := run.Mutation("remove " + path); err != nil {
		return PathChange{Error: err.Error()}
	}
	if err := os.Remove(path); err != nil {
		return PathChange{Error: err.Error()}
	}
	return PathChange{Changed: true}
}

// pathChange reads a module result as a primitive's answer.
func pathChange(r protocol.Result) PathChange {
	if r.Status != "succeeded" {
		return PathChange{Changed: r.Change == "changed", Error: r.Reason}
	}
	change := PathChange{Changed: r.Change == "changed" || r.Change == "would_change"}
	if r.FileChange != nil {
		change.Diff = r.FileChange.Diff
	}
	return change
}
