#!/bin/bash
echo "[1] Arrancando Neo-Node..."
nohup ./bin/neo-node > gameday.log 2>&1 &
sleep 5

echo "[2] Desatando FASE 1: Golpes Térmicos y Muerte M-Zero..."
./.agents/gameday/thermodynamic_strike.sh
sleep 10

echo "[3] Reanimación M-Zero y FASE 2: BGP Rogue y Wasm DDoS..."
nohup ./bin/neo-node > gameday.log 2>&1 &
sleep 5
docker-compose -f .agents/gameday/docker-compose-bgp.yaml up -d
docker run --name gameday_hping --rm -d alpine sh -c "apk add --no-cache hping3 && hping3 -S -p 8080 --flood host.docker.internal" >/dev/null

echo "[4] Soportando 10 segundos..."
sleep 10

echo "[5] Deteniendo Asalto y Contenedores..."
docker stop gameday_hping >/dev/null
docker-compose -f .agents/gameday/docker-compose-bgp.yaml down >/dev/null
kill -INT $(pgrep -x neo-node)

echo "--- 📊 TELEMETRÍA DE LA MATRIZ ---"
cat gameday.log
