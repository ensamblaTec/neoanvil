# Crypto domain checklist

Append this to the audit prompt when files involve cryptographic primitives
(blake2b, AEAD, KDFs, TLS, signing, encryption-at-rest, key handling).

## Invariants to verify

- **Nonce uniqueness**: nonces must NEVER repeat for a given key across
  process restarts, stream restarts, or counter rollovers. Verify nonce
  derivation (random vs counter) and that counter persists durably.
- **Key derivation**: salt must be per-context, not constant. KDF output
  must be domain-separated (e.g. HKDF info field).
- **AEAD vs MAC-then-encrypt**: AEAD constructions (ChaCha20-Poly1305,
  AES-GCM) prevent malleability. Reject custom MAC-then-encrypt.
- **Timing channels**: MAC verification, password compare, signature
  compare must use crypto/subtle.ConstantTimeCompare. byte-by-byte == leaks.
- **Algorithm negotiation**: reject downgrade. If multiple algorithms
  supported, the protocol must commit to the chosen one (e.g. include
  in transcript hash).
- **Key zeroization**: secret material in memory should be zeroed on
  Close()/free. Pass-by-value of secret strings/slices leaves copies in
  Go heap that escape escape analysis.
- **Random source**: use crypto/rand, NOT math/rand. Verify any custom
  RNG uses entropy properly (e.g. seeded once at init from rand).
- **Side-channel resistance**: branch-free comparison, fixed-time table
  lookups for AES, no key-dependent memory access patterns.

## Severity floor

For findings in this domain, use severity ≥ 6. Cryptographic mistakes
are rarely "low impact" — even a 100-bit weakness in a 256-bit cipher
is significant.

## Common compose-2-true-into-false-conclusion patterns

Watch for traces that combine two correct premises to reach a wrong
conclusion (DS hallucination pattern):

- "AEAD nonce reuse" + "salt is constant" → claim of "key compromise"
  (false: AEAD with proper salt rotation handles nonce reuse at the
  Poly1305 tag level via tag-on-nonce verification).
- "TLS 1.3 + custom record format" → claim of "downgrade vulnerability"
  (verify the record format actually breaks 1.3 invariants before claiming).
