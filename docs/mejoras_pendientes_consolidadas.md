# Mejoras pendientes consolidadas

> Estado: backlog vigente y fuente única para próximas rondas.
>
> Revisión: 2 de septiembre de 2026. Este documento reúne únicamente el
> trabajo que continúa pendiente después de contrastar las hojas de ruta de
> `docs/` con la implementación actual. Los demás documentos se conservan como
> historial de diseño y de cambios ya terminados.

## Resumen

Las grandes rondas anteriores están cerradas: distribución autónoma, frontend
modular, streaming SSE robusto, runtime coherente, migraciones SQLite,
recuperación agéntica, consulta autónoma, lectura de secciones completas,
watcher básico, paginación, trazas, reranking local, pools SQLite separados,
grafo incremental y backend ANN/HNSW opt-in.

Quedan siete líneas de mejora. Las tres primeras concentran el mayor valor:

1. activar realmente la puerta de calidad de recuperación en CI;
2. sustituir el sondeo completo del watcher por eventos y reindexación dirigida;
3. eliminar el trabajo O(corpus) que aún hace ANN al clonar y persistir el
   grafo después de un cambio pequeño.

---

## P0 — Activar la puerta de calidad de recuperación

### Problema

El job `recuperacion` de `.github/workflows/calidad.yml` busca
`evaluations/fixtures/ci_questions.json`. El fixture no está versionado y el job
termina con código 0 cuando falta. Por tanto, CI no está comprobando actualmente
los pisos de `evaluations/thresholds.json` y una regresión de recuperación puede
entrar sin ser detectada.

Las evaluaciones que usan Ollama real continúan siendo manuales. Esto es
razonable para CI alojada, pero falta una reproducción determinista obligatoria.

### Trabajo propuesto

- Grabar el fixture oficial con la combinación de modelos y configuración que
  se quiera usar como referencia:

  ```powershell
  go run ./cmd/evaluate -vault ./vault_ci `
    -dataset ./evaluations/ci_questions.json -record
  ```

- Revisar y versionar `evaluations/fixtures/ci_questions.json`.
- Calibrar los pisos de `evaluations/thresholds.json` ligeramente por debajo de
  los resultados observados.
- Cambiar CI para que la ausencia o incompatibilidad del fixture sea un error,
  no un aviso con salida correcta.
- Guardar en el fixture metadatos suficientes para detectar cambios de modelo,
  chunker, prefijos de embedding y configuración de recuperación.
- Añadir una comparación determinista de los backends semánticos `exact` y
  `ann`, sin permitir que ANN reduzca las métricas por debajo de los pisos.
- Mantener una ejecución manual o programada con Ollama real para detectar
  cambios que un fixture grabado no puede observar.

### Criterios de aceptación

- El job de recuperación siempre ejecuta `cmd/evaluate -check`.
- El job falla si el fixture falta, está incompleto o no corresponde a la
  configuración esperada.
- Se comprueban como mínimo `document_recall`, `recall_at_k`, `ndcg_at_k`,
  `citation_valid_rate`, `end_to_end_pass_rate` y
  `query_rewrite_pass_rate`.
- Una regresión intencionada en cualquiera de esas métricas hace fallar CI.

---

## P1 — Watcher por eventos y reindexación de rutas concretas

### Problema

`VaultWatcher` sondea el vault con un ticker. Cada ciclo ejecuta
`DiscoverMarkdownFiles` y hace `stat` de todos los Markdown. Cuando detecta un
cambio, llama a `Indexer.Index`, que vuelve a enumerar el vault y calcula el
SHA-256 de todos los archivos para descubrir cuáles cambiaron.

El mecanismo es portable y seguro, pero su coste crece con el tamaño del vault
aunque no haya actividad. Un único guardado sigue provocando trabajo
O(vault) antes de llegar a las operaciones dirigidas de SQLite y del grafo.

### Trabajo propuesto

- Incorporar eventos del sistema de archivos mediante una dependencia Go pura,
  sin CGO y compatible con Windows y Linux.
- Vigilar también los directorios nuevos y eliminar watches de los borrados.
- Acumular durante el debounce dos conjuntos normalizados:
  `changedPaths` y `deletedPaths`.
- Añadir al indexador una ruta dirigida, por ejemplo
  `IndexPaths(ctx, changedPaths, deletedPaths, onProgress)`, que:
  - sólo lea, analice y embeba los Markdown modificados;
  - elimine únicamente las rutas confirmadas como borradas;
  - preserve `ReplaceDocument`, `DeleteDocumentsNotIn` y las invariantes de
    FTS5, grafo y almacén vectorial;
  - no calcule hashes de archivos ajenos al lote.
- Mantener una reconciliación completa poco frecuente para cubrir eventos
  perdidos, desbordamientos del watcher, unidades de red y cambios durante una
  suspensión del equipo.
- Conservar las exclusiones de `.obsidian`, `_plantillas`, `_templates` y
  directorios ocultos.
- Hacer que `Retarget` sustituya de forma atómica los watches y el indexador del
  vault anterior.
- Exponer en `/api/status` el backend del watcher (`events`, `polling` o
  `reconciling`), el tamaño del lote y la fecha de la última reconciliación.

### Criterios de aceptación

- Editar un Markdown no enumera ni calcula hashes del resto del vault.
- Altas, modificaciones, renombrados y borrados en ráfaga producen una sola
  operación dirigida.
- Un evento perdido queda reparado por la reconciliación periódica.
- Sólo puede ejecutarse una indexación automática a la vez.
- El cambio de vault no usa después rutas ni componentes del runtime anterior.
- Pruebas con miles de archivos demuestran que el trabajo por una edición es
  proporcional al lote modificado, no al corpus.

---

## P1 — ANN incremental sin copiar ni reescribir todo el corpus

### Problema

El HNSW ya admite altas, tombstones, compactación, persistencia y fallback
exacto. Sin embargo, al publicar un snapshot nuevo se clona profundamente el
grafo anterior y después se serializa completo. Un cambio de una sola nota
vuelve a copiar vectores y vecinos O(corpus), y a reescribir todo el archivo
`.hnsw`.

Además, cargar un archivo persistido válido termina guardándolo otra vez aunque
no haya cambios. Los nodos HNSW conservan otra copia completa de cada vector,
además de la que ya existe en las filas del almacén vectorial.

### Trabajo propuesto

- Sustituir el clon profundo por una publicación inmutable con copy-on-write,
  un overlay de deltas o páginas de nodos compartidas entre snapshots.
- Representar altas y tombstones como un journal incremental en disco.
- Crear checkpoints compactos sólo por umbral de deltas, tombstones, tamaño o
  versión; no después de cada modificación.
- No volver a guardar un índice que acaba de cargarse y cuya firma ya coincide.
- Compartir el almacenamiento de vectores con el snapshot cuando sea seguro, o
  separar los vectores activos de los necesarios únicamente para atravesar
  tombstones.
- Añadir checksum del archivo o de sus bloques para detectar corrupción
  silenciosa que todavía pueda decodificarse como `gob` válido.
- Mantener siempre el snapshot anterior disponible para consultas concurrentes
  mientras se prepara el nuevo.
- Si un delta o checkpoint falla, conservar el backend exacto y registrar en
  estado y traza el motivo de la degradación.

### Criterios de aceptación

- Insertar o borrar una fracción pequeña de chunks no copia todos los vectores
  ni reescribe el checkpoint completo.
- Cargar un `.hnsw` válido no modifica el archivo.
- La memoria adicional durante un parche queda acotada por el delta más una
  pequeña estructura de publicación.
- La compactación mantiene `recall@k` y no bloquea las consultas que usan el
  snapshot anterior.
- Corrupción, versión incompatible y escritura incompleta producen fallback
  exacto sin hacer fallar la consulta.

---

## P1 — Selección adaptativa y validación real de ANN

### Problema

La prueba actual de `recall@10` usa vectores sintéticos. Falta validar ANN con
los embeddings, filtros y distribución de consultas de `vault_ci`. También
falta cubrir explícitamente combinaciones de `status`, `vigencia`, tags y
`allowedDocuments`.

El postfiltrado aumenta progresivamente `ef`, pero un subconjunto permitido muy
pequeño puede obligar a recorrer gran parte del HNSW. Para corpus pequeños y
equipos con muchos núcleos, el recorrido exacto puede ser más rápido. En la
medición local de 10.000 vectores de dimensión 384, el exacto quedó alrededor
de 0,86 ms y ANN alrededor de 1,27 ms por consulta.

### Trabajo propuesto

- Añadir a `cmd/evaluate` métricas ANN frente al top-k exacto sobre `vault_ci`:
  `recall@k`, solapamiento, pérdida máxima de puntuación y latencia.
- Probar consultas factuales, relacionales, filtros temporales, tags,
  frontmatter y conjuntos `allowedDocuments` pequeños.
- Añadir benchmarks ANN de 10k, 50k y 100k que separen claramente:
  construcción, carga persistida, parche, consulta y memoria.
- Usar el backend exacto cuando:
  - el corpus esté por debajo de un umbral medido;
  - un prefiltro produzca un conjunto pequeño;
  - ANN no esté disponible o su firma no sea válida.
- Considerar un prefiltro exacto para `allowedDocuments` pequeños en lugar de
  postfiltrar repetidamente el grafo global.
- Exponer la razón de selección del backend en la traza, no sólo `exact` o
  `ann`.

### Criterios de aceptación

- `recall@k` sobre `vault_ci` es al menos 0,98 para la configuración publicada.
- Los filtros devuelven los mismos documentos permitidos que el backend exacto.
- La aplicación elige exacto cuando ANN sería previsiblemente más lento.
- Los benchmarks reportan p50, p95, memoria, tiempo de construcción, tiempo de
  carga y coste del parche incremental.

---

## P2 — Tiempos por etapa en la traza de recuperación

### Problema

La traza actual explica el ranking mediante candidatos, puntuaciones y conteos,
pero no permite localizar qué etapa domina la latencia. La telemetría de tokens
del Verificador y del Redactor tampoco sustituye una medición extremo a extremo.

### Trabajo propuesto

- Añadir `timings_ms` a la traza con, como mínimo:
  - reformulación;
  - embedding de consulta;
  - búsqueda semántica y backend usado;
  - FTS5;
  - expansión del grafo;
  - fusión, boosts y MMR;
  - reranking;
  - hidratación y lectura amplia;
  - Verificador;
  - Redactor y tiempo hasta el primer delta SSE;
  - total.
- Medir con reloj monotónico y no registrar preguntas ni contenido del vault.
- Consolidar tiempos de subpreguntas en modo agéntico sin perder el detalle por
  llamada de herramienta.
- Mostrar los tiempos en `/api/retrieval/trace` y en el inspector de la UI.

### Criterios de aceptación

- La suma y el total son coherentes, incluidos caminos de abstención y error.
- La traza diferencia carga/parche del índice de una consulta ANN normal.
- Las métricas no incorporan información privada ni crean cardinalidad sin
  límite.

---

## P2 — Runtime cross-encoder distribuible

### Problema

Obsigon implementa y prueba el contrato HTTP local del cross-encoder, pero no
distribuye un runtime compatible. El usuario debe elegir, instalar y operar por
su cuenta un servicio con `/health` y `/rerank`. La característica es correcta
como backend opt-in, pero no tiene todavía una experiencia autónoma comparable
al resto del producto.

### Trabajo propuesto

- Publicar un runtime de referencia separado, local y reproducible, con versión
  fijada y checksum.
- Incluir instrucciones y scripts para instalarlo, arrancarlo y comprobar su
  modelo sin exponerlo fuera de loopback.
- Integrar el diagnóstico en el lanzador y `/api/status`, mostrando proceso,
  modelo, versión y error accionable.
- Mantenerlo opcional y fuera del binario Go principal para preservar
  `CGO_ENABLED=0` y no obligar a descargar ONNX/Python a quien no lo use.
- Añadir una prueba de contrato extremo a extremo contra el runtime de
  referencia en un job opcional o programado.

### Criterios de aceptación

- Un usuario puede activar `cross_encoder` siguiendo un flujo documentado y
  verificable, sin diseñar su propio servicio.
- El servicio sólo escucha en loopback y cualquier fallo conserva el ranking
  anterior.
- El ejecutable principal continúa compilando y distribuyéndose sin CGO.

---

## P3 — Calidad con perfiles de modelo ligeros

### Problema

El fallo de transporte SSE está resuelto, pero los modelos ligeros pueden
repetir frases de turnos anteriores, seguir peor el protocolo JSON o degradar
la síntesis multi-salto. El perfil predeterminado actual es más capaz, aunque
los perfiles ligeros siguen disponibles para equipos con menos memoria.

### Trabajo propuesto

- Añadir casos de regresión conversacional con dos o más turnos para detectar
  repetición injustificada de respuestas anteriores.
- Medir por perfil reformulación, obediencia al protocolo, citas válidas,
  abstención y calidad de redacción.
- Adaptar automáticamente límites y funciones por capacidad conocida del
  perfil: modo agéntico, reranking, contexto y presupuesto de salida.
- Mejorar los prompts sólo cuando la evaluación demuestre una ganancia; evitar
  reglas especiales no medidas para un único modelo.
- Mostrar en la UI una advertencia clara cuando un perfil no sea recomendable
  para recuperación agéntica o preguntas multi-salto.

### Criterios de aceptación

- Las pruebas detectan que una respuesta nueva copia indebidamente el turno
  anterior.
- Cada perfil publicado tiene capacidades y límites coherentes con resultados
  medidos.
- Cambiar a un modelo ligero degrada calidad de forma explícita y controlada,
  no mediante fallos de transporte o respuestas silenciosamente incoherentes.

---

## Orden recomendado de ejecución

1. Grabar el fixture y convertir la puerta de calidad en obligatoria.
2. Implementar watcher por eventos más `IndexPaths` dirigido.
3. Refactorizar publicación y persistencia incremental del HNSW.
4. Añadir evaluación real y selección adaptativa exacto/ANN.
5. Incorporar tiempos por etapa a la traza.
6. Distribuir un runtime cross-encoder de referencia.
7. Calibrar los perfiles ligeros con regresiones conversacionales.

Las etapas 2–4 deben apoyarse en la puerta de calidad de la etapa 1 para evitar
optimizar latencia a costa de recall, filtros, citas o abstención.

## Verificación común

Toda ronda debe mantener:

```powershell
gofmt -l .
go mod verify
go mod tidy -diff
go vet ./...
go test ./...
$env:CGO_ENABLED = "0"; go build ./...
npm ci --ignore-scripts --no-audit --no-fund
npm run check
npm test
```

Comprobaciones específicas recomendadas:

```powershell
go test -race ./internal/db ./internal/rag
go test ./internal/db -run 'Concurren|Graph|Alias'
go test ./internal/rag -run 'Watcher|HNSW|ANN|SearchAndGraph'
go test ./internal/rag -run '^$' `
  -bench 'BenchmarkSearchDocuments|BenchmarkANNSearch' -benchtime=100x
go run ./cmd/evaluate -vault ./vault_ci `
  -dataset ./evaluations/ci_questions.json `
  -fixtures ./evaluations/fixtures/ci_questions.json -check
```

El detector de carreras necesita CGO y un compilador C en el entorno de prueba;
el artefacto distribuible debe seguir verificándose con CGO desactivado.

## Documentos revisados

Este backlog consolida el trabajo todavía abierto descrito o derivado de:

- `fix_streaming_conexion.md`;
- `mejoras_concurrencia_grafo_ann.md`;
- `mejoras_p0_runtime_evaluacion_migraciones.md`;
- `mejoras_producto.md`;
- `mejoras_recuperacion_frontend_calidad.md`;
- `prompt_mejoras.md`;
- `prompt_mejoras_v02.md`;
- `prompt_mejoras_v03.md`.

Las tareas marcadas como implementadas en esos documentos no se repiten aquí.
Si cambia el backlog, debe actualizarse este archivo en lugar de crear otra hoja
de ruta paralela.
