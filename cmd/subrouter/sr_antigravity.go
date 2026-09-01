package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	baseaccount "github.com/manaflow-ai/subrouter/account"
	agentantigravity "github.com/manaflow-ai/subrouter/internal/agents/antigravity"
	"github.com/manaflow-ai/subrouter/internal/proxy"
)

const antigravityManagementHelp = `Usage: sr agy add <label>
       sr agy list
       sr agy remove <label>

Add imports the current plain 'agy' OAuth login into an isolated Subrouter profile.
Repeat after signing plain 'agy' into each account. The Keychain item is never changed.
Status can report each imported identity, plan, and model-family quota. The current
agy CLI has no transparent proxy hook, so routed pooling and pinning are unavailable.
Use plain 'agy' for direct OAuth access.
`

func (r srRunner) antigravityManage(ctx context.Context, args []string) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		fmt.Fprint(r.out, antigravityManagementHelp)
		return nil
	}
	store := &agentantigravity.Store{}
	switch args[0] {
	case "add", "import":
		if len(args) != 2 {
			return fmt.Errorf("usage: sr agy add <label>")
		}
		credential, ok, err := agentantigravity.ReadLocalCredential(ctx, time.Now())
		if err != nil {
			return fmt.Errorf("read current agy login: %w", err)
		}
		if !ok {
			return fmt.Errorf("plain agy is not logged in; sign in with 'agy', then rerun 'sr agy add %s'", strings.TrimSpace(args[1]))
		}
		grant := credential
		credential, err = agentantigravity.PrepareManagedCredential(ctx, r.client, credential, time.Now())
		if err != nil {
			return fmt.Errorf("validate current agy login before import: %w", err)
		}
		var acct baseaccount.Account
		err = proxy.PublishAccountDiskMutation(ctx, r.store.StoreDir(), func() (bool, error) {
			var saveErr error
			acct, saveErr = store.SaveManagedCredentialFromGrant(args[1], credential, grant)
			return saveErr == nil, saveErr
		})
		if err != nil {
			return fmt.Errorf("publish managed Antigravity credential: %w", err)
		}
		fmt.Fprintf(r.out, "Added isolated Antigravity account: %s. Run: sr status\n", acct.ID)
		return nil
	case "list", "ls":
		accounts, err := store.ForServing().ListAccounts(ctx)
		if err != nil {
			return err
		}
		if len(accounts) == 0 {
			fmt.Fprintln(r.out, "No isolated Antigravity accounts configured.")
			return nil
		}
		for _, acct := range accounts {
			fmt.Fprintf(r.out, "%s (%s)\n", acct.Label, acct.ID)
		}
		return nil
	case "remove", "rm":
		if len(args) != 2 {
			return fmt.Errorf("usage: sr agy remove <label>")
		}
		var removed bool
		err := proxy.PublishAccountDiskMutation(ctx, r.store.StoreDir(), func() (bool, error) {
			_, ok, removeErr := store.RemoveManagedAccount(args[1])
			removed = ok
			return ok, removeErr
		})
		if err != nil {
			return err
		}
		if !removed {
			return fmt.Errorf("no managed Antigravity account found matching %q", args[1])
		}
		fmt.Fprintf(r.out, "Removed Antigravity account %q.\n", strings.TrimSpace(args[1]))
		return nil
	default:
		return fmt.Errorf("unknown Antigravity command %q; use add, list, or remove", args[0])
	}
}

func (r srRunner) antigravityRemote(ctx context.Context, server srServerConfig, args []string) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		fmt.Fprint(r.out, antigravityManagementHelp)
		fmt.Fprintf(r.out, "Managed profiles are stored on server %s.\n", server.Name)
		return nil
	}
	switch args[0] {
	case "add", "import":
		if len(args) != 2 {
			return fmt.Errorf("usage: sr agy add <label>")
		}
		credential, ok, err := agentantigravity.ReadLocalCredential(ctx, time.Now())
		if err != nil {
			return fmt.Errorf("read current agy login: %w", err)
		}
		if !ok {
			return fmt.Errorf("plain agy is not logged in; sign in with 'agy', then rerun 'sr agy add %s'", strings.TrimSpace(args[1]))
		}
		grant := credential
		credential, err = agentantigravity.PrepareManagedCredential(ctx, r.client, credential, time.Now())
		if err != nil {
			return fmt.Errorf("validate current agy login before import: %w", err)
		}
		credential = antigravityRemoteImportCredential(credential, grant)
		if err := r.uploadServerAntigravityAccount(ctx, server, args[1], credential); err != nil {
			return err
		}
		fmt.Fprintf(r.out, "Added isolated Antigravity account %q to server %s. Run: sr status\n", strings.TrimSpace(args[1]), server.Name)
		return nil
	case "list", "ls":
		all, err := r.fetchServerAccounts(ctx, server)
		if err != nil {
			return err
		}
		found := false
		for _, acct := range all {
			if acct.Provider == baseaccount.ProviderAntigravity && acct.AuthMode == baseaccount.AuthModeOAuth &&
				agentantigravity.IsManagedAccountID(acct.ID) {
				found = true
				fmt.Fprintf(r.out, "%s (%s)\n", acct.Label, acct.ID)
			}
		}
		if !found {
			fmt.Fprintln(r.out, "No isolated Antigravity accounts configured on the selected server.")
		}
		return nil
	case "remove", "rm":
		if len(args) != 2 {
			return fmt.Errorf("usage: sr agy remove <label>")
		}
		if err := r.removeServerAntigravityAccount(ctx, server, args[1]); err != nil {
			return err
		}
		fmt.Fprintf(r.out, "Removed Antigravity account %q from server %s.\n", strings.TrimSpace(args[1]), server.Name)
		return nil
	default:
		return fmt.Errorf("unknown Antigravity command %q; use add, list, or remove", args[0])
	}
}

func antigravityRemoteImportCredential(prepared, original agentantigravity.CredentialInfo) agentantigravity.CredentialInfo {
	// Preserve the submitted grant for server-side duplicate detection while
	// carrying the OAuth client binding discovered during local validation. The
	// server independently refresh-attests this original grant before storage.
	prepared.RefreshToken = original.RefreshToken
	return prepared
}
