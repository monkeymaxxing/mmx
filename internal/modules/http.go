package modules

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"mmx/internal/protocol"
	"mmx/internal/value"
)

// httpRequest is one request with its private values resolved.
type httpRequest struct {
	Method                                string
	URL                                   string
	Headers                               map[string]string
	Body                                  []byte
	DangerouslySkipServerCertificateCheck bool
	TrustedCertificateAuthorityFile       string
	ExpectedHTTPStatusCodes               []int
}

// SendHTTPRequest sends one GET and checks the response status, as wait_for
// does.
func SendHTTPRequest(ctx context.Context, h HTTP) protocol.Result {
	return sendHTTPRequest(ctx, httpRequest{Method: http.MethodGet, URL: h.URL, ExpectedHTTPStatusCodes: h.ExpectedHTTPStatusCodes, TrustedCertificateAuthorityFile: h.TrustedCertificateAuthorityFile, DangerouslySkipServerCertificateCheck: h.DangerouslySkipServerCertificateCheck})
}

func sendHTTPRequest(ctx context.Context, h httpRequest) protocol.Result {
	failed := func(err error) protocol.Result {
		r := protocol.Failed(err)
		r.Change = "unchanged"
		return r
	}
	req, err := http.NewRequestWithContext(ctx, h.Method, h.URL, bytes.NewReader(h.Body))
	if err != nil {
		return failed(err)
	}
	if h.Body == nil {
		req.Body, req.ContentLength = http.NoBody, 0
	}
	for name, v := range h.Headers {
		// Go's client sends req.Host and ignores a Host entry in the
		// headers, so a check of a virtual host would reach the default one.
		if strings.EqualFold(name, "Host") {
			req.Host = v
			continue
		}
		req.Header.Set(name, v)
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: h.DangerouslySkipServerCertificateCheck} // explicit task choice removes server identity verification
	defer transport.CloseIdleConnections()
	if h.TrustedCertificateAuthorityFile != "" {
		pem, err := os.ReadFile(h.TrustedCertificateAuthorityFile)
		if err != nil {
			return failed(fmt.Errorf("reading trusted_certificate_authority_file: %w", err))
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return failed(fmt.Errorf("trusted_certificate_authority_file %s holds no PEM certificate", h.TrustedCertificateAuthorityFile))
		}
		transport.TLSClientConfig.RootCAs = pool
	}
	c := http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := c.Do(req)
	if err != nil {
		var certificateError *tls.CertificateVerificationError
		if errors.As(err, &certificateError) {
			err = fmt.Errorf("%w. For a private authority or a self-signed certificate, give trusted_certificate_authority_file; dangerously_skip_server_certificate_check: true turns off the check of the server's identity", err)
		}
		return failed(err)
	}
	defer res.Body.Close()
	return readHTTPResponse(res, h.ExpectedHTTPStatusCodes)
}

// readHTTPResponse keeps the bytes read even when the response body ends with
// an error. A separate reader lets tests control that interruption precisely.
func readHTTPResponse(res *http.Response, expected []int) protocol.Result {
	var body capBuffer
	_, readErr := io.Copy(&body, io.LimitReader(res.Body, outputLimit+1))
	r := protocol.Success("unchanged")
	r.HTTPStatusCode = res.StatusCode
	r.ResponseBody, r.ResponseBodyTruncated = body.Text(), body.truncated
	if readErr != nil {
		r.Status = "failed"
		r.Reason = fmt.Sprintf("HTTP status %d; reading response_body: %v", res.StatusCode, readErr)
		return r
	}
	if expected == nil {
		expected = []int{200}
	}
	if !slices.Contains(expected, res.StatusCode) {
		r.Status = "failed"
		codes := make([]string, len(expected))
		for i, code := range expected {
			codes[i] = strconv.Itoa(code)
		}
		r.Reason = fmt.Sprintf("HTTP status %d; expected %s", res.StatusCode, strings.Join(codes, " or "))
	}
	return r
}

// HTTP is one request from the node. The headers, the basic authentication,
// and the body are private values the module resolves.
type HTTP struct {
	DangerouslySkipServerCertificateCheck bool           `yaml:"dangerously_skip_server_certificate_check,omitempty" json:"dangerously_skip_server_certificate_check,omitempty"`
	URL                                   string         `yaml:"url" json:"url"`
	Method                                string         `yaml:"method,omitempty" json:"method,omitempty"`
	ExpectedHTTPStatusCodes               []int          `yaml:"expected_http_status_codes,omitempty" json:"expected_http_status_codes,omitempty"`
	Headers                               map[string]any `yaml:"headers,omitempty" json:"headers,omitempty"`
	BasicAuthUsername                     string         `yaml:"basic_auth_username,omitempty" json:"basic_auth_username,omitempty"`
	BasicAuthPassword                     string         `yaml:"basic_auth_password,omitempty" json:"basic_auth_password,omitempty"`
	JSONBody                              any            `yaml:"json_body,omitempty" json:"json_body,omitempty"`
	Body                                  string         `yaml:"body,omitempty" json:"body,omitempty"`
	TrustedCertificateAuthorityFile       string         `yaml:"trusted_certificate_authority_file,omitempty" json:"trusted_certificate_authority_file,omitempty"`
	ChangedWhen                           any            `yaml:"changed_when,omitempty" json:"changed_when,omitempty"`
}

var (
	httpMethods        = []string{http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete}
	httpWritingMethods = []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete}
	// A header name is an HTTP token.
	httpHeaderName = regexp.MustCompile("^[!#$%&'*+.^_`|~0-9A-Za-z-]+$")
)

// method is the request's method; GET when the task names none.
func (h *HTTP) method() string {
	if h.Method == "" {
		return http.MethodGet
	}
	return h.Method
}

// writes reports a request that changes something on the other side.
func (h *HTTP) writes() bool {
	return slices.Contains(httpWritingMethods, h.method())
}

// httpDescription is what mmx describe-module shows for http.
var httpDescription = Description{
	Name: "http", Topic: TopicCommandsChecksAndReboot, Schema: reflect.TypeOf(HTTP{}),
	Summary: "one HTTP or HTTPS request from the node that checks the response status: a read, or a write to an application's API",
	Fields: []Field{
		{Name: "url", Required: true, Notes: "HTTP or HTTPS; redirects are not followed; bounded by the node task time_limit_per_try and time_limit"},
		{Name: "method", Values: httpMethods, Default: "GET", Notes: "POST, PUT, PATCH, and DELETE are sent once on every run unless a condition skips the task, and never during a dryrun"},
		{Name: "expected_http_status_codes", Default: "[200]", Notes: "the list of accepted HTTP status codes"},
		{Name: "headers", Notes: "header name to value; write a token as a secret reference: a secret in a value hides the task's result, a value written literally does not"},
		{Name: "basic_auth_username", Notes: "HTTP basic authentication; with basic_auth_password"},
		{Name: "basic_auth_password", Notes: "a private value, normally a secret"},
		{Name: "json_body", Type: "any YAML value", Notes: "sent as JSON with Content-Type application/json; only with POST, PUT, PATCH, or DELETE; not with body"},
		{Name: "body", Notes: "text sent as written; only with POST, PUT, PATCH, or DELETE; not with json_body"},
		{Name: "trusted_certificate_authority_file", Notes: "absolute path on the node of a certificate authority's certificate, or a self-signed certificate, to trust for this request"},
		{Name: "dangerously_skip_server_certificate_check", Default: "false", Notes: "true removes verification of the HTTPS server identity; prefer trusted_certificate_authority_file for a private authority"},
		{Name: "changed_when", Type: "boolean or predicate", Default: "false for GET and HEAD, true for the other methods", Notes: "reads .this_node_task.http_status_code, .this_node_task.response_body, .this_node_task.response_body_json"},
	},
	Example: "- http:\n    url: http://127.0.0.1:8080/health\n",
	Results: []string{"status", "change", "http_status_code", "response_body", "response_body_json"},
	Dryrun:  "sends GET and HEAD, except inside a task block with proposed changes, where it is skipped; a GET or HEAD that fails after an earlier node task of the dryrun would change the node is not_checked, with the answer kept; never sends another method and reports it would_change",
}

// Prepare checks the inputs when the project loads.
func (h *HTTP) Prepare(base string) error {
	if h.Method != "" && !slices.Contains(httpMethods, h.Method) {
		return invalid("method", "method must be one of "+strings.Join(httpMethods, ", ")+", written in capitals")
	}
	if h.JSONBody != nil && h.Body != "" {
		return invalid("body", "set only json_body or body, not json_body and body together")
	}
	if (h.JSONBody != nil || h.Body != "") && !h.writes() {
		return invalid("method", "a request with json_body or body needs method POST, PUT, PATCH, or DELETE")
	}
	if (h.BasicAuthUsername == "") != (h.BasicAuthPassword == "") {
		return invalid("basic_auth_password", "set basic_auth_username and basic_auth_password together, or omit basic_auth_username and basic_auth_password")
	}
	for name := range h.Headers {
		if !httpHeaderName.MatchString(name) {
			return invalid("headers."+name, "invalid HTTP header name")
		}
		if h.BasicAuthUsername != "" && strings.EqualFold(name, "Authorization") {
			return invalid("headers."+name, "basic_auth_username sets the Authorization header; do not also set Authorization in headers")
		}
	}
	return validateHTTPStatusCodes(h.ExpectedHTTPStatusCodes)
}

// Arguments lists the inputs that may hold a reference, with their rules.
func (h *HTTP) Arguments(arguments *ArgumentList) {
	arguments.Public("url", &h.URL, httpURL)
	if h.TrustedCertificateAuthorityFile != "" {
		arguments.Public("trusted_certificate_authority_file", &h.TrustedCertificateAuthorityFile, absolute)
	}
	for _, name := range slices.Sorted(maps.Keys(h.Headers)) {
		arguments.Private("headers."+name, h.Headers[name], headerValue)
	}
	if h.BasicAuthUsername != "" {
		arguments.Private("basic_auth_username", h.BasicAuthUsername, scalar)
		arguments.Private("basic_auth_password", h.BasicAuthPassword, scalar)
	}
	if h.Body != "" {
		arguments.Private("body", h.Body, scalar)
	}
	if h.JSONBody != nil {
		arguments.Private("json_body", h.JSONBody, nil)
	}
}

// headerValue is one line of text.
var headerValue = rule("a header value must be one line of text", func(s string) bool { return !strings.ContainsAny(s, "\r\n\x00") })

// Reads lists the result rule, which reads the request's own result.
func (h *HTTP) Reads() Reads {
	return Reads{ResultConditions: map[string]any{"changed_when": h.ChangedWhen}}
}

// OnlyReads accepts GET and HEAD requests in checks_before_restart_or_reload.
func (h *HTTP) OnlyReads() error {
	if h.writes() {
		return fmt.Errorf("an http task in checks_before_restart_or_reload must read: method GET or HEAD; the checks run before the task_block's first write and must change nothing")
	}
	return nil
}

// Run sends the request. In a dryrun after an earlier change of the same
// node task block, a read would check a configuration that is not active
// yet, so it is skipped; a write is never sent during a dryrun.
func (h *HTTP) Run(ctx context.Context, task NodeTask) protocol.Result {
	return task.Runtime.HTTP(ctx, *h, task.Scope, task.PreviewChanged)
}

func (run Runtime) HTTP(ctx context.Context, h HTTP, s value.Scope, previewChanged bool) protocol.Result {
	request := httpRequest{Method: h.method(), URL: h.URL, Headers: map[string]string{}, TrustedCertificateAuthorityFile: h.TrustedCertificateAuthorityFile, DangerouslySkipServerCertificateCheck: h.DangerouslySkipServerCertificateCheck, ExpectedHTTPStatusCodes: h.ExpectedHTTPStatusCodes}
	for name, raw := range h.Headers {
		text, err := resolveScalar(s, raw)
		if err != nil {
			return protocol.ConfigurationError(fmt.Errorf("http.headers.%s: %w", name, err))
		}
		request.Headers[name] = text
	}
	if h.BasicAuthUsername != "" {
		username, err := resolveScalar(s, h.BasicAuthUsername)
		if err != nil {
			return protocol.ConfigurationError(fmt.Errorf("http.basic_auth_username: %w", err))
		}
		password, err := resolveScalar(s, h.BasicAuthPassword)
		if err != nil {
			return protocol.ConfigurationError(fmt.Errorf("http.basic_auth_password: %w", err))
		}
		basic, _ := http.NewRequest(http.MethodGet, "http://basic.invalid", nil)
		basic.SetBasicAuth(username, password)
		request.Headers["Authorization"] = basic.Header.Get("Authorization") //nolint:nilaway // NewRequest receives fixed valid arguments: GET, http://basic.invalid, and a nil body; construction cannot return an error.
	}
	switch {
	case h.Body != "":
		text, err := resolveScalar(s, h.Body)
		if err != nil {
			return protocol.ConfigurationError(fmt.Errorf("http.body: %w", err))
		}
		request.Body = []byte(text)
	case h.JSONBody != nil:
		resolved, err := s.Resolve(h.JSONBody)
		if err == nil {
			request.Body, err = json.Marshal(value.Plain(resolved))
		}
		if err != nil {
			return protocol.ConfigurationError(fmt.Errorf("http.json_body: %w", err))
		}
		if !hasHeader(request.Headers, "Content-Type") {
			request.Headers["Content-Type"] = "application/json"
		}
	}
	if run.Dryrun && h.writes() {
		if changed, literal := h.ChangedWhen.(bool); literal && !changed {
			return protocol.Result{Status: "succeeded", Change: "unchanged", Reason: "a dryrun does not send the HTTP method " + request.Method + "; changed_when is false"}
		}
		if h.ChangedWhen != nil {
			if _, literal := h.ChangedWhen.(bool); !literal {
				return protocol.Result{Status: "not_checked", Change: "unknown", Reason: "a dryrun does not send the HTTP method " + request.Method + ", and changed_when needs the HTTP response"}
			}
		}
		return protocol.Result{Status: "succeeded", Change: "would_change", Reason: "would send the HTTP method " + request.Method}
	}
	if run.Dryrun && previewChanged {
		return protocol.Result{Status: "skipped", Change: "unknown", Reason: "the http node task is skipped during the dryrun because the proposed configuration can be checked only after activation"}
	}
	if h.writes() {
		if err := run.Mutation("http " + request.Method); err != nil {
			return protocol.Failed(err)
		}
	}
	r := sendHTTPRequest(ctx, request)
	if r.HTTPStatusCode == 0 {
		// Not sent, or no answer: a write may still have reached the server.
		if h.writes() {
			r.Change = "unknown"
		}
		return r
	}
	changedWhen := h.ChangedWhen
	if changedWhen == nil {
		changedWhen = h.writes()
	}
	local := s.Local()
	local["this_node_task"] = protocol.ResultValue(r)
	changed, err := local.Predicate(changedWhen)
	switch {
	case err != nil:
		r.Status, r.Reason, r.FailureKind = "failed", err.Error(), "config"
	case changed:
		r.Change = "changed"
	}
	return r
}

func resolveScalar(s value.Scope, raw any) (string, error) {
	v, err := s.Resolve(raw)
	if err != nil {
		return "", err
	}
	return value.Scalar(v)
}

func hasHeader(headers map[string]string, name string) bool {
	for k := range headers {
		if strings.EqualFold(k, name) {
			return true
		}
	}
	return false
}
