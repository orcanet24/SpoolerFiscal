# Ejemplos de integración

Una factura de prueba contra `POST /v1/invoices/structured`, en seis lenguajes.
Todos leen la configuración de variables de entorno y emiten la misma factura
(dos ítems, total 20.00).

| Lenguaje | Archivo | Ejecutar |
| --- | --- | --- |
| cURL (bash) | [`curl/invoice.sh`](curl/invoice.sh) | `SPOOLER_TOKEN=... ./curl/invoice.sh` |
| PowerShell | [`curl/invoice.ps1`](curl/invoice.ps1) | `$env:SPOOLER_TOKEN="..."; .\curl\invoice.ps1` |
| PHP 8+ | [`php/invoice.php`](php/invoice.php) | `SPOOLER_TOKEN=... php php/invoice.php` |
| Go 1.25+ | [`go/main.go`](go/main.go) | `cd go && SPOOLER_TOKEN=... go run .` |
| Python 3.10+ | [`python/invoice.py`](python/invoice.py) | `SPOOLER_TOKEN=... python python/invoice.py` |
| Node.js 18+ | [`node/invoice.mjs`](node/invoice.mjs) | `SPOOLER_TOKEN=... node node/invoice.mjs` |
| Rust 1.75+ | [`rust/src/main.rs`](rust/src/main.rs) | `cd rust && SPOOLER_TOKEN=... cargo run` |

## Variables

| Variable | Omisión | Uso |
| --- | --- | --- |
| `SPOOLER_URL` | `http://127.0.0.1:8080` | base de la API REST |
| `SPOOLER_TOKEN` | — (obligatoria) | token compartido del `server.yaml` |
| `STATION` | `ECOM-01` | identificador del sistema emisor |
| `ORDER_ID` | `pedido-1001` | referencia e `Idempotency-Key` |

Ejemplo en PowerShell:

```powershell
$env:SPOOLER_URL   = "http://192.168.1.10:8080"
$env:SPOOLER_TOKEN = "CAMBIAR-ESTE-TOKEN"
$env:STATION       = "ECOM-01"
$env:ORDER_ID      = "pedido-1001"
```

Salida esperada:

```
Factura 1 emitida: total 20.00 (secuencia 1)
Impresora: 0|Factura 1|Total: 20.00|Impreso:1
```

Si repite el mismo `ORDER_ID`, la respuesta se recupera de la caché de
idempotencia y **no se imprime otra factura**.

## Antes de ejecutar

1. El servidor debe estar corriendo con `http.enabled: true`.
2. El puerto (`8080` por omisión) debe ser accesible desde el equipo del ejemplo.
3. El token debe coincidir con `token` del `configs/server.yaml`.
4. La impresora debe estar online y sin papel agotado.

Documentación completa: [`../docs/manual.html`](../docs/manual.html) y
[`../docs/API.md`](../docs/API.md).
