package herdr

import (
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
	// treat it as a possible gap and read the panes again.
	EventReconnected EventKind = "reconnected"
	// EventFailed means herdr refused to resubscribe after a reconnect, usually
	// because a pane in the subscription has gone. Err says why. It's always the
	// last event before the channel closes.
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
	// Err is set for EventFailed.
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
	dec  *json.Decoder
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
		dec:  json.NewDecoder(conn),
		stop: context.AfterFunc(ctx, func() { _ = conn.Close() }),
	}

	params := struct {
		Subscriptions []Subscription `json:"subscriptions"`
	}{subs}
	raw, err := exchange(conn, sc.dec, c.nextID(), method, params)
	if err == nil {
		err = decodeResult(method, raw, "subscription_started", nil)
	} else if ctxErr := ctx.Err(); ctxErr != nil {
		err = fmt.Errorf("herdr %s: %w", method, ctxErr)
	} else {
		err = fmt.Errorf("herdr %s: %w", method, err)
	}
	if err != nil {
		sc.close()
		return nil, err
	}
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

	for {
		streamEvents(sc, send)
		sc.close()
		if ctx.Err() != nil {
			return
		}

		var err error
		sc, err = c.reconnect(ctx, subs)
		if err != nil {
			var herr *Error
			if errors.As(err, &herr) {
				send(Event{Kind: EventFailed, Err: err})
			}
			return
		}
		if !send(Event{Kind: EventReconnected}) {
			sc.close()
			return
		}
	}
}

// reconnect keeps trying to subscribe again, backing off between attempts. It
// gives up when ctx ends or herdr itself refuses the subscription, since trying
// again won't change its mind.
func (c *Client) reconnect(ctx context.Context, subs []Subscription) (*subscriptionConn, error) {
	backoff := c.minBackoff
	timer := time.NewTimer(backoff)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timer.C:
		}

		sc, err := c.openSubscription(ctx, subs)
		if err == nil {
			return sc, nil
		}
		var herr *Error
		if errors.As(err, &herr) || ctx.Err() != nil {
			return nil, err
		}

		backoff = min(backoff*2, c.maxBackoff)
		timer.Reset(backoff)
	}
}

// streamEvents reads events from sc and hands them to send until the
// connection ends, a line can't be decoded or send says to stop. A decode error
// is treated like a dropped connection, because a JSON stream can't be resumed
// part way through a bad value.
func streamEvents(sc *subscriptionConn, send func(Event) bool) {
	for {
		var env struct {
			Event string          `json:"event"`
			Data  json.RawMessage `json:"data"`
			Error *Error          `json:"error"`
		}
		if err := sc.dec.Decode(&env); err != nil {
			return
		}
		if env.Error != nil {
			return
		}
		if env.Event == "" {
			continue
		}
		if !send(decodeEvent(env.Event, env.Data)) {
			return
		}
	}
}

// decodeEvent turns one envelope into an Event. herdr uses two naming styles:
// subscription events are dotted (pane.agent_status_changed) and lifecycle
// events use underscores (pane_closed, with the name repeated in data.type).
// Both map to the same kinds.
func decodeEvent(name string, data json.RawMessage) Event {
	ev := Event{Kind: EventOther, Name: name, Data: data}

	var fields struct {
		PaneID      string      `json:"pane_id"`
		WorkspaceID string      `json:"workspace_id"`
		AgentStatus AgentStatus `json:"agent_status"`
		Agent       string      `json:"agent"`
		Scroll      *ScrollInfo `json:"scroll"`
	}
	if len(data) > 0 {
		if err := json.Unmarshal(data, &fields); err != nil {
			return ev
		}
	}
	ev.PaneID = fields.PaneID
	ev.WorkspaceID = fields.WorkspaceID

	switch strings.Replace(name, ".", "_", 1) {
	case "pane_agent_status_changed":
		ev.Kind = EventAgentStatusChanged
		ev.AgentStatus = fields.AgentStatus
		ev.Agent = fields.Agent
	case "pane_scroll_changed":
		ev.Kind = EventScrollChanged
		ev.Scroll = fields.Scroll
	case "pane_closed":
		ev.Kind = EventPaneClosed
	case "pane_exited":
		ev.Kind = EventPaneExited
	}
	return ev
}
