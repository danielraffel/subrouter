package selectacct

import (
	"errors"
	"sort"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

var ErrNoAccounts = errors.New("no accounts available")

type Score struct {
	AccountID              string
	Headroom               float64
	ShortHeadroom          float64
	ShortResetAfterSeconds int64
	ExpiryPressure         float64
	Sessions               int
}

type Scheduler struct {
	scores        map[string]Score
	sessionCounts map[string]int
}

const MinNewSessionHeadroom = 0.40

func NewScheduler(scores []Score) Scheduler {
	byID := make(map[string]Score, len(scores))
	for _, score := range scores {
		byID[score.AccountID] = score
	}
	return Scheduler{scores: byID}
}

func (s Scheduler) WithScore(score Score) Scheduler {
	next := Scheduler{
		scores:        make(map[string]Score, len(s.scores)+1),
		sessionCounts: s.sessionCounts,
	}
	for accountID, existing := range s.scores {
		next.scores[accountID] = existing
	}
	next.scores[score.AccountID] = score
	return next
}

func (s Scheduler) WithSessionCounts(counts map[string]int) Scheduler {
	next := Scheduler{
		scores:        s.scores,
		sessionCounts: map[string]int{},
	}
	for accountID, count := range counts {
		next.sessionCounts[accountID] = count
	}
	return next
}

func (s Scheduler) Pick(candidates []accounts.Account) (accounts.Account, error) {
	if len(candidates) == 0 {
		return accounts.Account{}, ErrNoAccounts
	}

	sorted := append([]accounts.Account(nil), candidates...)
	sort.SliceStable(sorted, func(i, j int) bool {
		left := s.score(sorted[i].ID)
		right := s.score(sorted[j].ID)
		leftTier := selectionTier(sorted[i], left)
		rightTier := selectionTier(sorted[j], right)
		if leftTier != rightTier {
			return leftTier < rightTier
		}
		leftUsable := left.usableForNewSession()
		rightUsable := right.usableForNewSession()
		if leftUsable != rightUsable {
			return leftUsable
		}
		if leftUsable && rightUsable && left.ExpiryPressure != right.ExpiryPressure {
			return left.ExpiryPressure > right.ExpiryPressure
		}
		if left.Headroom != right.Headroom {
			return left.Headroom > right.Headroom
		}
		if left.Sessions != right.Sessions {
			return left.Sessions < right.Sessions
		}
		return sorted[i].ID < sorted[j].ID
	})
	return sorted[0], nil
}

func (s Scheduler) UsableForNewSession(accountID string) bool {
	return s.score(accountID).usableForNewSession()
}

func (s Scheduler) Exhausted(accountID string) bool {
	return s.score(accountID).exhausted()
}

func selectionTier(account accounts.Account, score Score) int {
	if account.AuthMode == accounts.AuthModeOAuth && score.usableForNewSession() {
		return 0
	}
	if account.AuthMode == accounts.AuthModeAPIKey {
		return 1
	}
	if account.AuthMode == accounts.AuthModeOAuth {
		return 2
	}
	return 3
}

func (s Scheduler) score(accountID string) Score {
	score, ok := s.scores[accountID]
	if !ok {
		score = Score{AccountID: accountID, Headroom: 1, ShortHeadroom: 1}
	}
	if s.sessionCounts != nil {
		score.Sessions = s.sessionCounts[accountID]
	}
	return score
}

func (s Score) usableForNewSession() bool {
	return s.Headroom >= MinNewSessionHeadroom && s.ShortHeadroom >= MinNewSessionHeadroom
}

func (s Score) exhausted() bool {
	return s.Headroom <= 0 || s.ShortHeadroom <= 0
}
