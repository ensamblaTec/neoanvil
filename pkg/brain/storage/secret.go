package storage

// Secret is a string type whose Stringer returns "[REDACTED]" to prevent
// accidental credential disclosure via fmt/panic/log output.
//
// [144.D] Using a plain `string` for API secrets means the value appears
// verbatim in Go panic stack traces (via fmt.Sprintf %v/default formatting)
// and in any log statement that accidentally includes the value. Secret's
// String() method intercepts all such paths and replaces the value with a
// static marker. Use Reveal() only inside the signing closure where the
// secret is consumed immediately and never leaves that scope.
type Secret string

// String implements fmt.Stringer — always returns "[REDACTED]".
// This intercepts fmt.Sprintf("%v", s), log.Printf, panic output,
// and any other path that invokes the default string representation.
func (s Secret) String() string { return "[REDACTED]" }

// Reveal returns the plaintext value. Call only where the secret is
// consumed immediately (e.g. inside an AWS credentials provider closure)
// and never assign the result to a variable that outlives the call site.
func (s Secret) Reveal() string { return string(s) }
