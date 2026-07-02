package selectacct

import (
	"sync"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

type SchedulerRef struct {
	mu         sync.RWMutex
	scheduler  Scheduler
	updatedAt  time.Time
	refreshing bool
	// exhaustedUntil expires request-time exhaustion marks. A mark set from a
	// rejected upstream response is only true until the account's rate-limit
	// window resets; without an expiry a recovered account stayed zero-scored
	// until the next SUCCESSFUL usage refresh, which under load can fail for
	// hours, leaving real quota unroutable while clients got 429s.
	exhaustedUntil map[string]time.Time
}

func NewSchedulerRef(scheduler Scheduler) *SchedulerRef {
	return &SchedulerRef{
		scheduler: scheduler,
		updatedAt: time.Now(),
	}
}

func (r *SchedulerRef) Get() Scheduler {
	r.pruneExpired(time.Now())
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.scheduler
}

// pruneExpired drops exhaustion marks whose window has reset, restoring the
// account to the optimistic default score so routing can try it again.
//
// Deliberate tradeoff: a lapsed mark cannot distinguish "recovered" from "fresh
// usage re-confirmed exhausted" (refreshes seed zero scores forward, so the two
// are indistinguishable here). We choose optimistic retry-once: if the account
// is still cooked, the one probe request is rejected upstream and the account
// is immediately re-marked with the NEW authoritative reset time, so the cost
// is bounded at one attempt per account per expiry window. The opposite choice
// (trusting a zero score without an expiry) is the failure this fixes: a
// recovered account's real quota stayed unroutable for hours. This matches the
// scheduler-wide philosophy that scores are load-balancing hints and the
// upstream response is the source of truth.
func (r *SchedulerRef) pruneExpired(now time.Time) {
	r.mu.RLock()
	anyExpired := false
	for _, until := range r.exhaustedUntil {
		if !until.After(now) {
			anyExpired = true
			break
		}
	}
	r.mu.RUnlock()
	if !anyExpired {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	next := Scheduler{scores: make(map[string]Score, len(r.scheduler.scores)), sessionCounts: r.scheduler.sessionCounts}
	for key, score := range r.scheduler.scores {
		next.scores[key] = score
	}
	for key, until := range r.exhaustedUntil {
		if until.After(now) {
			continue
		}
		delete(r.exhaustedUntil, key)
		delete(next.scores, key)
	}
	r.scheduler = next
}

func (r *SchedulerRef) Set(scheduler Scheduler) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.scheduler = scheduler
	r.retainExhaustedExpiriesLocked()
	r.updatedAt = time.Now()
}

// retainExhaustedExpiriesLocked reconciles mark expiries with an incoming
// refresh, by evidence class:
//   - Score shows headroom (or is gone): the mark is superseded; drop the expiry.
//   - Carried-forward zero (the account's own usage fetch failed, seed dragged
//     along, Fresh=false): keep the existing expiry. Clearing it would make the
//     request-time mark permanent again, recreating the stranded-recovered-
//     account failure this exists to fix.
//   - Fresh zero (a successful fetch re-confirmed exhaustion, Fresh=true):
//     re-anchor the expiry to this newest evidence — extend it to at least the
//     fresh window's own reset (floored at the default TTL) — so an OLDER
//     request-time expiry can never lapse a freshly-observed zero back to the
//     optimistic default. Expiries only extend here, never shorten, so an
//     authoritative long reset from a rejected response still holds.
func (r *SchedulerRef) retainExhaustedExpiriesLocked() {
	now := time.Now()
	for key := range r.exhaustedUntil {
		score, ok := r.scheduler.scores[key]
		switch {
		case !ok || !score.exhausted():
			delete(r.exhaustedUntil, key)
		case score.Fresh:
			until := now.Add(DefaultExhaustedTTL)
			if score.ShortResetAfterSeconds > 0 {
				if fromReset := now.Add(time.Duration(score.ShortResetAfterSeconds) * time.Second); fromReset.After(until) {
					until = fromReset
				}
			}
			if cap := now.Add(8 * 24 * time.Hour); until.After(cap) {
				until = cap
			}
			if until.After(r.exhaustedUntil[key]) {
				r.exhaustedUntil[key] = until
			}
		}
	}
}

// DefaultExhaustedTTL bounds an exhaustion mark when the upstream response gave
// no reset time. Short on purpose: re-marking a still-cooked account costs one
// failed attempt, while over-holding a recovered account starves routing of
// real quota.
const DefaultExhaustedTTL = 10 * time.Minute

func (r *SchedulerRef) MarkExhausted(provider accounts.Provider, accountID string) {
	r.MarkExhaustedUntil(provider, accountID, time.Now().Add(DefaultExhaustedTTL))
}

// MarkExhaustedUntil zero-scores the account until the given time, after which
// the mark expires and the account returns to the optimistic default so routing
// tries it again. Callers pass the upstream's own reset time
// (anthropic-ratelimit-unified-reset / Retry-After) when available.
func (r *SchedulerRef) MarkExhaustedUntil(provider accounts.Provider, accountID string, until time.Time) {
	if accountID == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.scheduler = r.scheduler.WithScore(Score{
		AccountID:     accountID,
		Provider:      provider,
		Headroom:      0,
		ShortHeadroom: 0,
	})
	if r.exhaustedUntil == nil {
		r.exhaustedUntil = make(map[string]time.Time)
	}
	r.exhaustedUntil[ScoreKey(provider, accountID)] = until
	r.updatedAt = time.Now()
}

// ExhaustedUntilFor reports the expiry recorded for an account's exhaustion
// mark, if any. Used by tests and diagnostics to verify TTL selection.
func (r *SchedulerRef) ExhaustedUntilFor(provider accounts.Provider, accountID string) (time.Time, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	until, ok := r.exhaustedUntil[ScoreKey(provider, accountID)]
	return until, ok
}

func (r *SchedulerRef) Stale(ttl time.Duration) bool {
	if ttl <= 0 {
		return false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.updatedAt.IsZero() || time.Since(r.updatedAt) >= ttl
}

func (r *SchedulerRef) BeginRefreshIfStale(ttl time.Duration) bool {
	if ttl <= 0 {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.refreshing || (!r.updatedAt.IsZero() && time.Since(r.updatedAt) < ttl) {
		return false
	}
	r.refreshing = true
	return true
}

func (r *SchedulerRef) FinishRefresh(scheduler Scheduler, update bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if update {
		r.scheduler = scheduler
		r.retainExhaustedExpiriesLocked()
	}
	r.updatedAt = time.Now()
	r.refreshing = false
}

func (r *SchedulerRef) Touch() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.updatedAt = time.Now()
}

func (r *SchedulerRef) SetUpdatedAt(updatedAt time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.updatedAt = updatedAt
}
