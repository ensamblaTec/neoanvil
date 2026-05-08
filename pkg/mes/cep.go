package mes

import (
	"bytes"
	"context"
	"log"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ensamblatec/neoanvil/pkg/sre"
)

// CEPRule define la condición táctica y el vector de ataque al PLC.
type CEPRule struct {
	TriggerState int
	PLCEndpoint  string
	Payload      []byte
}

// CEPEngine evalúa telemetría a 15k RPS sin bloqueos.
type CEPEngine struct {
	rules      atomic.Pointer[map[string]CEPRule]
	lastAction sync.Map // map[string]int64 (MachineID -> Timestamp UnixNano)
	httpClient *http.Client
	cooldown   int64 // Nanosegundos
	limit      chan struct{}
	_          [64]byte
}

// NewCEPEngine inicializa el motor con un cooldown estricto (ej: 5 segundos).
func NewCEPEngine(cooldown time.Duration) *CEPEngine {
	engine := &CEPEngine{
		cooldown: cooldown.Nanoseconds(),
		httpClient: sre.SafeHTTPClient(),
		limit: make(chan struct{}, 50),
	}

	// Inicializar matriz vacía para evitar nil pointers
	emptyRules := make(map[string]CEPRule)
	engine.rules.Store(&emptyRules)

	return engine
}

// UpdateRules permite inyectar nuevas directivas en caliente sin frenar el Read-Path (Patrón RCU).
func (cep *CEPEngine) UpdateRules(newRules map[string]CEPRule) {
	cep.rules.Store(&newRules)
	log.Printf("[SRE-CEP] Matriz de reglas actualizada. %d directivas activas.", len(newRules))
}

// Evaluate intercepta el evento antes del Dispatcher. Retorna true si ejecutó una acción crítica.
func (cep *CEPEngine) Evaluate(ctx context.Context, ev TelemetryEvent) {
	// 1. LECTURA LOCK-FREE: Extraemos el mapa atómico
	rulesMap := *cep.rules.Load()

	rule, exists := rulesMap[ev.MachineID]
	if !exists || ev.State != rule.TriggerState {
		return // No hay regla o el estado es normal
	}

	// 2. DEFENSA ANTI-STORM (Debounce SRE)
	now := time.Now().UnixNano()
	if lastFired, ok := cep.lastAction.Load(ev.MachineID); ok {
		if now-lastFired.(int64) < cep.cooldown {
			return // En periodo de gracia (Cooldown). Ignoramos el ruido.
		}
	}

	// 3. ACTUALIZACIÓN DE ESTADO (Fire-and-Forget optimista)
	cep.lastAction.Store(ev.MachineID, now)

	// 4. EJECUCIÓN DEL ATAQUE TÁCTICO (Asíncrono asintóticamente contenido)
	select {
	case cep.limit <- struct{}{}:
		go func() {
			defer func() { <-cep.limit }()
			cep.executeAction(ctx, ev.MachineID, rule)
		}()
	default:
		log.Printf("[SRE-CEP] Load Shedding: Límite de %d workers industriales excedido.", cap(cep.limit))
	}
}

func (cep *CEPEngine) executeAction(ctx context.Context, machineID string, rule CEPRule) {
	log.Printf("[SRE-CEP] REGLA DETONADA: Ejecutando E-Stop para máquina %s", machineID)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rule.PLCEndpoint, bytes.NewReader(rule.Payload))
	if err != nil {
		log.Printf("[SRE-ERROR] Fallo construyendo request de acción: %v", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := cep.httpClient.Do(req)
	if err != nil {
		log.Printf("[SRE-CRIT] El PLC %s no respondió al comando de emergencia: %v", machineID, err)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		log.Printf("[SRE-CRIT] El PLC %s rechazó el comando de emergencia (Status %d)", machineID, resp.StatusCode)
		return
	}

	log.Printf("[SRE-CEP] Comando ejecutado con éxito en máquina %s", machineID)
}
