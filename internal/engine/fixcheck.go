package engine

import "time"

// Stand-in until #12. The same error coming back during the settle period means
// still failing. Otherwise the verdict is always unverified, because this version
// can't tell whether anything ran to prove the fix. #12 adds fixed, using sawLine
// to notice healthy output.
//
// Every change to an entry's state after it's caught goes through this file, so
// #12 never has to edit engine.go.

// fixState tracks the settle period for one sent entry.
type fixState struct {
	settling bool
	from     time.Time
	until    time.Time
}

// sent records that r went to agent at now.
func (e *Engine) sent(r *record, agent string, _ time.Time) {
	r.entry.State = StateSent
	r.entry.Agent = agent
	r.fix = fixState{}
}

// agentFinished starts the settle period for every entry sent to agent and still
// waiting. If one has already started, it keeps its first start, so an agent that
// keeps finishing can't push the verdict out for ever.
func (e *Engine) agentFinished(agent string, now time.Time) {
	for _, r := range e.records {
		if r.entry.State != StateSent || r.entry.Agent != agent || r.fix.settling {
			continue
		}
		r.fix = fixState{settling: true, from: now, until: now.Add(e.opts.Settle)}
	}
}

// dismissed records that the person dismissed r.
func (e *Engine) dismissed(r *record) {
	r.entry.State = StateDismissed
	r.fix = fixState{}
}

// sawLine is called for every clean line from a watched source. The stand-in
// doesn't need it. #12 uses it to tell a quiet pane from a healthy one.
func (e *Engine) sawLine(_ SourceID, _ string, _ time.Time) {}

// pastSettle reports whether an error matched at matchedAt came too late to
// count toward r's settle period. Grouping makes such a repeat a new entry. It's
// decided by match time, so the outcome never depends on when Tick runs.
func (e *Engine) pastSettle(r *record, matchedAt time.Time) bool {
	return r.entry.State == StateSent && r.fix.settling && !matchedAt.Before(r.fix.until)
}

// recurred is called when an entry's error is seen again at matchedAt.
func (e *Engine) recurred(r *record, matchedAt time.Time) {
	// Only a repeat inside the settle period counts. One after it ended, but
	// before the verdict was given, doesn't.
	f := r.fix
	if r.entry.State != StateSent || !f.settling || matchedAt.Before(f.from) || !matchedAt.Before(f.until) {
		return
	}
	r.entry.State = StateStillFailing
	r.fix = fixState{}
}

// checkFixes gives a verdict on every entry whose settle period is over. It
// waits a further CaptureWait, so a repeat that matched just before the end has
// been filed (and has made the entry still failing) before the verdict.
func (e *Engine) checkFixes(now time.Time) {
	for _, r := range e.records {
		if !r.fix.settling || now.Before(r.fix.until.Add(e.opts.CaptureWait)) {
			continue
		}
		r.entry.State = StateUnverified
		r.fix = fixState{}
	}
}
