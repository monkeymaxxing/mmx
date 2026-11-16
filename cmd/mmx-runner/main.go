package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"mmx/internal/assets"
	"mmx/internal/endpoint"
	"mmx/internal/moduleio"
	"mmx/internal/modules"
	"mmx/internal/node"
	"mmx/internal/protocol"
	"mmx/internal/release"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		// The module helper uses the same exit codes as mmx module-json.
		var commandError *moduleio.CommandError
		if errors.As(err, &commandError) {
			os.Exit(4)
		}
		os.Exit(1)
	}
}
func run() error {
	if len(os.Args) < 2 {
		return fmt.Errorf("mmx-runner requires a command: setup, serve, upload, diff, status, stop, worker, activate-change, run-owed-restore, module-json, version, or a node request that reads JSON from standard input (%s)", strings.Join(node.RequestNames(), ", "))
	}
	if os.Args[1] == "module-json" {
		return moduleio.Run(os.Args[2:], os.Stdin, os.Stdout)
	}
	bin, err := os.Executable()
	if err != nil {
		return err
	}
	s := node.Server{Root: "/var/lib/mmx", RuntimeRoot: node.RuntimeRoot, Binary: bin}
	var r protocol.Response
	switch os.Args[1] {
	case "activate-change", "run-owed-restore":
		id, err := owedRestoreCommandID(os.Args[1:])
		if err != nil {
			return err
		}
		if err := s.Initialize(); err != nil {
			return err
		}
		if os.Args[1] == "activate-change" {
			return s.ActivateChange(context.Background(), id)
		}
		return s.RunOwedRestore(context.Background(), id)
	case "setup":
		b, err := protocol.ReadLimited(os.Stdin, 64*1024, fmt.Errorf("setup request exceeds 64 KiB"))
		if err != nil {
			return err
		}
		var request endpoint.SetupRequest
		if err := protocol.Decode(b, &request); err != nil {
			return err
		}
		result, err := endpoint.Setup(context.Background(), s, request)
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(result)
	case "serve":
		if os.Geteuid() != 0 {
			return fmt.Errorf("mmx-runner serve must run as root on the node")
		}
		if len(os.Args) != 3 {
			return fmt.Errorf("mmx-runner serve requires the node SSH port as an argument")
		}
		port, err := strconv.Atoi(os.Args[2])
		if err != nil || port < 1 || port > 65535 {
			return fmt.Errorf("node SSH port must be an integer from 1 to 65535")
		}
		return endpoint.Serve(s, port)
	case "version":
		return json.NewEncoder(os.Stdout).Encode(map[string]any{"protocol": protocol.Version, "version": release.Version})
	case "upload":
		if len(os.Args) != 5 {
			return fmt.Errorf("mmx-runner upload requires a node job ID, the asset SHA-256 hash, and the asset size in bytes")
		}
		size, e := strconv.ParseInt(os.Args[4], 10, 64)
		if e != nil {
			return fmt.Errorf("upload asset size must be an integer number of bytes")
		}
		input, e := uploadStdin()
		if e != nil {
			return e
		}
		defer input.Close()
		err = s.Upload(context.Background(), os.Args[2], assets.Ref{SHA256: os.Args[3], Size: size}, uploadInput{input})
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(map[string]string{"status": "uploaded"})
	case "diff":
		if len(os.Args) != 5 {
			return fmt.Errorf("mmx-runner diff requires a node job ID, the difference file SHA-256 hash, and the difference file size in bytes")
		}
		size, err := strconv.ParseInt(os.Args[4], 10, 64)
		if err != nil {
			return err
		}
		f, err := s.OpenDiff(os.Args[2], assets.Ref{SHA256: os.Args[3], Size: size})
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = assets.Copy(context.Background(), os.Stdout, f)
		return err
	case "status", "stop", "worker":
		if len(os.Args) != 3 {
			return fmt.Errorf("mmx-runner status, stop, and worker require a node job ID")
		}
		if os.Args[1] == "worker" {
			// Admission passes the job's locked worker.lock as descriptor 3.
			return s.Worker(os.Args[2], os.NewFile(3, "worker.lock"))
		}
		if os.Args[1] == "stop" {
			r, err = s.Stop(os.Args[2])
		} else {
			r, err = s.Status(os.Args[2])
		}
	default:
		if len(os.Args) != 2 {
			return fmt.Errorf("mmx-runner %s reads its JSON request from standard input and takes no arguments", os.Args[1])
		}
		// The node HTTPS API process prepares the state directory when it
		// starts; a node SSH request process does so before a confirmation.
		if os.Args[1] == protocol.RequestConfirmReachability {
			if err := s.Initialize(); err != nil {
				return err
			}
		}
		err := answerOverSSH(context.Background(), s, os.Args[1], os.Stdin, os.Stdout)
		if errors.Is(err, node.ErrUnknownRequest) {
			return fmt.Errorf("unknown mmx-runner command %q", os.Args[1])
		}
		return err
	}
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(r.Transport())
}

// answerOverSSH carries out one JSON node request that the controller process
// sent over SSH, reading the request from input and writing the answer to
// output. The connection's addresses come from sshd's session on the node.
func answerOverSSH(ctx context.Context, s node.Server, name string, input io.Reader, output io.Writer) error {
	connection := modules.ControllerConnection{Kind: protocol.ConnectionSSH}
	if fields := strings.Fields(os.Getenv("SSH_CONNECTION")); len(fields) == 4 {
		connection.SourceAddress, connection.NodeLocalAddress = fields[0], fields[2]
	}
	answer, err := s.Answer(ctx, name, input, connection)
	if err != nil {
		return err
	}
	return json.NewEncoder(output).Encode(answer)
}

// Register inherited SSH stdin with Go's poller so idle reads can time out.
func uploadStdin() (*os.File, error) {
	fd, err := syscall.Dup(int(os.Stdin.Fd()))
	if err != nil {
		return nil, err
	}
	if err := syscall.SetNonblock(fd, true); err != nil {
		syscall.Close(fd)
		return nil, err
	}
	return os.NewFile(uintptr(fd), "asset upload"), nil
}

type uploadInput struct{ *os.File }

func (r uploadInput) Read(p []byte) (int, error) {
	if err := r.SetReadDeadline(time.Now().Add(assets.StallTimeout)); err != nil {
		return 0, err
	}
	return r.File.Read(p)
}

func owedRestoreCommandID(args []string) (string, error) {
	if len(args) != 3 || args[1] != "--owed-restore-id" {
		return "", fmt.Errorf("mmx-runner activate-change and run-owed-restore require --owed-restore-id followed by 16 lowercase hexadecimal characters")
	}
	if err := protocol.ValidateOwedRestoreID(args[2]); err != nil {
		return "", err
	}
	return args[2], nil
}
