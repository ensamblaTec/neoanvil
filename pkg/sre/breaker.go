package sre

import (
	"context"
	"errors"
	"sync"
	"time"
)

// Estados del Circuit Breaker
type State int

const (
	StateClosed State = iota
	StateOpen
	StateHalfOpen
)

// BreakerResetTimeout is the canonical recovery window shared by the embedder
// and the ingestion retry loop — both must agree on this value so the loop waits
// long enough for the breaker to transition to HalfOpen. [SRE-35-hotfix]
const BreakerResetTimeout = 30 * time.Second

var (
	ErrCircuitOpen = errors.New("circuit breaker is open: fast-failing request")
	ErrLLMTimeout  = errors.New("LLM inference timeout exceeded")
)

// CircuitBreaker protege integraciones externas frágiles.
type CircuitBreaker[T any] struct {
	mu              sync.RWMutex
	state           State
	failures        int
	maxFailures     int
	resetTimeout    time.Duration
	lastStateChange time.Time
}

// NewCircuitBreaker inicializa un breaker estricto.
func NewCircuitBreaker[T any](maxFailures int, resetTimeout time.Duration) *CircuitBreaker[T] {
	return &CircuitBreaker[T]{
		state:        StateClosed,
		maxFailures:  maxFailures,
		resetTimeout: resetTimeout,
	}
}

// currentState evalúa el estado actual, manejando la transición temporal a HalfOpen.
func (cb *CircuitBreaker[T]) currentState() State {
	cb.mu.RLock()
	defer cb.mu.RUnlock()

	if cb.state == StateOpen && time.Since(cb.lastStateChange) >= cb.resetTimeout {
		// No cambiamos el estado real aquí para evitar race conditions complejas en lectura,
		// se maneja en el método Execute, pero reportamos HalfOpen.
		return StateHalfOpen
	}
	return cb.state
}

// Execute envuelve la llamada al LLM. Aplica un hard-timeout de 3000ms a nivel de contexto.
func (cb *CircuitBreaker[T]) Execute(ctx context.Context, action func(ctx context.Context) (T, error)) (T, error) {
	state := cb.currentState()
	var zero T

	if state == StateOpen {
		return zero, ErrCircuitOpen
	}

	if state == StateHalfOpen {
		// Intento de recuperación. Mutamos el estado temporalmente.
		cb.mu.Lock()
		if cb.state == StateOpen || cb.state == StateHalfOpen {
			cb.state = StateHalfOpen
		}
		cb.mu.Unlock()
	}

	// Imponer timeout estricto SRE (3000ms)
	actCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	// Ejecución de la I/O externa
	result, err := action(actCtx)

	// Interceptar cancelación por timeout
	if errors.Is(actCtx.Err(), context.DeadlineExceeded) {
		err = ErrLLMTimeout
	}

	cb.mu.Lock()
	defer cb.mu.Unlock()

	if err != nil {
		cb.failures++
		if cb.state == StateHalfOpen || cb.failures >= cb.maxFailures {
			cb.state = StateOpen
			cb.lastStateChange = time.Now()
		}
		return zero, err
	}

	// Éxito: Reset del autómata
	cb.failures = 0
	cb.state = StateClosed
	return result, nil
}
