# Network Security & Remote Access

Posture de red de NeoAnvil/Nexus: **loopback-only por defecto**, acceso remoto vía **túnel SSH**.
Relacionado: [ADR-010 Security Decisions](../adr/ADR-010-security-decisions-pilar-xxviii.md) ·
[Federation Guide](./neo-project-federation-guide.md) · directiva `[LEY-SEGURIDAD]`.

> Origen: hardening `42d0533` (2026-06-03) — `fix(security): bind SRE listener + Host default to loopback`.

---

## 1. Resumen

Toda la superficie de red de NeoAnvil queda atada a `127.0.0.1`. Nada escucha en la LAN.
El acceso desde otra máquina se hace tunelizando el dispatcher de Nexus por SSH; la identidad
es la llave SSH y el tráfico va cifrado. No se expone ningún puerto a la red ni a internet.

| Capa | Mecanismo | Garantía |
|---|---|---|
| Bind | Todos los listeners en `127.0.0.1` | No alcanzable desde la LAN |
| Transporte remoto | Túnel SSH (`ssh -L`) | Cifrado + identidad por llave |
| API de gestión | `X-Nexus-Token` en `/api/v1/*` | Defensa en profundidad |
| Firewall | macOS Application Firewall (opcional) | Belt-and-suspenders |

---

## 2. El problema que se corrigió

Antes del hardening, los procesos hijo `neo-mcp` mostraban sockets en **wildcard**
(`*:9453`, `*:9413`, `*:9484` — IPv6 `[::]`, todas las interfaces). Con el firewall de
macOS desactivado, eso los hacía alcanzables desde cualquier equipo de la red en
`http://<ip-lan>:<puerto>`.

El socket expuesto era el **SRE incident listener**:

- `cmd/neo-mcp/main.go` — se construía con `fmt.Sprintf(":%d", cfg.Server.SREListenerPort)`
  (host vacío → wildcard).
- `pkg/integrations/listener.go` — `net.Listen("tcp", l.addr)` aceptaba payloads TCP crudos
  que **encolan tareas en BoltDB** (`state.EnqueueTasks`). Un host de la LAN podía inyectar
  "incidentes" arbitrarios al orquestador.

Causa secundaria: `applyServerDefaults` en `pkg/config/config.go` ponía `Server.Host = "0.0.0.0"`
cuando el campo venía vacío — un default inseguro que, ante un `neo.yaml` sin `host`, bindeaba
todas las interfaces de forma silenciosa.

> El SSE worker **nunca** estuvo expuesto: Nexus le pasa al hijo el fd del listener vía
> `reservePort(cfg.Nexus.BindAddr, port)` (`pkg/nexus/process_pool.go`), y `BindAddr` es
> `127.0.0.1`. Sólo el SRE listener filtraba.

---

## 3. La corrección

1. **SRE listener → loopback fijo.** Se hardcodea `127.0.0.1:<port>` (igual que el servidor de
   diagnostics). El listener de incidentes es infra interna; permanece en loopback aun si un
   operador abre `Server.Host` para federación.

   ```go
   // cmd/neo-mcp/main.go
   listnr := integrations.NewSREListener(
       fmt.Sprintf("127.0.0.1:%d", cfg.Server.SREListenerPort), ...)
   ```

2. **Default de `Server.Host` → loopback.** Un host vacío ahora resuelve a `127.0.0.1`, nunca
   al wildcard. Quien necesite federación LAN pone `server.host` explícito en `neo.yaml`.

   ```go
   // pkg/config/config.go — applyServerDefaults
   if cfg.Server.Host == "" {
       cfg.Server.Host = "127.0.0.1"
       *ns = true
   }
   ```

---

## 4. Superficie de red (post-hardening)

Verificable con `lsof -nP -iTCP -sTCP:LISTEN | grep neo` — **cero** entradas `*:`.

| Servicio | Puerto (default) | Bind | Origen |
|---|---|---|---|
| Nexus dispatcher | 9000 | `127.0.0.1` | `nexus.bind_addr` (`~/.neo/nexus.yaml`) |
| HUD dashboard | 8087 | `127.0.0.1` | `nexus.dashboard_port` |
| neo-mcp SSE worker | dinámico (9100+) | `127.0.0.1` | fd heredado de Nexus (`reservePort`) |
| neo-mcp SRE listener | dinámico | `127.0.0.1` | hardcoded (este fix) |
| neo-mcp diagnostics/pprof | dinámico | `127.0.0.1` | hardcoded |
| Ollama llm / embed | 11434 / 11435 | `127.0.0.1` | `nexus.services.*.bind_addr` |

---

## 5. Auth token — alcance y advertencia

`nexus.api.auth_token` (header `X-Nexus-Token`) gatea **únicamente `/api/v1/*`**
(start/stop de workspaces, gestión de federación) — ver `cmd/neo-nexus/main.go`
(`if cfg.Nexus.API.AuthToken != ""`).

> ⚠️ **NO** gatea el dispatch MCP (`/mcp/sse`) ni el proxy OAuth. Por eso el token es
> defensa en profundidad para la API de gestión, **no** para el acceso a las tools MCP.
> La frontera real del MCP es el **túnel SSH**: como el bind queda en `127.0.0.1`, cualquier
> cosa que llegue a `:9000` por loopback a través del túnel ya está autenticada por SSH.

Configuración (`~/.neo/nexus.yaml`, fuera del repo):

```yaml
nexus:
  api:
    enabled: true
    auth_token: "<256-bit hex — generar con: openssl rand -hex 32>"
```

El token se aplica al **arrancar Nexus** (no es hot-reloadable por SIGHUP); requiere
restart del dispatcher.

---

## 6. Acceso remoto

### 6.1 Recomendado — Túnel SSH (misma red)

Requisitos en el Mac (una sola vez, requieren `sudo`):

```bash
# Firewall (defensa en profundidad — ya todo es loopback)
sudo /usr/libexec/ApplicationFirewall/socketfilterfw --setglobalstate on
sudo /usr/libexec/ApplicationFirewall/socketfilterfw --setstealthmode on
# Habilitar SSH / Remote Login
sudo systemsetup -setremotelogin on
```

Desde la PC remota (con su llave SSH autorizada en el Mac):

```bash
ssh -N -L 9000:127.0.0.1:9000 <usuario>@<ip-lan-del-mac>
```

El cliente MCP remoto apunta a `http://127.0.0.1:9000/mcp/sse` con header
`X-Neo-Workspace: <workspace>`. Routing de Nexus:
`target_workspace > URL path > X-Neo-Workspace header > active workspace`.

### 6.2 Endurecimiento de la llave (opcional)

Para que esa credencial sólo pueda hacer el forward y nada más, en
`~/.ssh/authorized_keys` del Mac antepón a la llave del cliente:

```
no-pty,no-X11-forwarding,permitopen="127.0.0.1:9000",command="echo tunnel-only" ssh-ed25519 AAAA...
```

### 6.3 Fuera de la red

No exponer puertos a internet. Usar el mismo esquema SSH sobre **Tailscale/WireGuard**
(mesh cifrado con ACL por dispositivo). El bind permanece en loopback.

---

## 7. Runbook de testeo

### 7.1 Local — confirmar que NO está expuesto

```bash
# 1. Ningún socket wildcard (debe salir vacío):
lsof -nP -iTCP -sTCP:LISTEN | grep neo | grep '\*:'

# 2. La IP LAN rechaza (simula una PC remota SIN túnel):
nc -z -G2 <ip-lan> 9000 && echo EXPUESTO || echo "✓ refused"

# 3. Loopback sí abre (target del túnel):
nc -z 127.0.0.1 9000 && echo "✓ open"

# 4. Auth en gestión:
curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:9000/api/v1/presence            # → 401
curl -s -o /dev/null -w '%{http_code}\n' -H 'X-Nexus-Token: <token>' \
     http://127.0.0.1:9000/api/v1/presence                                                 # → 200
```

### 7.2 Desde la otra PC — SIN túnel (debe FALLAR)

```bash
nc -z -w3 <ip-lan> 9000 ; echo "exit=$?"     # exit≠0 = ✓ aislado
curl -m5 http://<ip-lan>:9000/health         # refused = ✓
```

### 7.3 Desde la otra PC — CON túnel (debe FUNCIONAR)

```bash
ssh -N -L 9000:127.0.0.1:9000 <usuario>@<ip-lan> &
curl -s http://127.0.0.1:9000/health                                          # → {"status":"ok"...}
curl -s -H 'X-Nexus-Token: <token>' http://127.0.0.1:9000/api/v1/presence     # → 200 + JSON
```

Resultado esperado: **7.2 falla** (aislado) y **7.3 funciona** (vía túnel) → posture correcto.

---

## 8. Gotcha operativo — Nexus que ignora SIGTERM

Al reiniciar, un Nexus viejo puede **ignorar el SIGTERM** y seguir dueño de `:9000`; el nuevo
arranca y choca con `bind: address already in use` → muere. Engaña porque el watchdog del
viejo respawnea los hijos con el binario fresco (se ven correctos), pero el dispatcher sigue
siendo el viejo (sin el `auth_token`/config nuevos). Recuperación:

```bash
kill -9 <pid-nexus-viejo>
pkill -9 -x neo-mcp
lsof -nP -iTCP:9000 -sTCP:LISTEN          # confirmar :9000 libre
nohup ./bin/neo-nexus > /tmp/neo-nexus.log 2>&1 &
# verificar en el log: "API auth enabled" + "Dispatcher listening" SIN "NEXUS-FATAL"
```

Relacionado: directiva `[BOOT-DIAGNOSIS]` ("Doble Nexus: matar PID más bajo").
