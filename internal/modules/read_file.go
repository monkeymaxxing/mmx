package modules

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"syscall"
	"unicode/utf8"

	"mmx/internal/protocol"
	"mmx/internal/value"
)

// ReadFile reads one file for a later node task or a fleet stage output. It
// changes nothing, so it runs during dryrun too. A missing file fails the task,
// and in dryrun is not checked, since an earlier task may still create it. A
// secret content records the file it came from, so a resume can read it again.
func (run Runtime) readFileTask(ctx context.Context, r ReadFile) protocol.Result {
	if err := ValidateReadFile(r); err != nil {
		return protocol.ConfigurationError(err)
	}
	content, err := ReadNodeFile(r.Path)
	if errors.Is(err, os.ErrNotExist) {
		return run.notYet(err)
	}
	if err != nil {
		return protocol.Failed(err)
	}
	result := protocol.Success("unchanged")
	result.Content = content
	if *r.ContentIsSecret {
		result.Content = &value.Secret{Data: content, Available: true, FromNode: &value.NodeOrigin{ReadFilePath: r.Path}}
	}
	return result
}

// ReadNodeFile returns the content of the regular file at path, following
// links, up to ReadFileLimit: a string when it is UTF-8 text, otherwise
// value.Binary. For a missing file, errors.Is(err, os.ErrNotExist) holds. The
// read_file module and a resume that reads a read_file secret again both use it.
func ReadNodeFile(path string) (any, error) {
	// O_NONBLOCK: opening a named pipe must not wait for a writer.
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
		return nil, fmt.Errorf("%w: %s", os.ErrNotExist, path)
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	tooLarge := fmt.Errorf("read_file reads files up to %d MiB; %s is larger", ReadFileLimit>>20, path)
	if info.Size() > ReadFileLimit {
		return nil, tooLarge
	}
	b, err := io.ReadAll(io.LimitReader(file, ReadFileLimit+1))
	if err != nil {
		return nil, err
	}
	if len(b) > ReadFileLimit {
		return nil, tooLarge
	}
	if !utf8.Valid(b) {
		return value.Binary{Bytes: b}, nil
	}
	return string(b), nil
}

// ReadFile reads one file on the node so that a later node task, or through a
// fleet stage output another node, can use its content.
type ReadFile struct {
	Path string `yaml:"path" json:"path"`
	// ContentIsSecret is required, so every read_file task says whether its
	// content may appear in output and saved results.
	ContentIsSecret *bool `yaml:"content_is_secret" json:"content_is_secret"`
}

// ReadFileLimit is the largest file read_file reads: 1 MiB.
const ReadFileLimit = 1 << 20

func ValidateReadFile(r ReadFile) error {
	if r.Path == "" {
		return fmt.Errorf("read_file.path is required")
	}
	if r.ContentIsSecret == nil {
		return invalid("content_is_secret", "read_file.content_is_secret is required: true when the file holds a secret, such as a token or a key, so its content never appears in output or saved results; false otherwise")
	}
	return nil
}

// readFileDescription is what mmx describe-module shows for read_file.
var readFileDescription = Description{
	Name: "read_file", Topic: TopicFilesAndLinks, Schema: reflect.TypeOf(ReadFile{}),
	Summary: "reads one file on the node, so a later node task or, through a fleet stage output, another node can use its content",
	Fields: []Field{
		{Name: "path", Required: true, Notes: "absolute path of a regular file up to 1 MiB; links are followed"},
		{Name: "content_is_secret", Required: true, Notes: "true when the file holds a secret, such as a token or a key: the content is never printed or saved, an output made from it is secret too, and mmx resume reads the file again from this node when later work needs it; false otherwise"},
	},
	Example: "- id: join_token\n  read_file:\n    path: /var/lib/rancher/k3s/server/token\n    content_is_secret: true\n",
	Results: []string{"status", "change", "content"},
	Dryrun:  "runs; changes nothing and reports unchanged; a missing file reports not_checked, since an earlier node task may still create it",
}

// Prepare checks the inputs when the project loads.
func (r *ReadFile) Prepare(base string) error {
	return ValidateReadFile(*r)
}

// Arguments lists the inputs that may hold a reference, with their rules.
func (r *ReadFile) Arguments(arguments *ArgumentList) {
	arguments.Public("path", &r.Path, absolute)
}

// ResultIsSecret reports a task whose result is secret whatever it reads:
// content_is_secret: true.
func (r *ReadFile) ResultIsSecret() bool {
	return r.ContentIsSecret != nil && *r.ContentIsSecret
}

// Run does the module's work on the node.
func (r *ReadFile) Run(ctx context.Context, task NodeTask) protocol.Result {
	return task.Runtime.readFileTask(ctx, *r)
}
