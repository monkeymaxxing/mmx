// Package access owns controller-only credentials. Values never enter inventory
// structs, node job bundles, or endpoint configuration.
package access

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"golang.org/x/sys/unix"
)

type Credentials struct {
	Secret      func(string) (string, error)
	Interactive bool
	Prompt      func(string) (string, error)
	Notify      func(string)
	Context     context.Context
	mu          sync.Mutex
	values      map[string]string
}

func (c *Credentials) Get(kind, target, env string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.values == nil {
		c.values = map[string]string{}
	}
	key := kind + "/" + target + "/" + env
	if env != "" {
		value, ok := os.LookupEnv(env)
		if !ok || value == "" {
			return "", fmt.Errorf("%s for %s requires a value in environment variable %s", kind, target, env)
		}
		if strings.ContainsAny(value, "\r\n\x00") {
			return "", fmt.Errorf("invalid %s in environment variable %s: line breaks and NUL bytes are not allowed", kind, env)
		}
		c.values[key] = value
		return value, nil
	}
	if value, ok := c.values[key]; ok {
		return value, nil
	}
	if !c.Interactive {
		return "", fmt.Errorf("%s for %s is required; configure an environment variable for the credential or use an SSH key or ssh-agent (prompts are disabled)", kind, target)
	}
	prompt := c.Prompt
	if prompt == nil {
		prompt = c.readPassword
	}
	value, err := prompt(kind + " for " + target + ": ")
	if err != nil {
		return "", err
	}
	if value == "" || strings.ContainsAny(value, "\r\n\x00") {
		return "", fmt.Errorf("%s must not be empty or contain line breaks or NUL bytes", kind)
	}
	c.values[key] = value
	return value, nil
}

// IsTerminal reports whether the file descriptor is a terminal: only a
// terminal has terminal settings to read.
func IsTerminal(fd int) bool {
	_, err := unix.IoctlGetTermios(fd, getTermios)
	return err == nil
}

func (c *Credentials) readPassword(label string) (string, error) {
	// A blocking tty read cannot reliably be interrupted by closing its descriptor
	// on macOS. Keep canonical terminal editing, but poll a nonblocking descriptor
	// so cancellation restores the terminal without leaving a reader behind.
	fd, err := unix.Open("/dev/tty", unix.O_RDWR|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return "", fmt.Errorf("%sno terminal is available for the credential prompt; configure an environment variable for the credential", label)
	}
	defer unix.Close(fd)
	state, err := unix.IoctlGetTermios(fd, getTermios)
	if err != nil {
		return "", err
	}
	hidden := *state
	hidden.Lflag &^= unix.ECHO
	hidden.Lflag |= unix.ICANON | unix.ISIG
	hidden.Iflag |= unix.ICRNL
	if err := unix.IoctlSetTermios(fd, setTermios, &hidden); err != nil {
		return "", err
	}
	// Failure to restore terminal settings only requires manual terminal restoration.
	defer func() { _ = unix.IoctlSetTermios(fd, setTermios, state) }()
	if _, err := unix.Write(fd, []byte(label)); err != nil {
		return "", err
	}
	// The newline only formats the terminal after password entry and carries no managed data.
	defer func() { _, _ = unix.Write(fd, []byte("\n")) }()
	ctx := c.Context
	if ctx == nil {
		ctx = context.Background()
	}
	var value []byte
	var buffer [4096]byte
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if _, err := unix.Poll([]unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}, 100); err != nil && err != unix.EINTR {
			return "", err
		}
		n, err := unix.Read(fd, buffer[:])
		if err == unix.EAGAIN || err == unix.EINTR {
			continue
		}
		if err != nil {
			return "", err
		}
		if n == 0 {
			return "", io.EOF
		}
		value = append(value, buffer[:n]...)
		if len(value) > 16384 {
			return "", fmt.Errorf("credential input exceeds the 16384-byte limit")
		}
		if i := strings.IndexByte(string(value), '\n'); i >= 0 {
			return string(value[:i]), nil
		}
	}
}

func (c *Credentials) Redact(message string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, value := range c.values {
		if value != "" {
			message = strings.ReplaceAll(message, value, "[redacted]")
		}
	}
	return message
}

// Askpass bridges native OpenSSH to an in-memory callback over a private local
// socket. Neither command arguments nor the environment contain the password.
func Askpass(dir string, get func(string) (string, error)) (string, func(), error) {
	path := filepath.Join(dir, "askpass")
	l, err := net.Listen("unix", path)
	if err != nil {
		return "", nil, err
	}
	if err := os.Chmod(path, 0600); err != nil {
		l.Close()
		return "", nil, err
	}
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			func() {
				defer conn.Close()
				var prompt string
				if json.NewDecoder(io.LimitReader(conn, 8192)).Decode(&prompt) != nil {
					return
				}
				value, err := get(prompt)
				result := askpassReply{Value: value}
				if err != nil {
					result.Error = err.Error()
				}
				_ = json.NewEncoder(conn).Encode(result)
			}()
		}
	}()
	return path, func() { l.Close() }, nil
}

type askpassReply struct {
	Value string
	Error string
}

func RunAskpass(path, prompt string) error {
	conn, err := net.Dial("unix", path)
	if err != nil {
		return err
	}
	defer conn.Close()
	if err := json.NewEncoder(conn).Encode(prompt); err != nil {
		return err
	}
	var reply askpassReply
	if err := json.NewDecoder(io.LimitReader(conn, 16384)).Decode(&reply); err != nil {
		return err
	}
	if reply.Error != "" {
		return fmt.Errorf("%s", reply.Error)
	}
	_, err = fmt.Fprintln(os.Stdout, reply.Value)
	return err
}

// ReadPassword reuses the cancellation-aware hidden prompt for vault input.
func (c *Credentials) ReadPassword(label string) (string, error) {
	if c.Prompt != nil {
		return c.Prompt(label)
	}
	return c.readPassword(label)
}
func (c *Credentials) Named(kind, target, name, env string) (string, error) {
	if name == "" || c.Secret == nil {
		return c.Get(kind, target, env)
	}
	v, err := c.Secret(name)
	if err != nil {
		return "", err
	}
	if v == "" || strings.ContainsAny(v, "\r\n\x00") {
		return "", fmt.Errorf("invalid %s from named secret %s: the credential must not be empty or contain line breaks or NUL bytes", kind, name)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.values == nil {
		c.values = map[string]string{}
	}
	c.values[kind+"/"+target+"/"+name] = v
	return v, nil
}
