package proxy

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	agentclaude "github.com/manaflow-ai/subrouter/internal/agents/claude"
)

const accountDiskGenerationFile = ".account-generation"
const accountRollbackActiveFile = ".account-rollback-active"

const (
	accountRollbackJournalVersion = 2
	accountRollbackTargetGeneric  = "generic-callback"
	accountRollbackTargetClaude   = "claude-unpublished-profile"
	accountRollbackPrepared       = "prepared"
	accountRollbackRemoved        = "removed"
	accountRollbackPublished      = "published"
)

var errAccountRollbackIncomplete = errors.New("account credential rollback is incomplete")

type accountRollbackJournal struct {
	Version              int    `json:"version"`
	Target               string `json:"target"`
	TargetID             string `json:"target_id,omitempty"`
	TargetInstanceDir    string `json:"target_instance_dir,omitempty"`
	PreconditionVersion  string `json:"precondition_version,omitempty"`
	Progress             string `json:"progress"`
	CompletionGeneration string `json:"completion_generation,omitempty"`
}

func accountDiskGenerationPath(storeDir string) string {
	return filepath.Join(storeDir, accountDiskGenerationFile)
}

func accountRollbackActivePath(storeDir string) string {
	return filepath.Join(storeDir, accountRollbackActiveFile)
}

func accountRollbackActive(storeDir string) (bool, error) {
	_, found, err := readAccountRollbackJournal(storeDir)
	return found, err
}

func setAccountRollbackActive(storeDir string) error {
	if _, found, err := readAccountRollbackJournal(storeDir); err != nil || found {
		return err
	}
	return writeAccountRollbackJournal(storeDir, accountRollbackJournal{
		Version:  accountRollbackJournalVersion,
		Target:   accountRollbackTargetGeneric,
		Progress: accountRollbackPrepared,
	})
}

func claudeProfileRollbackVersion(profileName, instanceDir string) string {
	body := []byte(profileName + "\x00" + instanceDir)
	digest := sha256.Sum256(body)
	return hex.EncodeToString(digest[:])
}

func prepareClaudeProfileRollback(ctx context.Context, storeDir, profileName string) (accountRollbackJournal, bool, error) {
	if err := agentclaude.ValidateProfileNameAllowEmail(profileName); err != nil {
		return accountRollbackJournal{}, false, fmt.Errorf("invalid Claude rollback target %q: %w", profileName, err)
	}
	store := agentclaude.Store{Dir: storeDir}
	if _, found, err := readAccountRollbackJournal(storeDir); err != nil {
		return accountRollbackJournal{}, false, err
	} else if found {
		return accountRollbackJournal{}, false, errors.New("another account rollback journal is already active")
	}
	var journal accountRollbackJournal
	found, err := store.PrepareUnpublishedProfileRemovalContext(ctx, profileName, func(instanceDir string) error {
		journal = accountRollbackJournal{
			Version:             accountRollbackJournalVersion,
			Target:              accountRollbackTargetClaude,
			TargetID:            profileName,
			TargetInstanceDir:   instanceDir,
			PreconditionVersion: claudeProfileRollbackVersion(profileName, instanceDir),
			Progress:            accountRollbackPrepared,
		}
		return writeAccountRollbackJournal(storeDir, journal)
	})
	return journal, found, err
}

func readAccountRollbackJournal(storeDir string) (accountRollbackJournal, bool, error) {
	var journal accountRollbackJournal
	body, err := os.ReadFile(accountRollbackActivePath(storeDir))
	if errors.Is(err, os.ErrNotExist) {
		return journal, false, nil
	}
	if err != nil {
		return journal, false, err
	}
	if err := json.Unmarshal(body, &journal); err != nil {
		return journal, true, fmt.Errorf("decode account rollback journal: %w", err)
	}
	if journal.Version != accountRollbackJournalVersion {
		return journal, true, fmt.Errorf("account rollback journal version %d is unsupported", journal.Version)
	}
	switch journal.Target {
	case accountRollbackTargetGeneric:
		if journal.TargetID != "" || journal.TargetInstanceDir != "" || journal.PreconditionVersion != "" {
			return journal, true, errors.New("generic account rollback journal unexpectedly names a target")
		}
	case accountRollbackTargetClaude:
		if err := agentclaude.ValidateProfileNameAllowEmail(journal.TargetID); err != nil {
			return journal, true, fmt.Errorf("Claude account rollback journal target %q is invalid: %w", journal.TargetID, err)
		}
		if journal.PreconditionVersion == "" {
			return journal, true, errors.New("Claude account rollback journal precondition version is missing")
		}
		if filepath.Clean(journal.TargetInstanceDir) != journal.TargetInstanceDir || filepath.Base(journal.TargetInstanceDir) != journal.TargetInstanceDir || journal.TargetInstanceDir == "." {
			return journal, true, fmt.Errorf("Claude account rollback journal instance directory %q is invalid", journal.TargetInstanceDir)
		}
		wantVersion := claudeProfileRollbackVersion(journal.TargetID, journal.TargetInstanceDir)
		if journal.PreconditionVersion != wantVersion {
			return journal, true, fmt.Errorf("Claude account rollback journal identity version does not match target %q", journal.TargetID)
		}
	default:
		return journal, true, fmt.Errorf("account rollback journal target type %q is unsupported", journal.Target)
	}
	return journal, true, nil
}

func writeAccountRollbackJournal(storeDir string, journal accountRollbackJournal) (err error) {
	if err := os.MkdirAll(storeDir, 0o700); err != nil {
		return err
	}
	body, err := json.Marshal(journal)
	if err != nil {
		return err
	}
	body = append(body, '\n')
	file, err := os.CreateTemp(storeDir, ".account-rollback-*")
	if err != nil {
		return err
	}
	tempPath := file.Name()
	defer func() {
		if file != nil {
			err = errors.Join(err, file.Close())
		}
		if err != nil {
			_ = os.Remove(tempPath)
		}
	}()
	if err := file.Chmod(0o600); err != nil {
		return err
	}
	if _, err := file.Write(body); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	file = nil
	if err := os.Rename(tempPath, accountRollbackActivePath(storeDir)); err != nil {
		return err
	}
	return syncAccountStateDir(storeDir)
}

func reconcileCompletedAccountRollback(ctx context.Context, storeDir string, publishGeneration func(string) error) (bool, error) {
	journal, found, err := readAccountRollbackJournal(storeDir)
	if err != nil || !found {
		return false, err
	}
	switch journal.Progress {
	case accountRollbackPrepared:
		if journal.Target != accountRollbackTargetClaude {
			return false, errAccountRollbackIncomplete
		}
		removed, removeErr := replayPreparedClaudeProfileRollback(ctx, storeDir, journal)
		if !removed {
			return false, removeErr
		}
		if err := markAccountRollbackRemoved(storeDir); err != nil {
			return false, errors.Join(removeErr, err)
		}
		journal.Progress = accountRollbackRemoved
	case accountRollbackRemoved:
		// Publication below completes both a directly observed removed journal
		// and a prepared Claude rollback replayed above.
	}
	if journal.Progress == accountRollbackRemoved {
		if err := publishGeneration(storeDir); err != nil {
			return false, err
		}
		generation, err := readAccountDiskGeneration(storeDir)
		if err != nil || generation == "" {
			return false, errors.Join(err, errors.New("account rollback completion generation is unavailable"))
		}
		journal.Progress = accountRollbackPublished
		journal.CompletionGeneration = generation
		if err := writeAccountRollbackJournal(storeDir, journal); err != nil {
			return false, err
		}
	} else if journal.Progress == accountRollbackPublished {
		generation, err := readAccountDiskGeneration(storeDir)
		if err != nil {
			return false, err
		}
		if journal.CompletionGeneration == "" || generation != journal.CompletionGeneration {
			return false, errors.New("account rollback completion generation does not match")
		}
	} else {
		return false, errors.New("account rollback journal progress is invalid")
	}
	if err := clearAccountRollbackActive(storeDir); err != nil {
		return false, err
	}
	return true, nil
}

func replayPreparedClaudeProfileRollback(ctx context.Context, storeDir string, journal accountRollbackJournal) (bool, error) {
	store := agentclaude.Store{Dir: storeDir}
	completed, err := store.CompleteUnpublishedProfileRemovalContext(ctx, journal.TargetID, journal.TargetInstanceDir)
	if !completed {
		return false, errors.Join(err, fmt.Errorf("Claude rollback target %q cleanup is incomplete", journal.TargetID))
	}
	return true, err
}

func markAccountRollbackRemoved(storeDir string) error {
	journal, found, err := readAccountRollbackJournal(storeDir)
	if err != nil {
		return err
	}
	if !found || journal.Progress != accountRollbackPrepared {
		return errors.New("account rollback journal is not prepared")
	}
	journal.Progress = accountRollbackRemoved
	return writeAccountRollbackJournal(storeDir, journal)
}

func markAccountRollbackPublished(storeDir string) error {
	journal, found, err := readAccountRollbackJournal(storeDir)
	if err != nil {
		return err
	}
	if !found || journal.Progress != accountRollbackRemoved {
		return errors.New("account rollback journal is not removed")
	}
	generation, err := readAccountDiskGeneration(storeDir)
	if err != nil || generation == "" {
		return errors.Join(err, errors.New("account rollback completion generation is unavailable"))
	}
	journal.Progress = accountRollbackPublished
	journal.CompletionGeneration = generation
	return writeAccountRollbackJournal(storeDir, journal)
}

func resetAccountRollbackForRetry(storeDir string) error {
	journal, found, err := readAccountRollbackJournal(storeDir)
	if err != nil {
		return err
	}
	if !found || journal.Progress == accountRollbackPrepared {
		return nil
	}
	journal.Progress = accountRollbackPrepared
	journal.CompletionGeneration = ""
	return writeAccountRollbackJournal(storeDir, journal)
}

func clearAccountRollbackActive(storeDir string) error {
	if err := os.Remove(accountRollbackActivePath(storeDir)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return syncAccountStateDir(storeDir)
}

func syncAccountStateDir(storeDir string) error {
	return syncAccountStateDirForOS(runtime.GOOS, storeDir, os.Open)
}

func syncAccountStateDirForOS(goos, storeDir string, openDir func(string) (*os.File, error)) error {
	// Windows does not support syncing an opened directory through os.File.
	// Atomic rename/remove remains the established durability boundary there.
	if goos == "windows" {
		return nil
	}
	dir, err := openDir(storeDir)
	if err != nil {
		return err
	}
	err = dir.Sync()
	return errors.Join(err, dir.Close())
}

func readAccountDiskGeneration(storeDir string) (string, error) {
	body, err := os.ReadFile(accountDiskGenerationPath(storeDir))
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return string(body), nil
}

// advanceAccountDiskGeneration publishes one completed disk mutation to every
// overlapping supervisor worker. Callers hold the cross-process import lock,
// so truncation cannot expose a partial generation to another reload.
func advanceAccountDiskGeneration(storeDir string) (err error) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return fmt.Errorf("generate account state generation: %w", err)
	}
	if err := os.MkdirAll(storeDir, 0o700); err != nil {
		return err
	}
	file, err := os.CreateTemp(storeDir, ".account-generation-*")
	if err != nil {
		return err
	}
	tempPath := file.Name()
	defer func() {
		if file != nil {
			err = errors.Join(err, file.Close())
		}
		if err != nil {
			_ = os.Remove(tempPath)
		}
	}()
	if err := file.Chmod(0o600); err != nil {
		return err
	}
	if _, err := file.WriteString(hex.EncodeToString(value)); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	closeErr := file.Close()
	file = nil
	if closeErr != nil {
		return closeErr
	}
	if err := os.Rename(tempPath, accountDiskGenerationPath(storeDir)); err != nil {
		return err
	}
	if dir, openErr := os.Open(storeDir); openErr == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return nil
}

func (r *AccountRef) advanceDiskGeneration() error {
	publish := advanceAccountDiskGeneration
	if r.publishGenerationForTest != nil {
		publish = r.publishGenerationForTest
	}
	return publish(r.store.StoreDir())
}

// PublishAccountDiskMutation serializes a local CLI credential mutation with
// HTTP account imports. It publishes the new generation before invoking the
// mutation while still holding the same cross-process lock workers acquire to
// reload. A worker that observes the marker therefore waits until the mutation
// commits before reading credentials. Publication failure happens before any
// credential change; an unchanged or failed mutation may cause a harmless
// extra reload but can never leave committed credentials unpublished.
func PublishAccountDiskMutation(ctx context.Context, storeDir string, mutate func() (bool, error)) (err error) {
	return publishAccountDiskMutation(ctx, storeDir, advanceAccountDiskGeneration, mutate)
}

// WithAccountDiskMutationPublication holds the cross-process account
// transaction while mutate performs its own final freshness check. mutate must
// call publish immediately before the first durable credential mutation. This
// shape is for refreshers whose provider-specific lock must stay held across
// that final check, publication, and rotation; a no-op refresh never calls
// publish and therefore never advances the generation.
func WithAccountDiskMutationPublication(
	ctx context.Context,
	storeDir string,
	mutate func(publish func() error) error,
) error {
	return withAccountDiskMutationPublication(ctx, storeDir, advanceAccountDiskGeneration, mutate)
}

// RollbackUnpublishedAccountDiskMutation removes a credential that must never
// remain observable after rollback. A durable fail-closed marker is installed
// before removal; workers consult it independently of the generation marker
// and evict their credential snapshots while it exists. If the process crashes
// at any point, workers therefore serve no stored credentials rather than a
// stale deleted secret. A successful rollback publishes the removal before the
// marker is cleared.
func RollbackUnpublishedAccountDiskMutation(
	ctx context.Context,
	storeDir string,
	rollback func() error,
) error {
	return rollbackUnpublishedAccountDiskMutation(ctx, storeDir, advanceAccountDiskGeneration, rollback)
}

// RollbackUnpublishedClaudeProfileDiskMutation durably identifies and removes
// one unpublished Claude profile. Unlike the compatibility callback form, its
// prepared journal is replayable after a crash because it records the exact
// validated profile identity and a non-secret version of its registry entry.
func RollbackUnpublishedClaudeProfileDiskMutation(
	ctx context.Context,
	storeDir string,
	profileName string,
) (removed bool, err error) {
	return rollbackUnpublishedClaudeProfileDiskMutation(ctx, storeDir, profileName, advanceAccountDiskGeneration)
}

func rollbackUnpublishedClaudeProfileDiskMutation(
	ctx context.Context,
	storeDir string,
	profileName string,
	publishGeneration func(string) error,
) (removed bool, err error) {
	lock, err := lockAccountImportTransaction(ctx, storeDir)
	if err != nil {
		return false, err
	}
	defer func() { err = errors.Join(err, lock.Close()) }()
	_, found, err := prepareClaudeProfileRollback(ctx, storeDir, profileName)
	if err != nil || !found {
		return false, err
	}
	// Wake workers that are not already polling the fail-closed marker. The
	// marker remains authoritative if this publication or a later step fails.
	initialPublishErr := publishGeneration(storeDir)
	journal, _, err := readAccountRollbackJournal(storeDir)
	if err != nil {
		return false, errors.Join(initialPublishErr, err)
	}
	removed, cleanupErr := replayPreparedClaudeProfileRollback(ctx, storeDir, journal)
	if !removed {
		return false, errors.Join(initialPublishErr, cleanupErr)
	}
	if err := markAccountRollbackRemoved(storeDir); err != nil {
		return true, errors.Join(initialPublishErr, cleanupErr, err)
	}
	completionPublishErr := publishGeneration(storeDir)
	if completionPublishErr != nil {
		return true, errors.Join(initialPublishErr, cleanupErr, completionPublishErr)
	}
	if err := markAccountRollbackPublished(storeDir); err != nil {
		return true, errors.Join(initialPublishErr, cleanupErr, err)
	}
	return true, errors.Join(initialPublishErr, cleanupErr, clearAccountRollbackActive(storeDir))
}

func rollbackUnpublishedAccountDiskMutation(
	ctx context.Context,
	storeDir string,
	publishGeneration func(string) error,
	rollback func() error,
) (err error) {
	lock, err := lockAccountImportTransaction(ctx, storeDir)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, lock.Close()) }()
	if err := setAccountRollbackActive(storeDir); err != nil {
		return err
	}
	if err := resetAccountRollbackForRetry(storeDir); err != nil {
		return err
	}
	// Wake workers that are not already polling the fail-closed marker. The
	// marker remains authoritative if this publication or a later step fails.
	initialPublishErr := publishGeneration(storeDir)
	if err := rollback(); err != nil {
		return errors.Join(initialPublishErr, err)
	}
	if err := markAccountRollbackRemoved(storeDir); err != nil {
		return errors.Join(initialPublishErr, err)
	}
	completionPublishErr := publishGeneration(storeDir)
	if completionPublishErr != nil {
		return errors.Join(initialPublishErr, completionPublishErr)
	}
	if err := markAccountRollbackPublished(storeDir); err != nil {
		return errors.Join(initialPublishErr, err)
	}
	return errors.Join(initialPublishErr, clearAccountRollbackActive(storeDir))
}

func withAccountDiskMutationPublication(
	ctx context.Context,
	storeDir string,
	publishGeneration func(string) error,
	mutate func(publish func() error) error,
) error {
	return withAccountDiskTransaction(ctx, storeDir, func() error {
		published := false
		publish := func() error {
			if published {
				return nil
			}
			if err := publishGeneration(storeDir); err != nil {
				return err
			}
			published = true
			return nil
		}
		return mutate(publish)
	})
}

func publishAccountDiskMutation(
	ctx context.Context,
	storeDir string,
	publish func(string) error,
	mutate func() (bool, error),
) (err error) {
	return withAccountDiskTransaction(ctx, storeDir, func() error {
		if err := publish(storeDir); err != nil {
			return err
		}
		_, err := mutate()
		return err
	})
}

func withAccountDiskTransaction(ctx context.Context, storeDir string, mutate func() error) (err error) {
	lock, err := lockAccountImportTransaction(ctx, storeDir)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := lock.Close(); err == nil {
			err = closeErr
		}
	}()
	if _, err := reconcileCompletedAccountRollback(ctx, storeDir, advanceAccountDiskGeneration); err != nil {
		return err
	}
	return mutate()
}

func (r *AccountRef) reloadIfDiskGenerationChanged(ctx context.Context) (reloaded bool, generation uint64, err error) {
	if r == nil {
		return false, 0, nil
	}
	_, rollbackActive, markerErr := readAccountRollbackJournal(r.store.StoreDir())
	if rollbackActive {
		handled, reloaded, generation, err := r.reconcileOrEvictAccountRollback(ctx, markerErr)
		if handled {
			return reloaded, generation, err
		}
	} else if markerErr != nil {
		return false, 0, markerErr
	}
	diskGeneration, err := readAccountDiskGeneration(r.store.StoreDir())
	if err != nil {
		return false, 0, err
	}
	r.mu.RLock()
	unchanged := diskGeneration == r.diskGeneration
	generation = r.accountGeneration
	r.mu.RUnlock()
	if unchanged {
		return false, generation, nil
	}

	if err := lockMutexContext(ctx, &r.installMu); err != nil {
		return false, generation, err
	}
	defer r.installMu.Unlock()
	lock, err := tryLockAccountImportTransaction(ctx, r.store.StoreDir())
	if errors.Is(err, errAccountImportTransactionBusy) {
		// The generation is published before a credential mutation so a crash
		// can never strand committed state. While that transaction is still in
		// flight, keep serving the last complete snapshot and retry on the next
		// account lookup instead of stalling every request behind an OAuth token
		// exchange or another slow credential operation.
		return false, generation, nil
	}
	if err != nil {
		return false, generation, err
	}
	defer func() {
		if closeErr := lock.Close(); err == nil {
			err = closeErr
		}
	}()
	diskGeneration, err = readAccountDiskGeneration(r.store.StoreDir())
	if err != nil {
		return false, generation, err
	}
	r.mu.RLock()
	unchanged = diskGeneration == r.diskGeneration
	generation = r.accountGeneration
	r.mu.RUnlock()
	if unchanged {
		return false, generation, nil
	}
	_, generation, err = r.ReloadSnapshot()
	if err != nil {
		return false, generation, err
	}
	return true, generation, nil
}

func (r *AccountRef) reconcileOrEvictAccountRollback(ctx context.Context, markerErr error) (bool, bool, uint64, error) {
	if err := lockMutexContext(ctx, &r.installMu); err != nil {
		return true, false, 0, err
	}
	defer r.installMu.Unlock()
	lock, lockErr := tryLockAccountImportTransaction(ctx, r.store.StoreDir())
	if errors.Is(lockErr, errAccountImportTransactionBusy) {
		reloaded, generation := r.evictSnapshotForAccountRollbackLocked()
		return true, reloaded, generation, markerErr
	}
	if lockErr != nil {
		reloaded, generation := r.evictSnapshotForAccountRollbackLocked()
		return true, reloaded, generation, errors.Join(markerErr, lockErr)
	}
	defer lock.Close()
	if markerErr == nil {
		_, markerErr = reconcileCompletedAccountRollback(ctx, r.store.StoreDir(), advanceAccountDiskGeneration)
	}
	if markerErr != nil {
		reloaded, generation := r.evictSnapshotForAccountRollbackLocked()
		return true, reloaded, generation, markerErr
	}
	return false, false, r.Generation(), nil
}

func (r *AccountRef) evictSnapshotForAccountRollback(ctx context.Context) (bool, uint64, error) {
	if err := lockMutexContext(ctx, &r.installMu); err != nil {
		return false, 0, err
	}
	defer r.installMu.Unlock()
	reloaded, generation := r.evictSnapshotForAccountRollbackLocked()
	return reloaded, generation, nil
}

func (r *AccountRef) evictSnapshotForAccountRollbackLocked() (bool, uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.accounts) == 0 {
		return false, r.accountGeneration
	}
	r.accounts = nil
	r.accountGeneration++
	r.credentialRevision++
	return true, r.accountGeneration
}
