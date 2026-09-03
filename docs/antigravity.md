# Native Antigravity (AGY) profiles

Subrouter supports native AGY OAuth profiles on macOS without rewriting AGY's
endpoint. The vendor keeps one global Keychain item (`service=gemini`), so
Subrouter switches that item only for a launched process, verifies the selected
credential, holds a cross-process lock, and restores the previous opaque value
when the process exits.

## Add and launch profiles

1. Sign plain `agy` into one Google account.
2. Run `sr server use local`, then `sr agy add <label>`.
3. Sign plain `agy` into the next account and repeat with a different label.
4. Run `sr status` and confirm each verified email appears as its own profile.
5. Run `sr agy` for deterministic pooled selection, or
   `sr agy --account <label-or-email>` to pin a process.

Native profile selection is between launches. An existing AGY process is never
migrated to another identity, and native launches are serialized because the
vendor slot is global. Server-side `/antigravity` OAuth routing is the option
for parallel account-pooled requests.

## Recovery and acceptance evidence

If the host or process is hard-killed during a native launch, run:

```sh
sr server use local
sr agy recover
```

The recovery command replays the durable swap journal and restores the prior
Keychain slot. Do not delete the journal manually.

For an opt-in Darwin canary, use two already-authorized accounts and record
credential-free evidence for each launch: selected email after startup, one
minimal model request, refresh behavior, clean exit, and `sr status` afterward.
Only after both sequential launches pass may a concurrent test be attempted;
do not claim concurrent native pooling unless both processes retain their
identity through startup, request, refresh, and exit. A failed concurrent test
must leave native mode serialized and server-side pooling unchanged.

Plain `agy` remains direct and is outside Subrouter's managed profile lock.
