package feed

import (
	"fmt"
	"slices"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/swmcc/herdr-sightline/internal/config"
	"github.com/swmcc/herdr-sightline/internal/engine"
	"github.com/swmcc/herdr-sightline/internal/herdr"
)

// Queue mode (#19) sends errors without a keypress at the moment they go, so
// it keeps to the rules decided in #21. The engine owns the queue and the caps
// (see engine/queue.go). The feed owns the rest:
//
//   - Consent. Queue mode is off for every pane until m on one of its entries
//     turns it on, and the first time for a pane in a session that opens a
//     screen saying what it means, where only y says yes. sightline.toml can
//     ask for it, but only makes the feed remind the person.
//   - The agent. It goes only to an agent the feed is sure of: chosen without
//     asking, not blocked, not one that needs a second enter (another tab, a
//     send that may already have arrived) and not through a pin that matched
//     a terminal title, which the watched pane sets itself.
//   - Idle. The agent must have been idle for idleFor and not still be on an
//     earlier send. herdr is asked again just before sending, and the send is
//     refused if the agent isn't idle then.
//   - One at a time. autoSend runs on the tick, starts at most one send and
//     none while another is in flight.
//
// The message is the engine's, sent byte for byte, and the feed shows what
// went and when.

// idleFor is how long an agent must have been idle before queue mode sends
// to it, so a pause between two steps of its work isn't taken for the end.
const idleFor = 3 * time.Second

// clockFormat is how the feed shows the time of an automatic send.
const clockFormat = "15:04:05"

// toggleQueue switches the pane en came from between notify and queue. Turning
// it on for the first time in a session asks first.
func (m *Model) toggleQueue(en engine.Entry) {
	pane := string(en.Source.ID)
	name := m.paneName(pane)
	if !m.isWatching(pane) {
		m.status = fmt.Sprintf("%s isn't watched any more, so queue mode can't go on for it.", name)
		return
	}
	src := engine.SourceID(pane)
	if m.engine.QueueMode(src) {
		m.engine.SetQueueMode(src, false)
		m.status = fmt.Sprintf("Queue mode is off for %s. Its errors wait for enter again.", name)
		return
	}
	if !m.consented[pane] {
		m.mode, m.consentPane = modeConsent, pane
		return
	}
	m.queueOn(pane)
}

// queueOn turns queue mode on for a pane the person has agreed to.
func (m *Model) queueOn(pane string) {
	m.engine.SetQueueMode(engine.SourceID(pane), true)
	m.status = fmt.Sprintf("Queue mode is on for %s. Press m on one of its entries to turn it off.", m.paneName(pane))
}

// consentKey takes the answer to the consent screen. Only y says yes.
func (m *Model) consentKey(k tea.KeyMsg) {
	pane := m.consentPane
	m.mode, m.consentPane = modeList, ""
	if k.String() != "y" {
		m.status = fmt.Sprintf("Queue mode stays off for %s.", m.paneName(pane))
		return
	}
	if !m.isWatching(pane) {
		m.status = fmt.Sprintf("%s isn't watched any more, so queue mode can't go on for it.", m.paneName(pane))
		return
	}
	m.consented[pane] = true
	m.queueOn(pane)
}

// consentView says plainly what queue mode means before it goes on.
func (m *Model) consentView() string {
	w := max(m.width, 20)
	// The title can come from the program in the pane, so it's cut short and
	// herdr's pane id, which the pane can't set, is always there too.
	name := fmt.Sprintf("%s (pane %s)", fit(m.paneName(m.consentPane), 32), clean(m.consentPane))
	who := "Right now Sightline would ask which agent, so nothing would go until that's settled."
	if t, why := m.autoTargetForPane(m.consentPane); why == "" {
		who = fmt.Sprintf("Right now that's %s.", clean(t.Label))
	}
	waiting := 0
	for _, en := range m.entries {
		if string(en.Source.ID) == m.consentPane && en.State == engine.StateNew {
			waiting++
		}
	}
	paras := []string{
		bold.Render("Turn on queue mode for " + name + "?"),
		"",
		"Errors from this pane will be sent to an agent on their own, without you pressing enter, once it has been idle for a few seconds. " + who,
		"",
		"Anyone who can put text in this pane, such as someone sending requests to your dev server, can then get text in front of the agent without you reading it first. Sightline still marks it all as data from another pane, but nobody will have checked it.",
		"",
		fmt.Sprintf("Sightline holds back anything it isn't sure of, sends at most %d errors at a time and at most 3 messages to an agent in 10 minutes, and stops for this pane if a fix it sent didn't work.", engine.MaxAutoBatch),
	}
	if waiting > 0 {
		paras = append(paras, "", fmt.Sprintf("%s from this pane already in the feed will join the queue.", plural(waiting, "error", "errors")))
	}
	paras = append(paras, "", "This lasts until the feed closes or the pane stops being watched.")
	var out []string
	for _, p := range paras {
		out = append(out, wrapWords(p, w)...)
	}
	out = append(out, "", faint.Render(fit("y turn it on   any other key leaves it off", w)))
	return strings.Join(out, "\n")
}

// wrapWords breaks s into lines of at most w cells, at spaces.
func wrapWords(s string, w int) []string {
	if s == "" {
		return []string{""}
	}
	var out []string
	line := ""
	for _, word := range strings.Fields(s) {
		switch {
		case line == "":
			line = word
		case len([]rune(line))+1+len([]rune(word)) > w:
			out = append(out, line)
			line = word
		default:
			line += " " + word
		}
	}
	return append(out, line)
}

// autoTarget is who queue mode would send en to, or why it won't.
func (m *Model) autoTarget(en engine.Entry) (herdr.Target, string) {
	t, ask, note := m.chooseFor(en)
	switch {
	case ask != "":
		return t, "Sightline would ask first: " + ask
	case t.Status == herdr.AgentBlocked:
		return t, "the agent is waiting on a prompt"
	case m.needsConfirm(en, t) != "":
		return t, "sending it would need a second enter"
	case note == titlePinNote:
		return t, "the agent was only pinned by terminal title"
	case m.titleDecidesPin(string(en.Source.ID)):
		return t, "the pane has no label, so its own title decides whether a pin applies"
	case note != "":
		// A pin whose agent isn't running fell back to the tab. That's not
		// an agent Sightline is sure of, so it waits for a keypress.
		return t, "the pinned agent isn't running, so it fell back to the tab"
	}
	return t, ""
}

// titleDecidesPin reports whether pane has no label while sightline.toml pins
// some pane to an agent. Then the program in the pane chooses, by setting its
// title, whether a pin matches it, and so which agent it goes to. That's fine
// for a keypress, which shows the agent first, but not for queue mode.
func (m *Model) titleDecidesPin(pane string) bool {
	if p, ok := m.panes[m.resolve(pane)]; ok && p.Label != "" {
		return false
	}
	for _, pin := range m.cfg.Pins {
		if pin.Agent != "" {
			return true
		}
	}
	return false
}

// autoTargetForPane is autoTarget for a pane with no entry yet.
func (m *Model) autoTargetForPane(pane string) (herdr.Target, string) {
	return m.autoTarget(engine.Entry{Source: engine.Source{ID: engine.SourceID(pane)}})
}

// agentReady says why queue mode can't send to t at now, or "" if it can.
func (m *Model) agentReady(t herdr.Target, now time.Time) string {
	// You may be typing in a pane that has focus, and herdr doesn't say whether
	// a prompt clears a half typed draft. So queue mode waits until you move
	// away. checked asks herdr again just before sending.
	if p, ok := m.panes[t.PaneID]; ok && p.Focused {
		return focusedNote
	}
	if _, ok := m.awaiting[t.PaneID]; ok {
		return "waiting for it to finish what Sightline sent it"
	}
	since, idle := m.tracker.IdleSince(t.PaneID)
	switch {
	case !idle:
		return "waiting for it to go idle"
	case now.Sub(since) < idleFor:
		return "it has only just gone idle"
	}
	return m.engine.AutoWait(t.PaneID, now)
}

// autoSend starts the one automatic send queue mode may make now, if any. It's
// called on every tick and is the only thing that starts a send without a
// keypress. It takes the queued entries from the first pane that has any that
// may go, keeps those going to the same agent as the first, up to
// MaxAutoBatch, and hands them to the engine, which builds the message and
// counts it against the caps.
func (m *Model) autoSend() tea.Cmd {
	// Any send in flight, by hand or not, waits for its answer first, so two
	// prompts can never reach an agent before Sightline knows of the first.
	if m.autoBusy || len(m.sending) > 0 || !m.synced || m.engine.QueuePaused() {
		return nil
	}
	now := m.now()
	for _, pane := range m.watching {
		if pane == m.viewFresh {
			// The view of what would go from pane changed on this tick and
			// hasn't been on screen yet. It goes on a later tick.
			continue
		}
		ids, target := m.nextBatch(pane)
		if len(ids) == 0 || m.agentReady(target, now) != "" {
			continue
		}
		text, err := m.engine.AutoSend(ids, target.PaneID, now)
		if err != nil {
			continue
		}
		m.autoBusy, m.autoPane = true, target.PaneID
		for _, id := range ids {
			m.sending[id] = true
		}
		m.status = fmt.Sprintf("Queue mode is checking %s before sending...", clean(target.Label))
		msg := checkedMsg{ids: ids, auto: true, target: target, text: text}
		msg.wantSeq, msg.wantSeqKnown = m.tracker.StateChangeSeq(target.PaneID)
		return checkTargetWith(m.ctx, m.client, m.now, msg)
	}
	return nil
}

// nextBatch is what autoSend would hand the engine for pane once its agent is
// ready, and that agent. It's the queued entries from pane that may go, apart
// from any being sent, keeping only those that go to the same agent as the
// first, up to MaxAutoBatch. It's empty if the person hasn't agreed to queue
// mode for pane.
func (m *Model) nextBatch(pane string) ([]engine.EntryID, herdr.Target) {
	if !m.consented[pane] {
		return nil, herdr.Target{}
	}
	var ids []engine.EntryID
	var target herdr.Target
	for _, id := range m.engine.QueueCandidates(engine.SourceID(pane)) {
		en, ok := m.entry(id)
		if !ok || m.sending[id] {
			continue
		}
		t, why := m.autoTarget(en)
		if why != "" || (len(ids) > 0 && !sameAgent(t, target)) {
			continue
		}
		target = t
		ids = append(ids, id)
		if len(ids) == engine.MaxAutoBatch {
			break
		}
	}
	return ids, target
}

// openBatchView shows the batch queue mode would send with id, byte for byte
// as AutoSend would build it now (#92), if id would go with others. It
// reports whether it did. A batch of one is exactly what enter sends, so
// openView shows that as usual.
func (m *Model) openBatchView(id engine.EntryID) bool {
	en, ok := m.entry(id)
	if !ok || !en.Queued {
		return false
	}
	ids, target := m.nextBatch(string(en.Source.ID))
	if len(ids) < 2 || !slices.Contains(ids, id) {
		return false
	}
	text, err := m.engine.PreviewBatch(ids)
	if err != nil {
		return false
	}
	m.mode, m.viewID, m.viewText = modeView, id, text
	m.viewSent, m.viewBatch, m.viewTarget = false, ids, target
	return true
}

// followView keeps the view of a queued entry showing what queue mode would
// send with it now (#92). Lines can join an entry, and entries can join,
// leave or move in the queue, while the person reads it, so a lone entry can
// become a batch and a batch can fall back to one entry. refresh calls it
// while such a view is open.
//
// If what would go changed, the view shows the new bytes, says so, and holds
// queue mode back for that pane until the next tick (see autoSend), so
// nothing goes that wasn't on screen first.
func (m *Model) followView() {
	// While a send is being checked, nextBatch skips its entries, so the view
	// keeps what's going until the send is settled.
	if m.sending[m.viewID] || slices.ContainsFunc(m.viewBatch, func(id engine.EntryID) bool { return m.sending[id] }) {
		return
	}
	en, ok := m.entry(m.viewID)
	if !ok {
		// refresh says the entry was dropped. Nothing on screen can go now.
		m.viewWatch, m.viewGone = false, true
		return
	}
	text, ids, target := m.viewText, m.viewBatch, m.viewTarget
	top, inert, status := m.viewTop, m.viewInert || m.viewBatch != nil, m.status
	m.openView(m.viewID)
	// A view that has shown a batch stays one where enter does nothing until
	// v opens it again, so pane output can't turn it into one that sends.
	m.viewInert = inert
	if m.viewText == text && slices.Equal(m.viewBatch, ids) && sameAgent(m.viewTarget, target) {
		m.viewTop = top
		return
	}
	m.viewTop = 0
	now, _ := m.entry(m.viewID)
	if !now.Queued || m.viewSent {
		// It went, or queue mode stopped for its pane. Whatever the send set
		// the status to says what happened, so it stays.
		m.status = status
		return
	}
	m.viewFresh = string(en.Source.ID)
	m.status = "What queue mode would send changed while you were reading it. This is what would go now."
}

// batchHeader is the view's header line for a batch: who it goes to and
// which places in the queue it holds.
func (m *Model) batchHeader() string {
	q := m.engine.Queue()
	var places []string
	for _, id := range m.viewBatch {
		if i := slices.Index(q, id); i >= 0 {
			places = append(places, fmt.Sprint(i+1))
		}
	}
	return fmt.Sprintf("The batch queue mode will send to %s once it can, queued %s of %d · %s",
		clean(m.viewTarget.Label), joinAnd(places), len(q), plural(len(m.viewText), "byte", "bytes"))
}

// joinAnd lists words as a sentence does: "a", "a and b", "a, b and c".
func joinAnd(parts []string) string {
	if len(parts) < 2 {
		return strings.Join(parts, "")
	}
	return strings.Join(parts[:len(parts)-1], ", ") + " and " + parts[len(parts)-1]
}

// autoTargetStill reports whether the entries of an automatic send would
// still go to the agent it was checked for.
func (m *Model) autoTargetStill(msg checkedMsg) bool {
	for _, id := range msg.ids {
		en, ok := m.entry(id)
		if !ok {
			return false
		}
		if t, why := m.autoTarget(en); why != "" || !sameAgent(t, msg.target) {
			return false
		}
	}
	return true
}

// queueNote says where a queued entry stands: who it's waiting for and why it
// hasn't gone yet.
func (m *Model) queueNote(en engine.Entry) string {
	src := en.Source.ID
	t, why := m.autoTarget(en)
	who := "an agent"
	if t.PaneID != "" {
		who = clean(t.Label)
	}
	switch {
	case en.Hold != "":
		why = string(en.Hold) + ", so it waits for enter"
	case m.engine.Halted(src):
		why = "queue mode stopped for this pane, so it waits for enter"
	case m.engine.QueuePaused():
		why = "queue mode is paused"
	case why == "" && m.sending[en.ID]:
		why = "sending now"
	case why == "":
		why = m.agentReady(t, m.now())
	}
	if why == "" {
		why = "going on the next tick"
	}
	pos, of := m.queuePosition(en)
	return fmt.Sprintf("◷ %d of %d queued for %s, %s", pos, of, who, why)
}

// openSentView shows exactly what queue mode sent for id, if its latest send
// was automatic. It reports whether it did.
func (m *Model) openSentView(id engine.EntryID) bool {
	// Only while it waits on that send. Once there's a verdict, or it's still
	// failing with fresh lines, enter would send something else, and v shows
	// what enter sends.
	en, ok := m.entry(id)
	if !ok || !en.Auto || en.State != engine.StateSent {
		return false
	}
	s, ok := m.engine.LastAutoSend(id)
	if !ok {
		return false
	}
	m.mode, m.viewID, m.viewText, m.viewTop = modeView, id, s.Text, 0
	m.viewSent, m.viewSentAt, m.viewBatch = true, s.At, nil
	m.viewTarget = herdr.Target{PaneID: s.Agent, Label: m.agentLabel(s.Agent)}
	return true
}

// queueHeader is the header's lines about queue mode: which panes it's on for
// and which sightline.toml asks for.
func (m *Model) queueHeader() []string {
	var on, asked []string
	for _, pane := range m.watching {
		src := engine.SourceID(pane)
		name := m.paneName(pane)
		switch {
		case m.engine.Halted(src):
			on = append(on, name+" (stopped until you send one by hand)")
		case m.engine.QueueMode(src):
			on = append(on, name)
		case m.configAsksQueue(pane):
			asked = append(asked, name)
		}
	}
	var lines []string
	if len(on) > 0 {
		line := fmt.Sprintf("Queue mode on for %s · %d queued", strings.Join(on, ", "), len(m.engine.Queue()))
		if m.engine.QueuePaused() {
			line += " · paused, press s to carry on"
		}
		lines = append(lines, line)
	}
	if len(asked) > 0 {
		lines = append(lines, "sightline.toml asks for queue mode on "+strings.Join(asked, ", ")+". Press m on one of its entries to turn it on.")
	}
	return lines
}

// configAsksQueue reports whether sightline.toml asks for queue mode on pane.
// Only the pane's label counts, never its terminal title, which the program
// in the pane can set.
func (m *Model) configAsksQueue(pane string) bool {
	p, ok := m.panes[pane]
	if !ok || p.Label == "" {
		return false
	}
	for _, pin := range m.cfg.Pins {
		if pin.PaneTitle == p.Label {
			return pin.Mode == config.ModeQueue
		}
	}
	return false
}

// focusedNote is why queue mode waits while the agent's pane has focus.
const focusedNote = "its pane has focus, and you may be typing in it"
