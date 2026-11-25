# Instalación y despliegue (versión atómica, CMD-only)

> **Cambio 2026-11-24**: el despliegue real está en `C:\job\spooler` con un solo
> binario `server.exe` (~13 MB) y tres scripts CMD. No hay `bin\`, no hay
> `scripts\build.ps1` en producción, no hay PowerShell obligatorio.

## 1. Estructura de producción (`C:\job\spooler`)

```
C:\job\spooler\
├── server.exe              # binario único (Go static, sin deps externas)
├── server.new.exe          # binario nuevo (instalar.cmd lo usa para actualizar)
├── cliente.exe             # agente de estación (~8 MB)
├── configs\
│   └── server.yaml         # configuración completa (puerto, token, BD, logs)
├── data\
│   └── spooler.db          # SQLite WAL (auditoría, correlativo, idempotencia)
├── logs\
│   └── server.log          # rotativo 20 MB × 10
├── instalar.cmd            # instala/actualiza + arranca (ejecutar como Admin)
├── verificar.cmd           # health + sonda S2 + /v1/printer
└── desinstalar.cmd         # para y elimina servicio (datos y config se conservan)
```

## 2. Instalación (ejecutar como Administrador)

```cmd
C:\job\spooler\instalar.cmd
```

**Qué hace `instalar.cmd` automáticamente:**

1. **Para el servicio** si está corriendo (`sc stop`)
2. **Reemplaza el binario**: copia `server.new.exe` → `server.exe` (reintenta hasta 20 s si está en uso)
3. **Instala/actualiza el servicio** `SpoolerFiscalServer`:
   - Inicio automático (`AUTO_START`)
   - Recovery: reinicio a 5 s, 10 s, 30 s (contador diario 86400 s)
   - Reinicio también en fallos no-crash (código ≠ 0)
4. **Abre puerto 8080** en firewall (perfiles domain + private)
5. **Arranca el servicio** y espera a que la API responda (`/v1/health`)
6. **Muestra verificación completa**:
   - Health check
   - Estado impresora (sonda S2 real)
   - **Últimos números de control fiscales** (`/v1/printer`: serial, RIF, última factura, último Z, memoria)

## 3. Verificación en cualquier momento

```cmd
C:\job\spooler\verificar.cmd
```

Salida típica:
```
--- Servicio ---
NOMBRE_SERVICIO: SpoolerFiscalServer
        ESTADO             : 4  RUNNING

--- API ---
{"driver":"tfhka","protocol_version":1,"status":"ok","uptime_seconds":123}

--- Impresora (sonda S2) ---
{"printer":{"online":true,"status_byte":0,"error_byte":0,"raw":"S2 00000000000000000\n..."},"online":true,"blocked":false,"queue_depth":0}

--- Ultimos numeros de control ---
{"brand":"tfhka","serial":"Z7C7022079","fiscal_rif":"J-503895241",
 "last_invoice_number":10850,"last_credit_note":259,"last_debit_note":0,
 "last_non_fiscal_doc":921,"last_z_number":893,
 "fiscal_memory_total_mb":1908,"fiscal_memory_free_mb":1384}
```

## 4. Desinstalación (ejecutar como Administrador)

```cmd
C:\job\spooler\desinstalar.cmd
```
> Detiene y elimina el servicio. **Los datos (`data\spooler.db`) y la configuración (`configs\server.yaml`) NO se borran**.

## 5. Actualización de versión

1. Compile el nuevo binario (en máquina de desarrollo):
   ```bash
   go build -trimpath -ldflags "-s -w" -o server.exe ./cmd/server
   ```
2. Copie `server.exe` como `server.new.exe` a `C:\job\spooler\` (sobrescriba el anterior)
3. Ejecute **como Administrador**:
   ```cmd
   C:\job\spooler\instalar.cmd
   ```
   - El script detecta `server.new.exe`, para el servicio, reemplaza `server.exe`, reinstala el servicio y lo arranca.
4. Verifique: `C:\job\spooler\verificar.cmd`

## 6. Configuración (`configs\server.yaml`)

```yaml
token: sk-00001235986413sdgKJsdf34d546hgjAF

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
  read_gap_ms: 100
  response_timeout_ms: 20000
  max_response_bytes: 32768
  status_probe: "S2"
  command_terminator: "\r\n"      # el driver envuelve en STX/ETX/LRC
  open_retry_ms: 500

http:
  listen: 0.0.0.0:8080
  max_body_bytes: 262144
  idempotency_retention_days: 30

queue:
  max_pending: 256
  default_timeout_ms: 30000
  max_timeout_ms: 120000
  document_lock_timeout_ms: 120000

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

## 7. Estación (PC con Profit Plus) — Agente

### 7.1 Puerto virtual (com0com)

```cmd
# Como Administrador:
"C:\Program Files (x86)\com0com\setupc.exe" install PortName=COM3 PortName=COM4
```
- `COM3` → Profit Plus (impresora TFHKA, 9600 8N1)
- `COM4` → Agente (`virtual_com.name` en `agent.yaml`)

### 7.2 Configuración del agente (`configs\agent.yaml`)

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
  name: COM4
  baud_rate: 9600
```

### 7.3 Instalar agente (en la estación, como Admin)

```cmd
cliente.exe -install -config C:\SpoolerFiscal\configs\agent.yaml
sc start SpoolerFiscalAgent
```

## 8. Puesta en marcha con la impresora real

**ANTES**: servicio detenido (`sc stop SpoolerFiscalServer`), Profit Plus cerrado.

```cmd
# 1. Diagnóstico sin imprimir (obligatorio)
C:\job\spooler\verificar.cmd
# Debe mostrar serial Z7C7022079, RIF J-503895241, última factura 10850, memoria 1384/1908 MB

# 2. Arrancar servicio
C:\job\spooler\instalar.cmd

# 3. Primera venta real desde Profit Plus (no factura de prueba)
# 4. Verificar en /v1/documents y /v1/status que el correlativo subió y no hay duplicados
```

> **Nota**: hoy los documentos **no fiscales** funcionan e imprimen papel. Las facturas fiscales devuelven NAK (0x15) — ver sección 12 del MANUAL.md para desbloquear.

## 9. Respaldo

- Con servicio detenido: copie `C:\job\spooler\data\spooler.db` (y `-wal`/`-shm` si existen).
- La base es la evidencia del correlativo fiscal: respáldela antes de cualquier actualización.

## 10. Lo que NO hay en producción

| Lo que había antes | Estado ahora |
|-------------------|--------------|
| `bin\server.exe`, `bin\agent.exe` | ❌ Un solo `server.exe` + `cliente.exe` en raíz |
| `scripts\build.ps1`, `install-server.ps1`, `install-agent.ps1` | ❌ Solo `instalar.cmd`, `verificar.cmd`, `desinstalar.cmd` |
| `configs\agent.yaml` en servidor | ❌ Solo en la estación |
| `data\opencode-desktop-win-x64.exe` | ❌ Borrado (262 MB innecesarios) |
| `TfhkaWrapper\` (puente .NET) | ❌ Movido a `_obsoleto\` (solo diagnóstico histórico) |
| `.bak`, `.ps1` de prueba, logs de consola | ❌ Limpio |

**Resultado**: ~13 MB de binario + 3 CMD + config + BD + logs. Listo para producción.