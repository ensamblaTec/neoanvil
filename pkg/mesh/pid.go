package mesh

import (
	"sync/atomic"
)

// PIDController implements a Zero-Allocation, thread-safe PID loop for traffic and admission control.
type PIDController struct {
	kp, ki, kd float64
	integral   float64
	lastError  float64
	setpoint   float64

	// valve represents the traffic admission ratio [0.0, 1.0].
	// Using atomic.Value to strictly avoid locks (RWMutex) on the hot-path (Allow()).
	valve atomic.Value
}

// NewPID initializes a new PIDController with tuning parameters.
func NewPID(kp, ki, kd, setpoint float64) *PIDController {
	p := &PIDController{
		kp:       kp,
		ki:       ki,
		kd:       kd,
		setpoint: setpoint,
	}
	// Default to 100% traffic flow.
	p.valve.Store(1.0)
	return p
}

// Update calculates the new valve state based on current latency error.
// Expected to be called by a periodic gontroutine, not on every request.
func (p *PIDController) Update(currentLatency float64) {
	err := p.setpoint - currentLatency
	p.integral += err
	derivative := err - p.lastError

	out := (p.kp * err) + (p.ki * p.integral) + (p.kd * derivative)
	p.lastError = err

	if out > 1.0 {
		out = 1.0
	} else if out < 0.0 {
		out = 0.0
	}

	p.valve.Store(out)
}

// Allow returns the current traffic admission ratio dynamically.
func (p *PIDController) Allow() float64 {
	v := p.valve.Load()
	if v == nil {
		return 1.0
	}
	return v.(float64)
}
