# Manual de operación (actualizado 2026-11-24)

## Servicios en producción

| Servicio | Máquina | Binario | Comando de control |
|---|---|---|---|
| `SpoolerFiscalServer` | PC con la impresora HKA80 | `C:\job\spooler\server.exe` | `instalar.cmd` / `verificar.cmd` / `desinstalar.cmd` |
| `SpoolerFiscalAgent` | cada estación (Profit Plus) | `cliente.exe` | `cliente.exe -install -config ...` |

**Forma recomendada**: `instalar.cmd` (como Admin) instala el servicio con inicio automático, recovery y firewall. No use `sc create` a mano.

```cmd
# Ver estado
sc query SpoolerFiscalServer

# Ver configuración del servicio
sc qc SpoolerFiscalServer
sc qfailure SpoolerFiscalServer

# Arrancar / parar
sc start SpoolerFiscalServer
sc stop SpoolerFiscalServer

# Modo consola para diagnóstico (detenga el servicio antes)
C:\job\spooler\server.exe -console -config C:\job\spooler\configs\server.yaml
```

- El servidor arranca con `AUTO_START` y se reinicia a los 5 s, 10 s y 30 s si termina con error (contador diario 86400 s). También reinicia en fallos no-crash (código ≠ 0).
- El agente usa `delayed-auto` (com0com tarda en cargar). Si el COM aún no está, reintenta con backoff sin detenerse.
- El servicio fija su directorio de trabajo en el del ejecutable (`C:\job\spooler`), así las rutas relativas de `data\` y `logs\` no acaban en `C:\Windows\System32`.

## Logs y rotación

- Archivo: `C:\job\spooler\logs\server.log` (configurable en `server.yaml`).
- Rotación con lumberjack: `max_size_mb: 20`, `max_backups: 10`, `max_age_days: 30`, `compress: true`.
- Nivel: `debug | info | warn | error`. `debug` en el agente registra además los bytes en hexadecimal (`bridge.log_hex_payloads`).
- Cada minuto el servidor escribe una línea `métricas` con documentos, comandos, errores, duplicados, latencia promedio/máxima, profundidad de cola y estado.

## Permisos del token y de la auditoría

El YAML de configuración contiene el token compartido. Al arrancar, el servicio restringe la ACL del archivo y del directorio `data\` a SYSTEM, Administradores y el usuario que ejecuta el proceso (usa `icacls` con SIDs, así funciona en equipos en español o inglés).

```cmd
icacls C:\job\spooler\configs\server.yaml /inheritance:r /grant:r "*S-1-5-18:F" "*S-1-5-32-544:F"
icacls C:\job\spooler\data /inheritance:r /grant:r "*S-1-5-18:F" "*S-1-5-32-544:F"
```

`instalar.cmd` aplica esta ACL. El modo consola debe ejecutarse como administrador (o como el usuario que instaló), porque el token ya no es legible por el grupo `Usuarios`.

## Seguridad: token por estación y mTLS

- `token`: token compartido para la API externa y para las estaciones que no tengan uno propio.
- `station_tokens`: un token por estación. Con token propio, esa estación **sólo** se representa con su token: una petición que pida otra estación recibe `403`, y el token compartido deja de servirle. Una caja comprometida no puede facturar como otra.
- `tls.enabled` con `cert_file`/`key_file`: HTTPS.
- `tls.ca_file`: **mTLS**. El servidor exige certificado de cliente firmado por esa CA; el agente configura `cert_file`/`key_file` (los dos juntos).

Comprobación rápida (con `token-caja-01` de `CAJA-01`):

```cmd
REM debe responder 403: el token de CAJA-01 no emite como CAJA-02
curl.exe -i -X POST http://127.0.0.1:8080/v1/commands ^
  -H "Authorization: Bearer token-caja-01" ^
  -H "Content-Type: application/json" ^
  -d "{\"station\":\"CAJA-02\",\"command\":\"S2\"}"
```

## Capacidad y tiempos de espera

La impresora es el cuello de botella: un documento por vez. Medición del circuito completo (10 estaciones, 5 facturas cada una, 50 documentos) con impresora emulada y 8 ms por comando: **5,0 s, ~10 facturas/s, 101 ms por factura**. Una impresora fiscal real tarda del orden de 1-3 s por factura, así que la capacidad práctica es **0,3-1 factura por segundo en total**, sin importar cuántas cajas haya.

- Con 10 cajas y ráfagas simultáneas, la última factura espera `nº de facturas en cola × 2 s`. Suba `queue.default_timeout_ms` (30 s por omisión) a 60000-90000 si hay ráfagas.
- `queue.max_pending: 256` es holgado para 10 cajas; por encima de 30 cajas revise la memoria y los timeouts.
- El turno por factura es exclusivo: la caja que abre una factura no lo suelta hasta cerrarla. Evita que dos facturas se mezclen en el papel; a cambio, las demás esperan (no se rechazan).
- Un comando que vence en la cola **no se imprime**: se descarta y el cliente recibe `DeadlineExceeded`. Una factura nunca se completa con ítems viejos.

## API REST

`GET /v1/health` sirve para monitoreo y `GET /v1/status` devuelve cola, documento abierto y motivo de bloqueo. La emisión externa usa `POST /v1/invoices` (líneas TFHKA) o `/v1/invoices/structured` (ítems JSON); cada factura retiene el turno hasta el `101`, igual que una caja. Errores frecuentes: `409 printer_blocked` (papel o memoria), `504 timeout` (no se imprimió) y `429 queue_full`. Detalle en [`API.md`](API.md).

## Panel de operación

`http://<servidor>:8080/admin` sirve un panel para el operador:

- estado de la impresora, cola, documento abierto y motivo de bloqueo;
- últimos trabajos con **Cancelar** (si no llegó a papel) y **Reintentar** (si está pausado sin líneas impresas);
- últimos documentos emitidos.

La página no contiene secretos: pide el token y lo guarda sólo en la pestaña. Las mismas operaciones están en `POST /v1/jobs/{id}/cancel` y `/retry`. Si el operador no debe ver el panel, bloquéelo en el firewall o no lo publique.

## Qué registrar en la auditoría

Tabla `documents` (nunca se purga):

| Columna | Contenido |
|---|---|
| `sequence` | secuencia global del spooler |
| `doc_type` / `doc_number` | tipo y número asignado por la impresora |
| `station_id` / `session_id` | estación de origen y su sesión |
| `issued_at` | fecha/hora del registro |
| `request_sha256` / `response_sha256` | huellas del comando y la respuesta |
| `response_text` | respuesta cruda de la impresora |

Tabla `command_log`: hashes, latencia, resultado y error de cada comando (retención configurable con `database.retention_days`).

Consultas útiles (con cualquier visor SQLite, por ejemplo DB Browser for SQLite):

```sql
SELECT sequence, doc_type, doc_number, station_id, issued_at
FROM documents ORDER BY sequence DESC LIMIT 20;

SELECT doc_number, station_id, issued_at FROM documents
WHERE doc_type = 'invoice' AND date(issued_at) = date('now') ORDER BY doc_number;

SELECT station_id, COUNT(*) FROM documents GROUP BY station_id;
```

## Respaldo

Con el servicio detenido, copie `C:\job\spooler\data\spooler.db` (y `-wal`/`-shm` si existen). También puede copiarlos en caliente: SQLite en WAL mantiene consistencia, pero el respaldo en frío es el más simple. **La base es la evidencia del correlativo**: respáldela antes de cualquier actualización.

## Diagnóstico rápido

| Síntoma | Causa probable | Solución |
|---|---|---|
| `sc start` → `ERROR 5` | No es Admin | Ejecute `instalar.cmd` / `verificar.cmd` **como Administrador** |
| `verificar.cmd` → `curl: (7) Failed to connect` | Servicio detenido o puerto 8080 bloqueado | `sc query SpoolerFiscalServer`; firewall |
| `/v1/printer` → `serial: ""` o `rif: ""` | Impresora no respondió S1 | Revise cable, COM1, 9600 8N1; `sc stop` servicio antes de probar |
| `commands_failed` subiendo | NAK (0x15) de la impresora | Ver `server.log`; fiscal NAK = impresora rechaza comando (ver MANUAL.md §12) |
| `document_open: true` sin operador | Factura colgada | El spooler intenta cierre de recuperación a los 120 s (`document_lock_timeout_ms`) |
| `Serial port busy` al arrancar | Otro proceso retiene COM1 | `taskkill /F /IM server.exe` o reinicie el equipo |

## Windows Defender y los binarios

Defender puede marcar los ejecutables Go sin firmar como «virus o software potencialmente no deseado» (falso positivo). Síntomas: `go build` falla, `server.exe` desaparece, `Permission denied` al ejecutar.

Para desarrollo, excluya solo la carpeta del proyecto (PowerShell Admin):

```powershell
Add-MpPreference -ExclusionPath "C:\Profit_a\PrintSpooler\SpoolerFiscal"
```

En producción, prefiera firmar los binarios o usar la lista blanca del antivirus corporativo.

## Firma de los binarios (opcional pero recomendado)

Windows SmartScreen y algunos antivirus valoran la firma Authenticode. Un certificado OV/EV o Azure Trusted Signing evita avisos al usuario final.

```powershell
# Con certificado en el almacén
.\scripts\sign.ps1 -Thumbprint 0123ABCD... -Files "C:\job\spooler\server.exe","C:\job\spooler\cliente.exe"

# Desde PFX
.\scripts\sign.ps1 -PfxPath .\spooler.pfx -PfxPassword (Read-Host -AsSecureString) -Files ...
```

Verificar:
```powershell
Get-AuthenticodeSignature C:\job\spooler\server.exe | Format-List Status, StatusMessage, SignerCertificate
```

`Valid` = cadena de confianza correcta. `UnknownError` con autofirmado = firma existe pero CA no es pública.

## Actualización en producción

1. `sc stop SpoolerFiscalServer`
2. Respalde `C:\job\spooler\data\` y `C:\job\spooler\configs\`
3. Reemplace `C:\job\spooler\server.new.exe` (o recompile: `go build -trimpath -ldflags "-s -w" -o server.exe ./cmd/server`)
4. Ejecute **como Admin**: `C:\job\spooler\instalar.cmd`
   - Reemplaza binario, reinstala servicio, arranca, verifica
5. Verifique: `C:\job\spooler\verificar.cmd`
6. Haga una venta de control (no fiscal o fiscal si ya está desbloqueada)

## Límites y decisiones operativas

- **Un solo spooler por impresora.** Dos servidores sobre el mismo puerto físico se pelearían por el COM; el diseño asume un único escritor.
- **No hay reimpresión automática.** Si se corta la comunicación después de escribir, la factura pudo imprimirse: revise el papel antes de reintentar.
- **El correlativo legal lo asigna la impresora.** El spooler lo audita y detecta duplicados, pero no lo inventa.
- **Cambiar la numeración de la impresora invalida la auditoría**: si se reemplaza la máquina fiscal, documente el cambio y respalde la base.
- **Hoy**: documentos no fiscales ✅, facturas fiscales ❌ (NAK 0x15) — ver MANUAL.md §12.