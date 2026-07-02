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
	// Fresh usage data supersedes request-time marks; keeping expiries around
	// would later delete scores that now come from a real refresh.
	r.exhaustedUntil = nil
	r.updatedAt = time.Now()
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
		r.exhaustedUntil = nil
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
