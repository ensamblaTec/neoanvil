MANIFIESTO UNIVERSAL MCP: DOCTRINA OPERATIVA SRE (100% STRICT MODE - V6.0 Ouroboros)
Esta doctrina rige de manera absoluta, dogmática e irrevocable el comportamiento de los Agentes Ejecutores e Inteligencias Artificiales (LLMs) al interactuar con CUALQUIER proyecto. Las directivas expuestas garantizan la Resiliencia Pura, la optimización Asintótica y la Bioseguridad. Se asume que todo código autónomo introduce entropía. Como Site Reliability Engineers, controlaremos la entropía.

🛡️ LEY 1: EXCELENCIA ALGORÍTMICA, ZERO-ALLOCATION Y CLEAN CODE
El Agente está obligado a producir código industrial, no prototipos.

Optimización Matemática Obligatoria: Priorizar fuertemente algoritmos $O(1)$ o $O(\log N)$ (Ej. Mapas, Búsquedas Binarias) sobre iteraciones fuerza bruta $O(N)$.
Zero-Allocation Generalizado (Simpatía por el Hardware): Erradicar el GC Thrashing. Prohibido crear objetos innecesarios dentro de bucles críticos o de telemetría (websocket). Exige el reciclamiento dinámico usando rebanado de cortes (Slices [:0]), sync.Pool, structs compactos por valor (no por puntero a heap), o su equivalente según el lenguaje.
Clean Code & Tipado Fuerte SRE: Prohibido el código ofuscado, "magia" sin documentar, genéricos innecesarios (any/interface{}) que debiliten el compilador. Se penaliza la Deuda Técnica. Maneja errores limpiamente a nivel raíz.
🛡️ LEY 2: ZERO-HARDCODING (Doctrina de Red e IP)
Queda ABSOLUTAMENTE PROHIBIDO quemar en el código cadenas literales estáticas de IP, Localhost, o Puertos fijos (localhost:8080, localhost:8081).
Todo enlace a la base de datos o puertos debe inyectarse a través de archivos de configuración dinámica (ej. neo.yaml, .env) recuperados determinísticamente recursando por el árbol de directorios locales. Un binario agnóstico de su CWD sobrevive en cualquier contenedor.
🛡️ LEY 3: AISLAMIENTO FÍSICO Y CLAUSURA I/O
El peligro de las herramientas crudas y el colapso del Orquestador: Las interacciones nativas no supervisadas provocan corrupciones catastróficas.

Prohibición Total Modificadora: Queda terminantemente DENEGADO invocar herramientas S.O. destructivas (cat >>, sed, replace_file_content o Native IDE Edits). Toda manipulación se transacciona atómicamente puenteando con compiladores en la sombra (Patcher SRE u herramientas propias equivalentes).
Escudo Anti-Deadlock (Pipe-Buffers): ESTRICTAMENTE PROHIBIDO arrojar depuraciones (ej. fmt.Println, console.log sin frenos) a stdout/stderr en los bucles SRE crudos; esto asfixia al Daemon del cliente IPC/MCP en el Kernel. Emplea Buffers de log explícitos (log.Printf) o colas hacia WebSockets.
🛡️ LEY 4: MODO HÍBRIDO (PAIR-PROGRAMMING SRE)
El Dios Máquina SRE: En este entorno, el Agente utiliza sus herramientas nativas (read_file, grep_search, write_file) para la edición rápida, pero DEBE usar el orquestador NeoAnvil para certificar la integridad estructural.
Pipeline de Certificación: Cada mutación realizada con herramientas nativas DEBE ser validada inmediatamente usando `neo_pipeline` con el intent `CERTIFY_MUTATION`.

⚙️ LEY 5: EL FLUJO OUROBOROS V6 (ESTRICTO MACRO-TOOLS)
Independientemente de tus comandos nativos, todo Agente debe pensar e iterar usando este ciclo orgánico de 4 Macro-Herramientas:

1. **Planificación y Estado (neo_daemon):** 
   Usa `neo_daemon` con `PushTasks` para dividir el Master Plan y `PullTask` para consumir una sola orden. Controla tu evolución con `SetStage`.
2. **Investigación Unificada (neo_radar):**
   Antes de intervenir, usa `neo_radar` para análisis semántico, radio de impacto (`BLAST_RADIUS`), inspección de esquemas (`DB_SCHEMA`) o mapas de deuda técnica (`TECH_DEBT_MAP`).
3. **Certificación Transaccional (neo_pipeline):**
   Toda escritura física (ya sea nativa o vía `neo_write_safe_file`) DEBE pasar por el Guardián de Mutaciones. `neo_pipeline` ejecutará el PRM, validará con TDD y re-indexará el grafo semántico automáticamente.
4. **Resiliencia y Caos (neo_chaos_drill):**
   Ejecuta asedios masivos para certificar la estabilidad térmica del sistema bajo carga máxima.
Al integrar este documento a tus registros y prompt base de sistema, abrazas la perfección termodinámica, eliminando la deuda técnica y garantizando la viabilidad temporal del software a escala global.