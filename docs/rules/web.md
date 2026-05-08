# NeoAnvil Rules — Web (TypeScript / React / Next.js)

Plantilla de reglas específicas para proyectos Web orquestados por NeoAnvil V6.5.
Cubre: TypeScript · React · Next.js · Vite. Copiar a `.claude/rules/` del proyecto destino.

---

## Certificación

- **OBLIGATORIO** tras editar `.ts`, `.tsx`, `.js`, `.jsx`, `.css` → `neo_sre_certify_mutation`
- **Flujo Pair:** AST TS (regex CC + shadow) → Bouncer → `npm test` → Index
- **Flujo Fast:** AST TS → Index
- TS no tiene SSA en neo — CC es regex-based. Findings: `[cc_method:ast_regex]`
- Pre-commit hook bloquea archivos frontend sin sello

## AST_AUDIT en TypeScript

- Acepta globs: `AST_AUDIT src/**/*.tsx`, `AST_AUDIT app/**/*.ts`
- CC regex puede sobrestimar en componentes con muchos handlers inline — extraer a funciones nombradas
- Shadow detection: variables re-declaradas con `let`/`const` en distintos scopes

## FRONTEND_ERRORS

- `neo_radar(intent: "FRONTEND_ERRORS")` — captura errores React/Vite en tiempo real
- Usar antes de certificar si hay cambios en componentes críticos o en el bundle

## Inmutabilidad de Estado

- **PROHIBIDO** mutar estado directamente — siempre copias superficiales
- React: `setState(prev => ({ ...prev, field: value }))` — nunca `prev.field = value`
- Arrays: `[...arr, newItem]` / `arr.filter(...)` — nunca `arr.push()` / `arr.splice()`
- Redux/Zustand: reducers puros sin side effects

## Componentes React

- Componentes como **funciones puras** — mismo input → mismo output
- Hooks en el top level — nunca dentro de condicionales o loops
- `useCallback` / `useMemo` solo cuando el profiler confirma que es necesario — no prematuramente
- Separar lógica de negocio de presentación: custom hooks para lógica, componentes para UI

## TypeScript

- **Modo strict** activado — `tsconfig.json: "strict": true`
- No `any` — si es necesario, usar `unknown` + type guard, o `as Type` con comentario
- Tipos explícitos en props de componentes y en retornos de funciones públicas
- `interface` para objetos públicos/extensibles, `type` para uniones y aliases

## Zero-Hardcoding

- URLs de API, puertos, endpoints: variables de entorno `NEXT_PUBLIC_*` o `process.env.*`
- Secretos: NUNCA en código frontend — solo en `.env.local` (gitignoreado)
- Config en `next.config.js` o `vite.config.ts` — no inline en componentes

## Performance y Bundle

- No importar librerías completas si se usa solo una función: `import { fn } from 'lib'` no `import lib from 'lib'`
- Imágenes: usar `next/image` (Next.js) o `<img loading="lazy">` para defer
- Code splitting: `dynamic(() => import('./HeavyComponent'))` para componentes grandes
- No `console.log` en componentes de producción

## Directorios a excluir del índice

```yaml
ignore_dirs:
  - ".next"       # Next.js build output
  - "out"         # Next.js static export
  - "build"       # CRA / Vite build output
  - "dist"        # Vite dist
  - "node_modules"
  - ".nuxt"
  - "storybook-static"
  - "coverage"
  - ".turbo"
```

## I/O y Efectos

- Side effects solo en `useEffect`, eventos, o funciones de acción — no en render
- Fetch en Server Components (Next.js App Router) — no `useEffect` para data fetching en client si hay alternativa
- Error boundaries en el árbol de componentes para errores de render

## Comandos seguros

```
npm test                npm run build
npm run dev             npm run lint
npx next build          npx tsc --noEmit
git status / log / diff
```

## Next.js específico

- `app/` router (v13+): Server Components por defecto, `"use client"` solo cuando se necesita interactividad
- `layout.tsx` para estructura compartida, `page.tsx` para contenido de ruta
- Metadata API (`export const metadata`) en lugar de `<Head>` de pages router
- `next.config.js` para rewrites, headers, redirects — no middleware para lógica simple

## Commits

`feat(ui):`, `fix(component):`, `refactor(hooks):`, `test(page):`, `style:`, `chore:`, `docs:`
