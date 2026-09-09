# Obsigon

Aplicación autónoma en Go para consultar vaults de Obsidian mediante un RAG
local. Incluye servidor HTTP, frontend empotrado, indexación incremental,
búsqueda híbrida, grafo de wikienlaces, agentes, memoria conversacional y
SQLite con FTS5.

La única dependencia de ejecución para indexar y consultar es una instancia local de
Ollama con los modelos configurados.

## Capacidades

- Frontend incluido en el ejecutable mediante `embed.FS`, con override opcional
  para desarrollo.
- Indexación incremental SHA-256, FTS5 dirigido y resolución incremental de
  wikienlaces mediante índices de alias y fuentes afectadas.
- SQLite en WAL con pool concurrente de sólo lectura y un escritor serializado
  que adquiere el bloqueo al comenzar la transacción.
- Recarga atómica del índice vectorial sin bloquear búsquedas durante el escaneo
  de SQLite.
- Ranking semántico exacto por defecto o HNSW persistente y opt-in, con parcheo
  incremental, compactación de tombstones y fallback exacto ante errores.
- Troceado plano o padre-hijo con Markdown y sintaxis de Obsidian.
- Recuperación semántica, FTS5, RRF, MMR, grafo multi-salto y reranking LLM
  o cross-encoder local con degradación segura.
- Reformulación autónoma de seguimientos: la memoria resuelve pronombres y
  elipsis, pero nunca se concatena al texto que se embebe.
- Búsqueda por fragmento y lectura posterior de la sección completa, con
  outline, jerarquía de encabezados, frontmatter, tags anidados y `mtime`.
- Bonus acotados y auditables para hubs con backlinks y, sólo en preguntas
  temporales, para notas modificadas recientemente.
- Enrutador determinista por consulta (`factual`, `relational`, `hybrid` o
  `unknown`) sin mutar la configuración compartida.
- Abstención por similitud mínima antes de invocar modelos generativos.
- Consultas JSON y streaming SSE con Verificador y Redactor.
- Recuperación agéntica opcional con descomposición, herramientas locales y
  límites de iteraciones/tokens para preguntas multi-salto.
- Proyectos, conversaciones, memoria, telemetría, traza de recuperación e
  inspector de contexto.
- Watcher opcional del vault por eventos del sistema de archivos (fsnotify,
  sin CGO), reindexación dirigida sólo de las rutas tocadas, reconciliación
  completa periódica de respaldo y estado visible en la API y la UI.
- Indexación con progreso por archivo y cancelación, e historial paginado.
- Endpoints separados de liveness/readiness y cierre HTTP con plazo.
- Fuentes con enlaces profundos `obsidian://`.
- Logging JSON con identificador de petición y códigos HTTP diferenciados.
- Seguridad local: servidor, Ollama, `Host` y mutaciones limitados a loopback.

## Requisitos

- Go 1.23 o posterior para ejecutar desde el código fuente. El ejecutable de
  release no requiere Go.
- Ollama local.
- Modelos predeterminados: `gpt-oss:latest` y `nomic-embed-text`.

Obsigon nunca descarga modelos automáticamente. Para instalar los
predeterminados:

```powershell
ollama pull gpt-oss:latest
ollama pull nomic-embed-text
```

`gpt-oss:latest` pide unos 24 GB de RAM. En equipos más modestos, elige un perfil más ligero en la interfaz (por ejemplo «Ligero · qwen3.5:0.8b») o
ajusta `OLLAMA_CHAT_MODEL` en `.env`.

## Arranque

### Windows con un clic

Ejecuta `iniciar_windows.bat`. El lanzador PowerShell:

1. crea `.env` desde `.env.example` la primera vez;
2. usa `obsigon-server.exe` u `obsigon-windows-amd64.exe` si está junto a él y,
   en caso contrario, usa `go run ./cmd/server`;
3. comprueba el puerto, Ollama y los nombres de los modelos sin descargar nada;
4. espera a que el servidor esté listo y abre el navegador.

### Desde el código fuente

```powershell
Copy-Item .env.example .env
go run ./cmd/server
```

La interfaz queda disponible por defecto en <http://127.0.0.1:8000>.

### Desde un binario

El frontend viaja dentro del ejecutable, por lo que el binario puede copiarse a
otra carpeta sin llevar `app/`. Coloca allí un `.env` si quieres cambiar el
vault o cualquier otro valor y ejecuta:

```powershell
.\obsigon-windows-amd64.exe
```

`obsigon-windows-amd64.exe -version` muestra versión, commit y fecha de build.
El workflow de release inyecta esos datos con `-ldflags` y publica el ejecutable
sin CGO, un ZIP instalable con lanzadores/configuración/vault demo y su checksum
SHA-256.

## Configuración

Todas las opciones y sus valores iniciales están en `.env.example`. Las que
controlan esta ronda son:

| Variable | Predeterminado | Uso |
| --- | --- | --- |
| `APP_HOST` | `127.0.0.1` | Host del servidor; sólo admite `localhost` o loopback. |
| `APP_PORT` | `8000` | Puerto HTTP, entre 1 y 65535. |
| `APP_READ_HEADER_TIMEOUT_SECONDS` | `5` | Plazo para recibir cabeceras HTTP. |
| `APP_IDLE_TIMEOUT_SECONDS` | `90` | Plazo máximo de una conexión HTTP inactiva. |
| `APP_SHUTDOWN_TIMEOUT_SECONDS` | `10` | Plazo de cierre ordenado antes de forzar el cierre. |
| `RAG_WEBUI_DIR` | vacío | Override con `static/` y `templates/index.html`; vacío usa la UI empotrada. |
| `RAG_ANN` | `false` | Activa el índice semántico HNSW local; desactivado conserva el coseno exacto. |
| `RAG_ANN_M` | `16` | Vecinos máximos por nodo HNSW (4–64). |
| `RAG_ANN_EF_CONSTRUCTION` | `200` | Amplitud de construcción del HNSW (16–2000). |
| `RAG_ANN_EF_SEARCH` | `64` | Amplitud inicial de consulta ANN (8–2000). |
| `RAG_RERANK` | `false` | Activa el reranking mediante el modelo de chat local. |
| `RAG_RERANK_BACKEND` | `llm` | Backend `llm`, `cross_encoder` o `noop`. |
| `RAG_CROSS_ENCODER_URL` | vacío | Runtime HTTP local y loopback para el backend cross-encoder. |
| `RAG_CROSS_ENCODER_MODEL` | `cross-encoder/ms-marco-MiniLM-L-6-v2` | Modelo solicitado al runtime local. |
| `RAG_RERANK_CANDIDATES` | `12` | Primeros candidatos que puntúa el reranker. |
| `RAG_RERANK_WEIGHT` | `0.35` | Peso de relevancia LLM frente a la puntuación de fusión. |
| `RAG_QUERY_ROUTING` | `false` | Activa la política determinista por consulta. |
| `RAG_QUERY_ROUTER_RELATIONAL_PATTERNS` | lista incluida | Patrones auditables que señalan preguntas relacionales. |
| `RAG_QUERY_ROUTER_STRUCTURAL_TERMS` | lista incluida | Términos estructurales usados por el enrutador. |
| `RAG_QUERY_ROUTER_STRUCTURAL_THRESHOLD` | `2` | Repeticiones necesarias para activar la señal estructural. |
| `RAG_QUERY_REWRITE` | `true` | Reformula seguimientos como preguntas autónomas antes de recuperar; la memoria no entra en el embedding. |
| `RAG_QUERY_REWRITE_CONTEXT_TOKENS` | `4096` | Ventana de la llamada local y barata de reformulación. |
| `RAG_QUERY_REWRITE_MAX_OUTPUT_TOKENS` | `512` | Salida máxima del reformulador JSON. Los modelos de razonamiento (p. ej. `gpt-oss`) gastan parte de este presupuesto en su cadena de pensamiento aunque se pida `think:false`; un valor bajo puede agotarlo antes de emitir el JSON final. |
| `RAG_QUERY_REWRITE_MAX_MEMORY_CHARS` | `6000` | Memoria máxima usada sólo para resolver referencias. |
| `RAG_READ_LARGE` | `true` | Busca y rankea chunks, pero entrega al Redactor la sección Markdown completa. |
| `RAG_HUB_BOOST` / `RAG_HUB_BOOST_WEIGHT` | `true` / `0.08` | Bonifica notas con muchas fuentes de backlinks distintas. |
| `RAG_RECENCY_BOOST` / `RAG_RECENCY_BOOST_WEIGHT` | `true` / `0.08` | Usa `mtime` únicamente para consultas con intención de actualidad. |
| `RAG_WATCH` | `false` | Vigila altas, cambios, renombrados y borrados del vault por eventos del sistema de archivos. |
| `RAG_WATCH_RECONCILE_INTERVAL_MS` / `RAG_WATCH_DEBOUNCE_MS` | `300000` / `750` | Frecuencia de la reconciliación completa de respaldo y agrupación de ráfagas del watcher. |
| `RAG_HISTORY_PAGE_SIZE` / `RAG_HISTORY_MAX_PAGE_SIZE` | `50` / `200` | Límites de paginación de conversaciones y turnos. |
| `RAG_AGENT_MODE` | `false` | Activa la recuperación agéntica opt-in; el híbrido determinista sigue siendo el modo por defecto. |
| `RAG_AGENT_MAX_ITERATIONS` | `4` | Decisiones de herramienta permitidas por consulta (se limita a 3–5). |
| `RAG_AGENT_TOKEN_BUDGET` | `2200` | Presupuesto agregado de tokens de salida para descomposición y búsqueda. |
| `RAG_AGENT_MAX_SUBQUESTIONS` | `3` | Máximo de subpreguntas autónomas recuperadas antes del bucle. |
| `RAG_AGENT_CONTEXT_TOKENS` | `8192` | Ventana de contexto del planificador/agente de búsqueda. |
| `RAG_AGENT_MAX_OBSERVATION_CHARS` | `12000` | Límite por observación incorporada al siguiente turno del agente. |

`RAG_MIN_SIMILARITY` es la única puerta de abstención semántica dura. Si no hay
candidatos por encima de ese umbral, `search_notes` no deja que FTS o el grafo
rescaten esa búsqueda. En modo determinista esto evita llamar al reranker, al
Verificador y al Redactor; en modo agéntico el bucle aún puede probar una
herramienta ortogonal como `grep` o un filtro estructural. Un fallo o una
respuesta inválida del reranker conserva el orden anterior y no hace fallar la
consulta.

Con `RAG_ANN=true`, el ranking semántico usa un HNSW implementado en Go y
persistido como `<base SQLite>.hnsw`. Las altas se insertan en el grafo, las
bajas se conservan temporalmente como tombstones y una fragmentación superior
al 20 % fuerza su compactación. Un archivo ausente o desactualizado se
reconstruye; si está corrupto o no puede persistirse, la consulta vuelve al
coseno exacto. `/api/status` y la traza de recuperación indican explícitamente
el backend `ann` o `exact` usado.

El backend `cross_encoder` no enlaza ONNX dentro del binario Go: delega el
scoring a un runtime HTTP local configurado en `RAG_CROSS_ENCODER_URL`. Obsigon
comprueba `GET /health` y envía `POST /rerank` con `query` y `documents`; acepta
respuestas `scores[{id,score}]` o `results[{index,relevance_score}]`. La URL debe
ser loopback y cualquier fallo conserva el ranking de fusión anterior.

En el modo determinista, una pregunta con memoria pasa primero por una llamada
local de temperatura cero que devuelve exclusivamente la pregunta autónoma.
Sólo esa pregunta alimenta embedding, FTS5 y reranking; si la llamada falla se
usa la pregunta literal y se registra la degradación. El ranking sigue operando
sobre chunks. Una vez elegidos, `RAG_READ_LARGE` sustituye su contenido para el
prompt por la sección completa y conserva `matched_content` para auditar qué
fragmento produjo el acierto. La API acepta `frontmatter` como mapa de filtros
AND; un tag padre también coincide con descendientes (`proyecto` con
`proyecto/clinica`).

Con `RAG_AGENT_MODE=true`, Obsigon descompone primero la pregunta y ejecuta una
búsqueda híbrida por subpregunta. Después el modelo puede usar `search_notes`,
`open_note`, `read_section`, `neighbors`, filtros por tag/frontmatter/fecha,
`recent` y `grep`, hasta terminar con `finish` o alcanzar el límite. Las notas y
secciones sólo se abren si están indexadas y su ruta permanece dentro del vault;
los filtros originales de la consulta se aplican también a las herramientas.
La evidencia reunida conserva el Verificador y el Redactor existentes, incluido
el streaming final. Este modo requiere un modelo competente para seguir el
protocolo JSON; se recomienda `gpt-oss:latest` y no el perfil de 0,8 B.

El watcher vigila el vault por eventos (fsnotify), ignora `.obsidian`,
`_plantillas` y `_templates`, agrupa ráfagas por debounce y reindexa sólo las
rutas tocadas con `IndexPaths` en vez de enumerar el vault completo. Una
reconciliación completa poco frecuente repara eventos perdidos (desbordamientos,
unidades de red, suspensión del equipo). Si fsnotify no puede vigilar el vault
cae a un sondeo de respaldo que sigue pasando por el mismo debounce. Mantiene
una única indexación automática en curso. El botón de reconstrucción manual
muestra fase, archivo y progreso real, y permite cancelar.
`/health/live` comprueba vida del proceso y `/health/ready` verifica SQLite y el
generador local. `/api/status` informa de build, límites, reranking, watcher,
vault e índices; `/api/retrieval/trace` devuelve la última traza consolidada.
El historial usa `limit`/`offset`, y los turnos `limit`/`before_id`.

### Persistencia, concurrencia e índices incrementales

Los cambios de vault y de perfil de chat se serializan con las consultas en
curso. Un cambio de vault construye y publica conjuntamente el nuevo store,
indexador, recuperador y gestor de memoria; SQLite no se cierra hasta que han
terminado las peticiones y tareas automáticas que usaban el runtime anterior.

SQLite usa migraciones ordenadas mediante `PRAGMA user_version`. Antes de
migrar una base existente se crea una copia consistente, incluyendo los datos
pendientes del WAL, junto a la base original con el formato
`*.backup-v<origen>-to-v<destino>-<fecha>.sqlite3`. FTS5 se reconstruye al
cambiar su versión o detectar que el número de filas no coincide; el grafo, al
cambiar su versión. En funcionamiento normal, las lecturas usan varias
conexiones `query_only` y continúan mientras el escritor mantiene una
transacción WAL. Las escrituras pasan por una única conexión con
`_txlock=immediate`.

El índice de wikienlaces conserva todos los propietarios de cada alias,
ordenados por ruta, y un índice inverso de las notas que lo referencian. Una
alta, modificación o baja sólo recalcula las fuentes cuyo destino puede haber
cambiado, incluida la promoción de un alias previamente ensombrecido. La
reconstrucción total se conserva para migraciones y comprobaciones de
integridad.

## Privacidad y seguridad

- `OLLAMA_BASE_URL` sólo admite `localhost` o una dirección IP de loopback.
- Los modelos cuyo nombre contenga `cloud` se rechazan antes de arrancar.
- `APP_HOST` tampoco puede exponer el servidor fuera de loopback.
- El servidor rechaza `Host` no local y mutaciones cross-origin.
- SQLite, los vectores, las preguntas y el contenido del vault permanecen en el
  equipo local.
- Los errores enviados al cliente son genéricos; el detalle se registra en el
  proceso con el mismo `request_id` que devuelve la respuesta.

## Estructura

```text
app/              frontend empotrado, cliente API modular y pruebas Node
cmd/server/       arranque HTTP y sello de versión
cmd/evaluate/     evaluación y comparación graph_off/graph_on_1hop/router_real
docs/             notas de implementación y hoja de ruta
evaluations/      datasets hospitalario y sintético
internal/config/  configuración y barreras de privacidad
internal/db/      SQLite, FTS5, grafo, proyectos y conversaciones
internal/httpapi/ rutas, SSE, estáticos, errores y seguridad local
internal/ollama/  cliente local de embeddings, chat y streaming
internal/rag/     recuperación, HNSW, reranker, router, watcher y memoria
vault_demo/       vault hospitalario autónomo de demostración
vault_ci/         fixture sintético de regresión y grafo
```

## Verificación

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
go test ./internal/rag -run '^$' -bench BenchmarkVectorScan10K -benchtime=1x
go test ./internal/rag -run '^$' -bench BenchmarkSearchDocuments -benchtime=100x
go test ./internal/db -run 'Concurren|Graph|Alias'
go test ./internal/rag -run 'HNSW|ANN|SearchAndGraph'
go test ./internal/rag -run '^$' -bench 'BenchmarkSearchDocuments10K|BenchmarkANNSearch10K' -benchtime=100x
```

La CI ejecuta esas comprobaciones con Go `1.23.x` en `ubuntu-latest` y
`windows-latest`, además de `govulncheck` y una tarea Linux separada con
`go test -race ./...`. También compila y arranca el binario desde un directorio
temporal y comprueba que `/`, todos los módulos ES de `/static/*.js` y
`/static/styles.css` se sirven desde los recursos empotrados. El detector usa
CGO sólo en su tarea; el artefacto
distribuible se sigue validando deliberadamente con `CGO_ENABLED=0`.

Los benchmarks sintéticos `BenchmarkVectorScan10K`, `50K` y `100K` miden la
búsqueda exhaustiva mono-hilo y reportan el tamaño aproximado del conjunto de
vectores. `BenchmarkSearchDocuments10K/50K/100K` ejercen el camino real de
consulta (`rankRows`: filtros baratos primero, coseno sólo de los
supervivientes, reparto por rangos entre workers) con un umbral de similitud
realista y reportan `p50-us`, `p95-us` y `working-set-MiB`.
`BenchmarkANNSearch10K` mide el HNSW con los mismos vectores y añade
`ann-MiB`. El recorrido exacto puede seguir siendo más rápido en corpus
pequeños y equipos con muchos núcleos; por eso `RAG_ANN=false` es el valor
predeterminado y conviene comparar ambos benchmarks con el tamaño real del
vault antes de activarlo.

El almacén vectorial recarga el snapshot en memoria de forma incremental: ante
un cambio del índice sólo decodifica los fragmentos con id nuevo y descarta los
eliminados, y recae en la carga completa en la primera carga, si cambia la
dimensión o si una reindexación no deja filas supervivientes.

Cuando ANN está activo, el mismo cambio marca las bajas como tombstones e
inserta los ids nuevos en el HNSW. Al superar un 20 % de tombstones se compacta
el grafo. El archivo persistido incluye versión, dimensión, parámetros y firma
del snapshot; un archivo corrupto no hace fallar la consulta, sino que registra
la degradación y usa el ranking exacto.

`cmd/evaluate` admite un modo determinista sin red: `-record` graba las
respuestas de Ollama en `evaluations/fixtures/<dataset>.json`; `-fixtures <ruta>`
reproduce la evaluación desde ese archivo; `-check` compara las métricas con
`evaluations/thresholds.json` y sale con código ≠ 0 si alguna cae por debajo de
su piso. La CI ejecuta esa puerta con el fixture de `vault_ci` cuando existe.

Las evaluaciones con Ollama se pueden ejecutar por separado:

```powershell
go run ./cmd/evaluate -vault ./vault_demo `
  -dataset ./evaluations/hospital_typos_questions.json
go run ./cmd/evaluate -vault ./vault_ci `
  -dataset ./evaluations/ci_questions.json -compare-strategies
go run ./cmd/evaluate -vault ./vault_ci `
  -dataset ./evaluations/ci_questions.json -compare-phase1
```

El evaluador ejecuta recuperación, Verificador y Redactor. Los grupos de
alternativas de `expected_terms` se comprueban sobre la respuesta final y las
citas deben apuntar a una fuente recuperada. El informe separa las métricas de
recuperación (`document_recall`, `ndcg_at_k`, MRR), respuesta
(`answer_term_recall`, `citation_valid_rate`) y resultado completo
(`end_to_end_pass_rate`). `passes` representa ahora casos correctos de extremo
a extremo, incluidas las abstenciones esperadas. Los casos pueden incluir
`memory`, `expected_rewrite_terms` y `frontmatter`; el informe expone además
`recall_at_k` y `query_rewrite_pass_rate`.
