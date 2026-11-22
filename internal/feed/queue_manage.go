package feed

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/swmcc/herdr-sightline/internal/engine"
	"github.com/swmcc/herdr-sightline/internal/herdr"
)

// Managing the queue from the feed (#20): taking an entry out, moving it,
// pausing everything and seeing what queue mode has sent. None of these can
// make anything go sooner or to anyone else. They only hold things back or
// change the order things wait in, and the caps in the engine still apply.

// unqueue takes en out of the queue. It stays in the feed and waits for enter.
func (m *Model) unqueue(en engine.Entry) {
	switch {
	case !en.Queued:
		m.status = "That entry isn't queued."
	case m.sending[en.ID]:
		m.status = "Queue mode is sending that one right now."
	default:
		m.engine.Unqueue(en.ID)
		m.status = "Taken out of the queue. It waits for enter now."
	}
}

// moveQueued moves en one place earlier or later in the queue.
func (m *Model) moveQueued(en engine.Entry, earlier bool) {
	if !en.Queued {
		m.status = "That entry isn't queued."
		return
	}
	if !m.engine.MoveQueued(en.ID, earlier) {
		m.status = "It can't move any further that way."
		return
	}
	pos := 0
	for i, id := range m.engine.Queue() {
		if id == en.ID {
			pos = i + 1
		}
	}
	m.status = fmt.Sprintf("Moved to number %d in the queue.", pos)
}

// togglePause pauses or resumes every automatic send. A send already being
// checked is stopped too, because the check asks the engine again.
func (m *Model) togglePause() {
	paused := !m.engine.QueuePaused()
	m.engine.SetQueuePaused(paused)
	if paused {
		m.status = "Queue mode is paused. Nothing goes on its own until you press s again."
		return
	}
	m.status = "Queue mode carries on."
}

// queuePosition is en's place in the queue, counting from 1, and how many
// entries are queued, or 0 and 0 if it isn't queued.
func (m *Model) queuePosition(en engine.Entry) (int, int) {
	q := m.engine.Queue()
	for i, id := range q {
		if id == en.ID {
			return i + 1, len(q)
		}
	}
	return 0, 0
}

// openLog shows what queue mode has sent.
func (m *Model) openLog() {
	if len(m.engine.AutoLog()) == 0 {
		m.status = "Queue mode hasn't sent anything yet."
		return
	}
	m.mode, m.logCursor = modeLog, 0
}

func (m *Model) logKey(k tea.KeyMsg) {
	log := m.engine.AutoLog()
	switch k.String() {
	case "esc", "q", "a":
		m.mode = modeList
	case "j", "down":
		m.logCursor = min(m.logCursor+1, len(log)-1)
	case "k", "up":
		m.logCursor = max(m.logCursor-1, 0)
	case "enter", "v":
		if m.logCursor < len(log) {
			s := log[m.logCursor]
			m.mode, m.viewText, m.viewTop = modeView, s.Text, 0
			m.viewID = ""
			if len(s.Entries) > 0 {
				m.viewID = s.Entries[0]
			}
			m.viewSent, m.viewSentAt, m.viewBatch = true, s.At, nil
			m.viewWatch, m.viewInert, m.viewGone = false, false, false
			m.viewTarget = herdr.Target{PaneID: s.Agent, Label: m.agentLabel(s.Agent)}
		}
	}
}

// logView lists what queue mode has sent, newest first.
func (m *Model) logView() string {
	w := max(m.width, 20)
	log := m.engine.AutoLog()
	out := []string{faint.Render(fit("What queue mode has sent, newest first", w)), ""}
	room := max(m.height-5, 1)
	start := max(0, m.logCursor-room+1)
	for i := start; i < len(log) && i < start+room; i++ {
		s := log[i]
		line := fmt.Sprintf("%s  %s to %s from %s · %s", s.At.Format(clockFormat),
			plural(len(s.Entries), "error", "errors"), clean(m.agentLabel(s.Agent)),
			m.paneName(string(s.Source)), plural(len(s.Text), "byte", "bytes"))
		if s.Unsure {
			line += " · may not have arrived"
		}
		if i == m.logCursor {
			out = append(out, bold.Render(fit("› "+line, w)))
		} else {
			out = append(out, fit("  "+line, w))
		}
	}
	out = append(out, "", faint.Render(fit("j/k move  enter view exactly what went  esc back", w)))
	return strings.Join(out, "\n")
}
