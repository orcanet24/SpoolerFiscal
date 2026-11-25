# Pruebas del spooler (actualizado 2026-11-24)

> **Estado actual**: documentos no fiscales ✅, consultas S1-S5 ✅, facturas fiscales ❌ (NAK 0x15).
> Ver [MANUAL.md §12](MANUAL.md#12-estado-actual-fiscal-vs-no-fiscal) para desbloquear fiscal.

---

## 1. Qué debe estar corriendo

### 1.1 Contra impresora real (producción)

| Pieza | Comando | Puerto |
|---|---|---|
| Servidor spooler | `C:\job\spooler\instalar.cmd` (como Admin) | API HTTP `8080` |
| Estación (agente) | `cliente.exe -install -config ...` (como Admin) | COM virtual ↔ HTTP `8080` |
| Profit Plus | Apuntando al COM virtual (ej. COM3) | 9600 8N1 |

El servidor usa `configs\server.yaml` con:
```yaml
printer:
  driver: tfhka
  transport: serial
  port:
    name: COM1
    baud_rate: 9600
```

### 1.2 Contra simulador (laboratorio, sin hardware fiscal)

```bash
# Impresora fiscal emulada (proyecto SimuladorFiscalGO)
simuladorfiscal.exe serve --model tfhka --addr 127.0.0.1:2011

# Servidor apuntando al emulador (editar server.yaml):
printer:
  driver: tfhka
  transport: tcp
  tcp_address: 127.0.0.1:2011
```

Prueba automática end-to-end:
```bash
$env:SPOOLER_E2E_PRINTER_ADDR = "127.0.0.1:2011"
go test ./internal/e2e -run TestEndToEndInvoice -v
```

### 1.3 Papel real en térmica común (simprint)

`simprint` emula protocolo TFHKA e imprime en térmica 80 mm (ESC/POS). **No emite documentos fiscales válidos**: sirve para probar el circuito completo (correlativo, cola, pausa por papel, reintentos).

```powershell
# Térmica instalada en Windows (USB00x), ej. Xprinter XP-80
.\simprint.exe -listen 127.0.0.1:2011 -win-printer "Xprinter XP-80"

# Térmica de red (puerto 9100)
.\simprint.exe -listen 127.0.0.1:2011 -escpos-tcp 192.168.1.50:9100

# Solo texto en consola (vista previa, sin hardware)
.\simprint.exe -listen 127.0.0.1:2011
```

Con el servidor en `printer.transport = tcp`, `printer.tcp_address = 127.0.0.1:2011`, cada factura se imprime en la térmica. Tecla `p` + Enter alterna sensor de papel; `estado` muestra correlativos; `salir` guarda estado en `data\simprint_state.json`.

---

## 2. Cómo activar server y agente (producción)

```cmd
REM Servidor (como Administrador)
C:\job\spooler\instalar.cmd

REM Agente en la estación (como Administrador)
cliente.exe -install -config C:\SpoolerFiscal\configs\agent.yaml
sc start SpoolerFiscalAgent
```

Señales de que están vivos:
- Server: `instalar.cmd` muestra health + estado impresora + últimos números
- Cliente: `agente iniciado estacion=CAJA-01 servidor=http://IP:8080` en log

---

## 3. Ejemplos con curl (producción)

Todas las llamadas van a `http://<servidor>:8080` con `Authorization: Bearer <token>`.

### 3.1 Health y estado

```powershell
# Health (sin token)
curl http://127.0.0.1:8080/v1/health

# Estado impresora + sonda S2
curl -H "Authorization: Bearer $TOKEN" "http://127.0.0.1:8080/v1/status?probe=true"

# Últimos números de control (S1 + S5)
curl -H "Authorization: Bearer $TOKEN" "http://127.0.0.1:8080/v1/printer"
```

### 3.2 Documento no fiscal (funciona e imprime papel real)

```powershell
# 80$ abre
curl -X POST http://127.0.0.1:8080/v1/commands `
  -H "Authorization: Bearer $TOKEN" `
  -H "Content-Type: application/json" `
  -d '{"station":"TEST-CAJA","command_base64":"ODAk"}'

# 80! escribe línea
curl -X POST http://127.0.0.1:8080/v1/commands `
  -H "Authorization: Bearer $TOKEN" `
  -H "Content-Type: application/json" `
  -d '{"station":"TEST-CAJA","command_base64":"ODAhUFJVRUJBIFRlc3QK"}'

# 81 cierra e imprime
curl -X POST http://127.0.0.1:8080/v1/commands `
  -H "Authorization: Bearer $TOKEN" `
  -H "Content-Type: application/json" `
  -d '{"station":"TEST-CAJA","command_base64":"ODE="}'
```

### 3.3 Consultas S1/S2/S3/S4/S5 (funcionan)

```powershell
# S1 - identificación y correlativos
curl -X POST .../v1/commands -d '{"station":"TEST","command_base64":"UzE="}'

# S2 - estado operación
curl -X POST .../v1/commands -d '{"station":"TEST","command_base64":"UzI="}'

# S3 - totales por alícuota
curl -X POST .../v1/commands -d '{"station":"TEST","command_base64":"UzM="}'

# S4 - totales por forma de pago
curl -X POST .../v1/commands -d '{"station":"TEST","command_base64":"UzQ="}'

# S5 - memoria fiscal
curl -X POST .../v1/commands -d '{"station":"TEST","command_base64":"UzU="}'
```

### 3.4 Factura fiscal (HOY devuelve NAK 0x15)

```powershell
# Con cliente previo (obligatorio en TFHKA)
curl -X POST .../v1/commands -d '{"station":"TEST","command_base64":"aVIqSjEyMzQ1Njc4LTkK"}'  # iR*
curl -X POST .../v1/commands -d '{"station":"TEST","command_base64":"aVMqQ09OU1VNSURPUiBGSU5BTAo="}' # iS*

# Ítem + cierre
curl -X POST .../v1/commands -d '{"station":"TEST","command_base64":"IT0gIDEuMDAwICAgMTAuMDBDQUZFCg=="}' # ! 1.000 10.00CAFE
curl -X POST .../v1/commands -d '{"station":"TEST","command_base64":"MTAx"}'  # 101
```

> Si sigue NAK, ver [MANUAL.md §12.2](MANUAL.md#122-pr%C3%B3ximos-pasos-para-desbloquear-fiscal).

### 3.5 Factura vía /v1/invoices/structured (compila TFHKA automático)

```powershell
curl -X POST http://127.0.0.1:8080/v1/invoices/structured `
  -H "Authorization: Bearer $TOKEN" `
  -H "Content-Type: application/json" `
  -H "Idempotency-Key: pedido-1001" `
  -d '{
    "station": "ECOM-01",
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

Compila la secuencia canónica:
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

### 3.6 Reportes X y Z

```powershell
# X (lectura, no reinicia)
curl -X POST .../v1/commands -d '{"station":"TEST","command":"U0X"}'

# Z (cierre del día, reinicia acumulados)
curl -X POST .../v1/commands -d '{"station":"TEST","command":"U0Z"}'
```

---

## 4. Formatos del protocolo TFHKA

- **Ítem**: `prefijo + cantidad(%8.3f) + precio(%10.2f) + descripción`.
  Prefijos: `!` general 16 %, `"` reducida 8 %, `#` adicional 30 %, ` ` exento.
- **Ítem nota crédito**: `d<tipo> + cantidad + precio + descripción` (`tipo`: 1 general, 2 reducida, 3 adicional, 0 exento).
- **Ítem nota débito**: `` `<tipo> + cantidad + precio + descripción ``.
- **Cierre/totalización**: `101`. **Apertura NC/ND**: `d1` / `d2`.
- **Reportes**: `U0X` / `U1X` (X), `U0Z` / `U1Z` (Z).
- **Estado**: `S1` (identificación y correlativos), `S2` (estado y papel).

---

## 5. Consultas útiles

```powershell
# Estado del servidor (imprimible, cola, documento abierto, contadores)
curl http://127.0.0.1:8080/v1/status -H "Authorization: Bearer $TOKEN"

# Historial de documentos emitidos
curl "http://127.0.0.1:8080/v1/documents?limit=10" -H "Authorization: Bearer $TOKEN"

# Cola de trabajos REST
curl "http://127.0.0.1:8080/v1/jobs?status=done" -H "Authorization: Bearer $TOKEN"
```

La **idempotencia** evita reemitir: repetir un `POST` con la misma `Idempotency-Key` devuelve la respuesta guardada sin volver a imprimir.

---

## 6. Tests automatizados (CI/CD)

```bash
# Todos los tests unitarios e integración
go test ./...

# Solo tests end-to-end (requieren simulador en 127.0.0.1:2011)
$env:SPOOLER_E2E_PRINTER_ADDR = "127.0.0.1:2011"
go test ./internal/e2e -v

# Tests específicos
go test ./internal/e2e -run TestEndToEndInvoice -v
go test ./internal/e2e -run TestConcurrentStationsKeepSingleCorrelative -v
go test ./internal/e2e -run TestRestartPreservesCorrelativeAndHistory -v
```

**Tests que garantizan la corrección:**
- `TestEndToEndInvoice`: factura completa simulada
- `TestConcurrentStationsKeepSingleCorrelative`: 3 estaciones, 1 correlativo único
- `TestRestartPreservesCorrelativeAndHistory`: reinicio preserva BD y correlativo
- `TestLoadTenStations`: carga con 10 estaciones concurrentes

---

## 7. Otras marcas de impresora

`printer.driver` acepta: `tfhka`, `hasar`, `bixolon`, `epson`.
Los tres últimos comparten protocolo **IxBatch** (`@OpenFiscalReceipt`, `@PrintLineItem`, `@Close`, `@Status`); Epson antepone `0xESTADO|0xERROR|` a cada respuesta.

```yaml
printer:
  driver: hasar
  transport: tcp
  tcp_address: 127.0.0.1:2011
```

Factura nativa IxBatch:
```powershell
curl -X POST .../v1/invoices -d '{"station":"CAJA-01","lines":["@OpenFiscalReceipt","@PrintLineItem|CAFE|1.000|10.00|general","@Close"]}'
```

Notas IxBatch:
- El número del documento lo reporta el **cierre** (`@Close`); no hay consulta tipo `S1`, así que la conciliación previa no existe y el spooler audita al cerrar.
- `/v1/invoices/structured` **solo TFHKA** (responde `501 structured_not_supported`).
- Recuperación de documento colgado usa `@Close`, no `101`.