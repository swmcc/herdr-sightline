package herdr_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/swmcc/herdr-sightline/internal/herdr"
	"github.com/swmcc/herdr-sightline/internal/herdr/herdrtest"
)

// failsafe stops a broken test hanging forever. It's never what makes a passing
// test wait.
const failsafe = 5 * time.Second

func newClient(t *testing.T, srv *herdrtest.Server, opts ...herdr.Option) *herdr.Client {
	t.Helper()
	opts = append([]herdr.Option{herdr.WithBackoff(time.Millisecond, 5*time.Millisecond)}, opts...)
	return herdr.NewClient(herdr.UnixDialer(srv.Path), opts...)
}

// silence is a handler for a server that takes the request and never answers.
func silence(herdrtest.Request) (any, *herdrtest.ErrorBody) { return herdrtest.Raw(""), nil }

func nextRequest(t *testing.T, srv *herdrtest.Server) herdrtest.Request {
	t.Helper()
	select {
	case req := <-srv.Requests():
		return req
	case <-time.After(failsafe):
		t.Fatal("no request reached the server")
		return herdrtest.Request{}
	}
}

func reply(result any) herdrtest.Handler {
	return func(herdrtest.Request) (any, *herdrtest.ErrorBody) { return result, nil }
}

func paramsOf(t *testing.T, req herdrtest.Request) map[string]any {
	t.Helper()
	var params map[string]any
	if err := json.Unmarshal(req.Params, &params); err != nil {
		t.Fatalf("decoding params %s: %v", req.Params, err)
	}
	return params
}

func TestDial(t *testing.T) {
	srv := herdrtest.New(t)
	srv.Handle("ping", reply(map[string]any{"type": "pong", "version": "0.9.1", "protocol": 22}))
	ctx := context.Background()

	c, err := herdr.Dial(ctx, srv.Path)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	info, err := c.Ping(ctx)
	if err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if info != (herdr.ServerInfo{Version: "0.9.1", Protocol: 22}) {
		t.Errorf("Ping = %+v", info)
	}
}

func TestDialFailures(t *testing.T) {
	ctx := context.Background()

	t.Run("no socket", func(t *testing.T) {
		missing := filepath.Join(t.TempDir(), "nope.sock")
		if _, err := herdr.Dial(ctx, missing); err == nil || !strings.Contains(err.Error(), missing) {
			t.Errorf("err = %v, want one naming %s", err, missing)
		}
	})

	t.Run("env not set", func(t *testing.T) {
		t.Setenv(herdr.SocketEnv, "")
		if _, err := herdr.DialEnv(ctx); err == nil || !strings.Contains(err.Error(), herdr.SocketEnv) {
			t.Errorf("err = %v, want one naming %s", err, herdr.SocketEnv)
		}
	})

	t.Run("env set", func(t *testing.T) {
		srv := herdrtest.New(t)
		srv.Handle("ping", reply(map[string]any{"type": "pong", "version": "0.9.1", "protocol": 22}))
		t.Setenv(herdr.SocketEnv, srv.Path)
		if _, err := herdr.DialEnv(ctx); err != nil {
			t.Errorf("DialEnv: %v", err)
		}
	})
}

func TestReadPane(t *testing.T) {
	srv := herdrtest.New(t)
	srv.Handle("pane.read", reply(map[string]any{
		"type": "pane_read",
		"read": map[string]any{
			"pane_id": "w1:p2", "workspace_id": "w1", "tab_id": "w1:t1",
			"source": "recent_unwrapped", "format": "text",
			"text": "one\ntwo", "revision": 0, "truncated": true,
		},
	}))
	c := newClient(t, srv)

	got, err := c.ReadPane(context.Background(), "w1:p2", 300)
	if err != nil {
		t.Fatalf("ReadPane: %v", err)
	}
	want := herdr.PaneRead{PaneID: "w1:p2", WorkspaceID: "w1", TabID: "w1:t1", Text: "one\ntwo", Truncated: true}
	if got != want {
		t.Errorf("ReadPane = %+v, want %+v", got, want)
	}

	req := nextRequest(t, srv)
	if req.Method != "pane.read" {
		t.Errorf("method = %q", req.Method)
	}
	wantParams := map[string]any{"pane_id": "w1:p2", "source": "recent_unwrapped", "lines": 300.0, "strip_ansi": true}
	if params := paramsOf(t, req); !reflect.DeepEqual(params, wantParams) {
		t.Errorf("params = %v, want %v", params, wantParams)
	}
}

func TestReadPaneNeedsLines(t *testing.T) {
	srv := herdrtest.New(t)
	c := newClient(t, srv)
	if _, err := c.ReadPane(context.Background(), "w1:p2", 0); err == nil {
		t.Error("ReadPane with 0 lines succeeded, want an error")
	}
	select {
	case req := <-srv.Requests():
		t.Errorf("sent %s, want nothing sent", req.Method)
	default:
	}
}

func TestListPanes(t *testing.T) {
	srv := herdrtest.New(t)
	srv.Handle("pane.list", reply(json.RawMessage(`{"type":"pane_list","panes":[
		{"pane_id":"w1:p1","terminal_id":"t1","workspace_id":"w1","tab_id":"w1:t1","focused":true,
		 "cwd":"/src","agent":"claude","display_agent":"Claude","agent_status":"working",
		 "terminal_title_stripped":"claude","label":null,"revision":0},
		{"pane_id":"w1:p2","terminal_id":"t2","workspace_id":"w1","tab_id":"w1:t1","focused":false,
		 "cwd":null,"agent":null,"agent_status":"unknown","revision":0}
	]}`)))
	c := newClient(t, srv)

	got, err := c.ListPanes(context.Background())
	if err != nil {
		t.Fatalf("ListPanes: %v", err)
	}
	want := []herdr.Pane{
		{ID: "w1:p1", TerminalID: "t1", WorkspaceID: "w1", TabID: "w1:t1", Focused: true, Cwd: "/src", Agent: "claude",
			DisplayAgent: "Claude", AgentStatus: herdr.AgentWorking, Title: "claude"},
		{ID: "w1:p2", TerminalID: "t2", WorkspaceID: "w1", TabID: "w1:t1", AgentStatus: herdr.AgentUnknown},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ListPanes =\n%+v\nwant\n%+v", got, want)
	}
}

func TestPrompt(t *testing.T) {
	srv := herdrtest.New(t)
	srv.Handle("agent.prompt", reply(map[string]any{"type": "agent_prompted", "agent": map[string]any{"pane_id": "w1:p1"}}))
	c := newClient(t, srv)

	if err := c.Prompt(context.Background(), "w1:p1", "look at this"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	req := nextRequest(t, srv)
	wantParams := map[string]any{"target": "w1:p1", "text": "look at this"}
	if params := paramsOf(t, req); req.Method != "agent.prompt" || !reflect.DeepEqual(params, wantParams) {
		t.Errorf("sent %s %v, want agent.prompt %v", req.Method, params, wantParams)
	}
}

func TestCallFailures(t *testing.T) {
	tests := []struct {
		name         string
		handler      herdrtest.Handler
		wantNotFound bool
		wantText     string
	}{
		{
			name: "herdr error",
			handler: func(herdrtest.Request) (any, *herdrtest.ErrorBody) {
				return nil, &herdrtest.ErrorBody{Code: "pane_not_found", Message: "pane w1:p9 not found"}
			},
			wantNotFound: true,
			wantText:     "pane w1:p9 not found",
		},
		{
			name: "other herdr error",
			handler: func(herdrtest.Request) (any, *herdrtest.ErrorBody) {
				return nil, &herdrtest.ErrorBody{Code: "invalid_request", Message: "bad"}
			},
			wantText: "invalid_request",
		},
		{
			name:     "unexpected reply type",
			handler:  reply(map[string]any{"type": "ok"}),
			wantText: `reply type is "ok"`,
		},
		{
			name:     "reply with neither result nor error",
			handler:  reply(nil),
			wantText: "neither result nor error",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := herdrtest.New(t)
			srv.Handle("pane.read", tt.handler)
			c := newClient(t, srv)

			_, err := c.ReadPane(context.Background(), "w1:p9", 10)
			if err == nil {
				t.Fatal("ReadPane succeeded, want an error")
			}
			if got := herdr.IsPaneNotFound(err); got != tt.wantNotFound {
				t.Errorf("IsPaneNotFound = %v, want %v (err %v)", got, tt.wantNotFound, err)
			}
			if !strings.Contains(err.Error(), tt.wantText) {
				t.Errorf("err = %q, want it to contain %q", err, tt.wantText)
			}
			if n := strings.Count(err.Error(), "pane.read"); n != 1 {
				t.Errorf("err = %q names the method %d times, want once", err, n)
			}
		})
	}
}

func TestCallCancelled(t *testing.T) {
	srv := herdrtest.New(t)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	srv.Handle("pane.list", func(herdrtest.Request) (any, *herdrtest.ErrorBody) {
		<-release
		return map[string]any{"type": "pane_list", "panes": []any{}}, nil
	})
	c := newClient(t, srv)

	ctx, cancel := context.WithCancel(context.Background())
	errs := make(chan error, 1)
	go func() {
		_, err := c.ListPanes(ctx)
		errs <- err
	}()
	nextRequest(t, srv)
	cancel()

	select {
	case err := <-errs:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want context.Canceled", err)
		}
	case <-time.After(failsafe):
		t.Fatal("ListPanes didn't return after cancel")
	}
}

func TestCallTimesOut(t *testing.T) {
	srv := herdrtest.New(t)
	srv.Handle("pane.list", silence)
	c := newClient(t, srv, herdr.WithTimeout(50*time.Millisecond))

	_, err := c.ListPanes(context.Background())
	if !errors.Is(err, herdr.ErrTimeout) {
		t.Fatalf("err = %v, want ErrTimeout", err)
	}
	if !strings.Contains(err.Error(), "pane.list") {
		t.Errorf("err = %q, want it to name the method", err)
	}
}

func TestCallReplyTooLarge(t *testing.T) {
	srv := herdrtest.New(t)
	srv.Handle("pane.read", func(herdrtest.Request) (any, *herdrtest.ErrorBody) {
		return herdrtest.Raw(`{"id":"x","result":{"type":"pane_read","read":{"text":"` +
			strings.Repeat("x", herdr.MaxReplyBytes) + `"}}}` + "\n"), nil
	})
	c := newClient(t, srv)

	_, err := c.ReadPane(context.Background(), "w1:p1", 300)
	if !errors.Is(err, herdr.ErrTooLarge) {
		t.Errorf("err = %v, want ErrTooLarge", err)
	}
}

func TestOptionsIgnoreNonsense(t *testing.T) {
	srv := herdrtest.New(t)
	srv.Handle("ping", reply(map[string]any{"type": "pong", "version": "0.9.1", "protocol": 22}))
	// A zero timeout would fail every call at once and a zero backoff would
	// spin, so the first is ignored and the second raised.
	c := newClient(t, srv, herdr.WithTimeout(0), herdr.WithBackoff(0, 0))
	if _, err := c.Ping(context.Background()); err != nil {
		t.Fatalf("Ping: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events, err := c.Subscribe(ctx, herdr.PaneClosed())
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	nextSubscriber(t, srv).Drop()
	if ev := nextEvent(t, events); ev.Kind != herdr.EventReconnected {
		t.Errorf("event = %+v, want reconnected", ev)
	}
}
