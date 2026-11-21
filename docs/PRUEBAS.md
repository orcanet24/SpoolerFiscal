# Pruebas del spooler contra la impresora fiscal simulada

Guía para levantar el servidor y la estación, y enviar por `curl` los cinco
documentos de la prueba: **factura, nota de crédito, nota de débito, reporte X
y reporte Z**.

> Todo lo que se emite contra el simulador es un **documento de prueba, no
> fiscal**. Para facturar de verdad se usa la impresora fiscal certificada.

---

## 1. Qué debe estar corriendo

| Pieza | Cómo | Puerto |
|---|---|---|
| Impresora fiscal simulada | `C:\Proyectos\GO\SimuladorFiscalGO\iniciar_servicio.bat` (modo `serve`, modelo TFHKA) | TCP `127.0.0.1:2011` |
| Servidor spooler | `.\bin\server\iniciar.bat` | API HTTP `8080` |
| Estación (agente) | `.\bin\cliente\iniciar.bat` (opcional; requiere `com0com`) | COM virtual ↔ HTTP `8080` |

El servidor debe tener en `bin\server\configs\server.yaml`:

```yaml
printer:
  driver: tfhka
  transport: tcp
  tcp_address: 127.0.0.1:2011
```

El token de la API es el de `bin\server\.env` (`SPOOLER_TOKEN`). En los ejemplos
de abajo se usa `CAMBIAR-ESTE-TOKEN`; ajústalo si lo cambió.

---

## 2. Cómo activar server y cliente

Doble clic (o desde su carpeta):

```powershell
# servidor
C:\Proyectos\GO\SpoolerFiscal\bin\server\iniciar.bat

# estación (agente)
C:\Proyectos\GO\SpoolerFiscal\bin\cliente\iniciar.bat
```

El `.bat` cambia al directorio del binario y pasa `-config configs\...`, así no
hay que recordar rutas. Si prefiere el comando directo:

```powershell
cd C:\Proyectos\GO\SpoolerFiscal\bin\server
.\server.exe -console -config configs\server.yaml

cd C:\Proyectos\GO\SpoolerFiscal\bin\cliente
.\cliente.exe -console -config configs\agent.yaml
```

Señales de que están vivos:

- Server: `spooler fiscal escuchando addr=[::]:8080`.
- Cliente: `agente iniciado estacion=CAJA-01 servidor=http://127.0.0.1:8080`.

---

## 3. Ejemplos con curl

Todas las llamadas van a `http://127.0.0.1:8080` con el encabezado
`Authorization: Bearer <token>`.

### 3.1 Factura

```powershell
curl.exe -X POST http://127.0.0.1:8080/v1/invoices `
  -H "Authorization: Bearer CAMBIAR-ESTE-TOKEN" `
  -H "Content-Type: application/json" `
  -H "Idempotency-Key: factura-1" `
  -d "{\"station\":\"CAJA-01\",\"lines\":[\"!   1.000     10.00CAFE\",\"101\"]}"
```

Salida esperada: `HTTP 201` con `"document_type":"invoice"` y la respuesta de
la impresora en `lines` (`0|Item|...` y `0|Factura N|Total: ...`).

### 3.2 Nota de crédito

Se abre con `d1`, los ítems van con `d<tipo>` y se cierra con `101`:

```powershell
curl.exe -X POST http://127.0.0.1:8080/v1/invoices `
  -H "Authorization: Bearer CAMBIAR-ESTE-TOKEN" `
  -H "Content-Type: application/json" `
  -H "Idempotency-Key: nc-1" `
  -d "{\"station\":\"CAJA-01\",\"lines\":[\"d1\",\"d1   1.000     10.00DEVOLUCION\",\"101\"]}"
```

Salida esperada: `"document_type":"credit_note"` y `0|Nota de Credito N|...`.

### 3.3 Nota de débito

Se abre con `d2`, los ítems van con `` `<tipo> `` y se cierra con `101`:

```powershell
curl.exe -X POST http://127.0.0.1:8080/v1/invoices `
  -H "Authorization: Bearer CAMBIAR-ESTE-TOKEN" `
  -H "Content-Type: application/json" `
  -H "Idempotency-Key: nd-1" `
  -d "{\"station\":\"CAJA-01\",\"lines\":[\"d2\",\"`1   1.000     10.00AJUSTE\",\"101\"]}"
```

Salida esperada: `"document_type":"debit_note"` y `0|Nota de Debito N|...`.

### 3.4 Reporte X (lectura, no reinicia)

Los comandos sueltos usan `POST /v1/commands`:

```powershell
curl.exe -X POST http://127.0.0.1:8080/v1/commands `
  -H "Authorization: Bearer CAMBIAR-ESTE-TOKEN" `
  -H "Content-Type: application/json" `
  -H "Idempotency-Key: x-1" `
  -d "{\"station\":\"CAJA-01\",\"command\":\"U0X\"}"
```

Salida esperada: `HTTP 200` con `0|Reporte X #N|Ventas: ...|IVA: ...`.

### 3.5 Reporte Z (cierre del día, reinicia acumulados)

```powershell
curl.exe -X POST http://127.0.0.1:8080/v1/commands `
  -H "Authorization: Bearer CAMBIAR-ESTE-TOKEN" `
  -H "Content-Type: application/json" `
  -H "Idempotency-Key: z-1" `
  -d "{\"station\":\"CAJA-01\",\"command\":\"U0Z\"}"
```

Salida esperada: `HTTP 200` con `0|Reporte Z #N|Ventas: ...|IVA: ...`.

---

## 4. Formatos del protocolo TFHKA

- **Ítem**: `prefijo + cantidad(%8.3f) + precio(%10.2f) + descripción`.
  Prefijos: `!` general 16 %, `"` reducida 8 %, `#` adicional 30 %, ` ` exento.
- **Ítem de nota de crédito**: `d<tipo> + cantidad + precio + descripción`
  (`tipo`: 1 general, 2 reducida, 3 adicional, 0 exento).
- **Ítem de nota de débito**: `` `<tipo> + cantidad + precio + descripción ``.
- **Cierre/totalización**: `101`. **Apertura NC/ND**: `d1` / `d2`.
- **Reportes**: `U0X` / `U1X` (X), `U0Z` / `U1Z` (Z).
- **Estado**: `S1` (identificación y correlativos), `S2` (estado y papel).

---

## 5. Consultas útiles

```powershell
# estado del servidor (imprimible, cola, documento abierto, contadores)
curl.exe http://127.0.0.1:8080/v1/status -H "Authorization: Bearer CAMBIAR-ESTE-TOKEN"

# historial de documentos emitidos
curl.exe "http://127.0.0.1:8080/v1/documents?limit=10" -H "Authorization: Bearer CAMBIAR-ESTE-TOKEN"

# cola de trabajos REST
curl.exe "http://127.0.0.1:8080/v1/jobs?status=done" -H "Authorization: Bearer CAMBIAR-ESTE-TOKEN"
```

La **idempotencia** evita reemitir: repetir un `POST` con la misma
`Idempotency-Key` devuelve la respuesta guardada sin volver a imprimir.

---

## 6. Sobre otras marcas de impresora

El spooler **solo implementa el driver `tfhka`** (The Factory HKA). El
simulador (SimuladorFiscalGO) puede emular además Hasar, Bixolon y Epson, pero
esos modelos hablan otro protocolo (IxBatch) y **no** los entiende el spooler
hasta que se implemente y registre su driver (`fiscal.RegisterDriver`). Los
ejemplos de este documento son, por eso, con TFHKA.
