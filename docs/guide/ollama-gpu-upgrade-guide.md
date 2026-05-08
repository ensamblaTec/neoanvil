# Guía Operacional: Ollama GPU Fix — RTX 3090 / Ubuntu 24.04
# Proyecto: neoanvil | Fecha inicio: 2026-04-25

---

## BRIEFING — Estado actual (2026-04-25)

```
ESTADO:  ✅ COMPLETADO — 2026-04-25
FASE:    GPU activa, todas las verificaciones pasaron

Completado:
  [x] Diagnóstico completo del problema (CPU fallback)
  [x] Fix 3 aplicado: ldconfig /usr/local/lib/ollama (libggml-base.so.0 resuelve)
  [x] Systemd override limpio (sin LD_LIBRARY_PATH, solo perf flags)
  [x] Driver nvidia-driver-580-open instalado via apt
  [x] REBOOT ejecutado (kernel 6.8.0-110-generic, driver 580.126.09)
  [x] Verificaciones post-reboot — todas OK
  [x] Benchmark GPU: 118.2 TPS (objetivo >40, anterior 4.9 — mejora 24×)
```

**Resultado final:** RTX 3090 activa. Ollama usa backend cuda_v13 (CUDA 13.0).
PyTorch GPU: True. Neo-go embed (11435) también en GPU.

---

## Contexto del problema

El usuario reportó que las operaciones de Ollama en Linux eran mucho más lentas
que en Mac (Metal backend). La causa raíz: **el RTX 3090 nunca fue usado en Linux**.
Ollama corría en CPU (4.9 TPS) en vez del RTX 3090 (esperado: 40-80 TPS).
PyTorch también mostraba `GPU: False` con el mismo driver roto.

---

## Sistema

| Campo | Valor |
|-------|-------|
| OS | Ubuntu 24.04.4 LTS (noble) |
| Kernel | 6.8.0-107-generic |
| CPU | Intel i5-10400 |
| GPU | NVIDIA RTX 3090 (24 GB VRAM) |
| Driver anterior | 550.54.14 (roto para CUDA 12.8) |
| Driver instalado | nvidia-driver-580-open 580.126.09 |
| Ollama | 0.17.7 (puerto 11434, usuario ollama) |
| Ollama embed | Nexus-managed, puerto 11435, usuario ensamblatec |
| PyTorch | 2.6.0+cu124 → /home/ensamblatec/ai-envs/torch/ |
| Docker | 29.4.0 + nvidia-container-toolkit 1.19.0 |
| Desktop | XFCE4 sobre Xorg (Ubuntu Server + workspace mode) |
| neoanvil | /home/ensamblatec/go/src/github.com/ensamblatec/neoanvil |

---

## Diagnóstico — Causa Raíz

### Árbol causal

```
RTX 3090 no usada (4.9 TPS — CPU)
└── Ollama cae a CPU fallback silenciosamente
    └── ggml_cuda_init() falla en el runner subprocess
        ├── [RESUELTO fix3] libggml-base.so.0 no estaba en ldconfig
        │     → /usr/local/lib/ollama/ no registrado en /etc/ld.so.conf.d/
        │     → Fix: echo "/usr/local/lib/ollama" > /etc/ld.so.conf.d/ollama.conf
        └── [CAUSA PRINCIPAL — fix driver] Driver 550 soporta CUDA ≤ 12.4
              Ollama 0.17.7 empaqueta CUDA 12.8 (requiere driver ≥ 570)
              → Fix: instalar nvidia-driver-580-open
```

### Tabla de compatibilidad CUDA / Driver

| Componente | Versión CUDA | Driver mínimo |
|------------|-------------|---------------|
| Ollama 0.17.7 cuda_v12 backend | 12.8 | ≥ 570.xx |
| Ollama 0.17.7 cuda_v13 backend | 13.x | ≥ 575.xx |
| PyTorch 2.6.0+cu124 | 12.4 | ≥ 550.xx |
| **Driver anterior** | soporta hasta 12.4 | — |
| **Driver nuevo (580-open)** | soporta 12.8+ | — |

### Cómo Ollama detecta GPU (OLLAMA_NEW_ENGINE=false)

```
ollama serve
└── runner subprocess: /usr/local/bin/ollama runner --ollama-engine --port XXXX
    └── dlopen(/usr/local/lib/ollama/cuda_v12/libggml-cuda.so)
        ├── necesita: libggml-base.so.0 → antes: NOT FOUND → ahora: /usr/local/lib/ollama/
        ├── necesita: libcudart.so.12  → /usr/local/cuda/lib64/ (CUDA 12.4 sistema)
        └── ggml_cuda_init()
            └── FALLA: driver 550 no soporta CUDA 12.8 runtime
                → fallback a CPU sin mensaje de error visible
```

---

## Historial de fixes aplicados

### Fix 1 — LD_LIBRARY_PATH (APLICADO, ROTO)
```ini
# /etc/systemd/system/ollama.service.d/override.conf
Environment="LD_LIBRARY_PATH=/usr/local/lib/ollama:/usr/local/cuda/lib64"
```
Resultado: GPU completamente no detectada (peor). Causa: conflict entre
CUDA 12.4 sistema y CUDA 12.8 bundled de Ollama.

### Fix 2 — LD_LIBRARY_PATH v2 (APLICADO, ROTO)
```ini
Environment="LD_LIBRARY_PATH=/usr/local/lib/ollama/cuda_v12:/usr/local/lib/ollama"
```
Resultado: igual. Discovery subprocess falla en 236ms (vs 500ms+ esperado con GPU).

### Fix 3 — ldconfig (APLICADO, PARCIAL)
```bash
echo "/usr/local/lib/ollama" > /etc/ld.so.conf.d/ollama.conf
ldconfig
```
Resultado: `libggml-base.so.0` ahora resuelve correctamente (`ldconfig -p | grep libggml-base`
lo confirma). Pero GPU sigue en CPU porque driver 550 incompatible con CUDA 12.8.

### Fix Definitivo — Driver upgrade (INSTALADO, PENDIENTE REBOOT)
```bash
sudo apt update && sudo apt install -y nvidia-driver-580-open
# sudo reboot  ← PENDIENTE
```

---

## Estado de archivos del sistema (post fix3)

### /etc/systemd/system/ollama.service.d/override.conf
```ini
[Service]
Environment="OLLAMA_KEEP_ALIVE=-1"
Environment="OLLAMA_FLASH_ATTENTION=1"
Environment="OLLAMA_NUM_PARALLEL=4"
Environment="OLLAMA_MAX_LOADED_MODELS=2"
```

### /etc/ld.so.conf.d/ollama.conf
```
/usr/local/lib/ollama
```

### /etc/docker/daemon.json
```json
{
    "runtimes": {
        "nvidia": {
            "args": [],
            "path": "nvidia-container-runtime"
        }
    }
}
```

---

## Verificaciones Post-Reboot (ejecutar en orden)

### 1. Driver activo
```bash
nvidia-smi | head -15
# OK: Driver Version: 580.xxx | CUDA Version: 12.9
# FAIL: muestra 550.xxx → DKMS no compiló → ver sección troubleshooting
```

### 2. Ollama detecta GPU
```bash
journalctl -u ollama -b | grep "inference compute"
# OK:   library=cuda compute=8.6 name="NVIDIA GeForce RTX 3090" total="24.0 GiB"
# FAIL: library=cpu → ver sección troubleshooting
```

### 3. Benchmark TPS (el test definitivo)
```bash
time curl -s -X POST http://127.0.0.1:11434/api/generate \
  -H "Content-Type: application/json" \
  -d '{"model":"qwen2.5-coder:7b","prompt":"Explica el patrón observer en sistemas distribuidos con ejemplos en Go, ventajas y desventajas de cada enfoque.","stream":false,"options":{"num_predict":100,"temperature":0}}' \
  | python3 -c "import sys,json; d=json.load(sys.stdin); tps=d.get('eval_count',0)/max(d.get('eval_duration',1),1)*1e9; print(f'TPS: {tps:.1f} | Tokens: {d.get(\"eval_count\",0)}')"
# OK:   TPS > 40  (GPU activa)
# FAIL: TPS < 10  (sigue en CPU)
# ANTES del fix: 4.9 TPS
```

### 4. Docker GPU
```bash
sudo systemctl restart docker
docker run --rm --gpus all nvidia/cuda:12.4.0-base-ubuntu22.04 nvidia-smi
# OK: muestra RTX 3090 dentro del container
```

### 5. PyTorch
```bash
/home/ensamblatec/ai-envs/torch/bin/python -c \
  "import torch; print('GPU:', torch.cuda.is_available(), '|', torch.cuda.get_device_name(0) if torch.cuda.is_available() else 'no GPU')"
# OK: GPU: True | NVIDIA GeForce RTX 3090
# ANTES: GPU: False
```

### 6. Ollama embed instance (Nexus puerto 11435)
```bash
# Solo si Nexus está corriendo
curl -s http://127.0.0.1:11435/api/ps | python3 -m json.tool | grep -E "model|size_vram"
# OK: size_vram > 0 (modelo cargado en VRAM)
```

### 7. Neo-go — arrancar Nexus y verificar embeddings con GPU
```bash
cd /home/ensamblatec/go/src/github.com/ensamblatec/neoanvil
make rebuild-restart
sleep 10
curl -s http://127.0.0.1:9000/status | python3 -m json.tool | grep -E "status|uptime"
# OK: status=running para el workspace neoanvil
```

---

## Impacto del upgrade en componentes del sistema

| Componente | Impacto | Acción post-reboot |
|-----------|---------|-------------------|
| XFCE4 / Xorg | Ninguno | Automático (xserver-xorg-video-nvidia-580) |
| Docker 29.4 | Ninguno | `sudo systemctl restart docker` |
| nvidia-container-toolkit 1.19.0 | Compatible | Ninguna |
| PyTorch 2.6.0+cu124 | Mejora esperada | Ninguna |
| DKMS kernel module | Auto-rebuild | Automático en boot |
| Ollama 0.17.7 | Fix principal | Ninguna — auto-detecta GPU |
| Neo-go / Nexus | Embeddings más rápidos | `make rebuild-restart` |

---

## Servicios involucrados

| Servicio | Puerto | Usuario | Archivo de config |
|---------|--------|---------|-------------------|
| Ollama system | 11434 | ollama | /etc/systemd/system/ollama.service |
| Ollama override | — | — | /etc/systemd/system/ollama.service.d/override.conf |
| Ollama embed (Nexus) | 11435 | ensamblatec | ~/.neo/nexus.yaml → services.ollama_embed |
| Docker nvidia runtime | — | root | /etc/docker/daemon.json |
| Neo-go Nexus | 9000 | ensamblatec | ~/.neo/nexus.yaml |
| Neo-go workspace | 9142 | ensamblatec | neo.yaml (port determinístico) |

---

## Troubleshooting post-reboot

### Ollama sigue en CPU tras reboot
```bash
# 1. Confirmar driver nuevo activo
cat /proc/driver/nvidia/version
# debe mostrar 580.xxx

# 2. Confirmar ldconfig intacto
ldconfig -p | grep libggml-base
# debe mostrar: libggml-base.so.0 => /usr/local/lib/ollama/libggml-base.so.0

# 3. Ver discovery log completo
journalctl -u ollama -b --no-pager | head -50

# 4. Restart Ollama y re-verificar
sudo systemctl restart ollama && sleep 3
journalctl -u ollama -b | grep "inference compute"
```

### Xorg / XFCE no arranca tras reboot
```bash
# Intentar reinstalar el driver
sudo apt install --reinstall nvidia-driver-580-open
sudo dpkg-reconfigure nvidia-driver-580-open
sudo reboot
```

### Docker GPU no funciona
```bash
sudo nvidia-ctk runtime configure --runtime=docker
sudo systemctl restart docker
# Verificar:
docker run --rm --gpus all nvidia/cuda:12.4.0-base-ubuntu22.04 nvidia-smi
```

### PyTorch sigue con GPU: False
```bash
# El problema podría ser el PATH de libcuda.so.1
/home/ensamblatec/ai-envs/torch/bin/python -c \
  "import ctypes; lib = ctypes.CDLL('libcuda.so.1'); print('libcuda OK')"
# Si falla: ldconfig -p | grep libcuda
```

### DKMS no compiló el módulo para el kernel
```bash
dkms status
# Si muestra error para nvidia/580:
sudo dkms build -m nvidia -v 580.126.09 -k $(uname -r)
sudo dkms install -m nvidia -v 580.126.09 -k $(uname -r)
sudo reboot
```

---

## Resultado esperado final

```
nvidia-smi:           Driver 580.xxx activo, RTX 3090 visible
Ollama inference:     library=cuda, total=24.0 GiB VRAM
Benchmark TPS:        >40 TPS (vs 4.9 TPS CPU anterior) — mejora ~10x
PyTorch:              GPU: True | NVIDIA GeForce RTX 3090
Docker:               RTX 3090 accesible dentro de containers
Neo-go embeddings:    Más rápidos (Ollama embed en GPU en puerto 11435)
```
