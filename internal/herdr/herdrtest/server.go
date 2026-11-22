// Package herdrtest is a fake herdr socket for tests.
//
// It speaks the same newline delimited JSON as herdr: one request per
// connection, answered and then closed, except events.subscribe, which keeps its
// connection open so the test can push events and drop it to fake a restart.
//
// There are two levels. Server answers each method with whatever the test says,
// which suits testing how the client copes with odd replies. Herdr sits on top
// and holds panes, agents and tabs, which suits testing what the adapter does
// with them. Neither needs herdr installed.
package herdrtest

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// Request is one request the server received.
type Request struct {
	ID     string          `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

// ErrorBody is an error reply, as herdr sends it.
type ErrorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Handler answers a request with a result or an error. The result is sent as
// is, so it should include herdr's "type" field.
type Handler func(req Request) (result any, err *ErrorBody)

// Raw is a result a Handler can return to have the server write exactly those
// bytes instead of a reply, newlines and all. That covers replies that are too
// big, a reply and an event in one write, or (with an empty Raw) a server that
// never answers. Either way the server then holds the connection until the
// client hangs up. A non-empty Raw for events.subscribe also counts as a
// subscription and is delivered on Subscribers.
type Raw string

// Server is a fake herdr listening on a Unix socket.
type Server struct {
	// Path is the socket to dial.
	Path string

	ln          net.Listener
	requests    chan Request
	subscribers chan *Subscriber
	done        chan struct{}
	wg          sync.WaitGroup

	mu       sync.Mutex
	handlers map[string]Handler
	// subscribed, when set, hears about each subscription before its client
	// does.
	subscribed func(*Subscriber)
	conns      map[net.Conn]struct{}
	closed     bool
}

// New starts a server and stops it when the test ends.
func New(t testing.TB) *Server {
	t.Helper()
	// Unix socket paths are limited to about 100 bytes and t.TempDir can be
	// longer than that on macOS, so use a short directory of our own.
	dir, err := os.MkdirTemp("", "herdrtest")
	if err != nil {
		t.Fatalf("making socket dir: %v", err)
	}
	path := filepath.Join(dir, "herdr.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		_ = os.RemoveAll(dir)
		t.Fatalf("listening on %s: %v", path, err)
	}

	s := &Server{
		Path:        path,
		ln:          ln,
		requests:    make(chan Request, 1024),
		subscribers: make(chan *Subscriber, 64),
		done:        make(chan struct{}),
		handlers:    map[string]Handler{},
		conns:       map[net.Conn]struct{}{},
	}
	s.wg.Add(1)
	go s.accept()
	t.Cleanup(func() {
		s.Close()
		_ = os.RemoveAll(dir)
	})
	return s
}

// Handle sets how the server answers method. Without a handler a method gets
// an unknown_method error, except events.subscribe, which succeeds.
func (s *Server) Handle(method string, h Handler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handlers[method] = h
}

// Requests delivers every request the server receives, in order.
func (s *Server) Requests() <-chan Request { return s.requests }

// Subscribers delivers each subscription once the server has accepted it. A
// Herdr keeps track of its subscriptions itself, so on one this stays empty.
func (s *Server) Subscribers() <-chan *Subscriber { return s.subscribers }

// Close stops the server and waits for its goroutines to finish.
func (s *Server) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	close(s.done)
	_ = s.ln.Close()
	for c := range s.conns {
		_ = c.Close()
	}
	s.mu.Unlock()
	s.wg.Wait()
}

func (s *Server) accept() {
	defer s.wg.Done()
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			_ = conn.Close()
			return
		}
		s.conns[conn] = struct{}{}
		s.wg.Add(1)
		s.mu.Unlock()
		go s.serve(conn)
	}
}

func (s *Server) forget(conn net.Conn) {
	s.mu.Lock()
	delete(s.conns, conn)
	s.mu.Unlock()
	_ = conn.Close()
}

func (s *Server) serve(conn net.Conn) {
	defer s.wg.Done()
	defer s.forget(conn)

	dec := json.NewDecoder(conn)
	var req Request
	if err := dec.Decode(&req); err != nil {
		return
	}
	select {
	case s.requests <- req:
	default:
	}

	s.mu.Lock()
	h, ok := s.handlers[req.Method]
	subscribed := s.subscribed
	s.mu.Unlock()

	var result any
	var herr *ErrorBody
	switch {
	case ok:
		result, herr = h(req)
	case req.Method == "events.subscribe":
		result = map[string]string{"type": "subscription_started"}
	default:
		herr = &ErrorBody{Code: "unknown_method", Message: "no handler for " + req.Method}
	}

	sub := &Subscriber{conn: conn, ready: make(chan struct{}), done: make(chan struct{})}
	defer close(sub.done)
	raw, isRaw := result.(Raw)
	subscribing := req.Method == "events.subscribe" && herr == nil && (!isRaw || raw != "")
	if subscribing {
		var params struct {
			Subscriptions json.RawMessage `json:"subscriptions"`
		}
		_ = json.Unmarshal(req.Params, &params)
		sub.Subscriptions = params.Subscriptions
		// Before the reply, so the subscription is in place by the time the
		// client hears it started and can make something happen. Anything
		// pushed meanwhile waits for ready, so it can't overtake the reply.
		if subscribed != nil {
			subscribed(sub)
		}
	}
	switch {
	case herr != nil:
		_ = sub.write(map[string]any{"id": req.ID, "error": herr})
		return
	case isRaw:
		if raw != "" {
			if err := sub.writeLocked([]byte(raw)); err != nil {
				return
			}
			close(sub.ready)
		}
		if raw == "" || req.Method != "events.subscribe" {
			// Say nothing more and wait for the client to give up.
			for dec.Decode(new(json.RawMessage)) == nil {
			}
			return
		}
	default:
		if err := sub.write(map[string]any{"id": req.ID, "result": result}); err != nil {
			return
		}
		if req.Method != "events.subscribe" {
			return
		}
		close(sub.ready)
	}

	// When something else keeps track of subscriptions, the channel isn't used.
	// Nobody would drain it, so it would fill up and go stale.
	if subscribed == nil {
		select {
		case s.subscribers <- sub:
		case <-s.done:
			return
		}
	}

	// Hold the connection until the client hangs up or the test drops it.
	var discard json.RawMessage
	for dec.Decode(&discard) == nil {
	}
}

// Subscriber is one open subscription connection.
type Subscriber struct {
	// Subscriptions is the subscriptions array the client sent.
	Subscriptions json.RawMessage

	conn net.Conn
	mu   sync.Mutex
	// ready closes once the reply to the subscribe has gone, so events always
	// come after it, as they do from herdr.
	ready chan struct{}
	// done closes when the connection has ended.
	done chan struct{}
}

// ended reports whether the subscription's connection has gone.
func (sub *Subscriber) ended() bool {
	select {
	case <-sub.done:
		return true
	default:
		return false
	}
}

// PushLifecycle sends an event in herdr's other envelope shape, the one its
// lifecycle events use: an underscored name that's repeated as data.type, as in
// {"event":"pane_closed","data":{"type":"pane_closed","pane_id":"w1:p2"}}.
func (sub *Subscriber) PushLifecycle(event string, fields map[string]any) error {
	data := map[string]any{"type": event}
	for k, v := range fields {
		if k != "type" {
			data[k] = v
		}
	}
	return sub.Push(event, data)
}

// Push sends an event envelope: {"event": event, "data": data}. That's the
// shape herdr uses for subscription events such as pane.agent_status_changed.
func (sub *Subscriber) Push(event string, data any) error {
	line, err := json.Marshal(map[string]any{"event": event, "data": data})
	if err != nil {
		return fmt.Errorf("herdrtest: encoding event: %w", err)
	}
	return sub.PushRaw(string(line))
}

// PushRaw sends line as it is, followed by a newline. It waits until the
// subscribe has been answered, and gives up if the client stops reading for
// long enough that the write can't finish.
func (sub *Subscriber) PushRaw(line string) error {
	select {
	case <-sub.ready:
	case <-sub.done:
		return net.ErrClosed
	}
	return sub.writeLocked([]byte(line + "\n"))
}

// writeDeadline stops a push to a client that has stopped reading hanging the
// test that made it.
const writeDeadline = 5 * time.Second

func (sub *Subscriber) writeLocked(b []byte) error {
	sub.mu.Lock()
	defer sub.mu.Unlock()
	_ = sub.conn.SetWriteDeadline(time.Now().Add(writeDeadline))
	_, err := sub.conn.Write(b)
	return err
}

// Drop closes the connection, as herdr does when it restarts or hands off.
func (sub *Subscriber) Drop() {
	_ = sub.conn.Close()
}

func (sub *Subscriber) write(v any) error {
	line, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("herdrtest: encoding reply: %w", err)
	}
	return sub.writeLocked(append(line, '\n'))
}
