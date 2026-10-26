package feed

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/swmcc/herdr-sightline/internal/config"
	"github.com/swmcc/herdr-sightline/internal/engine"
	"github.com/swmcc/herdr-sightline/internal/herdr"
)

// Engine settings sightline.toml doesn't have.
const (
	// captureWait is how long the engine waits for the rest of a stack trace.
	captureWait = time.Second
	// cooldown is how long a dismissed crash loop stays dismissed after it
	// last printed.
	cooldown = time.Minute
)

// mode is what the keys currently act on.
type mode int

const (
	modeList mode = iota // the entries
	modeView             // the exact message for one entry
	modePick             // choosing an agent for one entry
)

// await is an agent that has been sent errors and hasn't finished yet.
type await struct {
	// keys are the names the engine knows the agent by: its pane id at the time
	// of each send. A pane that moves gets a new id, but the engine's entries
	// keep the old one.
	keys []string
	// seq is herdr's state change counter for the agent just before the last
	// send. If it has moved on and the agent is idle, it worked and finished,
	// even if no snapshot caught it working.
	seq      uint64
	seqKnown bool
	// sawWorking says the agent has been seen working since the last send.
	sawWorking bool
}

// Model is the feed. It owns the engine and is its only caller, so every engine
// call happens on the Bubble Tea update loop. See loops.go for everything else.
type Model struct {
	ctx    context.Context
	client *herdr.Client
	loops  *loops
	now    func() time.Time
	self   string // the feed's own pane, never watched

	engine     *engine.Engine
	cfg        config.Config
	cfgErr     error
	cfgPending bool // sightline.toml changed but the engine still has the old one

	tracker   *herdr.AgentTracker
	synced    bool
	herdrErr  error
	eventsErr error
	panes     map[string]herdr.Pane // the last snapshot's panes, by id

	listed   []string          // the watch list as last read, with --watch panes
	listErr  error             // what was wrong with the watch list file
	watching []string          // panes being watched now, in list order
	gone     map[string]bool   // panes that closed, so the list can't bring them back
	movedTo  map[string]string // old pane id to new, for panes that moved
	paneErrs map[string]error  // the last read error for each watched pane

	awaiting map[string]*await // agent pane id to what it's been sent
	labels   map[string]string // engine agent key to how the feed shows it

	picked  map[engine.EntryID]herdr.Target // agents chosen with t
	sending map[engine.EntryID]bool         // a check or send is in flight
	armed   engine.EntryID                  // enter was pressed once on a sent entry

	entries  []engine.Entry // what the list shows, newest first
	selected engine.EntryID

	mode       mode
	viewID     engine.EntryID
	viewText   string
	viewTop    int
	pickID     engine.EntryID
	pickList   []herdr.Target
	pickCursor int

	status string
	width  int
	height int
}

// newModel makes a feed with first as its config. loops is set by Run before
// the program starts.
func newModel(ctx context.Context, client *herdr.Client, now func() time.Time, self string, first config.Update) *Model {
	m := &Model{
		ctx:      ctx,
		client:   client,
		now:      now,
		self:     self,
		tracker:  herdr.NewAgentTracker(),
		panes:    map[string]herdr.Pane{},
		gone:     map[string]bool{},
		movedTo:  map[string]string{},
		paneErrs: map[string]error{},
		awaiting: map[string]*await{},
		labels:   map[string]string{},
		picked:   map[engine.EntryID]herdr.Target{},
		sending:  map[engine.EntryID]bool{},
		width:    80,
		height:   24,
	}
	m.applyConfig(first)
	return m
}

// engineOptions turns sightline.toml into engine options.
func engineOptions(cfg config.Config) (engine.Options, error) {
	matchers, err := engine.Presets(cfg.Presets...)
	if err != nil {
		return engine.Options{}, fmt.Errorf("building patterns: %w", err)
	}
	for _, p := range cfg.Patterns {
		matchers = append(matchers, engine.Regexp(p.Name, p.Regex))
	}
	var ignores []engine.Matcher
	for _, ig := range cfg.Ignores {
		ignores = append(ignores, engine.Regexp("ignore", ig.Regex))
	}
	return engine.Options{
		Matchers:     matchers,
		Ignores:      ignores,
		ContextLines: cfg.ContextLines,
		CaptureWait:  captureWait,
		Settle:       time.Duration(cfg.SettleSeconds) * time.Second,
		Cooldown:     cooldown,
	}, nil
}

// applyConfig takes a new sightline.toml. Pins apply straight away. The engine
// can't change its options, and making a new one would lose every entry, so
// patterns and settings only apply straight away while the feed is empty.
// Otherwise the feed says they apply after a restart.
func (m *Model) applyConfig(u config.Update) {
	m.cfgErr = u.Err
	opts, err := engineOptions(u.Config)
	if err != nil {
		m.cfgErr = err
		return
	}
	m.cfg = u.Config
	if m.engine != nil && (len(m.engine.Entries()) > 0 || m.engine.Dropped() > 0) {
		m.cfgPending = true
		return
	}
	m.engine = engine.New(opts)
	m.cfgPending = false
	for _, pane := range m.watching {
		m.engine.Watch(m.source(pane))
	}
}

// Init does nothing. The loops are started by Run.
func (m *Model) Init() tea.Cmd { return nil }

// Update is the only place the engine is called from.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
	case tea.KeyMsg:
		cmd = m.key(msg)
	case tickMsg:
		m.engine.Tick(m.now())
	case deltaMsg:
		m.delta(msg)
	case paneGoneMsg:
		m.dropPane(msg.pane, "closed")
	case paneErrMsg:
		if m.isWatching(msg.pane) {
			m.paneErrs[msg.pane] = msg.err
		}
	case snapshotMsg:
		m.snapshot(msg)
	case eventMsg:
		m.event(msg.ev)
	case eventsDownMsg:
		m.eventsErr = fmt.Errorf("%s: %w", msg.what, msg.err)
	case watchListMsg:
		m.listed, m.listErr = msg.panes, msg.err
		m.applyWatchList()
	case configMsg:
		m.applyConfig(msg.update)
	case checkedMsg:
		cmd = m.checked(msg)
	case sentMsg:
		m.sent(msg)
	}
	m.refresh()
	return m, cmd
}

// refresh copies the entries out of the engine and keeps the selection on the
// same entry where it can.
func (m *Model) refresh() {
	m.entries = m.entries[:0]
	for _, en := range m.engine.Entries() {
		if en.State != engine.StateDismissed {
			m.entries = append(m.entries, en)
		}
	}
	if m.cursor() < 0 {
		m.selected = ""
		if len(m.entries) > 0 {
			m.selected = m.entries[0].ID
		}
	}
	if m.mode == modeView {
		if _, ok := m.entry(m.viewID); !ok {
			m.mode = modeList
		}
	}
	if m.mode == modePick {
		if _, ok := m.entry(m.pickID); !ok {
			m.mode = modeList
		}
	}
}

// cursor is the selected entry's index, or -1.
func (m *Model) cursor() int {
	return slices.IndexFunc(m.entries, func(en engine.Entry) bool { return en.ID == m.selected })
}

func (m *Model) entry(id engine.EntryID) (engine.Entry, bool) {
	i := slices.IndexFunc(m.entries, func(en engine.Entry) bool { return en.ID == id })
	if i < 0 {
		return engine.Entry{}, false
	}
	return m.entries[i], true
}

// delta hands one read of a pane to the engine.
func (m *Model) delta(msg deltaMsg) {
	if !m.isWatching(msg.pane) {
		return
	}
	delete(m.paneErrs, msg.pane)
	d := msg.delta
	// The first read is what was on screen before watching started. Treating
	// it as news would turn old errors into fresh alerts, so it's skipped.
	if d.Initial {
		return
	}
	// One time for the whole read. The fix check discounts a gap's replayed
	// lines by matching the time of the Gap call to the Feed call, so they
	// must be the same instant. Unsure goes before the read's lines are fed.
	id := engine.SourceID(msg.pane)
	now := m.now()
	if d.Gap {
		m.engine.Gap(id, now)
	}
	if d.Ambiguous || msg.hintsLost {
		m.engine.Unsure(id, now)
	}
	if len(d.Lines) > 0 {
		m.engine.Feed(id, d.Lines, now)
	}
}

// snapshot takes a fresh look at herdr's panes and agents.
func (m *Model) snapshot(msg snapshotMsg) {
	if msg.err != nil {
		m.herdrErr = msg.err
		return
	}
	m.herdrErr = nil
	now := m.now()
	m.tracker.Sync(msg.snap, now)
	m.synced = true

	m.panes = make(map[string]herdr.Pane, len(msg.snap.Panes))
	var agentPanes []string
	for _, p := range msg.snap.Panes {
		m.panes[p.ID] = p
		if p.Agent != "" || p.DisplayAgent != "" {
			agentPanes = append(agentPanes, p.ID)
		}
	}
	for _, a := range msg.snap.Agents {
		if !slices.Contains(agentPanes, a.PaneID) {
			agentPanes = append(agentPanes, a.PaneID)
		}
		m.agentStatus(a.PaneID, a.Status, a.StateChangeSeq, true, now)
	}
	for _, p := range msg.snap.Panes {
		if p.Agent != "" || p.DisplayAgent != "" {
			m.agentStatus(p.ID, p.AgentStatus, 0, false, now)
		}
	}
	for _, pane := range m.watching {
		m.engine.Watch(m.source(pane))
	}
	if m.loops != nil {
		m.loops.followAgents(agentPanes)
	}
}

// agentStatus notes what an agent that has been sent errors is doing, and tells
// the engine when it has finished: idle or done after being seen working, or
// after its state change counter moved on since the send.
func (m *Model) agentStatus(pane string, status herdr.AgentStatus, seq uint64, haveSeq bool, now time.Time) {
	a, ok := m.awaiting[pane]
	if !ok {
		return
	}
	switch status {
	case herdr.AgentWorking:
		a.sawWorking = true
	case herdr.AgentIdle, herdr.AgentDone:
		moved := haveSeq && a.seqKnown && seq > a.seq
		if !a.sawWorking && !moved {
			return
		}
		for _, key := range a.keys {
			m.engine.AgentFinished(key, now)
		}
		delete(m.awaiting, pane)
	}
}

// event handles one herdr event.
func (m *Model) event(ev herdr.Event) {
	now := m.now()
	m.tracker.Observe(ev, now)
	switch ev.Kind {
	case herdr.EventAgentStatusChanged:
		m.agentStatus(ev.PaneID, ev.AgentStatus, 0, false, now)
	case herdr.EventPaneClosed:
		m.dropPane(ev.PaneID, "closed")
		delete(m.awaiting, ev.PaneID)
	case herdr.EventPaneExited:
		// The pane may stay open showing what it last printed, so watching
		// carries on. An agent that exited won't finish.
		delete(m.awaiting, ev.PaneID)
	case herdr.EventPaneMoved:
		m.paneMoved(ev.PreviousPaneID, ev.PaneID)
	case herdr.EventReconnected:
		m.eventsErr = nil
		if m.loops != nil {
			m.loops.requestSync()
		}
	case herdr.EventOther:
		if strings.Contains(ev.Name, "agent_detected") && m.loops != nil {
			m.loops.requestSync()
		}
	}
}

// paneMoved follows a pane to its new id.
func (m *Model) paneMoved(from, to string) {
	if from == "" || to == "" || from == to {
		return
	}
	m.movedTo[from] = to
	if a, ok := m.awaiting[from]; ok {
		delete(m.awaiting, from)
		m.awaiting[to] = a
	}
	for id, t := range m.picked {
		if t.PaneID == from {
			t.PaneID = to
			m.picked[id] = t
		}
	}
	if m.isWatching(from) {
		m.stopWatching(from)
		m.applyWatchList()
		// Output printed while the pane moved can't be seen by the new watcher,
		// whose first read is skipped.
		m.engine.Gap(engine.SourceID(to), m.now())
	}
}

// resolve follows a pane id through any moves to its id now.
func (m *Model) resolve(pane string) string {
	for range len(m.movedTo) {
		next, ok := m.movedTo[pane]
		if !ok {
			break
		}
		pane = next
	}
	return pane
}

func (m *Model) isWatching(pane string) bool { return slices.Contains(m.watching, pane) }

// applyWatchList starts and stops watching to match the list.
func (m *Model) applyWatchList() {
	var want []string
	for _, pane := range m.listed {
		pane = m.resolve(pane)
		if pane == m.self || m.gone[pane] || slices.Contains(want, pane) {
			continue
		}
		want = append(want, pane)
	}
	for _, pane := range slices.Clone(m.watching) {
		if !slices.Contains(want, pane) {
			m.stopWatching(pane)
		}
	}
	for _, pane := range want {
		if m.isWatching(pane) {
			continue
		}
		m.engine.Watch(m.source(pane))
		if m.loops != nil {
			m.loops.watchPane(pane)
		}
	}
	m.watching = want
}

// stopWatching stops a pane's loop and tells the engine. Its entries stay.
func (m *Model) stopWatching(pane string) {
	if m.loops != nil {
		m.loops.unwatchPane(pane)
	}
	m.engine.Unwatch(engine.SourceID(pane))
	delete(m.paneErrs, pane)
	m.watching = slices.DeleteFunc(m.watching, func(p string) bool { return p == pane })
}

// dropPane stops watching a pane that has gone, for good.
func (m *Model) dropPane(pane, why string) {
	if !m.isWatching(pane) {
		return
	}
	name := m.paneName(pane)
	m.stopWatching(pane)
	m.gone[pane] = true
	m.status = fmt.Sprintf("%s %s, so it's no longer watched.", name, why)
}

// source describes a watched pane for the engine: its label as the title, and
// its terminal title as the command when that says something more. Programs
// usually set the terminal title to what they're running.
func (m *Model) source(pane string) engine.Source {
	src := engine.Source{ID: engine.SourceID(pane)}
	p, ok := m.panes[pane]
	if !ok {
		return src
	}
	switch {
	case p.Label != "":
		src.Title = p.Label
		if p.Title != p.Label {
			src.Command = p.Title
		}
	default:
		src.Title = p.Title
	}
	return src
}

// paneName is how the feed names a pane: its label or title, else its id.
func (m *Model) paneName(pane string) string {
	src := m.source(pane)
	if src.Title != "" {
		return clean(src.Title)
	}
	return pane
}

// targetFor is who an entry would go to now, or why the feed is asking.
func (m *Model) targetFor(en engine.Entry) (herdr.Target, string) {
	if t, ok := m.picked[en.ID]; ok {
		return t, ""
	}
	if !m.synced {
		return herdr.Target{}, "still looking for agents"
	}
	c := m.tracker.Choose(m.resolve(string(en.Source.ID)), m.cfg.Pins)
	if !c.Chosen() {
		return herdr.Target{}, string(c.Ask)
	}
	return c.Target, ""
}

// key handles a keypress for whichever mode the feed is in.
func (m *Model) key(k tea.KeyMsg) tea.Cmd {
	if k.String() == "ctrl+c" {
		return tea.Quit
	}
	if k.String() != "enter" {
		m.armed = ""
	}
	switch m.mode {
	case modeView:
		return m.viewKey(k)
	case modePick:
		m.pickKey(k)
		return nil
	}
	return m.listKey(k)
}

func (m *Model) listKey(k tea.KeyMsg) tea.Cmd {
	i := m.cursor()
	switch k.String() {
	case "q":
		return tea.Quit
	case "j", "down":
		if i >= 0 && i+1 < len(m.entries) {
			m.selected = m.entries[i+1].ID
		}
	case "k", "up":
		if i > 0 {
			m.selected = m.entries[i-1].ID
		}
	case "enter":
		if i >= 0 {
			return m.send(m.entries[i].ID, "", false)
		}
	case "d":
		if i >= 0 {
			m.engine.Dismiss(m.entries[i].ID)
			m.status = "Dismissed."
		}
	case "n":
		// #14 replaces this with a suggested ignore rule.
		if i >= 0 {
			m.status = "Never again isn't built yet (#14). Press d to dismiss this one for now."
		}
	case "t":
		if i >= 0 {
			m.openPicker(m.entries[i])
		}
	case "v":
		if i >= 0 {
			m.openView(m.entries[i].ID)
		}
	}
	return nil
}

// openView shows exactly what enter would send for id.
func (m *Model) openView(id engine.EntryID) {
	text, err := m.engine.Message(id)
	if err != nil {
		m.status = fmt.Sprintf("Can't show the message: %v", err)
		return
	}
	m.mode, m.viewID, m.viewText, m.viewTop = modeView, id, text, 0
}

func (m *Model) viewKey(k tea.KeyMsg) tea.Cmd {
	page := max(m.viewHeight()-1, 1)
	switch k.String() {
	case "esc", "q", "v":
		m.mode = modeList
	case "j", "down":
		m.viewTop++
	case "k", "up":
		m.viewTop--
	case "pgdown", " ", "f":
		m.viewTop += page
	case "pgup", "b":
		m.viewTop -= page
	case "g", "home":
		m.viewTop = 0
	case "G", "end":
		m.viewTop = len(m.viewLines())
	case "enter":
		return m.send(m.viewID, m.viewText, true)
	}
	m.viewTop = min(max(m.viewTop, 0), max(len(m.viewLines())-m.viewHeight(), 0))
	return nil
}

func (m *Model) openPicker(en engine.Entry) {
	list := m.tracker.Candidates(m.resolve(string(en.Source.ID)))
	if len(list) == 0 {
		m.status = "There are no agents to pick from."
		return
	}
	m.mode, m.pickID, m.pickList, m.pickCursor = modePick, en.ID, list, 0
}

func (m *Model) pickKey(k tea.KeyMsg) {
	switch k.String() {
	case "esc", "q", "t":
		m.mode = modeList
	case "j", "down":
		m.pickCursor = min(m.pickCursor+1, len(m.pickList)-1)
	case "k", "up":
		m.pickCursor = max(m.pickCursor-1, 0)
	case "enter":
		t := m.pickList[m.pickCursor]
		m.picked[m.pickID] = t
		m.mode = modeList
		if t.Status == herdr.AgentBlocked {
			m.status = fmt.Sprintf("This entry goes to %s, but it's waiting on a prompt, so it can't be sent until that's dealt with.", clean(t.Label))
		} else {
			m.status = fmt.Sprintf("This entry goes to %s. Press enter to send.", clean(t.Label))
		}
	}
}

// blockedNote is why a blocked agent is never sent to.
const blockedNote = "it's waiting on a prompt, and sending types text and Enter, which could answer it. Deal with the prompt, then press enter again"

// send starts sending an entry after a keypress. It takes the message now, so
// what's sent is what the engine said at the moment of the keypress (or, from
// the view, exactly what was on screen). Then it checks the target in a
// command, and sends from checked. Nothing else ever starts a send.
func (m *Model) send(id engine.EntryID, viewed string, fromView bool) tea.Cmd {
	en, ok := m.entry(id)
	if !ok {
		return nil
	}
	if m.sending[id] {
		m.status = "Still sending that one."
		return nil
	}
	text, err := m.engine.Message(id)
	if err != nil {
		m.status = fmt.Sprintf("Not sent: %v", err)
		return nil
	}
	if fromView && text != viewed {
		m.viewText = text
		m.status = "The message changed while you were reading it. Check it again, then press enter to send."
		return nil
	}
	target, ask := m.targetFor(en)
	if ask != "" {
		m.status = fmt.Sprintf("Not sent: %s. Press t to pick an agent.", ask)
		return nil
	}
	label := clean(target.Label)
	if target.Status == herdr.AgentBlocked {
		m.status = fmt.Sprintf("Not sent to %s: %s.", label, blockedNote)
		return nil
	}
	if (en.State == engine.StateSent || en.State == engine.StateFixed) && m.armed != id {
		m.armed = id
		m.status = fmt.Sprintf("Already sent to %s. Press enter again to send it again.", clean(m.agentLabel(en.Agent)))
		return nil
	}
	m.armed = ""
	m.sending[id] = true
	m.status = fmt.Sprintf("Checking %s before sending...", label)
	return checkTarget(m.ctx, m.client, id, target, text)
}

// checkedMsg is a fresh look at the target pane just before sending.
type checkedMsg struct {
	id       engine.EntryID
	target   herdr.Target
	text     string
	pane     herdr.Pane
	found    bool
	seq      uint64
	seqKnown bool
	err      error
}

// checkTarget looks the target up again. Pane ids can go stale, the agent in a
// pane can change and an agent can become blocked between the last snapshot
// and the keypress.
func checkTarget(ctx context.Context, client *herdr.Client, id engine.EntryID, target herdr.Target, text string) tea.Cmd {
	return func() tea.Msg {
		msg := checkedMsg{id: id, target: target, text: text}
		panes, err := client.ListPanes(ctx)
		if err != nil {
			msg.err = err
			return msg
		}
		for _, p := range panes {
			if p.ID == target.PaneID {
				msg.pane, msg.found = p, true
			}
		}
		if agents, err := client.ListAgents(ctx); err == nil {
			for _, a := range agents {
				if a.PaneID == target.PaneID {
					msg.seq, msg.seqKnown = a.StateChangeSeq, true
				}
			}
		}
		return msg
	}
}

// checked sends the prompt if the target still looks right.
func (m *Model) checked(msg checkedMsg) tea.Cmd {
	label := clean(msg.target.Label)
	refuse := func(why string) tea.Cmd {
		delete(m.sending, msg.id)
		m.status = fmt.Sprintf("Not sent to %s: %s.", label, why)
		return nil
	}
	p := msg.pane
	switch {
	case msg.err != nil:
		return refuse(fmt.Sprintf("couldn't check it first (%v)", msg.err))
	case !msg.found:
		delete(m.picked, msg.id)
		return refuse("its pane has closed. Press t to pick another agent")
	case p.Agent == "" && p.DisplayAgent == "":
		delete(m.picked, msg.id)
		return refuse("its pane isn't running an agent any more")
	case msg.target.Kind != "" && p.Agent != msg.target.Kind:
		delete(m.picked, msg.id)
		return refuse(fmt.Sprintf("its pane now runs %s instead", clean(p.Agent)))
	case p.AgentStatus == herdr.AgentBlocked:
		return refuse(blockedNote)
	}
	return prompt(m.ctx, m.client, msg)
}

// sentMsg is the result of one agent.prompt.
type sentMsg struct {
	checked   checkedMsg
	promptErr error
}

// prompt makes exactly one agent.prompt call with the message taken at the
// keypress. It's never retried: a timeout may still have been delivered.
func prompt(ctx context.Context, client *herdr.Client, msg checkedMsg) tea.Cmd {
	return func() tea.Msg {
		err := client.Prompt(ctx, msg.target.PaneID, msg.text)
		return sentMsg{checked: msg, promptErr: err}
	}
}

// sent records a finished send.
func (m *Model) sent(sm sentMsg) {
	msg := sm.checked
	delete(m.sending, msg.id)
	label := clean(msg.target.Label)
	switch {
	case errors.Is(sm.promptErr, herdr.ErrTimeout):
		m.status = fmt.Sprintf("herdr didn't answer, so it may or may not have reached %s. Check before sending again.", label)
		return
	case sm.promptErr != nil:
		m.status = fmt.Sprintf("Not sent to %s: %v", label, clean(sm.promptErr.Error()))
		return
	}
	pane := msg.target.PaneID
	m.engine.MarkSent(msg.id, pane, m.now())
	m.labels[pane] = msg.target.Label
	a, ok := m.awaiting[pane]
	if !ok {
		a = &await{}
		m.awaiting[pane] = a
	}
	if !slices.Contains(a.keys, pane) {
		a.keys = append(a.keys, pane)
	}
	a.seq, a.seqKnown, a.sawWorking = msg.seq, msg.seqKnown, false
	m.status = fmt.Sprintf("Sent to %s.", label)
}

// agentLabel is how the feed shows an agent the engine knows by key.
func (m *Model) agentLabel(key string) string {
	if l, ok := m.labels[key]; ok {
		return l
	}
	return key
}
