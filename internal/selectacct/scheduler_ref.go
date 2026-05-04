package selectacct

import "sync"

type SchedulerRef struct {
	mu        sync.RWMutex
	scheduler Scheduler
}

func NewSchedulerRef(scheduler Scheduler) *SchedulerRef {
	return &SchedulerRef{scheduler: scheduler}
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
}
