package main

import (
	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/internal/agents/opencode"
	"github.com/manaflow-ai/subrouter/internal/agents/pi"
)

type srCompatSyncResult struct {
	Tool string
	Path string
	Err  error
}

func syncCodexCompatibleAuth(account accounts.StoredCodexAccount) []srCompatSyncResult {
	results := make([]srCompatSyncResult, 0, 2)

	openCodePath, err := opencode.SyncCodexAccount(account)
	results = append(results, srCompatSyncResult{Tool: "OpenCode", Path: openCodePath, Err: err})

	piPath, err := pi.SyncCodexAccount(account)
	results = append(results, srCompatSyncResult{Tool: "pi", Path: piPath, Err: err})

	return results
}
