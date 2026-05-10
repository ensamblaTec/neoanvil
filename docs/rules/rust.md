# NeoAnvil Rules — Rust

Plantilla de reglas específicas para proyectos Rust orquestados por NeoAnvil V10.6.
Copiar a `.claude/rules/` del proyecto destino junto con las reglas universales.

---

## Certificación

- **OBLIGATORIO** tras editar `.rs` → `neo_sre_certify_mutation`
- **Flujo Pair:** AST Rust (regex CC + shadow) → Bouncer → `cargo test` → Index
- **Flujo Fast:** AST Rust → Index
- Rust no tiene SSA en neo — CC es regex-based. Findings: `[cc_method:ast_regex]`
- `SHADOW_INFO` en AST_AUDIT es **INFO, no ERROR** — re-bindings con `let x = transform(x)` son idiomáticos en Rust. No bloquean certify

## AST_AUDIT en Rust

- Acepta globs: `AST_AUDIT src/**/*.rs`
- `SHADOW_INFO`: re-binding de variables es intencional en Rust (patrón común en parsers y builders) — no renombrar mecánicamente
- CC>15 sí es accionable: extraer funciones privadas

## COMPILE_AUDIT en Rust

- Retorna `symbol_map` con funciones públicas y structs via regex
- Útil para offset quirúrgico en archivos grandes (`impl` blocks pueden ser largos)

## Zero-Allocation (hot-paths)

- **No `.clone()` en hot-paths** — preferir referencias `&T` o `Cow<str>`
- Usar `Vec::with_capacity(n)` cuando el tamaño es conocido
- Preferir `&[T]` sobre `Vec<T>` en parámetros de función (más flexible, menos allocations)
- Strings: `&str` en parámetros, `String` solo cuando se necesita ownership
- Evitar `Box<dyn Trait>` en hot-paths — preferir generics o `impl Trait`

## Manejo de Errores

- PROHIBIDO `.unwrap()` en código de producción — usar `?` o `match`
- PROHIBIDO `.expect("...")` salvo en inicialización donde el panic es el comportamiento correcto
- `thiserror` para errores de librería, `anyhow` para aplicaciones
- No silenciar errores con `let _ = result;` salvo intencional documentado

## Seguridad Rust

- `unsafe` PROHIBIDO sin bloque de documentación que explique: qué invariante se mantiene, por qué es necesario, quién es responsable de upholdarlo
- No `std::mem::transmute` sin justificación explícita
- Inputs externos: validar antes de pasar a código unsafe

## Zero-Hardcoding

- Configuración: `config.toml` o `clap`/`serde` desde env vars
- Secretos: `${VAR_NAME}` en `neo.yaml` → valor en `.neo/.env`

## Estructura de módulos Rust

- `src/lib.rs` — API pública del crate
- `src/main.rs` — entrypoint minimal, solo wiring
- `src/<domain>/mod.rs` — submódulos por dominio
- `Cargo.toml` es la fuente autoritativa de dependencias — no editar `Cargo.lock` manualmente
- Features flags para compilación condicional: documentar en `Cargo.toml`

## Comandos seguros

```
cargo test              cargo test --release
cargo build             cargo build --release
cargo clippy -- -D warnings
cargo fmt               cargo check
git status / log / diff
```

## Clippy

- `cargo clippy -- -D warnings` como gate — 0 warnings en producción
- `#[allow(clippy::...)]` solo con comentario justificando por qué la sugerencia no aplica
- Categorías comunes a revisar: `clippy::perf`, `clippy::complexity`, `clippy::correctness`

## Commits

`feat(core):`, `fix(parser):`, `refactor(types):`, `test(api):`, `chore:`, `docs:`
