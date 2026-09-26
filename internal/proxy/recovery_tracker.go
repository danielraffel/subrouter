package proxy

import (
	"sort"
	"sync"
	"time"

	"github.com/manaflow-ai/subrouter/wake"
)

// RecoveryState is the proxy's read-only handoff to the shared cmux watcher.
// The proxy records provider/quota evidence; it never reads or writes a
// terminal surface.
type RecoveryState struct {
	Agent             string    `json:"agent"`
	SessionID         string    `json:"session_id"`
	Kind              string    `json:"kind"`
	Pool              string    `json:"pool,omitempty"`
	LastActivityAt    time.Time `json:"last_activity_at"`
	LastFailureAt     time.Time `json:"last_failure_at,omitempty"`
	ResetAt           time.Time `json:"reset_at,omitempty"`
	Failures          int       `json:"failures"`
	GoalAttempts      int       `json:"goal_attempts"`
	ContinueSent      bool      `json:"continue_sent"`
	GenerationBegan   bool      `json:"generation_began"`
	RequestTokens     int64     `json:"request_tokens,omitempty"`
	ResponseTokens    int64     `json:"response_tokens,omitempty"`
	ProviderHealthyAt time.Time `json:"provider_healthy_at,omitempty"`
}

type RecoveryTracker struct {
	mu    sync.Mutex
	state map[string]RecoveryState
}

func NewRecoveryTracker() *RecoveryTracker {
	return &RecoveryTracker{state: make(map[string]RecoveryState)}
}

func (t *RecoveryTracker) RecordCapacityFailure(agent, session, pool string, at time.Time) {
	t.record(agent, session, wake.KindCodexProvider, pool, at, time.Time{})
}

func (t *RecoveryTracker) RecordQuotaFailure(agent, session, kind, pool string, at, resetAt time.Time) {
	t.record(agent, session, kind, pool, at, resetAt)
}

func (t *RecoveryTracker) RecordProviderHealthy(agent, session string, at time.Time) {
	if t == nil || session == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	key := recoveryKey(agent, session)
	s := t.state[key]
	s.Agent, s.SessionID = agent, session
	s.LastActivityAt, s.ProviderHealthyAt = at.UTC(), at.UTC()
	t.state[key] = s
}

func (t *RecoveryTracker) RecordGeneration(agent, session string, began bool, requestTokens, responseTokens int64) {
	if t == nil || session == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	s := t.state[recoveryKey(agent, session)]
	s.Agent, s.SessionID = agent, session
	s.GoalAttempts++
	s.GenerationBegan = began
	s.RequestTokens += requestTokens
	s.ResponseTokens += responseTokens
	t.state[recoveryKey(agent, session)] = s
}

func (t *RecoveryTracker) List(now time.Time) []RecoveryState {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]RecoveryState, 0, len(t.state))
	for _, s := range t.state {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LastActivityAt.Before(out[j].LastActivityAt) })
	return out
}

func (t *RecoveryTracker) record(agent, session, kind, pool string, at, resetAt time.Time) {
	if t == nil || session == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	key := recoveryKey(agent, session)
	s := t.state[key]
	s.Agent, s.SessionID, s.Kind, s.Pool = agent, session, kind, pool
	s.LastActivityAt, s.LastFailureAt = at.UTC(), at.UTC()
	s.ResetAt = resetAt.UTC()
	s.Failures++
	t.state[key] = s
}

func recoveryKey(agent, session string) string { return agent + "\x00" + session }
