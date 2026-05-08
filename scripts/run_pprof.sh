#!/bin/bash
# ==========================================
# SRE Ouroboros PPROF Diagnostics
# ==========================================
echo "🛡️ [SRE] Iniciando Telemetría PPROF sobre el Orquestador..."
echo "Asegúrate de haber corrido neo_start_gameday SRE Tool para acumular entropía termodinámica."

read -p "Presiona ENTER para capturar el Flamegraph (Allocations) en 127.0.0.1:8085..."
go tool pprof -http=:8085 http://127.0.0.1:6060/debug/pprof/allocs
