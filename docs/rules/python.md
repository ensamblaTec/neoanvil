# NeoAnvil Rules — Python

Plantilla de reglas específicas para proyectos Python orquestados por NeoAnvil V6.5.
Copiar a `.claude/rules/` del proyecto destino junto con las reglas universales.

---

## Certificación

- **OBLIGATORIO** tras editar `.py` → `neo_sre_certify_mutation`
- **Flujo Pair:** AST Python (regex CC + shadow) → Bouncer → `pytest -x --tb=short` → Index
- **Flujo Fast:** AST Python → Index
- Python no tiene SSA — CC es regex-based, puede sobrestimar. Findings: `[cc_method:ast_regex]`
- Pre-commit hook bloquea `.py` sin sello

## AST_AUDIT en Python

- Acepta globs: `AST_AUDIT src/**/*.py`
- CC regex puede sobrestimar en funciones con strings multi-línea o decoradores complejos
- Shadow detection: variables re-asignadas en distintos scopes — revisar si son intencionales
- **No hay SSA-exact** — si CC>15 parece falso positivo, extraer helper y documentarlo

## COMPILE_AUDIT en Python

- Retorna `symbol_map` con clases, funciones y métodos Python via regex
- Menos preciso que Go (no hay go/ast) pero útil para offset quirúrgico en archivos grandes
- Combinar con `Grep "^def \|^class "` para búsqueda rápida de símbolos

## Zero-Loop (vectorización)

- NUNCA iterar sobre arrays/tensores con loops Python nativos en hot-paths
- Usar: `np.dot`, `@`, broadcasting, `torch.stack`, `F.normalize`, `albumentations.Compose`
- FAISS: `index.search(xq, k)` batch — nunca query por query en loop
- Generators (`yield`) preferidos sobre listas cuando el resultado es grande y se consume una vez

## Gestión de Memoria

- Modelos PyTorch/YOLO: cargar una sola vez al arrancar (lifespan FastAPI o módulo singleton)
- Tensores grandes: `del tensor; torch.cuda.empty_cache()` tras operaciones batch si hay presión
- OpenCV `np.ndarray`: no acumular en listas dentro de loops — procesar y liberar
- FAISS index: mantener en memoria, no recargar por request

## I/O y Logging

- PROHIBIDO `print()` en código de producción — usar `logging.getLogger(__name__)`
- PROHIBIDO `sys.stdout.write` en módulos de producción
- En pipelines de inferencia: `logger.info` / `logger.debug`

## Seguridad Python

- HTTP externo: `httpx.Client` o `requests.Session` con `timeout` explícito — NUNCA sin timeout
- Rutas de archivo: validar con `pathlib.Path.resolve()` — verificar que no escapen del workspace
- Inputs FastAPI: validar con Pydantic antes de pasar al pipeline
- No serializar objetos PyTorch/numpy directamente en respuestas JSON — convertir a tipos nativos

## Zero-Hardcoding

- Rutas de modelos, umbrales, dimensiones, puertos: `src/config.py` o `os.environ.get()`
- Secretos: `${VAR_NAME}` en `neo.yaml` → valor en `.neo/.env`
- No duplicar constantes entre módulos — `config.py` es la fuente única de verdad

## Estructura de módulos Python

- Todo código productivo en `src/` — sin scripts sueltos en raíz para lógica de negocio
- Imports absolutos (`from src.models.yolo import X`) preferidos sobre relativos en producción
- `__init__.py` vacío está bien — no sobrecargar con re-exports
- `src/config.py` — configuración global, constantes, rutas

## Comandos seguros

```
python3 -m pytest           python3 -m pytest -v
python3 -m pytest --cov     python3 -m mypy src/
python3 -m ruff check       python3 -m ruff format
python3 -m black            pip install -r requirements.txt
git status / log / diff
```

## Virtualenv

- El directorio del virtualenv (`venv/`, `vision/`, `.venv/`) va en `ignore_dirs` del `neo.yaml`
- **NUNCA** explorar `site-packages/` con neo tools — equivalente a `node_modules/`
- `requirements.txt` commiteado y actualizado — fuente autoritativa de dependencias
- Fijar versiones: `>=X.Y.Z,<X+1` cuando una versión mayor cambia API

## Commits

`feat(api):`, `fix(pipeline):`, `refactor(models):`, `test(inference):`, `chore:`, `docs:`
