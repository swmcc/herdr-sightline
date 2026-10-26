package herdr

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"sync/atomic"
	"time"
)

// SocketEnv is the environment variable herdr uses to tell a plugin where its
// socket lives.
const SocketEnv = "HERDR_SOCKET_PATH"

// Error codes herdr returns that callers are likely to want to act on.
const (
	CodePaneNotFound   = "pane_not_found"
	CodeInvalidRequest = "invalid_request"
)

// DialFunc opens one connection to herdr. It's the seam between the client and
// the transport: Unix sockets today, Windows named pipes later (#26).
type DialFunc func(ctx context.Context) (net.Conn, error)

// UnixDialer returns a DialFunc for the Unix socket at path.
func UnixDialer(path string) DialFunc {
	return func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", path)
	}
}

// Client talks to herdr's socket API.
//
// herdr answers one request per connection and then hangs up, which is also how
// its own CLI works. So every call dials, sends, reads one reply and closes.
// There's nothing to multiplex and no shared connection to guard, which keeps
// the client safe to use from any number of goroutines. Subscriptions are the
// exception: they hold their connection open (see Subscribe).
//
// Nothing here shells out to the herdr CLI at HERDR_BIN_PATH. Everything
// Sightline needs is on the socket.
type Client struct {
	dial       DialFunc
	ids        atomic.Uint64
	minBackoff time.Duration
	maxBackoff time.Duration
}

// Option changes how a Client behaves.
type Option func(*Client)

// WithBackoff sets how long Subscribe waits between reconnect attempts. It
// starts at first and doubles up to limit.
func WithBackoff(first, limit time.Duration) Option {
	return func(c *Client) {
		c.minBackoff, c.maxBackoff = first, limit
	}
}

// NewClient returns a Client that opens connections with dial. It doesn't
// connect until the first call.
func NewClient(dial DialFunc, opts ...Option) *Client {
	c := &Client{
		dial:       dial,
		minBackoff: 100 * time.Millisecond,
		maxBackoff: 5 * time.Second,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Dial returns a Client for the Unix socket at socketPath, after checking herdr
// answers a ping there.
func Dial(ctx context.Context, socketPath string, opts ...Option) (*Client, error) {
	c := NewClient(UnixDialer(socketPath), opts...)
	if _, err := c.Ping(ctx); err != nil {
		return nil, fmt.Errorf("connecting to herdr at %s: %w", socketPath, err)
	}
	return c, nil
}

// DialEnv is Dial using the socket path herdr puts in HERDR_SOCKET_PATH.
func DialEnv(ctx context.Context, opts ...Option) (*Client, error) {
	path := os.Getenv(SocketEnv)
	if path == "" {
		return nil, fmt.Errorf("%s is not set, so herdr can't be reached (is this running inside herdr?)", SocketEnv)
	}
	return Dial(ctx, path, opts...)
}

// Error is an error reply from herdr.
type Error struct {
	Method  string
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string {
	return fmt.Sprintf("herdr %s: %s (%s)", e.Method, e.Message, e.Code)
}

// IsPaneNotFound reports whether err is herdr saying the pane doesn't exist,
// which usually means it has closed.
func IsPaneNotFound(err error) bool {
	var herr *Error
	return errors.As(err, &herr) && herr.Code == CodePaneNotFound
}

type request struct {
	ID     string `json:"id"`
	Method string `json:"method"`
	Params any    `json:"params"`
}

type response struct {
	ID     string          `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *Error          `json:"error"`
}

// resultHead is the common head of every successful reply. The type names which
// variant came back.
type resultHead struct {
	Type string `json:"type"`
}

func (c *Client) nextID() string {
	return "sightline-" + strconv.FormatUint(c.ids.Add(1), 10)
}

// call sends one request on a fresh connection and decodes the reply's result
// into out, after checking its type is wantType.
func (c *Client) call(ctx context.Context, method string, params any, wantType string, out any) error {
	conn, err := c.dial(ctx)
	if err != nil {
		return fmt.Errorf("herdr %s: dialling: %w", method, err)
	}
	defer conn.Close()

	// Cancelling ctx unblocks any read or write in progress by expiring the
	// connection's deadline, so there's no goroutine to look after.
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Now()) })
	defer stop()

	raw, err := exchange(conn, json.NewDecoder(conn), c.nextID(), method, params)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("herdr %s: %w", method, ctxErr)
		}
		return fmt.Errorf("herdr %s: %w", method, err)
	}
	return decodeResult(method, raw, wantType, out)
}

// exchange writes one request to conn and reads one reply with dec. A herdr
// error reply comes back as *Error. The decoder is passed in so a subscription
// can keep reading events with it, since it may already have buffered some.
//
// herdr doesn't always echo the request id (a failed subscribe answers with a
// derived id), and each connection carries exactly one request, so the id isn't
// checked.
func exchange(conn net.Conn, dec *json.Decoder, id, method string, params any) (json.RawMessage, error) {
	if params == nil {
		params = struct{}{}
	}
	line, err := json.Marshal(request{ID: id, Method: method, Params: params})
	if err != nil {
		return nil, fmt.Errorf("encoding request: %w", err)
	}
	if _, err := conn.Write(append(line, '\n')); err != nil {
		return nil, fmt.Errorf("sending request: %w", err)
	}

	var resp response
	if err := dec.Decode(&resp); err != nil {
		return nil, fmt.Errorf("reading reply: %w", err)
	}
	if resp.Error != nil {
		resp.Error.Method = method
		return nil, resp.Error
	}
	if len(resp.Result) == 0 || string(resp.Result) == "null" {
		return nil, errors.New("reply has neither result nor error")
	}
	return resp.Result, nil
}

func decodeResult(method string, raw json.RawMessage, wantType string, out any) error {
	var head resultHead
	if err := json.Unmarshal(raw, &head); err != nil {
		return fmt.Errorf("herdr %s: decoding reply: %w", method, err)
	}
	if head.Type != wantType {
		return fmt.Errorf("herdr %s: reply type is %q, want %q", method, head.Type, wantType)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("herdr %s: decoding %s: %w", method, wantType, err)
	}
	return nil
}

// ServerInfo is what herdr says about itself in reply to a ping.
type ServerInfo struct {
	Version  string `json:"version"`
	Protocol int    `json:"protocol"`
}

// Ping checks herdr is answering and returns its version.
func (c *Client) Ping(ctx context.Context) (ServerInfo, error) {
	var info ServerInfo
	err := c.call(ctx, "ping", struct{}{}, "pong", &info)
	return info, err
}
