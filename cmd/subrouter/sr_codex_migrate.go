package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/internal/proxy"
)

const codexIsolationRemediation = "sr codex migrate-isolation"

func isCodexIsolationCommand(args []string) bool {
	return len(args) > 1 && args[0] == "codex" && args[1] == "migrate-isolation"
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
	stored, err := store.ListStored()
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
		return fmt.Errorf("usage: %s", codexIsolationRemediation)
	}
	switch args[0] {
	case "migrate-isolation":
		return r.migrateCodexIsolation(ctx, args[1:])
	default:
		return fmt.Errorf("unknown Codex account command %q; use '%s'", args[0], codexIsolationRemediation)
	}
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
