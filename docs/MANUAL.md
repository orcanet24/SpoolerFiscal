# Spooler Fiscal Central — Manual completo (actualizado 2026-11-24)

> **Resumen de cambios recientes (v1.0.0, build 2026-11-24):**
> - ✅ **Protocolo TFHKA real**: framing `STX + comando + ETX + LRC` (no texto plano). Implementado en `internal/fiscal/stxframe.go`.
> - ✅ **Tests end-to-end pasando**: 3 estaciones concurrentes, correlativo único, reinicio preserva BD.
> - ✅ **Deploy atómico en `C:\job\spooler`**: un solo binario `server.exe` (~13 MB), 3 scripts CMD (`instalar.cmd`, `verificar.cmd`, `desinstalar.cmd`).
> - ✅ **Cliente/agente probado**: `cliente.exe` (~8 MB) funciona contra el servicio real.
> - ✅ **Documentos no fiscales** funcionan e imprimen en papel real.
> - ⚠️ **Facturas fiscales (ítems + 101)** devuelven NAK (0x15) — pendiente configuración HKA80 (ver sección 12).

---

## 1. Arquitectura y componentes

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                         PC SERVIDOR (impresora HKA80 en COM1)              │
│  ┌─────────────────┐    STX/ETX/LRC    ┌────────────────────────────────┐  │
│  │  server.exe     │ ◄────────────────► │  The Factory HKA HKA80         │  │
│  │  (servicio      │     9600 8N1      │  USB\VID_28E9&PID_018A\...     │  │
│  │   Windows)      │                    │  COM1                          │  │
│  └────────┬────────┘                    └────────────────────────────────┘  │
│           │ HTTP :8080 (REST + token)                                      │
│           ▼                                                                 │
│  ┌────────────────────────────────────────────────────────────────────┐     │
│  │  SQLite embebido (data\spooler.db):                                │     │
│  │  - documents        (auditoría fiscal completa, nunca se purga)    │     │
│  │  - command_log      (hashes, latencia, resultado por comando)      │     │
│  │  - idempotency_keys (claves de reintento, retención 30 días)       │     │
│  │  - print_jobs       (cola duradera REST con estado y avance)       │     │
│  └────────────────────────────────────────────────────────────────────┘     │
└─────────────────────────────────────────────────────────────────────────────┘
           ▲
           │ HTTP :8080 (token Bearer)
           │
┌──────────┴──────────────────────────────────────────────────────────────────┐
│                         PC ESTACIÓN (Profit Plus)                          │
│  ┌─────────────────┐    COM virtual     ┌────────────────────────────────┐  │
│  │  Profit Plus    │ ◄────────────────► │  cliente.exe (agente)          │  │
│  │  (TFHKA, COM3)  │     9600 8N1      │  (servicio Windows)            │  │
│  └─────────────────┘    com0com         └────────────────────────────────┘  │
└─────────────────────────────────────────────────────────────────────────────┘
           │
           ▼
┌─────────────────────────────────────────────────────────────────────────────┐
│                         SISTEMAS EXTERNOS (E-commerce, ERP, CRM)           │
│  curl / fetch / http client  ──► POST /v1/invoices, /v1/commands           │
└─────────────────────────────────────────────────────────────────────────────┘
```

**Un solo puerto HTTP (8080)** para todo: agentes, ecommerce, panel `/admin`, monitoreo.

---

## 2. Despliegue rápido (CMD, sin PowerShell)

### 2.1 Estructura final en `C:\job\spooler`

```
C:\job\spooler\
├── server.exe              # binario único (~13 MB, Go static, sin deps)
├── server.new.exe          # binario nuevo (instalar.cmd lo usa para actualizar)
├── cliente.exe             # agente de estación (~8 MB)
├── configs\
│   └── server.yaml         # configuración (puerto, token, BD, logs)
├── data\
│   └── spooler.db          # SQLite WAL (auditoría, correlativo, idempotencia)
├── logs\
│   └── server.log          # rotativo 20 MB × 10
├── instalar.cmd            # instala/actualiza + arranca (ejecutar como Admin)
├── verificar.cmd           # health + sonda S2 + /v1/printer
└── desinstalar.cmd         # para y elimina servicio (datos y config se conservan)
```

### 2.2 Instalación (ejecutar como Administrador)

```cmd
C:\job\spooler\instalar.cmd
```

**Qué hace `instalar.cmd`:**
1. Para el servicio si está corriendo
2. Reemplaza `server.exe` por `server.new.exe` (reintenta si está en uso)
3. Instala/actualiza el servicio `SpoolerFiscalServer` (auto-start + recovery 5/10/30 s)
4. Abre puerto 8080 en firewall (redes domain/private)
5. Arranca el servicio y espera a que la API responda
6. Muestra: health, estado impresora (sonda S2), **últimos números de control** (`/v1/printer`)

### 2.3 Verificación en cualquier momento

```cmd
C:\job\spooler\verificar.cmd
```

### 2.4 Desinstalación (ejecutar como Administrador)

```cmd
C:\job\spooler\desinstalar.cmd
```
> Los datos (`data\spooler.db`) y la configuración (`configs\server.yaml`) **no se borran**.

---

## 3. Configuración (`configs\server.yaml`)

```yaml
# Token compartido (cámbielo en producción)
token: sk-00001235986413sdgKJsdf34d546hgjAF

# Impresora fiscal HKA80 (The Factory HKA)
printer:
  driver: tfhka
  transport: serial
  port:
    name: COM1
    baud_rate: 9600
    data_bits: 8
    parity: none
    stop_bits: "1"
    flow_control: none
  # Framing real de la HKA80: STX/ETX/LRC por comando
  read_gap_ms: 100                    # silencio que cierra la respuesta
  response_timeout_ms: 20000
  max_response_bytes: 32768
  write_settle_ms: 0
  status_probe: "S2"                  # sonda de estado
  command_terminator: "\r\n"          # el driver envuelve en STX/ETX/LRC
  open_retry_ms: 500

# API HTTP
http:
  listen: 0.0.0.0:8080
  max_body_bytes: 262144
  read_timeout_ms: 15000
  write_timeout_ms: 60000
  idempotency_retention_days: 30

# Cola y guardián
queue:
  max_pending: 256
  default_timeout_ms: 30000
  max_timeout_ms: 120000
  status_probe_interval_seconds: 15
  document_lock_timeout_ms: 120000
  correlative_sync_max_age_ms: 10000
  auto_close_open_document: true

guard:
  block_on_paper_out: true
  block_on_memory_full: true
  block_on_error: true

database:
  path: C:\job\spooler\data\spooler.db
  log_commands: true
  retention_days: 365

logging:
  level: info
  file: C:\job\spooler\logs\server.log
  console: true
  max_size_mb: 20
  max_backups: 10
  max_age_days: 30
  compress: true
```

---

## 4. API HTTP — Referencia completa

Base: `http://<servidor>:8080`  
Autenticación: `Authorization: Bearer <token>` (o `X-Spooler-Token`)

### 4.1 Endpoints

| Método | Ruta | Descripción |
|--------|------|-------------|
| `GET` | `/v1/health` | Salud y versión (público, sin token) |
| `GET` | `/v1/status?probe=true` | Estado impresora, cola, documento abierto, contadores |
| `GET` | `/v1/printer` | Modelo, serial, RIF, firmware, memoria fiscal, **últimos números** |
| `GET` | `/v1/documents?limit=50` | Historial fiscal registrado |
| `POST` | `/v1/commands` | Comando fiscal crudo (equivalente a `SendCommand`) |
| `POST` | `/v1/invoices` | Factura completa (líneas TFHKA), atómica, turno exclusivo |
| `POST` | `/v1/invoices/structured` | Factura desde ítems JSON, compilada a TFHKA |
| `GET` | `/v1/jobs?status=` | Cola duradera de facturas REST |
| `GET` | `/v1/jobs/{id}` | Detalle y resultado de un trabajo |
| `POST` | `/v1/jobs/{id}/cancel` | Cancela trabajo que no llegó a papel |
| `POST` | `/v1/jobs/{id}/retry` | Reencola trabajo sin líneas impresas |
| `GET` | `/admin` | Panel de operación (HTML, pide token en el navegador) |

### 4.2 Ejemplos curl

```bash
# Health (sin token)
curl http://127.0.0.1:8080/v1/health

# Estado impresora + sonda S2
curl -H "Authorization: Bearer $TOKEN" "http://127.0.0.1:8080/v1/status?probe=true"

# Últimos números de control (S1 + S5)
curl -H "Authorization: Bearer $TOKEN" "http://127.0.0.1:8080/v1/printer"
```

**Respuesta `/v1/printer` (ejemplo real):**
```json
{
  "brand": "tfhka",
  "model": "TFHKA",
  "serial": "Z7C7022079",
  "firmware": "n/d",
  "fiscal_rif": "J-503895241",
  "port": "COM1 @ 9600 8N1",
  "fiscal_memory_total_mb": 1908,
  "fiscal_memory_free_mb": 1384,
  "last_invoice_number": 10850,
  "last_credit_note": 259,
  "last_debit_note": 0,
  "last_non_fiscal_doc": 921,
  "last_z_number": 893
}
```

---

## 5. Emisión de facturas

### 5.1 POST /v1/invoices — Líneas TFHKA nativas

```bash
curl -X POST http://127.0.0.1:8080/v1/invoices \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -H "Idempotency-Key: pedido-1001" \
  -d '{
    "station": "ECOM-01",
    "client_ref": "pedido-1001",
    "lines": [
      "iR*J-12345678-9",
      "iS*CLIENTE C.A.",
      "!   1.000     10.00CAFE",
      "\"  2.000      5.00PAN",
      "101"
    ],
    "timeout_ms": 60000
  }'
```

**Respuesta 201:**
```json
{
  "station": "ECOM-01",
  "session": "api:ECOM-01",
  "order_ref": "pedido-1001",
  "completed": true,
  "invoice_number": 10851,
  "document_type": "invoice",
  "sequence": 42,
  "issued_at": "2026-11-24T18:15:23Z",
  "duplicate": false,
  "lines": [
    {"index": 0, "response": "...", "sequence": 41},
    {"index": 1, "response": "...", "sequence": 42},
    {"index": 2, "response": "0|Factura 10851|Total: 20.00|Impreso:10851", "sequence": 43}
  ],
  "elapsed_ms": 1240
}
```

### 5.2 POST /v1/invoices/structured — Desde ítems JSON

```bash
curl -X POST http://127.0.0.1:8080/v1/invoices/structured \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -H "Idempotency-Key: pedido-1002" \
  -d '{
    "station": "ECOM-01",
    "client_ref": "pedido-1002",
    "customer": {"rif": "J-12345678-9", "name": "CLIENTE C.A.",
                 "address": "Av. Principal", "phone": "0212-1234567"},
    "items": [
      {"description": "CAFE", "quantity": 1, "unit_price": 10.00, "tax": "general"},
      {"description": "PAN",  "quantity": 2, "unit_price": 5.00,  "tax": "reduced"}
    ],
    "discount": 1.00,
    "payments": [{"method": "tarjeta", "amount": 24.00}],
    "totals": {"total": 24.00}
  }'
```

**Compila la secuencia canónica TFHKA:**
```
iR*J-12345678-9
iS*CLIENTE C.A.
i01Av. Principal
i020212-1234567
!   1.000     10.00CAFE
"   2.000      5.00PAN
3
7001.00
103tarjeta
10024.00
101
```

### 5.3 Formato de líneas TFHKA (ítems)

| Prefijo | Alícuota | Formato |
|---------|----------|---------|
| `!` | General 16% | `!   1.000     10.00CAFE` |
| `"` | Reducida 8% | `"   1.000     10.00PAN` |
| `#` | Adicional 30% | `#   1.000     10.00LICOR` |
| espacio | Exento 0% | `    1.000     10.00AGUA` |

Formato fijo: `prefijo + cantidad(8.3) + precio(10.2) + descripción(40)`.

### 5.4 Idempotencia (obligatoria en producción)

- Envíe siempre `Idempotency-Key` (ej. número de pedido).
- Misma clave + mismo cuerpo → devuelve respuesta original con `Idempotent-Replay: true`, **no reimprime**.
- Misma clave + cuerpo distinto → `409 idempotency_conflict`.
- Sin clave → reintento puede duplicar la factura.

---

## 6. Comandos fiscales sueltos (POST /v1/commands)

Para RIF cliente, pagos, notas, reportes X/Z, documentos no fiscales:

```bash
# Cliente antes de ítems (obligatorio en TFHKA)
curl -X POST .../v1/commands -d '{"station":"ECOM-01","command_base64":"aVIqSjEyMzQ1Njc4LTkK"}'  # iR*
curl -X POST .../v1/commands -d '{"station":"ECOM-01","command_base64":"aVMqQ0xJRU5URSBGLkEuCg=="}' # iS*

# Documento no fiscal (funciona e imprime papel real)
curl -X POST .../v1/commands -d '{"station":"ECOM-01","command_base64":"ODAk"}'       # 80$
curl -X POST .../v1/commands -d '{"station":"ECOM-01","command_base64":"ODAhUFJVRUJB"}' # 80!PRUEBA
curl -X POST .../v1/commands -d '{"station":"ECOM-01","command_base64":"ODE="}'       # 81

# Reportes
curl -X POST .../v1/commands -d '{"station":"ECOM-01","command":"U0X"}'  # X
curl -X POST .../v1/commands -d '{"station":"ECOM-01","command":"U0Z"}'  # Z
```

> **`command_base64`** evita problemas de escape JSON. El servidor devuelve `response_base64` con los bytes íntegros de la impresora (incluyendo framing STX/ETX/LRC).

---

## 7. Agente de estación (cliente.exe)

### 7.1 Configuración (`configs\agent.yaml`)

```yaml
station_id: CAJA-01              # único por estación
server:
  address: http://IP-SERVIDOR:8080
  token: sk-00001235986413sdgKJsdf34d546hgjAF
  request_timeout_ms: 30000
bridge:
  local_queue_size: 16
  max_send_backoff_ms: 2000
virtual_com:
  name: COM4                     # extremo del agente (com0com)
  baud_rate: 9600
```

### 7.2 Instalación en la estación

1. Instale **com0com** (driver firmado): `setupc.exe install PortName=COM3 PortName=COM4`
   - `COM3` → Profit Plus
   - `COM4` → agente (`virtual_com.name`)
2. Copie `cliente.exe`, `configs\agent.yaml`, carpeta `logs\`
3. Pruebe: `cliente.exe -console -config configs\agent.yaml`
   - Debe mostrar: `COM virtual abierto` + `conectado al spooler`
4. Instale servicio (Admin): `cliente.exe -install -config C:\SpoolerFiscal\configs\agent.yaml`

### 7.3 Profit Plus

- Impresora: **TFHKA / The Factory HKA**
- Puerto: **COM3** (el par de com0com)
- Parámetros: **9600 8N1**, sin control de flujo

---

## 8. Concurrencia, cola y correlativo

- **Una sola cola global FIFO** (SQLite `print_jobs`, 256 pendientes máx).
- **Turno exclusivo por factura**: la estación que abre (ítem o cliente) retiene el turno hasta `101`.
- **Sincronización de correlativo**: antes de cada factura el spooler lee `S1` real y concilia con su BD.
- **Idempotencia por `Idempotency-Key`**: reintentos de red no duplican.
- **Tests que lo garantizan**:
  - `TestConcurrentStationsKeepSingleCorrelative`: 3 estaciones, 1 correlativo único
  - `TestRestartPreservesCorrelativeAndHistory`: reinicio preserva BD y correlativo

**Capacidad real**: la impresora imprime 1-3 s/factura → **0.3-1 factura/s total**, sin importar cuántas cajas.

---

## 9. Auditoría y base de datos

**`data\spooler.db` (SQLite WAL, nunca se purga `documents`):**

| Tabla | Contenido |
|-------|-----------|
| `documents` | `sequence`, `doc_type`, `doc_number`, `station_id`, `session_id`, `issued_at`, `request_sha256`, `response_sha256`, `response_text` |
| `command_log` | hashes, latencia, resultado, error por comando (retención 365 días) |
| `idempotency_keys` | claves de reintento (retención 30 días) |
| `print_jobs` | cola duradera REST con estado, avance, líneas, error |

**Consultas útiles:**
```sql
SELECT sequence, doc_type, doc_number, station_id, issued_at
FROM documents ORDER BY sequence DESC LIMIT 20;

SELECT doc_number, station_id, issued_at FROM documents
WHERE doc_type = 'invoice' AND date(issued_at) = date('now')
ORDER BY doc_number;
```

**Respaldo**: con servicio detenido, copie `data\spooler.db` (`-wal`/`-shm` si existen).

---

## 10. Monitoreo y panel

- **Health check**: `GET /v1/health` (para load balancer / systemd / sc)
- **Estado completo**: `GET /v1/status?probe=true` (cola, documento abierto, contadores, motivo de bloqueo)
- **Panel web**: `http://<servidor>:8080/admin` (pide token, lo guarda solo en la pestaña)
  - Estado impresora, cola, últimos trabajos con **Cancelar** / **Reintentar**
  - Últimos documentos emitidos

**Log rotativo**: `logs\server.log` (20 MB × 10, compresión, 30 días).  
**Métricas por minuto**: documentos, comandos, errores, duplicados, latencia promedio/máx, profundidad de cola, estado.

---

## 11. Seguridad

- **Token compartido** (`token`) + **token por estación** (`station_tokens`):
  - Estación con token propio **solo** emite con su token (no puede suplantar a otra).
  - Token compartido **no representa** a estación con token propio.
- **mTLS opcional**: `tls.enabled` + `tls.ca_file` → servidor exige certificado de cliente.
- **ACL en Windows**: al arrancar, el servicio restringe `configs\server.yaml` y `data\` a SYSTEM + Administradores + usuario del proceso (usa SIDs, funciona en ES/EN).
- **No exponga el puerto 8080 a Internet**.

---

## 12. Estado actual: Fiscal vs No Fiscal

| Operación | Estado | Detalle |
|-----------|--------|---------|
| **Documento no fiscal** (80$ / 80! / 81) | ✅ **FUNCIONA** | Imprime en papel real, ACK `0x06` |
| **Consultas S1/S2/S3/S4/S5** | ✅ **FUNCIONA** | Devuelven datos reales (serial, RIF, correlativos, memoria) |
| **Factura fiscal** (ítems `!`/`"`/`#` + `101`) | ❌ **NAK (0x15)** | La HKA80 rechaza la secuencia fiscal |
| **Nota crédito/débito** (`d1`/`d2` + `101`) | ❌ **NAK (0x15)** | Igual que factura |

### 12.1 Causas probables del NAK en fiscal

La impresora acepta no-fiscal pero rechaza fiscal. **No es bug del spooler** (entrega bytes exactos y devuelve respuesta cruda). Posibles causas en la HKA80:

1. **Falta apertura de jornada fiscal** (comando `U0`/`U1` o menú "Apertura Jornada")
2. **Modo prueba activo** (ver `S2`: `status_byte=0x01`-`0x03` = modo prueba)
3. **Cliente obligatorio antes de ítems** (TFHKA exige `iR*`/`iS*` antes del primer `!`)
4. **Falta configuración de tasas/alícuotas** en la impresora
5. **Memoria fiscal bloqueada** (S5 dice 1384 KB libres de 1908 → OK)

### 12.2 Próximos pasos para desbloquear fiscal

```bash
# 1. Ver modo actual (S2 ya hecho: status_byte=0 = "Modo fiscal - en espera" ✅)

# 2. Probar apertura de jornada / modo fiscal
POST /v1/commands {"command_base64":"VTI="}     # U2 - estado extendido
POST /v1/commands {"command_base64":"VTQ="}     # U4 - modo fiscal

# 3. Enviar cliente ANTES del ítem (obligatorio en TFHKA)
POST /v1/commands {"command_base64":"aVIqSjEyMzQ1Njc4LTkK"}  # iR*RIF
POST /v1/commands {"command_base64":"aVMqQ09OU1VNSURPUiBGSU5BTAo="}  # iS*NOMBRE

# 4. Luego el ítem
POST /v1/commands {"command_base64":"IT0gIDEuMDAwICAgMTAuMDBDQUZFCg=="}  # ! 1.000 10.00CAFE
POST /v1/commands {"command_base64":"MTAx"}  # 101
```

Si con cliente previo sigue NAK → revisar en la impresora física: menú "Modo Fiscal", "Apertura Jornada", "Configuración Tasas". El manual del SDK (PDF en `C:\Users\Usuario10\Desktop\TFHKA Venezuela - VB SDK\Library\`) detalla la secuencia exacta.

---

## 13. Actualización

1. `sc stop SpoolerFiscalServer`
2. Respalde `data\` y `configs\`
3. Reemplace `server.new.exe` (o recompile: `go build -trimpath -ldflags "-s -w" -o server.exe ./cmd/server`)
4. Ejecute **como Admin**: `C:\job\spooler\instalar.cmd`
   - Reemplaza binario, reinstala servicio, arranca, verifica
5. Verifique: `C:\job\spooler\verificar.cmd`
6. Haga una venta de control (no fiscal o fiscal si ya está desbloqueada)

---

## 14. Diagnóstico rápido

| Síntoma | Qué revisar |
|---------|-------------|
| `sc start` → `ERROR 5` | Ejecute `instalar.cmd` / `verificar.cmd` **como Administrador** |
| `verificar.cmd` → `curl: (7) Failed to connect` | Servicio detenido o puerto 8080 bloqueado |
| `/v1/printer` → `serial: ""` o `rif: ""` | Impresora no respondió S1; revise cable, COM1, 9600 8N1 |
| `commands_failed` subiendo | Ver `server.log`; NAK = impresora rechaza comando |
| `document_open: true` sin operador | Factura colgada; el spooler intenta cierre de recuperación a los 120 s |

---

## 15. Contrato formal y ejemplos

- **OpenAPI**: `docs/openapi.yaml` (servido en `/docs/openapi.yaml`)
- **Ejemplos en 6 lenguajes**: `examples/` (curl, PHP, Go, Python, Node.js, Rust)
- **Protocolo TFHKA completo**: `docs/PROTOCOLO_TFHKA.md`
- **Instalación paso a paso**: `docs/INSTALACION.md`
- **Operación diaria**: `docs/OPERACION.md`
- **Puesta en marcha con impresora real**: `docs/PUESTA_EN_MARCHA.md`

---

> **Última actualización**: 2026-11-24  
> **Versión del binario**: 1.0.0 (build `go build -trimpath -ldflags "-s -w"`)  
> **Protocolo**: TFHKA real con framing STX/ETX/LRC (no texto plano)  
> **Deploy**: `C:\job\spooler` — un binario, tres CMD, un servicio Windows