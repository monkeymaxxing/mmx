package endpoint

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"mmx/internal/assets"
)

func assetPath(id string, ref assets.Ref) string {
	return "/v1/assets/" + id + "/" + ref.SHA256 + "/" + strconv.FormatInt(ref.Size, 10)
}
func parseAssetPath(path string) (string, assets.Ref, error) {
	parts := strings.Split(strings.TrimPrefix(path, "/v1/assets/"), "/")
	if len(parts) != 3 {
		return "", assets.Ref{}, fmt.Errorf("invalid asset path")
	}
	size, err := strconv.ParseInt(parts[2], 10, 64)
	ref := assets.Ref{SHA256: parts[1], Size: size}
	if err != nil {
		return "", ref, fmt.Errorf("invalid asset size")
	}
	return parts[0], ref, ref.Validate()
}
func (c *Client) Upload(ctx context.Context, id string, ref assets.Ref, input io.ReadSeeker, progress func(int64) error) error {
	path := assetPath(id, ref)
	// The connection is authenticated before any request, so the node refuses
	// an unauthenticated upload without reading its body.
	if _, err := input.Seek(0, io.SeekStart); err != nil {
		return err
	}
	transfer, reader, finish := assets.Transfer(ctx, input, assets.StallTimeout, progress)
	defer finish()
	request, err := http.NewRequestWithContext(transfer, "PUT", c.URL+path, reader)
	if err != nil {
		return err
	}
	request.ContentLength = ref.Size
	if ref.Size == 0 {
		request.Body = http.NoBody
	}
	request.Header.Set("Content-Type", "application/octet-stream")
	request.Header.Set("Expect", "100-continue")
	client := *c.HTTP
	client.Timeout = 0
	response, err := client.Do(request)
	if transfer.Err() != nil {
		return context.Cause(transfer)
	}
	if err != nil {
		return err
	}
	defer response.Body.Close()
	var result struct {
		Status string `json:"status"`
	}
	if err := decodeResponse(response, &result); err != nil {
		return err
	}
	if result.Status != "uploaded" {
		return fmt.Errorf("node did not confirm the file upload")
	}
	return nil
}

type deadlineReader struct {
	input   io.Reader
	control *http.ResponseController
}

func (r deadlineReader) Read(p []byte) (int, error) {
	if err := r.control.SetReadDeadline(time.Now().Add(assets.StallTimeout)); err != nil {
		return 0, err
	}
	return r.input.Read(p)
}
func (s *Server) upload(w http.ResponseWriter, r *http.Request) error {
	id, ref, err := parseAssetPath(r.URL.Path)
	if err != nil {
		return err
	}
	if r.ContentLength != ref.Size {
		return fmt.Errorf("file upload Content-Length does not match the declared file size")
	}
	control := http.NewResponseController(w)
	if err := control.SetWriteDeadline(time.Time{}); err != nil {
		return err
	}
	// Clearing the request read deadline is cleanup after the upload result is determined.
	defer func() { _ = control.SetReadDeadline(time.Time{}) }()
	return s.Node.Upload(r.Context(), id, ref, deadlineReader{r.Body, control})
}
