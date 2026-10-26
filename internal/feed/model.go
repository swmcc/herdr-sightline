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
	// since is when the last send was made.
	since time.Time
}

// Model is the feed. It owns the engine and is its only caller, so every engine
// call happens on the Bubble Tea update loop. See loops.go for everything else.
type Model struct {
	ctx    context.Context
	client *herdr.Client
	loops  *loops
	now    func() time.Time
	self   string // the feed's own pane, never watched

	engine *engine.Engine
	cfg    config.Config
	cfgErr error

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
	armed   engine.EntryID                  // enter was pressed once on an entry that needs a second
	// armedTarget is the agent that first press was for, and armedAt when it
	// was pressed. A confirm lasts confirmFor.
	armedTarget herdr.Target
	armedAt     time.Time
	// maybeSent are entries whose send failed in a way that may still have
	// delivered it, with who to.
	maybeSent map[engine.EntryID]string

	entries  []engine.Entry // what the list shows, newest first
	selected engine.EntryID
	// selectionLost says the selected entry went without the person doing
	// anything, so nothing is selected until they choose again.
	selectionLost bool

	mode       mode
	viewID     engine.EntryID
	viewText   string
	viewTop    int
	viewTarget herdr.Target // who the view said enter sends to
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
		ctx:       ctx,
		client:    client,
		now:       now,
		self:      self,
		tracker:   herdr.NewAgentTracker(),
		panes:     map[string]herdr.Pane{},
		gone:      map[string]bool{},
		movedTo:   map[string]string{},
		paneErrs:  map[string]error{},
		awaiting:  map[string]*await{},
		labels:    map[string]string{},
		picked:    map[engine.EntryID]herdr.Target{},
		sending:   map[engine.EntryID]bool{},
		maybeSent: map[engine.EntryID]string{},
		width:     80,
		height:    24,
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

// applyConfig takes sightline.toml, first when the feed starts and then each
// time it changes. Everything applies straight away, without losing entries
// (see engine.Reconfigure for when each setting takes effect). A broken file
// keeps whatever config is running and shows the error. It returns how many new
// entries the ignore rules dismissed.
func (m *Model) applyConfig(u config.Update) (dismissed int) {
	m.cfgErr = u.Err
	opts, err := engineOptions(u.Config)
	if err != nil {
		m.cfgErr = err
	}
	if m.engine != nil {
		if m.cfgErr != nil {
			return 0
		}
		m.cfg = u.Config
		return m.engine.Reconfigure(opts)
	}
	if err != nil {
		// The feed needs an engine whatever the file says.
		u.Config = config.Default()
		if opts, err = engineOptions(u.Config); err != nil {
			panic(fmt.Sprintf("the default config doesn't build an engine: %v", err))
		}
	}
	m.cfg = u.Config
	m.engine = engine.New(opts)
	return 0
}

// reloadConfig applies a change to sightline.toml and says so.
func (m *Model) reloadConfig(u config.Update) {
	n := m.applyConfig(u)
	switch {
	case m.cfgErr != nil:
		m.status = "sightline.toml has a problem, so the last good config is still in use."
	case n > 0:
		m.status = fmt.Sprintf("Reloaded sightline.toml. Its ignore rules dismissed %s.", plural(n, "entry", "entries"))
	default:
		m.status = "Reloaded sightline.toml."
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
		if m.fromCurrentLoop(msg.pane, msg.gen) {
			m.delta(msg)
		}
	case paneGoneMsg:
		if m.fromCurrentLoop(msg.pane, msg.gen) {
			m.dropPane(msg.pane, "closed")
		}
	case paneErrMsg:
		if m.fromCurrentLoop(msg.pane, msg.gen) {
			m.paneErrs[msg.pane] = msg.err
		}
	case snapshotMsg:
		m.snapshot(msg)
	case eventMsg:
		m.event(msg.ev, msg.global)
	case eventsDownMsg:
		m.eventsErr = fmt.Errorf("%s: %w", msg.what, msg.err)
	case eventsUpMsg:
		// After an outage, catch up on whatever happened while it was down.
		if m.eventsErr != nil && m.loops != nil {
			m.loops.requestSync()
		}
		m.eventsErr = nil
	case watchListMsg:
		m.listed, m.listErr = msg.panes, msg.err
		m.applyWatchList()
	case configMsg:
		m.reloadConfig(msg.update)
	case checkedMsg:
		cmd = m.checked(msg)
	case sentMsg:
		m.sent(msg)
	}
	m.refresh()
	return m, cmd
}

// fromCurrentLoop reports whether a message about pane came from the loop
// watching it now, rather than one stopped earlier.
func (m *Model) fromCurrentLoop(pane string, gen uint64) bool {
	return m.isWatching(pane) && m.loops != nil && m.loops.current(pane, gen)
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
	// Enter only ever acts on an entry the person chose. If the selected one
	// went without them doing anything (dropped by the cap, which a flood of
	// errors can force), nothing is selected until they press j or k. Moving
	// the cursor for them could put someone else's entry under the next enter.
	switch {
	case m.selected != "" && m.cursor() < 0:
		m.selected, m.selectionLost = "", true
		m.status = "The selected entry was dropped to keep the feed short. Press j or k to choose another."
	case m.selected == "" && !m.selectionLost && len(m.entries) > 0:
		// The first entry in an empty feed, before anything was chosen.
		m.selected = m.entries[0].ID
	}
	// The same goes for the view and the picker. They stay open, so an enter
	// meant for them can't fall through to the list (see viewKey and pickKey).
	if m.mode == modeView {
		if _, ok := m.entry(m.viewID); !ok {
			m.status = "This entry was dropped, so enter won't send it. Press esc to go back."
		}
	}
	if m.mode == modePick {
		if _, ok := m.entry(m.pickID); !ok {
			m.status = "This entry was dropped, so there's nothing to pick for. Press esc to go back."
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
	// agent.list is fetched after pane.list, so it's the newer of the two and
	// a pane's own status only counts for an agent agent.list doesn't have.
	inAgents := map[string]bool{}
	for _, a := range msg.snap.Agents {
		inAgents[a.PaneID] = true
		if !slices.Contains(agentPanes, a.PaneID) {
			agentPanes = append(agentPanes, a.PaneID)
		}
	}
	for _, p := range msg.snap.Panes {
		if (p.Agent != "" || p.DisplayAgent != "") && !inAgents[p.ID] {
			m.agentStatus(p.ID, p.AgentStatus, 0, false, msg.snap.Taken, now)
		}
	}
	for _, a := range msg.snap.Agents {
		m.agentStatus(a.PaneID, a.Status, a.StateChangeSeq, true, msg.snap.Taken, now)
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
// after its state change counter moved on since the send. seen is when herdr
// was asked. Anything asked before the last send says nothing about it, so a
// slow snapshot can't finish a send it never saw.
func (m *Model) agentStatus(pane string, status herdr.AgentStatus, seq uint64, haveSeq bool, seen, now time.Time) {
	a, ok := m.awaiting[pane]
	if !ok || seen.Before(a.since) {
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
func (m *Model) event(ev herdr.Event, global bool) {
	now := m.now()
	m.tracker.Observe(ev, now)
	switch ev.Kind {
	case herdr.EventAgentStatusChanged:
		m.agentStatus(ev.PaneID, ev.AgentStatus, 0, false, now, now)
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
		if global {
			m.eventsErr = nil
		}
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
	// A read of the old id can lose the race with this event and find the pane
	// gone first. Then it was dropped as closed, but it only moved.
	if m.isWatching(from) || m.gone[from] {
		delete(m.gone, from)
		if m.isWatching(from) {
			m.stopWatching(from)
		}
		m.status = ""
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

// pinNote says when an entry's agent was chosen by a pin matching the watched
// pane's terminal title. The pane sets that title itself, so it's worth seeing.
func (m *Model) pinNote(en engine.Entry) string {
	if _, ok := m.picked[en.ID]; ok || !m.synced {
		return ""
	}
	c := m.tracker.Choose(m.resolve(string(en.Source.ID)), m.cfg.Pins)
	if c.Chosen() && c.Rule == herdr.ChosenByPin && c.PinField == herdr.PinByTerminalTitle {
		return " (pinned by terminal title)"
	}
	return ""
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

// move selects the entry after or before the one at i. With nothing selected
// it selects the newest.
func (m *Model) move(i int, down bool) {
	if len(m.entries) == 0 {
		return
	}
	m.selectionLost = false
	switch {
	case i < 0:
		m.selected = m.entries[0].ID
	case down && i+1 < len(m.entries):
		m.selected = m.entries[i+1].ID
	case !down && i > 0:
		m.selected = m.entries[i-1].ID
	}
}

func (m *Model) listKey(k tea.KeyMsg) tea.Cmd {
	i := m.cursor()
	switch k.String() {
	case "q":
		return tea.Quit
	case "j", "down", "k", "up":
		m.move(i, k.String() == "j" || k.String() == "down")
	case "enter":
		if i >= 0 {
			return m.send(m.entries[i].ID, "", false)
		}
	case "d":
		if i >= 0 {
			m.engine.Dismiss(m.entries[i].ID)
			m.status = "Dismissed."
			// The person dismissed it, so moving on to the next one is theirs.
			switch {
			case i+1 < len(m.entries):
				m.selected = m.entries[i+1].ID
			case i > 0:
				m.selected = m.entries[i-1].ID
			default:
				m.selected = ""
			}
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
	m.viewTarget = herdr.Target{}
	if en, ok := m.entry(id); ok {
		if t, ask := m.targetFor(en); ask == "" {
			m.viewTarget = t
		}
	}
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
		if _, ok := m.entry(m.pickID); !ok {
			m.status = "This entry was dropped, so there's nothing to pick for. Press esc to go back."
			return
		}
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
	target, ask := m.targetFor(en)
	if fromView && (text != viewed || !sameAgent(target, m.viewTarget)) {
		// The view promised these bytes to this agent. If either changed (a
		// pane can change its own title, and a pin can follow it), show the
		// new version rather than send something that wasn't on screen.
		m.openView(id)
		m.status = "The message or its agent changed while you were reading it. Check it again, then press enter to send."
		return nil
	}
	if ask != "" {
		m.status = fmt.Sprintf("Not sent: %s. Press t to pick an agent.", ask)
		return nil
	}
	label := clean(target.Label)
	if target.Status == herdr.AgentBlocked {
		m.status = fmt.Sprintf("Not sent to %s: %s.", label, blockedNote)
		return nil
	}
	// A confirm is for one entry going to one agent. If a sync changed the
	// target between the two presses (a pane can retitle itself to match a
	// pin), the second press asks again, naming the new agent.
	confirmed := m.armed == id && sameAgent(m.armedTarget, target) && m.now().Sub(m.armedAt) <= confirmFor
	if confirm := m.needsConfirm(en, target); confirm != "" && !confirmed {
		m.armed, m.armedTarget, m.armedAt = id, target, m.now()
		m.status = fmt.Sprintf("%s Press enter again to send it to %s.", confirm, label)
		return nil
	}
	m.armed = ""
	m.sending[id] = true
	m.status = fmt.Sprintf("Checking %s before sending...", label)
	return checkTarget(m.ctx, m.client, m.now, id, target, text)
}

// confirmFor is how long a first enter stays good for. A confirm left lying
// around for minutes isn't a decision about what's on screen now.
const confirmFor = 10 * time.Second

// sameAgent reports whether two targets are the same agent: the same pane, the
// same kind and the same name. A different agent can start in the same pane.
func sameAgent(a, b herdr.Target) bool {
	return a.PaneID == b.PaneID && a.Kind == b.Kind && a.Name == b.Name
}

// needsConfirm says why a send needs a second enter, or "" if it doesn't.
func (m *Model) needsConfirm(en engine.Entry, target herdr.Target) string {
	_, picked := m.picked[en.ID]
	switch {
	case m.maybeSent[en.ID] != "":
		return fmt.Sprintf("This may already have reached %s.", clean(m.maybeSent[en.ID]))
	case en.State == engine.StateSent || en.State == engine.StateFixed:
		return fmt.Sprintf("Already sent to %s.", clean(m.agentLabel(en.Agent)))
	case !target.SameTab && !picked:
		// Only a pin can choose an agent in another tab, and a pin can match
		// a terminal title, which the watched pane sets itself.
		return fmt.Sprintf("%s is in another tab.", clean(target.Label))
	}
	return ""
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
	// nameClash is the pane of another agent whose herdr name is the target's
	// pane id, if there is one.
	nameClash string
	// checkedAt is when the check began, just before the send. Agent statuses
	// asked for before it say nothing about this send.
	checkedAt time.Time
	err       error
}

// checkTarget looks the target up again. Pane ids can go stale, the agent in a
// pane can change and an agent can become blocked between the last snapshot
// and the keypress.
func checkTarget(ctx context.Context, client *herdr.Client, now func() time.Time, id engine.EntryID, target herdr.Target, text string) tea.Cmd {
	return func() tea.Msg {
		msg := checkedMsg{id: id, target: target, text: text, checkedAt: now()}
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
		agents, err := client.ListAgents(ctx)
		if err != nil {
			msg.err = err
			return msg
		}
		for _, a := range agents {
			if a.PaneID == target.PaneID {
				msg.seq, msg.seqKnown = a.StateChangeSeq, true
			}
			if a.Name == target.PaneID && a.PaneID != target.PaneID {
				msg.nameClash = a.PaneID
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
	case msg.target.Kind != "" && p.Agent != msg.target.Kind,
		msg.target.Kind == "" && p.Agent != "":
		delete(m.picked, msg.id)
		return refuse(fmt.Sprintf("its pane now runs %s instead", clean(p.Agent)))
	case msg.nameClash != "":
		// agent.prompt's target can be a pane id or an agent's herdr name, and
		// herdr's schema doesn't say which wins when they collide. An agent
		// renamed to look like this pane's id could then take the prompt.
		return refuse(fmt.Sprintf("the agent in %s is named %s, the same as this pane's id, so herdr might send it there", msg.nameClash, msg.target.PaneID))
	case p.AgentStatus == herdr.AgentBlocked:
		return refuse(blockedNote)
	}
	// A permission dialog can still open between this check and the prompt.
	// The gap is one round trip, and herdr narrows it further by refusing
	// agent.prompt itself (agent_blocked) when the agent is blocked as the
	// prompt arrives.
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
	var refused *herdr.Error
	switch {
	case errors.As(sm.promptErr, &refused):
		// herdr answered and said no, so nothing was typed.
		m.status = fmt.Sprintf("Not sent to %s: %v", label, sm.promptErr)
		return
	case sm.promptErr != nil:
		// Anything else may have failed after herdr had the request: a lost
		// reply, a timeout, a cancel. So it may have been typed, and the next
		// enter asks first.
		m.maybeSent[msg.id] = msg.target.Label
		m.status = fmt.Sprintf("It may or may not have reached %s (%v). Check before sending again.", label, sm.promptErr)
		return
	}
	delete(m.maybeSent, msg.id)
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
	a.seq, a.seqKnown, a.sawWorking, a.since = msg.seq, msg.seqKnown, false, msg.checkedAt
	m.status = fmt.Sprintf("Sent to %s.", label)
}

// agentLabel is how the feed shows an agent the engine knows by key.
func (m *Model) agentLabel(key string) string {
	if l, ok := m.labels[key]; ok {
		return l
	}
	return key
}
