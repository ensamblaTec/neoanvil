# Conectar Claude Code a NeoAnvil por SSH (remoto)

Cómo operar el orquestador NeoAnvil desde otra máquina sin exponer nada a la red.
Complementa: [Network Security & Remote Access](./network-security-remote-access.md).

> Posture: todo bindea a `127.0.0.1`. El acceso remoto **no abre puertos** — usa SSH.
> La identidad es la llave SSH; el tráfico va cifrado.

---

## Valores de tu instalación

Reemplaza estos placeholders por los de tu máquina:

| Placeholder | Cómo obtenerlo (en el Mac que hostea NeoAnvil) |
|---|---|
| `<MAC_IP>` | `ipconfig getifaddr en0` (o `en1`) |
| `<MAC_USER>` | `whoami` |
| `<WORKSPACE_ID>` | de `.mcp.json` o `~/.neo/workspaces.json` (ej. `neoanvil-35694`) |
| `<REPO_PATH>` | ruta del repo en el Mac |

NeoAnvil = `bin/neo-nexus`: lee `~/.neo/nexus.yaml`, spawnea un hijo `neo-mcp` por
workspace, gestiona Ollama y expone MCP por **SSE en `127.0.0.1:9000`**.

---

## Dos modelos de conexión

| | **A — Terminal sobre SSH** (recomendado) | **B — Cliente en la PC remota + túnel** |
|---|---|---|
| Dónde corre Claude Code | en el Mac (solo se muestra por SSH) | en la PC remota |
| Tráfico MCP | nunca sale del Mac (loopback) | viaja por el túnel SSH cifrado |
| ¿Necesita túnel `ssh -L`? | No | Sí |
| `.mcp.json` | idéntico | idéntico |

En ambos el `.mcp.json` apunta a `127.0.0.1:9000`; en el modelo B el túnel hace que
ese puerto local sea el del Mac.

---

## Modelo A — Terminal sobre SSH (lo más simple)

1. Desde la PC remota, conecta por SSH al Mac:
   ```bash
   ssh <MAC_USER>@<MAC_IP>
   ```
2. Ve al repo y abre Claude Code:
   ```bash
   cd <REPO_PATH>
   claude
   ```
3. Claude Code lee el `.mcp.json` del repo y se conecta a NeoAnvil. Verifica con un
   `neo_radar intent=BRIEFING`.

No hace falta nada más: Claude Code corre **en** el Mac, así que llega a Nexus por el
loopback del propio Mac.

---

## Modelo B — Cliente Claude en la PC remota

1. En la PC remota, abre el túnel (déjalo corriendo en una terminal):
   ```bash
   ssh -N -L 9000:127.0.0.1:9000 <MAC_USER>@<MAC_IP>
   ```
2. En el directorio donde correrás Claude en la PC remota, crea el `.mcp.json`:
   ```bash
   cat > .mcp.json <<'JSON'
   {
     "mcpServers": {
       "neoanvil": {
         "type": "sse",
         "url": "http://127.0.0.1:9000/workspaces/<WORKSPACE_ID>/mcp/sse"
       }
     }
   }
   JSON
   ```
3. Abre Claude Code ahí. El cliente cree hablar con un Nexus local; el túnel reenvía
   sus bytes cifrados al Mac. Verifica con `neo_radar intent=BRIEFING`.

---

## Autorizar una PC nueva (login con llave)

La credencial de entrada es la **llave pública** de la PC remota en el
`authorized_keys` del Mac.

1. En la PC remota, obtén (o genera) su llave pública:
   ```bash
   cat ~/.ssh/id_ed25519.pub 2>/dev/null || \
     (ssh-keygen -t ed25519 -C "pc-remota" && cat ~/.ssh/id_ed25519.pub)
   ```
2. En el Mac, agrégala con permisos correctos:
   ```bash
   mkdir -p ~/.ssh && chmod 700 ~/.ssh
   echo 'ssh-ed25519 AAAA... pc-remota' >> ~/.ssh/authorized_keys
   chmod 600 ~/.ssh/authorized_keys
   ```
3. Verifica desde la PC remota (en una terminal nueva, sin cerrar la actual):
   ```bash
   ssh <MAC_USER>@<MAC_IP> 'echo OK-key-login'   # no debe pedir password
   ```

Remote Login (sshd) debe estar activo en el Mac: System Settings → General → Sharing →
Remote Login, o `sudo systemsetup -setremotelogin on`.

**Endurecimiento opcional** (la llave solo puede tunelizar a `:9000`, sin terminal) —
útil únicamente para el modelo B. En `~/.ssh/authorized_keys`, antepón a la llave:
```
no-pty,no-X11-forwarding,permitopen="127.0.0.1:9000",command="echo tunnel-only" ssh-ed25519 AAAA...
```
> No uses la versión restringida si quieres el modelo A (te quita la terminal).

---

## Arrancar NeoAnvil (si no está corriendo)

```bash
cd <REPO_PATH>
if curl -sf http://127.0.0.1:9000/health >/dev/null; then
  echo "✓ NeoAnvil ya está arriba"
else
  [ -x bin/neo-nexus ] || make build-mcp build-nexus
  nohup ./bin/neo-nexus > /tmp/neo-nexus.log 2>&1 & disown
  sleep 6
  curl -sf http://127.0.0.1:9000/health && echo "✓ arriba" || { echo "✗ fallo"; tail -20 /tmp/neo-nexus.log; }
fi
# esperar a que los workspaces queden running
curl -s http://127.0.0.1:9000/status | python3 -c "import sys,json;[print(' ',w['id'],w['status']) for w in json.load(sys.stdin)]"
```

Si ves `bind: address already in use`, hay un Nexus zombie (ver el gotcha de SIGTERM en
[network-security-remote-access.md](./network-security-remote-access.md#8-gotcha-operativo--nexus-que-ignora-sigterm)).

---

## Verificación rápida

```bash
curl -s http://127.0.0.1:9000/health          # → {"status":"ok",...}
# dentro de Claude Code:
#   neo_radar intent=BRIEFING                  → SRE BRIEFING
```

Recuerda: el `auth_token` (`X-Nexus-Token`) gatea solo `/api/v1/*`, **no** el dispatch
MCP. La frontera del MCP es el SSH (bind loopback). Detalle en el doc de seguridad.
