package herdr

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"
)

// Subscription is one kind of event to hear about, as events.subscribe takes it.
//
// pane.output_matched is deliberately missing. It's edge triggered and misses
// bursts (see #1), so Sightline reads panes itself with ReadPane instead.
type Subscription struct {
	Type   string `json:"type"`
	PaneID string `json:"pane_id,omitempty"`
}

// AgentStatusChanged subscribes to status changes of the agent in a pane.
func AgentStatusChanged(paneID string) Subscription {
	return Subscription{Type: "pane.agent_status_changed", PaneID: paneID}
}

// ScrollChanged subscribes to a pane's scroll position changing, which happens
// when lines move into its history. It's a hint to read the pane sooner.
func ScrollChanged(paneID string) Subscription {
	return Subscription{Type: "pane.scroll_changed", PaneID: paneID}
}

// PaneClosed subscribes to any pane closing.
func PaneClosed() Subscription { return Subscription{Type: "pane.closed"} }

// PaneExited subscribes to the process in any pane exiting.
func PaneExited() Subscription { return Subscription{Type: "pane.exited"} }

// EventKind says what an Event is about.
type EventKind string

// The kinds of Event a subscription delivers.
const (
	EventAgentStatusChanged EventKind = "agent_status_changed"
	EventScrollChanged      EventKind = "scroll_changed"
	EventPaneClosed         EventKind = "pane_closed"
	EventPaneExited         EventKind = "pane_exited"
	// EventReconnected means the connection dropped and has been re-established
	// with the same subscriptions. Anything could have happened in between, so
	// treat it as a possible gap and read the panes again. Err says why the
	// old connection ended, for debugging.
	EventReconnected EventKind = "reconnected"
	// EventFailed means herdr refused to resubscribe after a reconnect because
	// a pane in the subscription has gone or the request is invalid. Err says
	// why. It's always the last event before the channel closes.
	EventFailed EventKind = "failed"
	// EventOther is any event Sightline doesn't have a kind for. Name and Data
	// hold what herdr sent.
	EventOther EventKind = "other"
)

// ScrollInfo is where a pane is scrolled to.
type ScrollInfo struct {
	OffsetFromBottom    uint64 `json:"offset_from_bottom"`
	MaxOffsetFromBottom uint64 `json:"max_offset_from_bottom"`
	ViewportRows        uint64 `json:"viewport_rows"`
}

// Event is one thing herdr told a subscription about, or a note from the client
// itself (EventReconnected and EventFailed).
type Event struct {
	Kind EventKind
	// Name is herdr's own name for the event, exactly as sent.
	Name        string
	PaneID      string
	WorkspaceID string
	// AgentStatus and Agent are set for EventAgentStatusChanged.
	AgentStatus AgentStatus
	Agent       string
	// Scroll is set for EventScrollChanged.
	Scroll *ScrollInfo
	// Data is the event's payload as herdr sent it.
	Data json.RawMessage
	// Err is set for EventFailed and EventReconnected.
	Err error
}

// Subscribe holds a connection open to herdr and delivers the events for subs
// on the returned channel.
//
// If the connection drops (herdr restarting or handing off to a new server) it
// reconnects with backoff, subscribes again and sends EventReconnected. The
// channel closes when ctx is cancelled, or after an EventFailed if herdr refuses
// to resubscribe. The goroutine behind it stops at the same moment, so cancel
// ctx and drain or abandon the channel to clean up.
//
// The first subscribe happens before Subscribe returns, so a bad subscription
// or an unreachable herdr is reported straight away as an error.
//
// One pane that has gone fails the whole resubscribe, so keep subscriptions
// that name a pane apart from global ones like PaneClosed, in separate calls.
// Then a closed pane can't take the global stream down with it.
func (c *Client) Subscribe(ctx context.Context, subs ...Subscription) (<-chan Event, error) {
	if len(subs) == 0 {
		return nil, errors.New("herdr events.subscribe: no subscriptions given")
	}
	sc, err := c.openSubscription(ctx, subs)
	if err != nil {
		return nil, err
	}
	events := make(chan Event, 64)
	go c.runSubscription(ctx, subs, sc, events)
	return events, nil
}

// subscriptionConn is a connection that has been through the subscribe
// handshake and is now streaming events.
type subscriptionConn struct {
	conn net.Conn
	r    *bufio.Reader
	stop func() bool
}

func (sc *subscriptionConn) close() {
	sc.stop()
	_ = sc.conn.Close()
}

func (c *Client) openSubscription(ctx context.Context, subs []Subscription) (*subscriptionConn, error) {
	const method = "events.subscribe"
	conn, err := c.dial(ctx)
	if err != nil {
		return nil, fmt.Errorf("herdr %s: dialling: %w", method, err)
	}
	// Closing the connection when ctx ends unblocks the handshake or the event
	// reader, whichever is running.
	sc := &subscriptionConn{
		conn: conn,
		r:    bufio.NewReaderSize(conn, readBufferSize),
		stop: context.AfterFunc(ctx, func() { _ = conn.Close() }),
	}

	// The handshake gets the same timeout as a request. A timeout here isn't
	// permanent, so a reconnect that hangs goes round the backoff loop again.
	_ = conn.SetDeadline(time.Now().Add(c.timeout))
	params := struct {
		Subscriptions []Subscription `json:"subscriptions"`
	}{subs}
	raw, err := exchange(conn, sc.r, MaxEventBytes, c.nextID(), method, params)
	if err == nil {
		err = decodeResult(method, raw, "subscription_started", nil)
	} else {
		err = c.callError(ctx, method, err)
	}
	if err != nil {
		sc.close()
		return nil, err
	}
	// A running subscription can sit quiet for as long as nothing happens.
	_ = conn.SetDeadline(time.Time{})
	return sc, nil
}

// runSubscription owns sc and events. It streams events, reconnects when the
// connection drops and closes events when it's done.
func (c *Client) runSubscription(ctx context.Context, subs []Subscription, sc *subscriptionConn, events chan<- Event) {
	defer close(events)

	send := func(ev Event) bool {
		select {
		case events <- ev:
			return true
		case <-ctx.Done():
			return false
		}
	}

	backoff := c.minBackoff
	for {
		delivered, cause := streamEvents(sc, send)
		sc.close()
		if ctx.Err() != nil {
			return
		}

		// Only start again from the shortest wait if the last connection did
		// some work. Otherwise a server that accepts and then hangs up straight
		// away would be hammered, with a reconnected event every time.
		if delivered {
			backoff = c.minBackoff
		}
		var err error
		sc, backoff, err = c.reconnect(ctx, subs, backoff)
		if err != nil {
			if permanent(err) {
				send(Event{Kind: EventFailed, Err: err})
			}
			return
		}
		if !send(Event{Kind: EventReconnected, Err: cause}) {
			sc.close()
			return
		}
	}
}

// reconnect keeps trying to subscribe again, waiting first and backing off
// between attempts. It returns the wait to use next time. It gives up when ctx
// ends or herdr refuses the subscription in a way trying again won't fix.
func (c *Client) reconnect(ctx context.Context, subs []Subscription, backoff time.Duration) (*subscriptionConn, time.Duration, error) {
	timer := time.NewTimer(backoff)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil, backoff, ctx.Err()
		case <-timer.C:
		}
		backoff = min(backoff*2, c.maxBackoff)

		sc, err := c.openSubscription(ctx, subs)
		if err == nil {
			return sc, backoff, nil
		}
		if permanent(err) || ctx.Err() != nil {
			return nil, backoff, err
		}
		timer.Reset(backoff)
	}
}

// permanent reports whether err is herdr refusing a subscription for a reason
// that won't change by asking again. Anything else, including other herdr
// errors that a restarting server might send, is worth retrying.
func permanent(err error) bool {
	var herr *Error
	if !errors.As(err, &herr) {
		return false
	}
	return herr.Code == CodePaneNotFound || herr.Code == CodeInvalidRequest
}

// streamEvents reads events from sc and hands them to send until the
// connection ends, a line can't be decoded or send says to stop. It reports
// whether it delivered anything and why the stream ended. A decode error is
// treated like a dropped connection, because a JSON stream can't be resumed part
// way through a bad value.
func streamEvents(sc *subscriptionConn, send func(Event) bool) (delivered bool, cause error) {
	for {
		var env struct {
			Event string          `json:"event"`
			Data  json.RawMessage `json:"data"`
			Error *Error          `json:"error"`
		}
		line, err := readLine(sc.r, MaxEventBytes)
		if err != nil {
			return delivered, fmt.Errorf("herdr events.subscribe: reading events: %w", err)
		}
		if err := json.Unmarshal(line, &env); err != nil {
			return delivered, fmt.Errorf("herdr events.subscribe: decoding an event: %w", err)
		}
		if env.Error != nil {
			env.Error.Method = "events.subscribe"
			return delivered, env.Error
		}
		if env.Event == "" {
			continue
		}
		if !send(decodeEvent(env.Event, env.Data)) {
			return delivered, nil
		}
		delivered = true
	}
}

// decodeEvent turns one envelope into an Event. herdr uses two naming styles:
// subscription events are dotted (pane.agent_status_changed) and lifecycle
// events use underscores (pane_closed, with the name repeated in data.type).
// Both map to the same kinds.
//
// The kind comes from the name alone and the fields are filled as best they
// can be, so a pane closing still arrives as EventPaneClosed even if herdr
// changes the shape of one of its fields.
func decodeEvent(name string, data json.RawMessage) Event {
	ev := Event{Kind: EventOther, Name: name, Data: data}
	switch strings.Replace(name, ".", "_", 1) {
	case "pane_agent_status_changed":
		ev.Kind = EventAgentStatusChanged
	case "pane_scroll_changed":
		ev.Kind = EventScrollChanged
	case "pane_closed":
		ev.Kind = EventPaneClosed
	case "pane_exited":
		ev.Kind = EventPaneExited
	}

	var fields struct {
		PaneID      string      `json:"pane_id"`
		WorkspaceID string      `json:"workspace_id"`
		AgentStatus AgentStatus `json:"agent_status"`
		Agent       string      `json:"agent"`
		Scroll      *ScrollInfo `json:"scroll"`
	}
	// A field of the wrong type is skipped and the rest still filled, which is
	// all that's wanted here, so the error is deliberately ignored.
	_ = json.Unmarshal(data, &fields)
	ev.PaneID = fields.PaneID
	ev.WorkspaceID = fields.WorkspaceID
	switch ev.Kind {
	case EventAgentStatusChanged:
		ev.AgentStatus = fields.AgentStatus
		ev.Agent = fields.Agent
	case EventScrollChanged:
		ev.Scroll = fields.Scroll
	}
	return ev
}
