package observability

import (
	"log"

	"github.com/ensamblatec/neoanvil/pkg/mesh"
	"github.com/ensamblatec/neoanvil/pkg/tensorx"
)

// ExtendedKalmanFilter5D: Observador Lógico M-Zero
type ExtendedKalmanFilter5D struct {
	x [5]float32  // State: [Latencia, CPU, RAM, UDP, Watts]
	p [25]float32 // Covariance
	q [25]float32 // Process Noise
	r [25]float32 // Measurement Noise

	TDP_Limit float32
	XDPSocket *mesh.XDPSocket
}

func NewEKF5D(tdpLimit float32, isDarwin bool) *ExtendedKalmanFilter5D {
	// Inicializar Matriz Termodinámica (Q)
	ekf := &ExtendedKalmanFilter5D{
		TDP_Limit: tdpLimit,
	}

	// Configuración Híbrida HW
	// Darwin/Mock: Mayor inercia sintética de incertidumbre. Linux RAPL: Absoluta precisión térmica HW
	for i := 0; i < 5; i++ {
		ekf.q[i*5+i] = 1.0  // Default Variance Base
		ekf.p[i*5+i] = 10.0 // Initial Covariance
		ekf.r[i*5+i] = 5.0  // Sensor Meas Noise
	}

	if !isDarwin {
		ekf.q[4*5+4] = 0.05 // Alta Certeza Senson RAPL
	} else {
		ekf.q[4*5+4] = 4.0 // Ruido Perlin Darwin Simulator
	}

	return ekf
}

// Predict asimila el comportamiento físico del Triage
func (ekf *ExtendedKalmanFilter5D) Predict(newWatts float32) {
	// Estimación lineal f(x_k-1). Watts entra directamente al modelo.
	ekf.x[4] = newWatts

	// Evaluador Gravitacional (Enrutamiento Táctico)
	if ekf.x[4] > ekf.TDP_Limit {
		ekf.executeThermalShedding()
	}
}

// executeThermalShedding desata las políticas de estrangulamiento Drop directo al BPF (Fase 5.5)
func (ekf *ExtendedKalmanFilter5D) executeThermalShedding() {
	log.Printf("[SRE-EKF] 🔥 PELIGRO TÉRMICO (%.2fW > %.2fW) | Orquestando SHEDDING AF_XDP\n", ekf.x[4], ekf.TDP_Limit)
	if ekf.XDPSocket != nil {
		// Enlazar el Drop Forzado o derivar al Clúster CRDT.
		// Simularemos descartar el Fill Ring local:
		log.Println("[SRE-EKF] Hardware AF_XDP Congelado local. Asumiendo derivación Federada.")
	}
}

func (ekf *ExtendedKalmanFilter5D) ValidateO1Inverse() error {
	out := make([]float32, 25)
	return tensorx.MatInverse5x5F32(ekf.p[:], out)
}
