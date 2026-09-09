# Plan de despliegue multiusuario — Fase 0

Entregable de la fase 0 del proyecto de servidor local. Convierte la aplicación
actual, diseñada como herramienta de un solo puesto, en un servicio de red con
usuarios identificados, sin depender de la compra del hardware.

Todo el trabajo descrito puede completarse mientras se tramita el expediente.

---

## Principio de diseño

**La aplicación sigue escuchando solo en loopback.** No se expone a la red.
Delante se coloca un proxy inverso que termina TLS, autentica contra el
directorio corporativo y reenvía a `127.0.0.1:8000`.

Esto no es un rodeo: es lo que hace **seguro** confiar en una cabecera de
identidad. Si la aplicación fuera alcanzable directamente desde la red,
cualquiera podría enviar la cabecera y suplantar a otro usuario. Al estar
enlazada a loopback, el proxy es el único camino posible.

`config.NormalizeLoopbackHost` ya fuerza `APP_HOST` a una dirección de bucle
local y rechaza cualquier otra. **Esa función no se toca.** Es la garantía de
todo el modelo.

---

## Corrección respecto al planteamiento inicial

La idea de "no tocar `security.go` y que el proxy reescriba `Host` a loopback"
no funciona. `originMatches` (security.go:92) compara el host del `Origin`
enviado por el navegador con el host de la petición. Reescribiendo `Host` a
`127.0.0.1`, el `Origin: https://rag.centro.local` dejaría de coincidir y todas
las peticiones POST, PATCH y DELETE devolverían 403.

El cambio correcto es el inverso: **el proxy conserva el `Host` original** y la
aplicación aprende a aceptar una lista de hosts permitidos, con loopback como
valor por defecto. Así toda la lógica antifalsificación de peticiones
(`Sec-Fetch-Site` y `Origin`) sigue funcionando sin modificarse, porque origen y
host vuelven a coincidir.

---

## Lote 1 — Perímetro

**Objetivo:** que la aplicación acepte peticiones reenviadas por el proxy sin
perder ninguna de las protecciones actuales.

### Cambios en el código

**`config.go`**

Añadir a `Settings`:

```go
AllowedHosts []string // RAG_ALLOWED_HOSTS, separados por comas
AuthMode     string   // RAG_AUTH_MODE: "single" (por defecto) | "header"
AuthHeader   string   // RAG_AUTH_HEADER, por defecto "X-Auth-User"
```

`AllowedHosts` vacío significa "solo loopback", que es el comportamiento actual.

**`security.go`**

Sustituir la comprobación de la línea 17. Hoy:

```go
if !ok || !isLoopback(requestHost) {
```

Pasa a consultar una lista, manteniendo loopback siempre permitido:

```go
func hostAllowed(host string, allowed []string) bool {
    if isLoopback(host) {
        return true
    }
    for _, candidate := range allowed {
        if strings.EqualFold(strings.TrimSpace(candidate), host) {
            return true
        }
    }
    return false
}
```

`Security` pasa a recibir la configuración. El resto de la función —cabeceras de
respuesta, `Sec-Fetch-Site`, `originMatches`— **no se modifica**.

### Configuración del proxy

Ejemplo con nginx. En un centro donde informática ya opera Apache,
`mod_authnz_ldap` resuelve lo mismo y suele ser más fácil de aprobar.

```nginx
server {
    listen 443 ssl;
    server_name rag.centro.local;

    ssl_certificate     /etc/ssl/certs/rag.crt;
    ssl_certificate_key /etc/ssl/private/rag.key;

    auth_basic           "Documentación PDPCM";
    auth_basic_user_file /etc/nginx/rag.htpasswd;   # o auth_request contra LDAP

    location / {
        proxy_pass http://127.0.0.1:8000;

        # El Host original se conserva: es lo que hace que Origin siga cuadrando
        proxy_set_header Host              $host;
        proxy_set_header X-Forwarded-For   $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;

        # Sobrescribe SIEMPRE, incluso si el cliente la envía. Sin esta línea
        # cualquiera puede suplantar a otro usuario.
        proxy_set_header X-Auth-User       $remote_user;

        # Necesario para el streaming de respuestas (SSE)
        proxy_buffering    off;
        proxy_read_timeout 300s;
    }
}
```

`APP_HOST` se mantiene en `127.0.0.1`. `RAG_ALLOWED_HOSTS=rag.centro.local`.

### Criterio de aceptación

- Desde otro equipo de la red, `https://rag.centro.local` pide credenciales y funciona.
- Desde otro equipo, `http://<ip-del-servidor>:8000` no responde: rechazado por
  la dirección de escucha, no por una cabecera.
- Las peticiones POST desde el navegador siguen funcionando (`Origin` válido).
- Una petición con `Host` falsificado devuelve 400.
- El streaming de respuestas no se corta ni se acumula.

**Esfuerzo: 1–2 días**, mayoritariamente configuración.

---

## Lote 2 — Identidad en la aplicación

**Objetivo:** que cada petición sepa quién la hace, sin romper la instalación
monousuario ni los 111 tests existentes.

### Cambios en el código

**Fichero nuevo `identity.go`** (paquete `httpapi`):

```go
type contextKey string

const userKey contextKey = "rag.user"

const SingleUser = "local"

func Identity(next http.Handler, settings config.Settings) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        user := SingleUser
        if settings.AuthMode == "header" {
            user = normalizeUser(r.Header.Get(settings.AuthHeader))
            if user == "" {
                writeJSON(w, http.StatusUnauthorized,
                    map[string]any{"detail": "Usuario no identificado."})
                return
            }
        }
        ctx := context.WithValue(r.Context(), userKey, user)
        next.ServeHTTP(w, r.WithContext(ctx))
    })
}

func UserFromContext(ctx context.Context) string {
    if value, ok := ctx.Value(userKey).(string); ok && value != "" {
        return value
    }
    return SingleUser
}
```

`normalizeUser` recorta espacios, pasa a minúsculas y rechaza cualquier cosa que
no case con `^[a-z0-9._-]{1,64}$`.

**`server.go`**: encadenar `Identity` después de `Security` en `Handler()`.

**`server.go` (`status`, línea 181 aprox.)**: incluir el usuario identificado en
la respuesta, para poder verificar el montaje de un vistazo.

### Por qué `single` es el valor por defecto

Con `RAG_AUTH_MODE=single`, todo se atribuye al usuario `local` y el
comportamiento es idéntico al actual. Eso mantiene verde la batería de tests sin
tocarla y permite desplegar los lotes 4 y 5 antes de activar la autenticación.

### Criterio de aceptación

- `GET /api/status` devuelve el usuario identificado.
- Con `RAG_AUTH_MODE=header` y sin cabecera, la respuesta es 401.
- Con `RAG_AUTH_MODE=single`, los 111 tests siguen pasando sin modificaciones.

**Esfuerzo: 1 día.**

---

## Lote 3 — Control de admisión

**Objetivo:** que la carga concurrente produzca espera ordenada en lugar de
errores por tiempo de espera.

Este es el lote con mejor relación entre esfuerzo e impacto. Conviene hacerlo
aunque el resto se retrase.

### Problema actual

`net/http` lanza una goroutine por petición sin techo. No existe semáforo ni
cola en ninguna parte del proyecto. Treinta consultas simultáneas son treinta
llamadas concurrentes a Ollama, y como `client.go` fija un `Timeout`, el modo de
fallo bajo carga es que las consultas **fallen**, no que esperen.

### Cambios en el código

**Fichero nuevo `admission.go`**:

```go
type Admission struct {
    slots chan struct{}
    wait  time.Duration
}

func NewAdmission(limit int, wait time.Duration) *Admission {
    return &Admission{slots: make(chan struct{}, max(1, limit)), wait: wait}
}

func (a *Admission) Acquire(ctx context.Context) (func(), bool) {
    timer := time.NewTimer(a.wait)
    defer timer.Stop()
    select {
    case a.slots <- struct{}{}:
        return func() { <-a.slots }, true
    case <-timer.C:
        return nil, false
    case <-ctx.Done():
        return nil, false
    }
}
```

**`server.go`**: envolver únicamente `query` (línea 421) y `queryStream`
(línea 462). Si no se obtiene hueco, responder **503 con cabecera
`Retry-After`**, no un error genérico: el frontend puede reintentar.

**Segundo semáforo, independiente y de tamaño 1, para la indexación**, de modo
que una reindexación no compita con las consultas por el acelerador.

**`config.go`**:

```
RAG_MAX_CONCURRENT_QUERIES   por defecto 3
RAG_ADMISSION_WAIT_SECONDS   por defecto 30
```

### Criterio de aceptación

Lanzar 20 consultas en paralelo contra el equipo actual:

- Antes del cambio: se esperan errores por tiempo de espera.
- Después: todas responden, con latencia creciente y sin fallos.

Guardar ambas mediciones. Son la prueba objetiva para el apartado 2 del
documento de propuesta.

**Esfuerzo: medio día.**

---

## Lote 4 — Migración de esquema a versión 2

**Objetivo:** añadir el modelo de usuarios a la base de datos.

La infraestructura ya existe: `applyMigrations` (store.go:164) aplica cada
versión en una transacción y `LastMigrationBackup` conserva copia previa. Solo
hay que añadir un caso.

### Cambios en el código

**`store.go`**

```go
LatestSchemaVersion = 2
```

En el `switch` de `applyMigrations`:

```go
case 2:
    statements = schemaV2Statements()
```

Nueva función:

```go
func schemaV2Statements() []string {
    return []string{
        `CREATE TABLE IF NOT EXISTS users (
            id TEXT PRIMARY KEY,
            display_name TEXT NOT NULL DEFAULT '',
            role TEXT NOT NULL DEFAULT 'member',
            created_at TEXT NOT NULL)`,

        `INSERT INTO users(id, display_name, role, created_at)
         VALUES('local', 'Usuario local', 'admin', datetime('now'))
         ON CONFLICT(id) DO NOTHING`,

        `ALTER TABLE conversations ADD COLUMN owner_id TEXT NOT NULL DEFAULT 'local'`,
        `ALTER TABLE projects      ADD COLUMN owner_id TEXT NOT NULL DEFAULT 'local'`,
        `ALTER TABLE projects      ADD COLUMN memory_scope TEXT NOT NULL DEFAULT 'shared'`,

        `CREATE TABLE IF NOT EXISTS project_members (
            project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
            user_id    TEXT NOT NULL REFERENCES users(id)    ON DELETE CASCADE,
            role       TEXT NOT NULL DEFAULT 'member',
            PRIMARY KEY(project_id, user_id))`,

        `INSERT INTO project_members(project_id, user_id, role)
         SELECT id, 'local', 'owner' FROM projects
         ON CONFLICT DO NOTHING`,

        `CREATE INDEX IF NOT EXISTS idx_conversations_owner_updated
            ON conversations(owner_id, updated_at DESC)`,
        `CREATE INDEX IF NOT EXISTS idx_project_members_user
            ON project_members(user_id)`,
    }
}
```

El valor por defecto `'local'` hace que toda la información existente quede
atribuida al usuario actual. La migración es reversible restaurando la copia.

**`migration_test.go`**: añadir un caso que parta de una base v1 con datos y
verifique que tras migrar todas las filas tienen `owner_id = 'local'`.

### Criterio de aceptación

- Una base de datos existente migra sin pérdida y sin filas huérfanas.
- `PRAGMA user_version` devuelve 2.
- La aplicación arranca contra una base v1 y contra una v2.

**Esfuerzo: 1 día.**

---

## Lote 5 — Ámbito de usuario en las consultas

**Objetivo:** que cada persona vea lo suyo.

### Cambios en el código

**`workspace.go`** — añadir parámetro de usuario y filtrar **en SQL**, nunca en
Go después de recuperar las filas:

| Función | Línea aprox. | Filtro |
|---|---|---|
| `ListProjects` | 49 | proyectos donde el usuario es miembro |
| `CreateProject` | 201 | fija `owner_id` y alta en `project_members` |
| `RenameProject` | 221 | exige rol `owner` |
| `DeleteProject` | 245 | exige rol `owner` |
| `ListConversations` | 319 | `owner_id = ?` |
| `ListConversationsPage` | 337 | `owner_id = ?` |
| `GetConversation` | 426 | `owner_id = ?`, si no 404 |
| `DeleteConversation` | 549 | `owner_id = ?` |

**Devolver 404, no 403**, cuando una conversación existe pero es de otro: un 403
confirma que ese identificador existe.

**`resources.go`** — los handlers (`listProjects` 244, `createProject` 257,
`listConversations` 316, `getConversation` 333, `deleteConversation` 378)
obtienen el usuario con `UserFromContext(r.Context())` y lo pasan al store.

### La decisión que no es técnica

`project_memory` es hoy un resumen compartido entre las conversaciones de un
proyecto. Con varias personas, las preguntas exploratorias de una acaban en el
contexto de otra. Puede ser conocimiento útil acumulado o ruido que degrada las
respuestas de todos.

Por eso la migración añade `projects.memory_scope` con valores `shared` y
`user`. **Se despliega en `shared`, que es el comportamiento actual, y se mide.**
No es una decisión que deba tomarse a priori.

### Criterio de aceptación

- Dos usuarios simultáneos no ven las conversaciones del otro en el listado.
- Acceder por identificador a una conversación ajena devuelve 404.
- Un usuario no propietario no puede renombrar ni borrar un proyecto.

**Esfuerzo: 2–3 días.**

---

## Lote 6 — Eliminar el estado global compartido

**Objetivo:** que la configuración de una persona no altere la de las demás.

### Problema actual

`POST /api/config/chat-profile` (server.go:79) acaba en `client.SetChatModel`
(client.go:38), que muta un campo único del cliente. `POST /api/vault/select`
(server.go:80) hace lo equivalente con el vault. **Con diez personas, quien
cambia el perfil se lo cambia a todos, en mitad de sus consultas.**

### Cambios en el código

**`client.go`** — el modelo deja de ser estado del cliente y pasa a viajar en
cada llamada. `chatModel` queda solo como valor por defecto. Afecta a las
firmas de `Chat`, `ChatStream` y a `chatRequest` (línea 291 y siguientes).

**`server.go` / `resources.go`** — el perfil de modelo pasa a ser un ajuste de
proyecto. La tabla `projects` ya tiene columnas de ajustes por agente
(`verifier_context_tokens`, `writer_context_tokens`), así que el sitio existe.

**`/api/vault/select`** — restringir a usuarios con rol `admin`. El vault es
propiedad de la instalación, no de la persona.

### Beneficio adicional

Este mismo refactor es **exactamente** el que hace falta para usar modelos
distintos en verificador y redactor. Al pasar el modelo por llamada, asignar uno
pequeño a la verificación y uno mayor a la redacción se vuelve trivial.

Advertencia operativa para cuando se haga: **o ambos modelos caben residentes a
la vez, o se usa uno solo**. Si Ollama tiene que descargar y cargar en cada
consulta, el servicio se vuelve inutilizable. Exige
`OLLAMA_MAX_LOADED_MODELS >= 2`, `keep_alive` largo y verificación con
`ollama ps`.

### Criterio de aceptación

- Dos usuarios con perfiles de modelo distintos consultan a la vez sin
  interferirse.
- Un usuario sin rol `admin` no puede cambiar el vault.

**Esfuerzo: 3–5 días.** Es el lote más invasivo; va el último por eso.

---

## Lote 7 — Encender el índice aproximado y medir

`ann.go` implementa HNSW con persistencia, parcheado incremental y filtrado
integrado, pero `RAG_ANN` está a `false` por defecto (config.go:206).

Activar `RAG_ANN=true` y comparar contra el escaneo actual sobre el mismo
conjunto de preguntas: latencia de recuperación, coincidencia de resultados en
las primeras posiciones y memoria adicional (`ANNMemoryMB` ya se expone en
`VectorStats`).

Con el volumen actual la diferencia puede ser pequeña. **El objetivo no es
ganar velocidad hoy, es tener el dato antes de que el repositorio crezca.**

**Esfuerzo: medio día más el tiempo de medición.**

---

## Orden y calendario

| Semana | Lotes | Resultado |
|---|---|---|
| 1 | 3, 1 | Servicio accesible en red con autenticación y sin fallos por concurrencia |
| 2 | 2, 4 | Identidad y esquema listos, todavía en modo monousuario |
| 3 | 5 | Separación real entre personas: **piloto con 3 usuarios** |
| 4 | 6, 7 | Configuración por proyecto y medición del índice |

El lote 3 va primero porque es medio día y elimina el fallo más visible. El
piloto de tres usuarios arranca al final de la semana 3.

---

## Riesgos

| Riesgo | Mitigación |
|---|---|
| Suplantación mediante la cabecera de identidad | La aplicación solo escucha en loopback (`NormalizeLoopbackHost`) y el proxy sobrescribe la cabecera incondicionalmente. Ambas condiciones deben verificarse tras cada cambio de configuración. |
| Migración sobre base de datos en uso | Detener el servicio, verificar la copia que genera `LastMigrationBackup` y probar antes sobre una copia. |
| El streaming se rompe tras el proxy | `proxy_buffering off` y `proxy_read_timeout` amplio. Verificar con una consulta larga real, no con `curl` a un endpoint corto. |
| Fuga de contexto entre usuarios vía memoria de proyecto | `memory_scope` desplegado en `shared`, medido antes de decidir. |
| Regresión silenciosa en el filtrado por usuario | Test que cree dos usuarios y compruebe aislamiento en cada listado, no solo en uno. |

---

## Lo que este plan **no** incluye

- Alta y baja de usuarios desde la interfaz. Los usuarios los gestiona el
  directorio corporativo; la tabla `users` solo los registra al primer acceso.
- Permisos por documento. El gancho existe (`allowedDocuments` viaja desde
  `retrieval.go:188` hasta `matchesSemanticFilters` en `ann.go:373`), pero no
  hace falta mientras todo el equipo consulte la misma documentación. Se aborda
  cuando entre un segundo servicio.
- Réplicas de la aplicación. Con diez personas un solo proceso sobra.

---

## Resumen

Entre **dos y cuatro semanas de trabajo efectivo**, sin depender de la compra.
Al terminar, la fase 0 del documento de propuesta deja de ser una previsión: se
llega a la reunión con un piloto de tres personas funcionando y con dos
mediciones que valen más que cualquier argumento —el comportamiento bajo carga
antes y después del control de admisión, y la línea base de calidad con el
modelo actual—.
