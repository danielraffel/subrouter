package selectacct

import (
	"sync"
	"time"
)

type SchedulerRef struct {
	mu        sync.RWMutex
	scheduler Scheduler
	updatedAt time.Time
}

func NewSchedulerRef(scheduler Scheduler) *SchedulerRef {
	return &SchedulerRef{
		scheduler: scheduler,
		updatedAt: time.Now(),
	}
}

func (r *SchedulerRef) Get() Scheduler {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.scheduler
}

func (r *SchedulerRef) Set(scheduler Scheduler) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.scheduler = scheduler
	r.updatedAt = time.Now()
}

func (r *SchedulerRef) MarkExhausted(accountID string) {
	if accountID == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.scheduler = r.scheduler.WithScore(Score{
		AccountID:     accountID,
		Headroom:      0,
		ShortHeadroom: 0,
	})
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
