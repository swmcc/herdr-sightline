package engine

import "time"

// Stand-in until #12. The same error coming back during the settle period means
// still failing. Otherwise the verdict is always unverified, because this version
// can't tell whether anything ran to prove the fix. #12 adds fixed.

// settle tracks the settle period for one sent entry.
type settle struct {
	running bool
	from    time.Time
	until   time.Time
}

func (s *settle) start(now time.Time, d time.Duration) {
	*s = settle{running: true, from: now, until: now.Add(d)}
}

// recurred is called when an entry's error is seen again at matchedAt.
func (e *Engine) recurred(r *record, matchedAt time.Time) {
	if r.entry.State != StateSent || !r.settle.running || matchedAt.Before(r.settle.from) {
		return
	}
	r.entry.State = StateStillFailing
	r.settle = settle{}
}

// checkFixes gives a verdict on every entry whose settle period is over.
func (e *Engine) checkFixes(now time.Time) {
	for _, r := range e.records {
		if !r.settle.running || now.Before(r.settle.until) {
			continue
		}
		r.entry.State = StateUnverified
		r.settle = settle{}
	}
}
