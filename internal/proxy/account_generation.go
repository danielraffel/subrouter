package proxy

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

const accountDiskGenerationFile = ".account-generation"

func accountDiskGenerationPath(storeDir string) string {
	return filepath.Join(storeDir, accountDiskGenerationFile)
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
	return mutate()
}

func (r *AccountRef) reloadIfDiskGenerationChanged(ctx context.Context) (reloaded bool, generation uint64, err error) {
	if r == nil {
		return false, 0, nil
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
