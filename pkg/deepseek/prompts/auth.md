# Auth domain checklist

Append this to the audit prompt when files involve authentication, ACL
enforcement, credential storage, or workspace authorization.

## Invariants to verify

- **Source of identity**: caller identity must come from the request
  TRANSPORT (TLS client cert, signed JWT, OAuth bearer), not from the
  request BODY. A self-asserted `target_workspace` field in JSON-RPC
  arguments is attacker-controlled.
- **ACL fail-closed**: when allowlist is empty/missing, default to
  DENY, not ALLOW. Same for capability checks — absent grant ≠ "all
  permitted".
- **Credential at rest**: tokens stored on disk must be 0600 perms,
  parent dir 0700. Verify `os.Chmod` happens BEFORE the file gets
  populated (chmod-after writes a vulnerable window).
- **Audit trail**: every Load/Save of credentials must append to a
  hash-chained log so tampering is detectable. Just timestamps + hashes,
  not the secret values.
- **Provider lookup**: case-insensitive normalization on input before
  exact match — otherwise "DeepSeek" vs "deepseek" creates shadow
  accounts.
- **Symlink hardening on creds path**: keystore.Save must reject if
  the target is a symlink (`O_NOFOLLOW` or `Lstat`).
- **Information disclosure on error**: ACL-deny error message MUST NOT
  enumerate valid identifiers. "Permission denied" not "user X not in
  workspace Y allowlist [a, b, c]".
- **Concurrent Save**: file lock + atomic rename. Two concurrent Saves
  must not interleave bytes.

## Severity floor

For findings in this domain, severity ≥ 7 unless the threat model
already requires local shell access (then SEV 5-6 for defense-in-depth).

## Common patterns

- ACL bypass via body field: see ÉPICA 143.A (DS rediscovered as
  F-fol-2 in 152 audit). Paths: workspaceID extracted from JSON args
  fallback when header missing.
- Permission disclosure via error reason: helpful for ops, gold for
  attackers enumerating allowed identifiers.
