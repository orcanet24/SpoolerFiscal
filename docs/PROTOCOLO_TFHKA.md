# Protocolo fiscal soportado: TFHKA (The Factory HKA), ASCII

Implementación: `internal/fiscal/tfhka.go`. El agente **no** interpreta nada de
esto: transporta bytes. Toda la interpretación ocurre en el servidor, junto a la
impresora.

## 1. Transporte y framing

- Serie: 9600 8N1 (o 115200 8N1), sin control de flujo. Parámetros en
  `printer.port`.
- Comando: texto ASCII terminado en `CR+LF` (`0x0D 0x0A`), configurable en
  `printer.command_terminator`. **Con `CR` solo la TFHKA no responde** (probado
  contra el simulador de referencia); use `"\r\n"`.
- Respuesta: texto ASCII terminado en `CR`/`LF`.
- **Delimitación por silencio:** una trama termina cuando el emisor deja de enviar
  durante `read_gap_ms` (30 ms por omisión). No se recorta ni se normaliza nada:
  los bytes viajan íntegros en ambos sentidos.

## 2. Comandos usados

| Comando | Función | Respuesta típica |
| --- | --- | --- |
| `S1` | Identificación y correlativos | `0\|maquina\|rif\|auditoria\|corte\|ultFact\|ultNC\|ultND\|ultDNF\|ventas\|fecha` |
| `S2` | Estado de la operación | `condicion\|tipoDoc\|items\|bases\|iva\|apagar\|pagos` |
| `S3` | Totales por alícuota | `baseG\|baseR\|baseA\|tipo1\|tipo2\|tipo3` |
| `S4` | Totales por forma de pago | `0\|monto1\|…\|0.00` |
| `S5` | Memoria fiscal | `maquina\|rif\|nroAuditoria\|totalMB\|libreMB\|documentos` |
| `S8E` / `S8P` | Encabezados / pie de página | campos separados por `\|` |
| `101` | Cierre y totalización de factura | `0\|Factura N\|Total: X\|Impreso:id` |
| `3` / `4` | Subtotal con / sin impresión | `0\|Subtotal: …` |
| `100<monto>` | Pago | `0\|Pago: …\|Pendiente: …` |
| `103<medio>` | Descripción del medio de pago | `0\|Descripcion pago` |
| `700/701<monto>` | Descuento / recargo | `0\|Descuento: …` |
| `iR*`, `iS*`, `i01`, `i02` | RIF, nombre, dirección, teléfono del cliente | `0\|… registrado` |
| `iF*`, `iD*`, `iI*` | Factura, fecha y serial referenciados (notas) | `0\|…` |
| `0` | Apertura de gaveta | `0\|Gaveta abierta` |
| `80$`, `80!`, `80*`, `81` | Documento no fiscal (abrir, texto, contenido, cerrar) | `0\|DNF …` |
| `d1…`, `d2…` | Nota de crédito / débito | `0\|Nota de … N\|Total: X` |
| `U0X`/`U1X`, `U0Z`/`U1Z` | Reportes X / Z | `0\|Reporte X #N\|Ventas: …` |

Ítems de factura: `prefijo + cantidad(8.3) + precio(10.2) + descripción`, con
`!` = alícuota general (16 %), `"` = reducida (8 %), `#` = adicional (30 %),
espacio = exento.

## 3. Bloque de estado y errores

Las respuestas pueden venir precedidas por dos bytes de estado en hexadecimal:

```
0x04|0x00|0|Factura 12|Total: 116.00|Impreso:3
```

Si el firmware no envía ese bloque, los flags de estado quedan "no reportado" y
sólo se usan los errores observados. El spooler acepta ambos formatos.

**Estado (Anexo 1):** `0x01`-`0x03` modo prueba; `0x04` fiscal en espera; `0x05`
emisión fiscal; `0x06` emisión no fiscal; `0x07`-`0x09` memoria casi llena;
`0x0A`-`0x0C` memoria fiscal llena.

**Errores (Anexo 2):** `0x00` sin error; `0x01` fin de papel; `0x02` error mecánico
de papel; `0x03` ambos; `0x50`/`0x5C` comando inválido; `0x54` tasa inválida;
`0x58` sin directivas; `0x60` error fiscal; `0x64` error de memoria fiscal;
`0x6C` memoria fiscal llena; `0x70` buffer completo; `0x80` error de comunicación;
`0x89` sin respuesta; `0x90` error de LRC; `0x91` error interno; `0x99` error de
archivo.

Formato de error: `ERROR|0x50|Comando invalido`.

## 4. Detección de documentos

El servidor registra un documento cuando la respuesta (ya sin bloque de estado)
coincide con:

```
^(0\|)?(Factura|Nota de Credito|Nota de Debito|Doc No Fiscal)\s+(\d+)\|(Total:\s*([^|]*))?
```

De ahí salen tipo, número y total. El número es el que asignó la impresora; el
spooler agrega su `sequence` y los hashes SHA-256 del comando y de la respuesta.

## 5. Guardián de estado

Con `guard.block_on_paper_out`, `block_on_memory_full` o `block_on_error`, el
spooler rechaza la emisión (HTTP `409 printer_blocked`) si el último estado
conocido tiene papel agotado, memoria fiscal llena o un error físico/fiscal
(`0x01`-`0x03`, `0x60`, `0x64`, `0x6C`, `0x80`, `0x89`, `0x90`). Los errores de
comando (`0x50`, `0x54`, …) no bloquean: son rechazos de una instrucción, no
fallas de la máquina. El bloqueo se libera con la siguiente respuesta limpia.

## 6. Contrato HTTP (API del servidor)

Un solo puerto (`http.listen`, 8080 por omisión). Todas las rutas exigen el
token compartido en `Authorization: Bearer <token>` (o `X-Spooler-Token`), con
comparación en tiempo constante, salvo `GET /v1/health`. `tls.enabled` con
`cert_file`/`key_file` sirve HTTPS.

| Ruta | Uso |
| --- | --- |
| `GET /v1/health` | Conectividad y versión del contrato (pública) |
| `POST /v1/commands` | Bytes fiscales crudos → respuesta íntegra, estado y documento; es el extremo que usa el agente |
| `GET /v1/status` | Estado de impresora, cola y contadores; `?probe=1` consulta a la impresora |
| `GET /v1/printer` | Modelo, serial, RIF, firmware y memoria fiscal |
| `POST /v1/invoices` | Factura por líneas TFHKA (sistemas externos) |
| `POST /v1/invoices/structured` | Factura por ítems JSON con validación de totales |

El agente envía `command_base64` y recibe `response_base64`, así los bytes
fiscales viajan íntegros. Usa `Idempotency-Key` con la referencia del comando:
un reintento por red no duplica la emisión.

**Errores del spooler:**

| Situación | Código HTTP |
| --- | --- |
| Token inválido o ausente | `401 unauthorized` |
| Estación fuera de la lista blanca | `403 forbidden` |
| Guardián bloquea (papel/memoria/error) | `409 printer_blocked` |
| Cola llena | `429 queue_full` |
| Impresora caída o sin respuesta | `503 printer_unavailable` |
| Timeout de la petición en cola | `504 timeout` |
