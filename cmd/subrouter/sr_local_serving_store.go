package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

const (
	localServingStoreSchema       = "subrouter.local-serving-store/v1"
	localServingStoreProbeTimeout = 10 * time.Second
)

type localServingStoreBinding struct {
	Schema      string `json:"schema"`
	AccountsDir string `json:"accounts_dir"`
}

type localServingStoreExpectation struct {
	Absent bool
	SHA256 string
	Mode   os.FileMode
}

func localServingStoreBindingPath(store accounts.CodexStore) string {
	return filepath.Join(store.StoreDir(), ".local-serving-store.json")
}

// localServingStore keeps the ordinary CLI state independent from a separately
// enrolled supervised daemon while still letting the CLI prove the exact store
// behind the loopback listener. An explicit SUBROUTER_STATE_DIR remains the
// highest authority and never consults the binding.
func localServingStore(store accounts.CodexStore) (accounts.CodexStore, error) {
	if explicitLocalStateAuthority() {
		return store, nil
	}
	path := localServingStoreBindingPath(store)
	_, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return store, nil
	}
	if err != nil {
		return accounts.CodexStore{}, fmt.Errorf("inspect local serving-store binding: %w", err)
	}
	if _, err := validatePrivateLocalServingStorePath(filepath.Dir(path), true); err != nil {
		return accounts.CodexStore{}, fmt.Errorf("validate local serving-store binding directory: %w", err)
	}
	file, err := openPrivateLocalServingStoreBinding(path)
	if err != nil {
		return accounts.CodexStore{}, fmt.Errorf("open local serving-store binding: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || info.Size() > 4096 {
		return accounts.CodexStore{}, errors.New("local serving-store binding is too large")
	}
	var binding localServingStoreBinding
	decoder := json.NewDecoder(io.LimitReader(file, 4097))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&binding); err != nil {
		return accounts.CodexStore{}, fmt.Errorf("decode local serving-store binding: %w", err)
	}
	if err := ensureLocalServingStoreJSONEOF(decoder); err != nil {
		return accounts.CodexStore{}, fmt.Errorf("decode local serving-store binding: %w", err)
	}
	accountsDir := filepath.Clean(strings.TrimSpace(binding.AccountsDir))
	if binding.Schema != localServingStoreSchema || !filepath.IsAbs(accountsDir) || accountsDir == string(filepath.Separator) {
		return accounts.CodexStore{}, errors.New("local serving-store binding is invalid")
	}
	canonicalAccountsDir, err := validatePrivateLocalServingStorePath(accountsDir, true)
	if err != nil || canonicalAccountsDir != accountsDir {
		return accounts.CodexStore{}, errors.New("local serving account store must be an existing canonical private directory")
	}
	authorityKey, err := openPrivateLocalServingStoreBinding(filepath.Join(filepath.Dir(accountsDir), ".store-authority-key"))
	if err != nil {
		return accounts.CodexStore{}, fmt.Errorf("validate local serving-store authority key: %w", err)
	}
	_ = authorityKey.Close()
	return accounts.CodexStore{Dir: accountsDir}, nil
}

func ensureLocalServingStoreJSONEOF(decoder *json.Decoder) error {
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("unexpected trailing JSON value")
		}
		return err
	}
	return nil
}

func bindLocalServingStore(stateDir string, store accounts.CodexStore, out io.Writer) error {
	return bindLocalServingStoreIfCurrent(context.Background(), stateDir, store, out, localServingStoreExpectation{})
}

func bindLocalServingStoreIfCurrent(
	ctx context.Context,
	stateDir string,
	store accounts.CodexStore,
	out io.Writer,
	expectation localServingStoreExpectation,
) error {
	if explicitLocalStateAuthority() {
		return errors.New("bind-state must run with SUBROUTER_STATE_DIR unset so it updates the default CLI metadata")
	}
	stateDir = filepath.Clean(strings.TrimSpace(stateDir))
	if !filepath.IsAbs(stateDir) || stateDir == string(filepath.Separator) {
		return errors.New("serving state directory must be an absolute non-root path")
	}
	canonicalStateDir, err := validatePrivateLocalServingStorePath(stateDir, true)
	if err != nil {
		return errors.New("serving state directory must be an existing private directory")
	}
	accountsDir, err := filepath.EvalSymlinks(filepath.Join(canonicalStateDir, "codex", "accounts"))
	if err != nil {
		return errors.New("serving account store must be an existing private directory")
	}
	accountsDir, err = validatePrivateLocalServingStorePath(accountsDir, true)
	if err != nil {
		return errors.New("serving account store must be an existing private directory")
	}
	servingStore := accounts.CodexStore{Dir: accountsDir}
	path := localServingStoreBindingPath(store)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create serving-store binding directory: %w", err)
	}
	if _, err := validatePrivateLocalServingStorePath(filepath.Dir(path), true); err != nil {
		return fmt.Errorf("validate serving-store binding directory: %w", err)
	}
	lock, err := lockLocalServingStoreBinding(path)
	if err != nil {
		return fmt.Errorf("lock serving-store binding: %w", err)
	}
	defer lock.Close()
	if err := validateLocalServingStoreExpectation(path, expectation); err != nil {
		return err
	}
	client, err := newLocalStoreAttestedClient(
		&http.Client{Timeout: localServingStoreProbeTimeout}, localBaseURL(), servingStore,
	)
	if err != nil {
		return err
	}
	healthURL, err := healthURLFor(localBaseURL())
	if err != nil {
		return fmt.Errorf("verify local serving state: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, healthURL, nil)
	if err != nil {
		return fmt.Errorf("verify local serving state: %w", err)
	}
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("verify local serving state: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("verify local serving state: %s", response.Status)
	}
	if authorityKey, err := openPrivateLocalServingStoreBinding(filepath.Join(filepath.Dir(accountsDir), ".store-authority-key")); err != nil {
		return fmt.Errorf("validate local serving-store authority key: %w", err)
	} else {
		_ = authorityKey.Close()
	}
	payload, err := json.Marshal(localServingStoreBinding{Schema: localServingStoreSchema, AccountsDir: accountsDir})
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".local-serving-store-*")
	if err != nil {
		return fmt.Errorf("stage serving-store binding: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(append(payload, '\n')); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("publish serving-store binding: %w", err)
	}
	if err := syncLocalServingStoreBindingDirectory(filepath.Dir(path)); err != nil {
		return fmt.Errorf("sync serving-store binding directory: %w", err)
	}
	fmt.Fprintf(out, "Local CLI bound to serving state %s\n", canonicalStateDir)
	return nil
}

func validateLocalServingStoreExpectation(path string, expectation localServingStoreExpectation) error {
	if !expectation.Absent && expectation.SHA256 == "" {
		return nil
	}
	if expectation.Absent {
		if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
			return nil
		} else if err != nil {
			return fmt.Errorf("inspect current serving-store binding: %w", err)
		}
		return errors.New("serving-store binding changed before publication: expected absence")
	}
	file, err := openPrivateLocalServingStoreBinding(path)
	if err != nil {
		return fmt.Errorf("inspect current serving-store binding: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("inspect current serving-store binding: %w", err)
	}
	if info.Mode().Perm() != expectation.Mode.Perm() {
		return errors.New("serving-store binding changed before publication: mode mismatch")
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, io.LimitReader(file, 4097)); err != nil {
		return fmt.Errorf("hash current serving-store binding: %w", err)
	}
	if info.Size() > 4096 || !strings.EqualFold(fmt.Sprintf("%x", hash.Sum(nil)), expectation.SHA256) {
		return errors.New("serving-store binding changed before publication: content mismatch")
	}
	return nil
}

func parseLocalServingStoreExpectation(args []string) (localServingStoreExpectation, error) {
	var expectation localServingStoreExpectation
	for index := 0; index < len(args); index++ {
		switch args[index] {
		case "--if-current-absent":
			expectation.Absent = true
		case "--if-current-sha256":
			if index+1 >= len(args) {
				return expectation, errors.New("--if-current-sha256 requires a value")
			}
			index++
			expectation.SHA256 = strings.ToLower(strings.TrimSpace(args[index]))
		case "--if-current-mode":
			if index+1 >= len(args) {
				return expectation, errors.New("--if-current-mode requires a value")
			}
			index++
			mode, err := strconv.ParseUint(strings.TrimSpace(args[index]), 8, 9)
			if err != nil {
				return expectation, errors.New("--if-current-mode must be an octal private-file mode")
			}
			expectation.Mode = os.FileMode(mode)
		default:
			return expectation, fmt.Errorf("unknown bind-state option %q", args[index])
		}
	}
	if expectation.Absent && (expectation.SHA256 != "" || expectation.Mode != 0) {
		return expectation, errors.New("--if-current-absent cannot be combined with SHA or mode expectations")
	}
	if (expectation.SHA256 == "") != (expectation.Mode == 0) {
		return expectation, errors.New("--if-current-sha256 and --if-current-mode must be supplied together")
	}
	if expectation.SHA256 != "" {
		if len(expectation.SHA256) != 64 {
			return expectation, errors.New("--if-current-sha256 must be a 64-character hex digest")
		}
		for _, char := range expectation.SHA256 {
			if !strings.ContainsRune("0123456789abcdef", char) {
				return expectation, errors.New("--if-current-sha256 must be a 64-character hex digest")
			}
		}
		if expectation.Mode.Perm()&0o077 != 0 {
			return expectation, errors.New("--if-current-mode must be private")
		}
	}
	return expectation, nil
}

func unbindLocalServingStore(store accounts.CodexStore, out io.Writer) error {
	if explicitLocalStateAuthority() {
		return errors.New("unbind-state must run with SUBROUTER_STATE_DIR unset so it updates the default CLI metadata")
	}
	path := localServingStoreBindingPath(store)
	lock, err := lockLocalServingStoreBinding(path)
	if err != nil {
		return fmt.Errorf("lock serving-store binding: %w", err)
	}
	defer lock.Close()
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove local serving-store binding: %w", err)
	} else if err == nil {
		if syncErr := syncLocalServingStoreBindingDirectory(filepath.Dir(path)); syncErr != nil {
			return fmt.Errorf("sync serving-store binding directory: %w", syncErr)
		}
	}
	fmt.Fprintln(out, "Local CLI serving-state binding removed")
	return nil
}
