package main

import (
	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/internal/agents/opencode"
	"github.com/manaflow-ai/subrouter/internal/agents/pi"
)

type cxCompatSyncResult struct {
	Tool string
	Path string
	Err  error
}

func syncCodexCompatibleAuth(account accounts.StoredCodexAccount) []cxCompatSyncResult {
	results := make([]cxCompatSyncResult, 0, 2)

	openCodePath, err := opencode.SyncCodexAccount(account)
	results = append(results, cxCompatSyncResult{Tool: "OpenCode", Path: openCodePath, Err: err})

	piPath, err := pi.SyncCodexAccount(account)
	results = append(results, cxCompatSyncResult{Tool: "pi", Path: piPath, Err: err})

	return results
}
