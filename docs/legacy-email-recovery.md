# Legacy release-candidate local-email recovery

This operator utility exists only for older release-candidate databases that
predate email-first local identity. Final `0.1.0` does not upgrade those schema
histories in place; use this recovery only before an operator-reviewed export
or rollback. Fresh installations and invitations are email-first.

For a pre-017 installation whose local platform administrator has no `users.email`
value, use the `kuberploy-admin-recover` utility during a maintenance window.
It requires direct PostgreSQL access, the exact administrator UUID, and new
operator-supplied email/password files. It refuses non-administrators, service
accounts, missing credentials, different existing emails, and different
credential identities.

1. Back up PostgreSQL and stop API/worker writers for the maintenance window.
2. Obtain the exact administrator UUID from a trusted, access-controlled SQL
   query. Never use display name as the selector.
3. Create two mode-0600 files outside the repository: one containing the new
   email and one containing the new password. Do not put either value in shell
   arguments, logs, Kubernetes manifests, or this document.
4. Run the utility beside the database connection:

```sh
go build -o /secure/operator/bin/kuberploy-admin-recover ./cmd/kuberploy-admin-recover

KUBERPLOY_DATABASE_URL='postgresql://<operator-supplied-connection>' \
KUBERPLOY_RECOVERY_USER_ID='<exact-admin-uuid>' \
KUBERPLOY_RECOVERY_EMAIL_FILE='/absolute/path/admin-email' \
KUBERPLOY_RECOVERY_PASSWORD_FILE='/absolute/path/admin-password' \
KUBERPLOY_RECOVERY_CONFIRM='recover-email:<exact-admin-uuid>' \
/secure/operator/bin/kuberploy-admin-recover
```

The command atomically sets the normalized email and new Argon2id password,
revokes all existing sessions, and records a metadata-only audit event. It does
not print either secret. Repeating it with the same exact user ID and recovered
email safely replaces the password and revokes sessions again; this supports a
recovery retry after an interrupted operator handoff. Start API/worker again,
sign in with the email, and remove temporary files using the operator's secure
disposal procedure.

Do not use a different email to reset an existing user. Do not run the first
pass against a fresh installation. If the exact legacy administrator cannot be
identified safely, restore from backup or provision a fresh database; do not
guess an identity.
