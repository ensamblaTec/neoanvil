# Concurrency domain checklist

Append this to the audit prompt when files involve goroutines, mutexes,
channels, sync.Map, atomic operations, or distributed state machines.

## Invariants to verify

- **Mutex coverage**: every shared field must be protected by exactly
  one mutex (or be atomic). `proc.Status` written under pp.mu.Lock()
  and read under pp.mu.RLock() — verify no read paths bypass the lock.
- **Lock ordering**: when N mutexes exist, fixed acquisition order
  prevents deadlocks. Document and enforce: e.g. always pp.mu before
  proc.cmd.Wait, never the reverse.
- **Atomic vs mutex**: atomic.Int64 is for counter-style fields. For
  multi-field consistency (e.g. status + pid + last_ping together)
  use mutex. Mixing atomic + mutex on same struct is usually a bug.
- **Goroutine lifecycle**: every `go fn()` must have a documented exit
  path (ctx.Done, channel close, condition). Goroutines that wait
  forever leak.
- **WaitGroup before delete**: when one goroutine spawns N work
  goroutines and the parent map removes the work-record entry, use
  `sync.WaitGroup` to ensure work goroutines complete before deletion
  to prevent nil-pointer use-after-remove.
- **Channel direction**: prefer send-only `chan<- T` and receive-only
  `<-chan T` in function signatures. Bidirectional channels in APIs
  are red flags.
- **State machine atomicity**: transitions should use CAS where possible.
  "if status == X { status = Y }" without a lock is a TOCTOU race.
- **singleflight key lifecycle**: standard library singleflight runs
  fn ONCE for concurrent callers but a NEW call after fn returns
  triggers fresh fn. Don't expect persistent caching from singleflight
  alone.

## Severity floor

For findings in this domain, severity ≥ 6. Concurrency bugs are often
race-condition timing-dependent — they manifest under load, not in
single-threaded testing.

## Common compose-2-true-into-false-conclusion patterns

- "Mutex held across long operation" + "single mutex" → claim of
  "no deadlock possible" (false: still blocks contended readers,
  causing latency cliffs that look like deadlocks under load).
- "atomic.Int64 read" + "concurrent atomic writes" → claim of "safe"
  (true for the single field; false if you need consistency between
  multiple atomic fields read in succession).
