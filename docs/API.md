# API HTTP del spooler fiscal

El servidor expone una API HTTP/JSON que usan los agentes de estación (Profit
Plus) y también **ecommerce, ERP web, CRM o cualquier sistema** que necesite
imprimir en la impresora fiscal compartida.

Contrato formal: [`api/openapi.yaml`](../api/openapi.yaml). Configuración:
sección `http` de [`configs/server.yaml`](../configs/server.yaml).

Manual completo (instalación, operación y ejemplos en seis lenguajes):
[`manual.html`](manual.html) — también servido en `http://servidor:8080/docs`.
Ejemplos ejecutables: [`../examples/`](../examples/README.md).

## ¿Por qué HTTP en el servidor y no un gateway aparte?

Un proceso intermedio (gateway HTTP) obliga a desplegar, configurar y actualizar
una segunda pieza, con su propio token y su propio ciclo de vida. Peor aún: si
alguien arranca dos gateways, cada uno abre sus sesiones y el turno por factura
se reparte entre dos colas.

La API HTTP vive **dentro de `server.exe`** y usa el mismo `Spooler`:

- una sola cola serializada y un solo correlativo;
- el mismo turno exclusivo por factura que las estaciones COM;
- el mismo guardián de estado (papel, memoria, errores);
- la misma auditoría SQLite y los mismos permisos de archivo;
- un solo binario, un solo servicio de Windows, un solo token.

Si el volumen crece, el cuello de botella seguirá siendo la impresora (0,3-1
factura/s), no el transporte.

## Activación

```yaml
http:
  enabled: true
  listen: 0.0.0.0:8080
  max_body_bytes: 262144
  read_timeout_ms: 15000
  write_timeout_ms: 60000
  idempotency_retention_days: 30
```

Reinicie el servicio. El log muestra `api REST escuchando addr=...`. Si
`tls.enabled` está activo, la REST sirve **HTTPS** con el mismo certificado del
servidor (sin exigir certificado de cliente: autentica el token).

Comprobación sin credenciales:

```bash
curl http://127.0.0.1:8080/v1/health
```

```json
{"status":"ok","version":"1.0.0","protocol_version":1,"driver":"tfhka","uptime_seconds":42}
```

## Autenticación

Todas las rutas salvo `/v1/health` exigen el token compartido:

```
Authorization: Bearer <token>
```

También se acepta `X-Spooler-Token: <token>`. El token es el mismo de las
estaciones (`token` del YAML o `SPOOLER_TOKEN`). Se compara en tiempo constante.
Use HTTPS si la API cruza la red de la empresa; no exponga el puerto a Internet.

## Endpoints

| Método | Ruta | Uso |
| --- | --- | --- |
| `GET` | `/v1/health` | Salud y versión, sin token |
| `GET` | `/v1/status?probe=true` | Estado de impresora, cola, documento abierto y contadores |
| `GET` | `/v1/printer` | Modelo, serial, RIF, firmware y memoria fiscal |
| `GET` | `/v1/documents?limit=50` | Historial fiscal registrado |
| `POST` | `/v1/commands` | Un comando fiscal suelto (equivalente a `SendCommand`) |
| `POST` | `/v1/invoices` | Una factura completa (líneas TFHKA), atómica y con turno exclusivo |
| `POST` | `/v1/invoices/structured` | Una factura desde ítems y totales JSON, compilada a TFHKA |

La sesión de una petición REST es implícita: `api:<station>`. No hay que abrir ni
cerrar sesión.

## Emitir una factura

`POST /v1/invoices` recibe las **líneas TFHKA ya formateadas**. La primera debe
abrir la factura y la última debe cerrarla con `101`; el servidor valida esa
estructura antes de imprimir y ejecuta todo bajo el turno exclusivo. No incluya
`CR`/`LF` en las líneas: el servidor añade `printer.command_terminator` (y
normaliza el que ya venga, como el de Profit Plus).

```bash
curl -X POST http://127.0.0.1:8080/v1/invoices \
  -H "Authorization: Bearer CAMBIAR-ESTE-TOKEN" \
  -H "Content-Type: application/json" \
  -H "Idempotency-Key: pedido-1001" \
  -d '{
    "station": "ECOM-01",
    "client_ref": "pedido-1001",
    "lines": [
      "!   1.000     10.00CAFE",
      "!   2.000      5.00PAN",
      "101"
    ]
  }'
```

```json
{
  "station": "ECOM-01",
  "session": "api:ECOM-01",
  "order_ref": "pedido-1001",
  "completed": true,
  "invoice_number": 42,
  "document_type": "invoice",
  "sequence": 42,
  "issued_at": "2026-11-14T19:21:53Z",
  "duplicate": false,
  "lines": [
    {"index": 0, "response": "0|OK", "sequence": 41},
    {"index": 1, "response": "0|OK", "sequence": 42},
    {"index": 2, "response": "0|Factura 42|Total: 22.00|Impreso:42", "sequence": 43}
  ],
  "elapsed_ms": 12
}
```

El código es `201 Created`. `order_ref` conserva el `client_ref` que usted envió
(o la `Idempotency-Key` si no envió ninguno). El `client_ref` dentro de
`document` lleva `#índice` porque identifica una línea concreta; ver
«Referencia de cada línea» más abajo.

Guarde `invoice_number` y `issued_at`: son los datos fiscales que su ecommerce
debe conservar junto al pedido.

### Cómo armar las líneas (TFHKA)

| Prefijo | Alícuota | Ejemplo |
| --- | --- | --- |
| `!` | General 16 % | `!   1.000     10.00CAFE` |
| `"` | Reducida 8 % | `"   1.000     10.00PAN` |
| `#` | Adicional 30 % | `#   1.000     10.00LICOR` |
| espacio | Exento | `    1.000     10.00AGUA` |

Formato: `prefijo + cantidad(8.3) + precio(10.2) + descripción`. El cierre es
`101`. Para pagos, descuentos, RIF del cliente, notas de crédito o reportes X/Z,
use `POST /v1/commands` con los comandos de
[`PROTOCOLO_TFHKA.md`](PROTOCOLO_TFHKA.md).

**Por qué no se compone la factura desde un JSON de ítems:** el formato fiscal, la
secuencia de pago y las validaciones dependen del modelo de impresora y de la
normativa vigente. El spooler no inventa documentos fiscales: ejecuta la
secuencia que su sistema armó y la audita byte a byte. Si prefiere un contrato de
alto nivel, se puede añadir un compilador de factura específico con validación
contra el hardware real.

## Emitir una factura desde ítems JSON

`POST /v1/invoices/structured` compila la factura a partir de ítems y totales.
Es la vía para un ecommerce que no quiere conocer el formato de línea fiscal.

```bash
curl -X POST http://127.0.0.1:8080/v1/invoices/structured \
  -H "Authorization: Bearer CAMBIAR-ESTE-TOKEN" \
  -H "Content-Type: application/json" \
  -H "Idempotency-Key: pedido-1001" \
  -d '{
    "station": "ECOM-01",
    "client_ref": "pedido-1001",
    "items": [
      {"description": "CAFE", "quantity": 1, "unit_price": 10.00, "tax": "general"},
      {"description": "PAN",  "quantity": 2, "unit_price": 5.00,  "tax": "reduced"}
    ],
    "totals": {"subtotal": 17.88, "tax": 2.12, "total": 20.00}
  }'
```

Respuesta (`201`):

```json
{
  "station": "ECOM-01",
  "completed": true,
  "invoice_number": 1,
  "document_type": "invoice",
  "compiled_lines": [
    "!   1.000     10.00CAFE",
    "\"   2.000      5.00PAN",
    "101"
  ],
  "totals": {"subtotal": 17.88, "tax": 2.12, "total": 20.00},
  "document": {"number": 1, "response_text": "0|Factura 1|Total: 20.00|Impreso:1"}
}
```

### Reglas de compilación

| `tax` | Prefijo | Alícuota |
| --- | --- | --- |
| `general` (omisión) | `!` | 16 % |
| `reduced` | `"` | 8 % |
| `additional` | `#` | 30 % |
| `exempt` | espacio | 0 % |

- `unit_price` es el precio **con IVA incluido**, como lo maneja la TFHKA.
- `quantity` admite hasta 3 decimales (0.001-9999.999); `unit_price` hasta 2
  (0-9.999.999,99). Fuera de rango se rechaza para no romper el ancho fijo.
- `description`: ASCII imprimible, máximo 40 caracteres, sin `|` ni saltos de
  línea. Se rechaza cualquier intento de inyectar comandos.
- Se añade una línea por ítem y el cierre `101`; el spooler valida la estructura
  antes de imprimir.
- `totals` es opcional. Si viene, `total` es obligatorio y se compara con el
  calculado (tolerancia de un centavo); `subtotal` y `tax` se comparan sólo si
  son mayores que cero. Una discrepancia responde `400 totals_mismatch` y **no se
  imprime**.

Los totales se calculan por línea (cantidad × precio, redondeado a centavos) y
luego se separa la base y el IVA. Es la misma cuenta que hace la impresora, así
que el `Total:` de la respuesta debe coincidir con `totals.total`.

**No incluye** datos de cliente ni formas de pago: para RIF, nombre, pagos o
notas de crédito use `/v1/commands` con los comandos de
[`PROTOCOLO_TFHKA.md`](PROTOCOLO_TFHKA.md). Si su operación necesita un contrato
completo de facturación (cliente, pagos, descuentos), conviene ampliar este
compilador con validación contra la impresora real.

## Reintentos seguros (idempotencia)

Envíe siempre `Idempotency-Key` (por ejemplo, el número de pedido):

- misma clave + mismo cuerpo → devuelve la respuesta original con
  `Idempotent-Replay: true` y **no vuelve a imprimir**;
- misma clave + cuerpo distinto → `409 idempotency_conflict`;
- sin clave, un reintento puede emitir una segunda factura.

Las claves se purgan según `http.idempotency_retention_days` (30 por omisión).
Sólo se guardan las respuestas exitosas: si la factura falló a mitad, el
reintento vuelve a intentarlo (y puede encontrar el turno retenido; ver abajo).

### Referencia de cada línea (`client_ref#índice`)

`client_ref` identifica el pedido, pero la impresora ejecuta varios comandos por
factura. Para que la auditoría distinga cada uno, `documents` guarda una
referencia por línea:

| `client_ref` guardado | Línea |
|---|---|
| `pedido-1001#0` | primer ítem compilado |
| `pedido-1001#1` | segundo ítem |
| `pedido-1001#2` | cierre `101` |

El sufijo es el índice 0-based dentro de `compiled_lines` (o de las líneas que
envió a `/v1/invoices`). Por eso el objeto `document` de la respuesta —el de la
última línea, el cierre— muestra `"client_ref": "pedido-1001#2"` aunque usted
haya enviado `pedido-1001`. No afecta a la idempotencia: la clave sigue siendo
`Idempotency-Key`. Sin `client_ref` ni clave, la referencia queda vacía y no se
añade sufijo.

## Concurrencia y turno

La impresora imprime una factura por vez. Cuando llega una factura REST:

1. espera su turno (las peticiones de otras estaciones se atienden en orden);
2. imprime todas sus líneas sin que nadie se intercale;
3. libera el turno al procesar el `101`.

Con 10 cajas y ráfagas, la última factura espera aproximadamente
`facturas en cola × 2 s`. Ajuste `queue.default_timeout_ms` a 60-90 s y use un
`timeout_ms` acorde. Si el tiempo vence, el comando **no se imprime** y recibe
`504 timeout`.

Si una línea falla a mitad (por ejemplo, la impresora se apaga), el turno puede
quedar retenido hasta `queue.document_lock_timeout_ms`. La respuesta incluye
`document_open` y `document_owner`; el operador puede esperar la liberación
automática o intervenir en la impresora.

## Códigos de error

| HTTP | `code` | Significado |
| --- | --- | --- |
| 400 | `invalid_request` | JSON, estación, ítems o líneas inválidas (incluye falta de apertura/cierre) |
| 400 | `totals_mismatch` | Los totales declarados no coinciden con los calculados |
| 401 | `unauthorized` | Token ausente o incorrecto |
| 403 | `station_not_allowed` | Estación fuera de `allowed_stations` |
| 409 | `printer_blocked` | Papel agotado, memoria fiscal llena o error fiscal |
| 409 | `idempotency_conflict` | Clave ya usada con otro cuerpo |
| 413 | `body_too_large` | Cuerpo mayor de `http.max_body_bytes` |
| 415 | `unsupported_media_type` | `Content-Type` distinto de `application/json` |
| 429 | `queue_full` | Cola llena; reintente con espera |
| 503 | `printer_unavailable` | Impresora caída o sin respuesta |
| 504 | `timeout` | Venció en la cola; no se imprimió |
| 502 | `printer_error` | Error devuelto por la impresora durante la ejecución |

Formato:

```json
{"error":{"code":"printer_blocked","message":"papel agotado","document_open":false}}
```

Cuando falla una línea concreta, el error añade `line_index`.

## Límites

- `station`: 64 caracteres (`A-Z a-z 0-9 - _ . :`).
- `lines`: hasta 500, cada una de 1024 bytes.
- `max_body_bytes`: 256 KiB por omisión.
- `timeout_ms` se recorta a `queue.max_timeout_ms`.

## Ejemplo desde Node.js

```js
const res = await fetch("http://spooler.local:8080/v1/invoices", {
  method: "POST",
  headers: {
    "Authorization": `Bearer ${process.env.SPOOLER_TOKEN}`,
    "Content-Type": "application/json",
    "Idempotency-Key": `pedido-${order.id}`,
  },
  body: JSON.stringify({
    station: "ECOM-01",
    client_ref: `pedido-${order.id}`,
    lines: [...order.items.map(toFiscalLine), "101"],
    timeout_ms: 60000,
  }),
});
if (!res.ok) throw new Error(`spooler ${res.status}: ${await res.text()}`);
const factura = await res.json();
await order.update({ fiscalNumber: factura.invoice_number });
```

## Monitoreo

`GET /v1/health` sirve para un chequeo de vida; `GET /v1/status` añade cola,
documento abierto, contadores y motivo de bloqueo. Ambos son de sólo lectura y no
consumen turno.
