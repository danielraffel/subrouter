package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/internal/proxy"
	"github.com/manaflow-ai/subrouter/internal/storepath"
)

const codexIsolationRemediation = "sr codex migrate-isolation"
const codexIsolationComparisonRemediation = "verify candidate and retiring credential stores before activation"
const codexAccountUsage = "sr codex isolation-check [--json] [--retiring-state-dir PATH] or sr codex migrate-isolation [--device-auth]"

var errCodexIsolationCheckFailed = errors.New("codex credential isolation preflight failed")

func isCodexAccountCommand(args []string) bool {
	return len(args) > 1 && args[0] == "codex" &&
		(args[1] == "isolation-check" || args[1] == "migrate-isolation")
}

func isCodexIsolationCheckCommand(args []string) bool {
	return len(args) > 1 && args[0] == "codex" && args[1] == "isolation-check"
}

type codexIsolationCheckResult struct {
	SchemaVersion              int                             `json:"schema_version"`
	OK                         bool                            `json:"ok"`
	AccountsRequiringMigration int                             `json:"accounts_requiring_migration"`
	Remediation                string                          `json:"remediation,omitempty"`
	Comparison                 *codexIsolationComparisonResult `json:"comparison,omitempty"`
}

type codexIsolationComparisonResult struct {
	OK                              bool `json:"ok"`
	RootsDistinct                   bool `json:"roots_distinct"`
	CandidateStoreAnchored          bool `json:"candidate_store_anchored"`
	EffectiveStoresDistinct         bool `json:"effective_stores_distinct"`
	CandidateAccountCount           int  `json:"candidate_account_count"`
	RetiringAccountCount            int  `json:"retiring_account_count"`
	CandidateDuplicateSelectorCount int  `json:"candidate_duplicate_normalized_selector_count"`
	RetiringDuplicateSelectorCount  int  `json:"retiring_duplicate_normalized_selector_count"`
	NormalizedSelectorsUnique       bool `json:"normalized_account_selectors_unique"`
	NonzeroAccountInventory         bool `json:"nonzero_account_inventory"`
	AccountInventoryMatch           bool `json:"account_inventory_match"`
	CandidateMissingIdentityCount   int  `json:"candidate_missing_immutable_identity_count"`
	RetiringMissingIdentityCount    int  `json:"retiring_missing_immutable_identity_count"`
	ImmutableIdentitiesComplete     bool `json:"immutable_account_identities_complete"`
	CandidateOAuthAccountCount      int  `json:"candidate_oauth_account_count"`
	CandidateUntrustedOAuthCount    int  `json:"candidate_untrusted_oauth_count"`
	CandidateOAuthOriginsTrusted    bool `json:"candidate_oauth_origins_trusted"`
	CandidateMissingRefreshCount    int  `json:"candidate_missing_oauth_refresh_token_count"`
	CandidateDuplicateChainCount    int  `json:"candidate_duplicate_oauth_refresh_chain_count"`
	CandidateOAuthChainsValid       bool `json:"candidate_oauth_refresh_chains_valid"`
	SharedOAuthRefreshTokenCount    int  `json:"shared_oauth_refresh_token_count"`
	OAuthRefreshTokenChainsUnique   bool `json:"oauth_refresh_token_chains_unique"`
}

type activeCodexAuthSnapshot struct {
	exists bool
	body   []byte
}

func snapshotActiveCodexAuth() (activeCodexAuthSnapshot, error) {
	body, err := os.ReadFile(accounts.DefaultCodexAuthPath())
	if errors.Is(err, os.ErrNotExist) {
		return activeCodexAuthSnapshot{}, nil
	}
	if err != nil {
		return activeCodexAuthSnapshot{}, err
	}
	return activeCodexAuthSnapshot{exists: true, body: body}, nil
}

func (snapshot activeCodexAuthSnapshot) unchanged() (bool, error) {
	current, err := snapshotActiveCodexAuth()
	if err != nil {
		return false, err
	}
	return snapshot.exists == current.exists && bytes.Equal(snapshot.body, current.body), nil
}

func codexIsolationTargets(store accounts.CodexStore) ([]accounts.StoredCodexAccount, error) {
	return codexIsolationTargetsFromList(store, store.ListStored)
}

func codexIsolationTargetsReadOnly(store accounts.CodexStore) ([]accounts.StoredCodexAccount, error) {
	return codexIsolationTargetsFromList(store, store.ListStoredReadOnly)
}

func codexIsolationTargetsFromList(
	store accounts.CodexStore,
	list func() ([]accounts.StoredCodexAccount, error),
) ([]accounts.StoredCodexAccount, error) {
	stored, err := list()
	if err != nil {
		return nil, err
	}
	active, activeOK, activeErr := accounts.ReadActiveCodexAuth()
	if activeErr != nil {
		// Explicit provenance remains authoritative when interactive auth is
		// unreadable. Missing provenance still requires migration.
		activeOK = false
	}
	targets := make([]accounts.StoredCodexAccount, 0)
	for _, account := range stored {
		if account.IsAPIKey() || account.ProviderOrDefault() != accounts.ProviderCodex || account.Auth.Tokens == nil {
			continue
		}
		trusted := account.OAuthCredentialOrigin == accounts.CodexOAuthOriginIsolatedServerLogin ||
			account.OAuthCredentialOrigin == accounts.CodexOAuthOriginServerAttested
		sharesActive := activeOK && active.Tokens != nil && active.Tokens.RefreshToken != "" &&
			active.Tokens.RefreshToken == account.Auth.Tokens.RefreshToken
		if !trusted || sharesActive {
			targets = append(targets, account)
		}
	}
	return targets, nil
}

func compareCodexIsolationStores(
	candidateRoot, retiringRoot string,
	candidateStore, retiringStore accounts.CodexStore,
) (codexIsolationComparisonResult, error) {
	normalizedCandidate, err := normalizeStateRoot(candidateRoot)
	if err != nil {
		return codexIsolationComparisonResult{}, fmt.Errorf("normalize candidate state root: %w", err)
	}
	normalizedRetiring, err := normalizeStateRoot(retiringRoot)
	if err != nil {
		return codexIsolationComparisonResult{}, fmt.Errorf("normalize retiring state root: %w", err)
	}
	rootsDistinct, err := distinctStateRoots(normalizedCandidate, normalizedRetiring)
	if err != nil {
		return codexIsolationComparisonResult{}, err
	}
	normalizedCandidateStore, err := normalizeStateRoot(candidateStore.Dir)
	if err != nil {
		return codexIsolationComparisonResult{}, fmt.Errorf("normalize candidate credential store: %w", err)
	}
	normalizedRetiringStore, err := normalizeStateRoot(retiringStore.Dir)
	if err != nil {
		return codexIsolationComparisonResult{}, fmt.Errorf("normalize retiring credential store: %w", err)
	}
	expectedCandidateStore, err := normalizeStateRoot(filepath.Join(normalizedCandidate, "codex", "accounts"))
	if err != nil {
		return codexIsolationComparisonResult{}, fmt.Errorf("normalize expected candidate credential store: %w", err)
	}
	candidateStoreAnchored := normalizedCandidateStore == expectedCandidateStore
	effectiveStoresDistinct, err := distinctStateRoots(normalizedCandidateStore, normalizedRetiringStore)
	if err != nil {
		return codexIsolationComparisonResult{}, fmt.Errorf("compare effective credential stores: %w", err)
	}
	candidate, err := candidateStore.ListStoredReadOnly()
	if err != nil {
		return codexIsolationComparisonResult{}, fmt.Errorf("inspect candidate credential store: %w", err)
	}
	retiring, err := retiringStore.ListStoredReadOnly()
	if err != nil {
		return codexIsolationComparisonResult{}, fmt.Errorf("inspect retiring credential store: %w", err)
	}

	candidateIDs, candidateMissingIdentity, candidateDuplicateSelectors := codexStoredAccountInventory(candidate)
	retiringIDs, retiringMissingIdentity, retiringDuplicateSelectors := codexStoredAccountInventory(retiring)
	nonzeroInventory := len(candidate) > 0 && len(retiring) > 0
	selectorsUnique := candidateDuplicateSelectors == 0 && retiringDuplicateSelectors == 0
	identitiesComplete := candidateMissingIdentity == 0 && retiringMissingIdentity == 0
	inventoryMatch := nonzeroInventory && selectorsUnique && identitiesComplete && equalStoredAccountInventories(candidateIDs, retiringIDs)

	candidateRefresh := make(map[[sha256.Size]byte]int)
	retiringRefresh := make(map[[sha256.Size]byte]struct{})
	candidateOAuthCount := 0
	candidateUntrusted := 0
	candidateMissingRefresh := 0
	for _, account := range candidate {
		if !storedCodexOAuthAccount(account) {
			continue
		}
		candidateOAuthCount++
		if !trustedCodexOAuthOrigin(account.OAuthCredentialOrigin) {
			candidateUntrusted++
		}
		if account.Auth.Tokens == nil || strings.TrimSpace(account.Auth.Tokens.RefreshToken) == "" {
			candidateMissingRefresh++
			continue
		}
		token := strings.TrimSpace(account.Auth.Tokens.RefreshToken)
		candidateRefresh[sha256.Sum256([]byte(token))]++
	}
	for _, account := range retiring {
		if !storedCodexOAuthAccount(account) {
			continue
		}
		if account.Auth.Tokens != nil {
			if token := strings.TrimSpace(account.Auth.Tokens.RefreshToken); token != "" {
				retiringRefresh[sha256.Sum256([]byte(token))] = struct{}{}
			}
		}
	}
	candidateDuplicateChains := 0
	for _, count := range candidateRefresh {
		if count > 1 {
			candidateDuplicateChains++
		}
	}
	sharedRefresh := 0
	for fingerprint := range candidateRefresh {
		if _, shared := retiringRefresh[fingerprint]; shared {
			sharedRefresh++
		}
	}

	result := codexIsolationComparisonResult{
		RootsDistinct:                   rootsDistinct,
		CandidateStoreAnchored:          candidateStoreAnchored,
		EffectiveStoresDistinct:         effectiveStoresDistinct,
		CandidateAccountCount:           len(candidate),
		RetiringAccountCount:            len(retiring),
		CandidateDuplicateSelectorCount: candidateDuplicateSelectors,
		RetiringDuplicateSelectorCount:  retiringDuplicateSelectors,
		NormalizedSelectorsUnique:       selectorsUnique,
		NonzeroAccountInventory:         nonzeroInventory,
		AccountInventoryMatch:           inventoryMatch,
		CandidateMissingIdentityCount:   candidateMissingIdentity,
		RetiringMissingIdentityCount:    retiringMissingIdentity,
		ImmutableIdentitiesComplete:     identitiesComplete,
		CandidateOAuthAccountCount:      candidateOAuthCount,
		CandidateUntrustedOAuthCount:    candidateUntrusted,
		CandidateOAuthOriginsTrusted:    candidateUntrusted == 0,
		CandidateMissingRefreshCount:    candidateMissingRefresh,
		CandidateDuplicateChainCount:    candidateDuplicateChains,
		CandidateOAuthChainsValid:       candidateMissingRefresh == 0 && candidateDuplicateChains == 0,
		SharedOAuthRefreshTokenCount:    sharedRefresh,
		OAuthRefreshTokenChainsUnique:   sharedRefresh == 0,
	}
	result.OK = result.RootsDistinct && result.CandidateStoreAnchored &&
		result.EffectiveStoresDistinct && result.NonzeroAccountInventory &&
		result.AccountInventoryMatch && result.NormalizedSelectorsUnique &&
		result.ImmutableIdentitiesComplete &&
		result.CandidateOAuthOriginsTrusted &&
		result.CandidateOAuthChainsValid && result.OAuthRefreshTokenChainsUnique
	return result, nil
}

func storedCodexOAuthAccount(account accounts.StoredCodexAccount) bool {
	return !account.IsAPIKey() && account.ProviderOrDefault() == accounts.ProviderCodex
}

func trustedCodexOAuthOrigin(origin accounts.CodexOAuthCredentialOrigin) bool {
	return origin == accounts.CodexOAuthOriginIsolatedServerLogin ||
		origin == accounts.CodexOAuthOriginServerAttested
}

type codexStoredAccountIdentity struct {
	provider           accounts.Provider
	authMode           string
	immutableAccountID string
}

func codexStoredAccountInventory(stored []accounts.StoredCodexAccount) (map[string]codexStoredAccountIdentity, int, int) {
	inventory := make(map[string]codexStoredAccountIdentity, len(stored))
	missingIdentity := 0
	duplicateSelectors := 0
	for _, account := range stored {
		if id := strings.ToLower(strings.TrimSpace(account.Email)); id != "" {
			if _, exists := inventory[id]; exists {
				duplicateSelectors++
			}
			identity := codexStoredAccountIdentity{provider: account.ProviderOrDefault()}
			if account.IsAPIKey() {
				identity.authMode = "apikey"
			} else {
				identity.authMode = "oauth"
				identity.immutableAccountID = strings.TrimSpace(accounts.ExtractChatGPTAccountID(account.Auth))
				if identity.immutableAccountID == "" {
					missingIdentity++
				}
			}
			inventory[id] = identity
		}
	}
	return inventory, missingIdentity, duplicateSelectors
}

func equalStoredAccountInventories(left, right map[string]codexStoredAccountIdentity) bool {
	if len(left) != len(right) {
		return false
	}
	for id, identity := range left {
		if other, ok := right[id]; !ok || other != identity {
			return false
		}
	}
	return true
}

func normalizeStateRoot(root string) (string, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		return "", errors.New("state root is empty")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	abs = filepath.Clean(abs)
	cursor := abs
	missing := make([]string, 0)
	for {
		resolved, evalErr := filepath.EvalSymlinks(cursor)
		if evalErr == nil {
			for index := len(missing) - 1; index >= 0; index-- {
				resolved = filepath.Join(resolved, missing[index])
			}
			return filepath.Clean(resolved), nil
		}
		if !errors.Is(evalErr, os.ErrNotExist) {
			return "", evalErr
		}
		parent := filepath.Dir(cursor)
		if parent == cursor {
			return abs, nil
		}
		missing = append(missing, filepath.Base(cursor))
		cursor = parent
	}
}

func distinctStateRoots(candidate, retiring string) (bool, error) {
	if candidate == retiring {
		return false, nil
	}
	candidateInfo, candidateErr := os.Stat(candidate)
	if candidateErr != nil && !errors.Is(candidateErr, os.ErrNotExist) {
		return false, fmt.Errorf("stat candidate state root: %w", candidateErr)
	}
	retiringInfo, retiringErr := os.Stat(retiring)
	if retiringErr != nil && !errors.Is(retiringErr, os.ErrNotExist) {
		return false, fmt.Errorf("stat retiring state root: %w", retiringErr)
	}
	if candidateErr == nil && retiringErr == nil && os.SameFile(candidateInfo, retiringInfo) {
		return false, nil
	}
	return true, nil
}

func isolatedCodexAuthSharesActive(auth accounts.CodexAuthFile) bool {
	active, ok, err := accounts.ReadActiveCodexAuth()
	if err != nil || !ok || active.Tokens == nil || auth.Tokens == nil {
		return false
	}
	return active.Tokens.RefreshToken != "" && active.Tokens.RefreshToken == auth.Tokens.RefreshToken
}

func codexIsolationDoctorCheck(store accounts.CodexStore) doctorCheck {
	targets, err := codexIsolationTargets(store)
	if err != nil {
		return doctorCheck{"fail", "Codex isolation", err.Error()}
	}
	if len(targets) > 0 {
		return doctorCheck{
			"fail", "Codex isolation",
			fmt.Sprintf("%d account(s) need isolated re-login; run '%s'", len(targets), codexIsolationRemediation),
		}
	}
	return doctorCheck{"ok", "Codex isolation", "all local OAuth accounts are isolated"}
}

func localCodexStoreServesLegacy(store accounts.CodexStore) bool {
	runner := srRunner{store: store}
	server, selected, err := runner.defaultRemoteServer()
	if err != nil {
		return false
	}
	if selected {
		return sameEndpoint(server.URL, localBaseURL())
	}
	configured, err := defaultCodexBaseURLForHealth()
	return err == nil && (configured == "" || sameEndpoint(configured, localBaseURL()))
}

func printCodexIsolationStatus(out io.Writer, store accounts.CodexStore) error {
	targets, err := codexIsolationTargets(store)
	if err != nil {
		return err
	}
	if len(targets) > 0 {
		_, err = fmt.Fprintf(out, "Codex isolation: %d account(s) need isolated re-login; run '%s'\n\n", len(targets), codexIsolationRemediation)
	}
	return err
}

func reportCodexIsolationRemaining(out io.Writer, store accounts.CodexStore) {
	remaining, err := codexIsolationTargets(store)
	if err != nil {
		fmt.Fprintf(out, "Could not recount remaining Codex accounts: %v\n", err)
		return
	}
	fmt.Fprintf(out, "%d account(s) still need migration.\n", len(remaining))
}

func (r srRunner) codexAccount(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: %s", codexAccountUsage)
	}
	switch args[0] {
	case "isolation-check":
		return r.checkCodexIsolation(args[1:])
	case "migrate-isolation":
		return r.migrateCodexIsolation(ctx, args[1:])
	default:
		return fmt.Errorf("unknown Codex account command %q; usage: %s", args[0], codexAccountUsage)
	}
}

func (r srRunner) checkCodexIsolation(args []string) error {
	flags := flag.NewFlagSet("sr codex isolation-check", flag.ContinueOnError)
	flags.SetOutput(r.errOut)
	jsonOutput := flags.Bool("json", false, "print machine-readable JSON")
	retiringStateDir := flags.String("retiring-state-dir", "", "compare against the retiring service state root")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("usage: sr codex isolation-check [--json] [--retiring-state-dir PATH]")
	}
	comparisonRequested := false
	flags.Visit(func(parsed *flag.Flag) {
		if parsed.Name == "retiring-state-dir" {
			comparisonRequested = true
		}
	})
	if comparisonRequested && strings.TrimSpace(*retiringStateDir) == "" {
		return errors.New("--retiring-state-dir must not be empty")
	}
	targets, err := codexIsolationTargetsReadOnly(r.store)
	if err != nil {
		return err
	}
	result := codexIsolationCheckResult{
		SchemaVersion:              1,
		OK:                         len(targets) == 0,
		AccountsRequiringMigration: len(targets),
	}
	if comparisonRequested {
		normalizedRetiring, normalizeErr := normalizeStateRoot(*retiringStateDir)
		if normalizeErr != nil {
			return fmt.Errorf("normalize retiring state root: %w", normalizeErr)
		}
		comparison, compareErr := compareCodexIsolationStores(
			storepath.StateDir(), *retiringStateDir, r.store,
			accounts.CodexStoreForStateRootReadOnlyInspection(normalizedRetiring),
		)
		if compareErr != nil {
			return compareErr
		}
		result.Comparison = &comparison
		result.OK = result.OK && comparison.OK
	}
	if !result.OK {
		if result.Comparison != nil && !result.Comparison.OK {
			result.Remediation = codexIsolationComparisonRemediation
		} else {
			result.Remediation = codexIsolationRemediation
		}
	}
	if *jsonOutput {
		if err := json.NewEncoder(r.out).Encode(result); err != nil {
			return err
		}
	} else if result.OK {
		fmt.Fprintln(r.out, "Codex credential isolation preflight passed.")
	} else if result.Comparison != nil && !result.Comparison.OK {
		fmt.Fprintf(r.out, "Codex credential isolation comparison failed: %s.\n", result.Remediation)
	} else {
		fmt.Fprintf(r.out, "Codex credential isolation preflight failed: %d account(s) require migration; run '%s'.\n", result.AccountsRequiringMigration, result.Remediation)
	}
	if !result.OK {
		return errCodexIsolationCheckFailed
	}
	return nil
}

func (r srRunner) migrateCodexIsolation(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet(codexIsolationRemediation, flag.ContinueOnError)
	flags.SetOutput(r.errOut)
	deviceAuth := flags.Bool("device-auth", false, "use codex login --device-auth")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("usage: %s [--device-auth]", codexIsolationRemediation)
	}
	targets, err := codexIsolationTargets(r.store)
	if err != nil {
		return err
	}
	if len(targets) == 0 {
		fmt.Fprintln(r.out, "All local Codex OAuth accounts are isolated. No migration needed.")
		return nil
	}
	originalActive, err := snapshotActiveCodexAuth()
	if err != nil {
		return fmt.Errorf("snapshot local Codex auth: %w", err)
	}
	fmt.Fprintf(r.out, "Found %d Codex OAuth account(s) that need isolated re-login.\n", len(targets))
	fmt.Fprintln(r.out, "Each login uses a temporary CODEX_HOME; ~/.codex/auth.json will not be changed.")

	for index, target := range targets {
		fmt.Fprintf(r.out, "\nSign in as %s (%d/%d).\n", target.Email, index+1, len(targets))
		auth, email, loginErr := r.isolatedCodexLogin(ctx, *deviceAuth)
		if loginErr != nil {
			reportCodexIsolationRemaining(r.out, r.store)
			return fmt.Errorf("isolated login for %s: %w", target.Email, loginErr)
		}
		unchanged, checkErr := originalActive.unchanged()
		if checkErr != nil {
			reportCodexIsolationRemaining(r.out, r.store)
			return fmt.Errorf("verify local Codex auth after login: %w", checkErr)
		}
		if !unchanged {
			reportCodexIsolationRemaining(r.out, r.store)
			return errors.New("local Codex auth changed unexpectedly; no stored credential was replaced for this login")
		}
		if !strings.EqualFold(strings.TrimSpace(email), strings.TrimSpace(target.Email)) {
			fmt.Fprintln(r.out, "No stored credential was changed for this login.")
			reportCodexIsolationRemaining(r.out, r.store)
			return fmt.Errorf("logged in as %s, expected %s", email, target.Email)
		}
		if isolatedCodexAuthSharesActive(auth) {
			fmt.Fprintln(r.out, "No stored credential was changed for this login.")
			reportCodexIsolationRemaining(r.out, r.store)
			return errors.New("isolated login returned the active Codex refresh-token chain; retry the isolated login")
		}
		err := proxy.PublishAccountDiskMutation(ctx, r.store.StoreDir(), func() (bool, error) {
			replaceErr := r.store.ReplaceStoredOAuthWithIsolated(ctx, target.Email, auth)
			return replaceErr == nil, replaceErr
		})
		if err != nil {
			reportCodexIsolationRemaining(r.out, r.store)
			return fmt.Errorf("replace isolated credential for %s: %w", target.Email, err)
		}
		fmt.Fprintf(r.out, "Migrated %s (%d remaining).\n", target.Email, len(targets)-index-1)
	}

	remaining, err := codexIsolationTargets(r.store)
	if err != nil {
		return err
	}
	if len(remaining) != 0 {
		return fmt.Errorf("migration incomplete: %d account(s) still need isolated re-login", len(remaining))
	}
	unchanged, err := originalActive.unchanged()
	if err != nil {
		return err
	}
	if !unchanged {
		return errors.New("local Codex auth changed unexpectedly during migration")
	}
	fmt.Fprintf(r.out, "\nMigrated %d Codex OAuth account(s). Local Codex auth was left unchanged.\n", len(targets))
	return nil
}
