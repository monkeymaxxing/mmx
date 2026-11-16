package endpoint

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"mmx/internal/protocol"

	"golang.org/x/crypto/ssh"
)

type Client struct {
	URL, User, Pin string
	Signers        []ssh.Signer
	HTTP           *http.Client
	mu             sync.Mutex
	selected       int
}
type HTTPError struct {
	Status int
	Reason string
}

func (e *HTTPError) Error() string { return fmt.Sprintf("HTTPS %d: %s", e.Status, e.Reason) }

func NewClient(url, user, pin string, signers []ssh.Signer) *Client {
	c := &Client{URL: strings.TrimSuffix(url, "/"), User: user, Pin: pin, Signers: signers, selected: -1}
	t := &http.Transport{DialTLSContext: c.dial, ResponseHeaderTimeout: 15 * time.Second, MaxIdleConnsPerHost: 2, Proxy: nil, ExpectContinueTimeout: time.Second}
	c.HTTP = &http.Client{Transport: t, Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
		return fmt.Errorf("redirects from the node HTTPS API are not allowed")
	}}
	return c
}

// NewConfirmationClient owns a transport for one reachability attempt. The
// authentication exchange and request use that new connection; ordinary
// progress clients keep their reusable transports.
func NewConfirmationClient(url, user, pin string, signers []ssh.Signer) *Client {
	c := NewClient(url, user, pin, signers)
	c.HTTP.Transport.(*http.Transport).DisableKeepAlives = true
	return c
}
func (c *Client) Close() { c.HTTP.CloseIdleConnections() }

// Reachability failures allow SSH fallback. TLS identity errors, HTTP errors,
// protocol failures, and authorization rejection never do; the controller's
// connect instead answers a saved identity that no longer matches with setup
// through verified SSH.
func ReachabilityFailure(err error) bool {
	var identity *IdentityError
	var status *HTTPError
	if errors.As(err, &identity) || errors.As(err, &status) {
		return false
	}
	var op *net.OpError
	var network net.Error
	return (errors.As(err, &network) && network.Timeout()) || (errors.As(err, &op) && (op.Op == "dial" || op.Op == "read" || op.Op == "write")) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
}

func (c *Client) Call(ctx context.Context, method, path string, body []byte, out any) error {
	r, err := http.NewRequestWithContext(ctx, method, c.URL+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	r.Header.Set("Content-Type", "application/json")
	response, err := c.HTTP.Do(r)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	return decodeResponse(response, out)
}
func decodeResponse(response *http.Response, out any) error {
	if response.StatusCode != 200 {
		data, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return &HTTPError{response.StatusCode, strings.TrimSpace(string(data))}
	}
	data, err := protocol.ReadLimited(response.Body, protocol.ResponseLimit, protocol.ErrResponseLimit)
	if err != nil {
		return err
	}
	return protocol.Decode(data, out)
}

func sign(signer ssh.Signer, data []byte) (*ssh.Signature, error) {
	if signer.PublicKey().Type() == ssh.KeyAlgoRSA {
		algorithm, ok := signer.(ssh.AlgorithmSigner)
		if !ok {
			return nil, fmt.Errorf("SSH RSA signer must support SHA-256 signatures")
		}
		return algorithm.SignWithAlgorithm(rand.Reader, data, ssh.KeyAlgoRSASHA256)
	}
	return signer.Sign(rand.Reader, data)
}

// dial opens a TLS connection to the pinned node and authenticates it once,
// as an SSH login does. The transport sends every request on an authenticated
// connection, so requests carry no proof of their own.
func (c *Client) dial(ctx context.Context, network, address string) (net.Conn, error) {
	raw, err := (&net.Dialer{Timeout: 3 * time.Second}).DialContext(ctx, network, address)
	if err != nil {
		return nil, err
	}
	conn := tls.Client(raw, ClientTLS(c.Pin))
	handshake, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := conn.HandshakeContext(handshake); err != nil {
		raw.Close()
		return nil, err
	}
	// Authentication includes node authorization, so it gets the same bound as
	// waiting for a response header.
	if err := conn.SetDeadline(time.Now().Add(15 * time.Second)); err != nil {
		conn.Close()
		return nil, err
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Now()) })
	err = c.authenticate(conn)
	if !stop() {
		err = ctx.Err()
	}
	if err == nil {
		err = conn.SetDeadline(time.Time{})
	}
	if err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}

// authenticate signs the connection's keying material with the first key the
// node accepts, starting with the one that worked last.
func (c *Client) authenticate(conn *tls.Conn) error {
	if len(c.Signers) == 0 {
		return fmt.Errorf("no usable administrator SSH key; load a key into ssh-agent or configure identity_file")
	}
	state := conn.ConnectionState()
	binding, err := state.ExportKeyingMaterial(sessionLabel, nil, 32)
	if err != nil {
		return err
	}
	c.mu.Lock()
	order := []int{}
	if c.selected >= 0 {
		order = append(order, c.selected)
	}
	for i := range c.Signers {
		if i != c.selected {
			order = append(order, i)
		}
	}
	c.mu.Unlock()
	in := bufio.NewReader(conn)
	var last error
	for _, i := range order {
		signature, err := sign(c.Signers[i], signedSession(c.Pin, c.User, binding))
		if err != nil {
			last = err
			continue
		}
		body, _ := json.Marshal(sessionProof{User: c.User, Key: c.Signers[i].PublicKey().Marshal(), Signature: ssh.Marshal(signature)})
		err = c.session(conn, in, body)
		if err == nil {
			c.mu.Lock()
			c.selected = i
			c.mu.Unlock()
			return nil
		}
		last = err
		var status *HTTPError
		if !errors.As(err, &status) || status.Status != 403 {
			return err
		}
	}
	return last
}

// session sends one authentication request on conn before the transport owns
// it, and reads the whole answer so the connection stays usable.
func (c *Client) session(conn net.Conn, in *bufio.Reader, body []byte) error {
	r, err := http.NewRequest("POST", c.URL+"/v1/session", bytes.NewReader(body))
	if err != nil {
		return err
	}
	r.Header.Set("Content-Type", "application/json")
	if err := r.Write(conn); err != nil {
		return err
	}
	response, err := http.ReadResponse(in, r)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	var result struct {
		Status string `json:"status"`
	}
	err = decodeResponse(response, &result)
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64*1024))
	if err == nil && result.Status != "authenticated" {
		err = fmt.Errorf("unexpected MMX authentication response")
	}
	return err
}
