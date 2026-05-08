#!/bin/bash

# ==============================================================================
# NeoAnvil SRE RL Training Gym
# ==============================================================================
# Script para someter al Agente a un ciclo darwinista de autómata celular 
# ==============================================================================

echo "⚙️  [RL GYM] Compilando artillería (Sandbox y Stress)..."
go build -o bin/sandbox cmd/sandbox/main.go
go build -o bin/stress cmd/stress/main.go

if [ $? -ne 0 ]; then
    echo "❌ Error compilando el entorno. Abortando..."
    exit 1
fi

echo "🟢 [RL GYM] Desplegando Sandbox Destructible en segundo plano..."
./bin/sandbox &
SANDBOX_PID=$!

trap "echo '🛑 [RL GYM] Cerrando Sandbox...'; kill -9 $SANDBOX_PID" EXIT

echo ""
echo "====================================================================="
echo "                  ENTRENAMIENTO SRE ACTIVADO"
echo "====================================================================="
echo ""
echo "COPIA Y PEGA EL SIGUIENTE PROMPT AL AGENTE EN TU VENTANA DE CHAT MCP:"
echo "---------------------------------------------------------------------"
echo "/fluido-sre-universal ¡Iniciamos el ciclo RL contra tu propio Sandbox!"
echo ""
echo "Misión:"
echo "1. Analiza el archivo 'cmd/sandbox/main.go'."
echo "2. Detecta el Mutex Ineficiente y el Memory Leak global."
echo "3. Usa 'neo_apply_patch' para refactorizar la asincronía."
echo "4. Ejecuta 'neo_start_gameday' pasando 'target_url': 'http://localhost:8080/sandbox', 'concurrency': 300, y 'requests_per_worker': 500."
echo "5. Evalúa la respuesta de los Logs de estrés."
echo "6. Si el throughput mejoró, usa 'neo_rem_sleep'. Si empeoró, usa 'neo_git_rollback'."
echo "---------------------------------------------------------------------"
echo ""
echo "⚠️  El Sandbox (PID: $SANDBOX_PID) está escuchando en el puerto 8080..."
echo "Presiona CTRL+C para detener el Gimnasio y apagar el entorno local."

# Bucle de retención
while true; do
  sleep 10
done
