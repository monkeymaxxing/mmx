package node

import (
	"fmt"
	"path/filepath"

	"mmx/internal/modules"
	"mmx/internal/protocol"
)

// ReadFile answers a resume that reads a read_file secret again: the file's
// content, read as the read_file module reads it. It changes nothing and
// takes no node lock.
func (s Server) ReadFile(request protocol.ReadFileRequest) (protocol.ReadFileResponse, error) {
	if !filepath.IsAbs(request.Path) {
		return protocol.ReadFileResponse{}, fmt.Errorf("read-file requires an absolute file path on the node")
	}
	content, err := modules.ReadNodeFile(request.Path)
	if err != nil {
		return protocol.ReadFileResponse{}, err
	}
	return protocol.ReadFileResponse{Content: content}, nil
}
