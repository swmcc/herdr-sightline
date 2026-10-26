package herdr_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/swmcc/herdr-sightline/internal/herdr"
	"github.com/swmcc/herdr-sightline/internal/herdr/herdrtest"
)

func nextSubscriber(t *testing.T, srv *herdrtest.Server) *herdrtest.Subscriber {
	t.Helper()
	select {
	case sub := <-srv.Subscribers():
		return sub
	case <-time.After(failsafe):
		t.Fatal("no subscription reached the server")
		return nil
	}
}

func nextEvent(t *testing.T, events <-chan herdr.Event) herdr.Event {
	t.Helper()
	select {
	case ev, ok := <-events:
		if !ok {
			t.Fatal("events closed, want another event")
		}
		return ev
	case <-time.After(failsafe):
		t.Fatal("no event arrived")
		return herdr.Event{}
	}
}

func waitClosed(t *testing.T, events <-chan herdr.Event) {
	t.Helper()
	deadline := time.After(failsafe)
	for {
		select {
		case _, ok := <-events:
			if !ok {
				return
			}
		case <-deadline:
			t.Fatal("events never closed")
		}
	}
}

func TestSubscribeSendsSubscriptions(t *testing.T) {
	srv := herdrtest.New(t)
	c := newClient(t, srv)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if _, err := c.Subscribe(ctx, herdr.AgentStatusChanged("w1:p1"), herdr.ScrollChanged("w1:p2"), herdr.PaneClosed()); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	sub := nextSubscriber(t, srv)
	want := `[{"type":"pane.agent_status_changed","pane_id":"w1:p1"},{"type":"pane.scroll_changed","pane_id":"w1:p2"},{"type":"pane.closed"}]`
	if string(sub.Subscriptions) != want {
		t.Errorf("subscriptions = %s, want %s", sub.Subscriptions, want)
	}
}

func TestSubscribeDecodesEvents(t *testing.T) {
	tests := []struct {
		name  string
		event string
		data  string
		want  herdr.Event
	}{
		{
			name:  "agent status, dotted envelope",
			event: "pane.agent_status_changed",
			data:  `{"pane_id":"w1:p1","workspace_id":"w1","agent_status":"blocked","agent":"claude","title":null}`,
			want: herdr.Event{Kind: herdr.EventAgentStatusChanged, Name: "pane.agent_status_changed",
				PaneID: "w1:p1", WorkspaceID: "w1", AgentStatus: herdr.AgentBlocked, Agent: "claude"},
		},
		{
			name:  "agent status, underscored envelope",
			event: "pane_agent_status_changed",
			data:  `{"type":"pane_agent_status_changed","pane_id":"w1:p1","workspace_id":"w1","agent_status":"idle"}`,
			want: herdr.Event{Kind: herdr.EventAgentStatusChanged, Name: "pane_agent_status_changed",
				PaneID: "w1:p1", WorkspaceID: "w1", AgentStatus: herdr.AgentIdle},
		},
		{
			name:  "pane closed",
			event: "pane_closed",
			data:  `{"type":"pane_closed","pane_id":"w1:p2","workspace_id":"w1"}`,
			want:  herdr.Event{Kind: herdr.EventPaneClosed, Name: "pane_closed", PaneID: "w1:p2", WorkspaceID: "w1"},
		},
		{
			name:  "pane exited",
			event: "pane_exited",
			data:  `{"type":"pane_exited","pane_id":"w1:p2","workspace_id":"w1"}`,
			want:  herdr.Event{Kind: herdr.EventPaneExited, Name: "pane_exited", PaneID: "w1:p2", WorkspaceID: "w1"},
		},
		{
			name:  "scroll changed",
			event: "pane.scroll_changed",
			data:  `{"pane_id":"w1:p2","workspace_id":"w1","scroll":{"offset_from_bottom":0,"max_offset_from_bottom":42,"viewport_rows":30}}`,
			want: herdr.Event{Kind: herdr.EventScrollChanged, Name: "pane.scroll_changed", PaneID: "w1:p2", WorkspaceID: "w1",
				Scroll: &herdr.ScrollInfo{MaxOffsetFromBottom: 42, ViewportRows: 30}},
		},
		{
			name:  "something else",
			event: "tab_created",
			data:  `{"type":"tab_created","tab":{"tab_id":"w1:t2"}}`,
			want:  herdr.Event{Kind: herdr.EventOther, Name: "tab_created"},
		},
		{
			name:  "data that isn't an object still keeps its kind",
			event: "pane_closed",
			data:  `"not an object"`,
			want:  herdr.Event{Kind: herdr.EventPaneClosed, Name: "pane_closed"},
		},
		{
			name:  "a field of the wrong type doesn't lose the rest",
			event: "pane_closed",
			data:  `{"type":"pane_closed","pane_id":"w1:p2","workspace_id":7}`,
			want:  herdr.Event{Kind: herdr.EventPaneClosed, Name: "pane_closed", PaneID: "w1:p2"},
		},
	}

	srv := herdrtest.New(t)
	c := newClient(t, srv)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events, err := c.Subscribe(ctx, herdr.PaneClosed())
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	sub := nextSubscriber(t, srv)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := sub.Push(tt.event, json.RawMessage(tt.data)); err != nil {
				t.Fatalf("Push: %v", err)
			}
			got := nextEvent(t, events)
			if string(got.Data) != tt.data {
				t.Errorf("data = %s, want %s", got.Data, tt.data)
			}
			got.Data = nil
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("event = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestSubscribeSkipsLinesWithoutAnEvent(t *testing.T) {
	srv := herdrtest.New(t)
	c := newClient(t, srv)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events, err := c.Subscribe(ctx, herdr.PaneClosed())
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	sub := nextSubscriber(t, srv)

	_ = sub.PushRaw(`{"something":"unexpected"}`)
	_ = sub.Push("pane_closed", map[string]string{"pane_id": "w1:p2"})
	if ev := nextEvent(t, events); ev.Kind != herdr.EventPaneClosed {
		t.Errorf("event = %+v, want the pane_closed after the odd line", ev)
	}
}

func TestSubscribeReconnects(t *testing.T) {
	tests := []struct {
		name string
		cut  func(*herdrtest.Subscriber)
	}{
		{name: "connection dropped", cut: func(s *herdrtest.Subscriber) { s.Drop() }},
		{name: "garbage on the stream", cut: func(s *herdrtest.Subscriber) { _ = s.PushRaw(`{not json`) }},
		{name: "error on the stream", cut: func(s *herdrtest.Subscriber) {
			_ = s.PushRaw(`{"id":"x","error":{"code":"shutting_down","message":"bye"}}`)
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := herdrtest.New(t)
			c := newClient(t, srv)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			events, err := c.Subscribe(ctx, herdr.AgentStatusChanged("w1:p1"))
			if err != nil {
				t.Fatalf("Subscribe: %v", err)
			}
			first := nextSubscriber(t, srv)
			tt.cut(first)

			ev := nextEvent(t, events)
			if ev.Kind != herdr.EventReconnected {
				t.Fatalf("event = %+v, want reconnected", ev)
			}
			if ev.Err == nil {
				t.Error("reconnected event has no cause")
			}
			second := nextSubscriber(t, srv)
			if string(second.Subscriptions) != string(first.Subscriptions) {
				t.Errorf("resubscribed with %s, want %s", second.Subscriptions, first.Subscriptions)
			}
			_ = second.Push("pane.agent_status_changed", map[string]string{"pane_id": "w1:p1", "agent_status": "done"})
			if ev := nextEvent(t, events); ev.AgentStatus != herdr.AgentDone {
				t.Errorf("event after reconnect = %+v, want status done", ev)
			}
		})
	}
}

func TestSubscribeRetriesUntilHerdrIsBack(t *testing.T) {
	srv := herdrtest.New(t)
	c := newClient(t, srv)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	events, err := c.Subscribe(ctx, herdr.PaneClosed())
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	first := nextSubscriber(t, srv)
	nextRequest(t, srv)

	// Answer the next two attempts like a server that's still starting, with a
	// temporary error and then nonsense, and let the third through.
	attempts := make(chan struct{}, 8)
	srv.Handle("events.subscribe", func(herdrtest.Request) (any, *herdrtest.ErrorBody) {
		attempts <- struct{}{}
		switch len(attempts) {
		case 1:
			return nil, &herdrtest.ErrorBody{Code: "shutting_down", Message: "handing off"}
		case 2:
			return json.RawMessage(`{"type":"not_ready"}`), nil
		}
		return map[string]string{"type": "subscription_started"}, nil
	})
	first.Drop()

	if ev := nextEvent(t, events); ev.Kind != herdr.EventReconnected {
		t.Fatalf("event = %+v, want reconnected", ev)
	}
	if n := len(attempts); n != 3 {
		t.Errorf("subscribe attempts after the drop = %d, want 3", n)
	}
}

func TestSubscribeGivesUpWhenHerdrRefuses(t *testing.T) {
	srv := herdrtest.New(t)
	c := newClient(t, srv)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	events, err := c.Subscribe(ctx, herdr.AgentStatusChanged("w1:p1"))
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	first := nextSubscriber(t, srv)
	srv.Handle("events.subscribe", func(herdrtest.Request) (any, *herdrtest.ErrorBody) {
		return nil, &herdrtest.ErrorBody{Code: "pane_not_found", Message: "pane w1:p1 not found"}
	})
	first.Drop()

	ev := nextEvent(t, events)
	if ev.Kind != herdr.EventFailed || !herdr.IsPaneNotFound(ev.Err) {
		t.Fatalf("event = %+v, want failed with pane not found", ev)
	}
	waitClosed(t, events)
}

func TestSubscribeFailsUpFront(t *testing.T) {
	srv := herdrtest.New(t)
	srv.Handle("events.subscribe", func(herdrtest.Request) (any, *herdrtest.ErrorBody) {
		return nil, &herdrtest.ErrorBody{Code: "pane_not_found", Message: "pane w1:p9 not found"}
	})
	c := newClient(t, srv)

	_, err := c.Subscribe(context.Background(), herdr.AgentStatusChanged("w1:p9"))
	if !herdr.IsPaneNotFound(err) {
		t.Errorf("err = %v, want pane not found", err)
	}
	if _, err := c.Subscribe(context.Background()); err == nil || !strings.Contains(err.Error(), "no subscriptions") {
		t.Errorf("err = %v, want no subscriptions", err)
	}
}

func TestSubscribeStopsOnCancel(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, srv *herdrtest.Server, sub *herdrtest.Subscriber)
	}{
		{name: "while streaming"},
		{
			name: "while reconnecting",
			setup: func(t *testing.T, srv *herdrtest.Server, sub *herdrtest.Subscriber) {
				// Every retry is hung up on, so the client stays in its backoff loop.
				tried := make(chan struct{}, 1)
				srv.Handle("events.subscribe", func(herdrtest.Request) (any, *herdrtest.ErrorBody) {
					select {
					case tried <- struct{}{}:
					default:
					}
					return json.RawMessage(`{"type":"not_ready"}`), nil
				})
				sub.Drop()
				select {
				case <-tried:
				case <-time.After(failsafe):
					t.Fatal("client never retried")
				}
			},
		},
		{
			name: "while the caller isn't reading",
			setup: func(t *testing.T, srv *herdrtest.Server, sub *herdrtest.Subscriber) {
				// More events than the channel buffers, so the client blocks sending.
				for range 100 {
					if err := sub.Push("pane_closed", map[string]string{"pane_id": "w1:p2"}); err != nil {
						t.Fatalf("Push: %v", err)
					}
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := herdrtest.New(t)
			c := newClient(t, srv)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			events, err := c.Subscribe(ctx, herdr.PaneClosed())
			if err != nil {
				t.Fatalf("Subscribe: %v", err)
			}
			sub := nextSubscriber(t, srv)
			if tt.setup != nil {
				tt.setup(t, srv, sub)
			}
			cancel()
			waitClosed(t, events)
		})
	}
}

func TestSubscribeHandshakeTimesOut(t *testing.T) {
	srv := herdrtest.New(t)
	srv.Handle("events.subscribe", silence)
	c := newClient(t, srv, herdr.WithTimeout(50*time.Millisecond))

	if _, err := c.Subscribe(context.Background(), herdr.PaneClosed()); !errors.Is(err, herdr.ErrTimeout) {
		t.Errorf("err = %v, want ErrTimeout", err)
	}
}

func TestSubscribeRetriesAHungHandshake(t *testing.T) {
	srv := herdrtest.New(t)
	c := newClient(t, srv, herdr.WithTimeout(50*time.Millisecond))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	events, err := c.Subscribe(ctx, herdr.PaneClosed())
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	first := nextSubscriber(t, srv)

	// The first attempt after the drop is never answered, like a server stuck
	// part way through a handoff. The next one goes through.
	attempts := make(chan struct{}, 8)
	srv.Handle("events.subscribe", func(req herdrtest.Request) (any, *herdrtest.ErrorBody) {
		attempts <- struct{}{}
		if len(attempts) == 1 {
			return silence(req)
		}
		return map[string]string{"type": "subscription_started"}, nil
	})
	first.Drop()

	if ev := nextEvent(t, events); ev.Kind != herdr.EventReconnected {
		t.Fatalf("event = %+v, want reconnected", ev)
	}
	if n := len(attempts); n != 2 {
		t.Errorf("subscribe attempts after the drop = %d, want 2", n)
	}
}

func TestSubscribeQuietStreamDoesNotTimeOut(t *testing.T) {
	srv := herdrtest.New(t)
	c := newClient(t, srv, herdr.WithTimeout(20*time.Millisecond))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	events, err := c.Subscribe(ctx, herdr.PaneClosed())
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	sub := nextSubscriber(t, srv)

	// Letting time pass is the point of this test, not a way to synchronise.
	// If the handshake deadline were left on the connection, the stream would
	// fail during the wait and the next event would be a reconnect.
	time.Sleep(100 * time.Millisecond)
	_ = sub.Push("pane_closed", map[string]string{"pane_id": "w1:p2"})
	if ev := nextEvent(t, events); ev.Kind != herdr.EventPaneClosed {
		t.Errorf("event = %+v, want the pane_closed", ev)
	}
}

func TestSubscribeEventTooLarge(t *testing.T) {
	srv := herdrtest.New(t)
	c := newClient(t, srv)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	events, err := c.Subscribe(ctx, herdr.PaneClosed())
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	sub := nextSubscriber(t, srv)
	pushed := make(chan struct{})
	go func() {
		defer close(pushed)
		_ = sub.PushRaw(strings.Repeat("x", herdr.MaxEventBytes+1))
	}()
	defer func() { <-pushed }()

	ev := nextEvent(t, events)
	if ev.Kind != herdr.EventReconnected || !errors.Is(ev.Err, herdr.ErrTooLarge) {
		t.Errorf("event = %+v, want reconnected because of ErrTooLarge", ev)
	}
}

func TestSubscribeEventInTheSameWriteAsTheReply(t *testing.T) {
	srv := herdrtest.New(t)
	srv.Handle("events.subscribe", func(herdrtest.Request) (any, *herdrtest.ErrorBody) {
		return herdrtest.Raw(`{"id":"x","result":{"type":"subscription_started"}}` + "\n" +
			`{"event":"pane_closed","data":{"type":"pane_closed","pane_id":"w1:p3","workspace_id":"w1"}}` + "\n"), nil
	})
	c := newClient(t, srv)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	events, err := c.Subscribe(ctx, herdr.PaneClosed())
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if ev := nextEvent(t, events); ev.Kind != herdr.EventPaneClosed || ev.PaneID != "w1:p3" {
		t.Errorf("event = %+v, want pane_closed for w1:p3", ev)
	}
}
