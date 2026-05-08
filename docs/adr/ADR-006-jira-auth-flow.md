# ADR-006: Jira Authentication Flow — API Token vs OAuth 2.0 (3LO)

- **Fecha:** 2026-04-28
- **Estado:** Aceptado
- **Pilar:** XXIII (Épica 125)
- **Bloquea:** Épica 125.2 (binario `cmd/plugin-jira/`)

## Contexto

El plugin Jira (Épica 125) necesita autenticarse contra `*.atlassian.net`. La
política de credenciales de Atlassian Cloud cambió sustancialmente entre
2024-2026 y la decisión de método debe tomarse contra el estado actual, no
contra documentación histórica.

### Estado de Atlassian Cloud (verificado 2026-04-28)

- **API tokens** (cuenta Atlassian, scope-by-user):
  - Desde **2024-12-15**: tokens nuevos creados con expiración configurable,
    máximo **1 año**.
  - Desde **2025-03-13**: tokens existentes (creados antes de 2024-12-15)
    forzados a expirar entre **2026-03-14 y 2026-05-12**. *Estamos dentro
    de esa ventana ahora mismo.*
  - No existen tokens "indefinidos". Rotación anual es obligatoria.
  - Header: `Authorization: Basic base64(email:token)`.

- **OAuth 2.0 (3LO)** (app-scoped, user-authorized):
  - Three-legged flow: app obtiene authorization code → access token + refresh
    token. Access token vive ~1h, refresh token rota access tokens automáticamente.
  - Requiere registrar app en `developer.atlassian.com` y declarar scopes
    estáticos.
  - Atlassian recomienda 3LO para apps de marketplace y para integraciones
    multi-tenant.

- **PAT (Personal Access Token):**
  - Concepto distinto: existe en Jira **Server / Data Center**, no en Cloud.
  - En Cloud el equivalente es el "API token" arriba descrito. La propuesta
    original confundió ambos términos.

## Opciones consideradas

### 1. API Token (Basic Auth con email + token)

- **Pros:**
  - Setup trivial: 1 comando, 1 secreto, 0 redirect URIs.
  - No requiere registrar una app en Atlassian.
  - Funciona para uso individual (un developer, un workspace).
  - Coherente con el patrón actual de `pkg/auth/keystore.go` (provider + token).
- **Contras:**
  - **Rotación obligatoria anual** — el plugin debe detectar 401/403 por
    expiración y guiar al usuario a re-emitir el token.
  - Token = impersonation de usuario humano. Acción de plugin se atribuye
    al usuario propietario del token (no al "app"). Audit trail confuso en
    instalaciones de equipo.
  - No hay refresh automático.
  - No es scope-limited: el token tiene los mismos permisos que el usuario.

### 2. OAuth 2.0 (3LO)

- **Pros:**
  - Refresh automático — access token rota cada hora vía refresh token.
  - Scope-limited — el usuario otorga permisos específicos al app
    (`read:jira-work`, `write:jira-work`, etc.).
  - Audit trail apropiado — Atlassian distingue "app X actuando en nombre
    de usuario Y".
  - Es la dirección que Atlassian recomienda para integraciones serias.
- **Contras:**
  - Requiere registrar el plugin como app en `developer.atlassian.com`.
  - UX de `neo login`: redirect a navegador, callback HTTP local, estado.
  - Necesita persistir refresh tokens cifrados (vault encryption del
    Épica 124 es prerequisito).
  - Sobre-engineering para uso single-user.

### 3. Ambos (estrategia dual)

- **Pros:** mejor UX según contexto: API token para developer solo, OAuth
  para equipos.
- **Contras:** dos rutas de código, dos sets de tests, más superficie de bugs.
  El segundo backend se construye solo si la demanda real lo justifica.

## Decisión

**Adoptar API Token como método inicial. Diseñar el vault y el plugin para
que OAuth 2.0 (3LO) pueda añadirse como segundo backend sin breaking changes.**

Rationale:
- El consumidor inicial de PILAR XXIII es un developer individual integrando
  su workspace neoanvil. Setup trivial gana.
- La rotación obligatoria de Atlassian convierte el "token expira" en
  problema real, no hipotético — el plugin debe manejarlo bien con CUALQUIER
  método.
- Diseñar el vault con `Provider.Refresh()` interface (Épica 124.3) deja el
  hook listo para 3LO sin reescritura.
- 3LO se añade en una épica posterior cuando exista demanda multi-tenant real.

### Implementación concreta

1. **`neo login --provider jira --email <e> --token <t> --domain <d>`** crea
   un `CredEntry` en `pkg/auth/keystore.go` con campos:
   ```
   Provider:    "jira"
   Type:        "api_token"
   Email:       <user>
   Token:       <encrypted>
   Domain:      <empresa.atlassian.net>
   ExpiresAt:   <now + 365d>   // mejor estimado, refinable
   RefreshFn:   nil            // OAuth lo poblaría
   ```
2. El plugin Jira recibe `JIRA_EMAIL`, `JIRA_TOKEN`, `JIRA_DOMAIN` como env
   vars al spawn (vía `env_from_vault` del manifest).
3. Cliente HTTP del plugin:
   - `Authorization: Basic base64(email:token)` en cada request.
   - On `401/403`: emite evento estructurado `auth_expired`, no reintenta.
   - On `429`: respeta `Retry-After`, exponential backoff.
4. **`neo auth status`** comando del CLI: lista credenciales + días hasta
   expiración. Warning a 30 días, alert a 7 días.

### Cuándo migrar a 3LO

Triggers que justifican abrir épica de OAuth 3LO:
- Demanda real de uso multi-tenant (>1 usuario distinto compartiendo plugin).
- Necesidad de scope-limited tokens (auditor exige `read-only`).
- Integración con marketplace de Atlassian.

## Consecuencias

### Positivas

- Plugin Jira shipea en Épica 125 sin desbloquear OAuth flow (~2-3 semanas
  de trabajo evitadas).
- El usuario obtiene Jira working ya, no espera un OAuth dance.
- El vault queda diseñado para acomodar refresh tokens cuando se necesiten.

### Negativas

- Rotación manual anual recae en el usuario. Mitigación: `neo auth status` +
  warning proactivo en BRIEFING cuando un token expira en <30 días.
- Audit trail en Jira muestra "user X" no "neoanvil plugin", confuso si el
  team comparte el workspace neoanvil.

## Referencias

- [API tokens will now have a maximum one-year expiry — Atlassian Community](https://community.atlassian.com/forums/Jira-articles/API-tokens-will-now-have-a-maximum-one-year-expiry/ba-p/2880029)
- [OAuth 2.0 (3LO) apps — Jira Cloud platform](https://developer.atlassian.com/cloud/jira/platform/oauth-2-3lo-apps/)
- [Manage API tokens for your Atlassian account](https://support.atlassian.com/atlassian-account/docs/manage-api-tokens-for-your-atlassian-account/)
- [Atlassian Cloud changes Jul 28 to Aug 4, 2025](https://confluence.atlassian.com/cloud/blog/2025/08/atlassian-cloud-changes-jul-28-to-aug-4-2025)
- ADR-005 — Plugin architecture (subprocess MCP).
