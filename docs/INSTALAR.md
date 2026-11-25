# Instalador CMD-only — `instalar.cmd`, `verificar.cmd`, `desinstalar.cmd`

> **Ubicación**: `C:\job\spooler\`  
> **Ejecutar siempre como Administrador** (botón derecho → "Ejecutar como administrador").

---

## 1. `instalar.cmd` — Instala / Actualiza + Arranca + Verifica

```cmd
C:\job\spooler\instalar.cmd
```

### Qué hace (paso a paso):

1. **Comprueba Admin** (`net session`). Si falla → sale con error y mensaje.
2. **Para el servicio** si está corriendo (`sc stop SpoolerFiscalServer`).
3. **Reemplaza binario** si existe `server.new.exe`:
   - `copy /Y server.new.exe server.exe` (reintenta 20× con 1 s si está en uso).
   - Borra `server.new.exe` tras copiar.
4. **Instala/actualiza el servicio** `SpoolerFiscalServer`:
   - `sc create` con `binPath= "C:\job\spooler\server.exe -config C:\job\spooler\configs\server.yaml"`
   - `start= auto` (inicio automático)
   - `DisplayName= "Spooler Fiscal Central"`
   - `sc description` + `sc failure` (reinicio 5 s / 10 s / 30 s, contador diario)
   - `sc start` (arranca ya)
5. **Firewall**: abre puerto 8080 TCP (perfiles domain + private).
6. **Espera health** (`/v1/health` hasta ~20 s).
7. **Muestra verificación completa**:
   - `sc query` (estado del servicio)
   - `GET /v1/health`
   - `GET /v1/status?probe=true` (sonda S2 real)
   - `GET /v1/printer` (**últimos números de control fiscales**)

### Salida típica final:

```
--- Servicio ---
NOMBRE_SERVICIO: SpoolerFiscalServer
        ESTADO             : 4  RUNNING

--- API ---
{"driver":"tfhka","protocol_version":1,"status":"ok","uptime_seconds":2}

--- Impresora (sonda S2) ---
{"printer":{"online":true,"status_byte":0,"error_byte":0,"raw":"S2 00000000000000000\n..."},"online":true,"blocked":false,"queue_depth":0}

--- Ultimos numeros de control ---
{"brand":"tfhka","serial":"Z7C7022079","fiscal_rif":"J-503895241",
 "last_invoice_number":10850,"last_credit_note":259,"last_debit_note":0,
 "last_non_fiscal_doc":921,"last_z_number":893,
 "fiscal_memory_total_mb":1908,"fiscal_memory_free_mb":1384}
```

---

## 2. `verificar.cmd` — Comprobación rápida en cualquier momento

```cmd
C:\job\spooler\verificar.cmd
```

### Qué muestra:

1. `sc query SpoolerFiscalServer` (estado RUNNING/STOPPED)
2. `GET /v1/health` (API viva)
3. `GET /v1/status?probe=true` (impresora online, cola, documento abierto, contadores)
4. `GET /v1/printer` (**serial, RIF, última factura, último Z, memoria fiscal**)

Úselo después de cualquier actualización, reinicio o incidencia.

---

## 3. `desinstalar.cmd` — Para y elimina el servicio

```cmd
C:\job\spooler\desinstalar.cmd
```

### Qué hace:

1. `server.exe -uninstall` (para y elimina servicio via `golang.org/x/sys/windows/svc/mgr`)
2. **NO borra** `configs\server.yaml` ni `data\spooler.db` ni `logs\`.

---

## 4. Flujo de actualización de versión

```cmd
REM 1. En máquina de desarrollo:
go build -trimpath -ldflags "-s -w" -o server.exe ./cmd/server

REM 2. Copie server.exe como server.new.exe a C:\job\spooler\

REM 3. En servidor (como Administrador):
C:\job\spooler\instalar.cmd
```

El script detecta `server.new.exe`, para el servicio, reemplaza `server.exe`, reinstala el servicio y lo arranca.

---

## 5. Requisitos previos

| Requisito | Verificación |
|---|---|
| Windows 10/11 / Server 2019+ x64 | `ver` |
| Impresora HKA80 en COM1 (9600 8N1) | `C:\job\spooler\detectar.exe` |
| Puerto 8080 libre | `netstat -an | findstr :8080` |
| Permisos de Administrador | `net session` (sin error) |
| `configs\server.yaml` existe | `dir C:\job\spooler\configs\server.yaml` |

---

## 6. Estructura final en `C:\job\spooler`

```
C:\job\spooler\
├── server.exe              # binario actual (~13 MB)
├── server.new.exe          # binario nuevo (para actualizar)
├── cliente.exe             # agente estación (~8 MB)
├── configs\server.yaml     # configuración completa
├── data\spooler.db         # SQLite WAL (auditoría, correlativo)
├── logs\server.log         # rotativo 20 MB × 10
├── instalar.cmd
├── verificar.cmd
└── desinstalar.cmd
```

**Nada más**. Sin `bin\`, sin `scripts\`, sin PowerShell obligatorio, sin .NET.

---

## 7. Solución de problemas

| Síntoma | Qué revisar |
|---|---|
| `instalar.cmd` → `ERROR: abre este archivo como Administrador` | Botón derecho → "Ejecutar como administrador" |
| `sc start` → `ERROR 5: Acceso denegado` | No es Admin; `verificar.cmd` también fallará |
| `verificar.cmd` → `curl: (7) Failed to connect` | Servicio STOPPED o puerto 8080 bloqueado |
| `/v1/printer` → `serial: ""` | Impresora no respondió S1; `sc stop` servicio, revise cable/COM1 |
| `commands_failed` sube en `/v1/status` | NAK (0x15) de la impresora; ver MANUAL.md §12 |

---

## 8. Desinstalación completa (incluyendo datos)

```cmd
C:\job\spooler\desinstalar.cmd
REM Luego, si quiere borrar todo:
rmdir /s /q C:\job\spooler
```