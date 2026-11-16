package endpoint

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"mmx/internal/assets"
	"mmx/internal/differences"
)

func (c *Client) DownloadDiff(ctx context.Context, id string, ref assets.Ref, dir string) error {
	path := "/v1/diffs/" + id + "/" + ref.SHA256 + "/" + strconv.FormatInt(ref.Size, 10)
	r, err := http.NewRequestWithContext(ctx, "GET", c.URL+path, nil)
	if err != nil {
		return err
	}
	response, err := c.HTTP.Do(r)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return decodeResponse(response, nil)
	}
	if response.ContentLength != ref.Size {
		return fmt.Errorf("full difference response Content-Length does not match the recorded size")
	}
	return assets.Receive(ctx, dir, ref, response.Body)
}

func (s *Server) downloadDiff(w http.ResponseWriter, r *http.Request) {
	id, ref, err := parseAssetPath(strings.Replace(r.URL.Path, "/v1/diffs/", "/v1/assets/", 1))
	if err != nil || ref.Size > differences.Budget {
		http.Error(w, "invalid full difference reference", 400)
		return
	}
	f, err := s.Node.OpenDiff(id, ref)
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Length", strconv.FormatInt(ref.Size, 10))
	_, _ = assets.Copy(r.Context(), w, f)
}
