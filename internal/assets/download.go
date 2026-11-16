package assets

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func ValidateDownloadURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Fragment != "" || strings.Contains(raw, "{{") {
		return fmt.Errorf("download requires an HTTPS URL with a host name and without a {{ }} reference, credentials, or a fragment (#)")
	}
	return nil
}

// ValidDigest reports whether digest is a SHA-256 as 64 hexadecimal digits.
func ValidDigest(digest string) bool { return digestPattern.MatchString(strings.ToLower(digest)) }

func ValidateDownload(raw, digest string) error {
	if err := ValidateDownloadURL(raw); err != nil {
		return err
	}
	if !digestPattern.MatchString(strings.ToLower(digest)) {
		return fmt.Errorf("download.sha256 requires 64 hexadecimal digits")
	}
	return nil
}

// Download publishes only verified bytes, using the same private asset store as
// local sources. Size is discovered while streaming, never from a HEAD request.
func Download(ctx context.Context, dir, raw, digest string) (Ref, error) {
	if err := ValidateDownload(raw, digest); err != nil {
		return Ref{}, err
	}
	return download(ctx, dir, raw, digest, http.DefaultTransport, StallTimeout)
}

// Reuse links the file with the pinned digest that an earlier run saved in one
// of earlierDirs into dir, and checks its bytes again. It reports false when
// no earlier file passes, such as none saved, a link across filesystems, or
// changed bytes; the caller then fetches the file.
func Reuse(ctx context.Context, dir string, earlierDirs []string, digest string) (Ref, bool) {
	digest = strings.ToLower(digest)
	if !ValidDigest(digest) || os.MkdirAll(dir, 0700) != nil {
		return Ref{}, false
	}
	target := filepath.Join(dir, digest)
	for _, earlier := range earlierDirs {
		created := true
		if err := os.Link(filepath.Join(earlier, digest), target); err != nil {
			if !os.IsExist(err) {
				continue
			}
			created = false // this run saved it already
		}
		if st, err := os.Lstat(target); err == nil {
			ref := Ref{SHA256: digest, Size: st.Size(), Path: target}
			if Verify(ctx, ref) == nil {
				return ref, true
			}
		}
		if !created {
			return Ref{}, false
		}
		os.Remove(target)
	}
	return Ref{}, false
}

// DownloadUnchecked saves the file as it arrives, for a task that says
// dangerously_skip_checksum: true. The saved bytes are then fixed for the run
// by their own SHA-256, like every other asset.
func DownloadUnchecked(ctx context.Context, dir, raw string) (Ref, error) {
	return download(ctx, dir, raw, "", http.DefaultTransport, StallTimeout)
}

// download compares the bytes with digest unless digest is empty.
func download(ctx context.Context, dir, raw, digest string, transport http.RoundTripper, idle time.Duration) (Ref, error) {
	if err := ValidateDownloadURL(raw); err != nil {
		return Ref{}, err
	}
	client := &http.Client{Transport: transport, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return fmt.Errorf("download stopped after reaching the limit of 10 redirects")
		}
		return ValidateDownloadURL(req.URL.String())
	}}
	// Start the idle timer before connection setup and headers. Bind the body
	// before the first read; cancellation interrupts both requests and body reads.
	var input struct{ io.Reader }
	transfer, reader, finish := Transfer(ctx, &input, idle, nil)
	defer finish()
	req, err := http.NewRequestWithContext(transfer, http.MethodGet, raw, nil)
	if err != nil {
		return Ref{}, err
	}
	req.Header.Set("Accept-Encoding", "identity")
	resp, err := client.Do(req)
	if err != nil {
		if transfer.Err() != nil {
			return Ref{}, context.Cause(transfer)
		}
		return Ref{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Ref{}, fmt.Errorf("download returned HTTP %d", resp.StatusCode)
	}
	input.Reader = resp.Body
	ref, err := write(transfer, dir, reader, strings.ToLower(digest), nil)
	if transfer.Err() != nil {
		return Ref{}, context.Cause(transfer)
	}
	return ref, err
}
