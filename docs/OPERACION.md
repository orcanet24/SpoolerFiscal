# Manual de operación

## Servicios

| Servicio | Máquina | Binario |
| --- | --- | --- |
| `SpoolerFiscalServer` | PC con la impresora | `server.exe` |
| `SpoolerFiscalAgent` | cada estación | `agent.exe` |

La forma recomendada de instalarlos es [`INSTALAR.md`](INSTALAR.md) (servicio con
inicio automático, reinicio ante fallos y permisos restringidos).

```powershell
Get-Service SpoolerFiscalServer, SpoolerFiscalAgent
sc.exe qc SpoolerFiscalServer        # START_TYPE: AUTO_START
sc.exe qfailure SpoolerFiscalServer  # acciones de reinicio
sc.exe stop SpoolerFiscalServer
sc.exe start SpoolerFiscalServer

# modo consola para diagnóstico (detenga el servicio antes)
C:\SpoolerFiscal\server.exe -console -config C:\SpoolerFiscal\configs\server.yaml
```

- El servidor arranca con `AUTO_START` y se reinicia a los 5 s, 10 s y 30 s si
  termina con error (contador diario).
- El agente usa `delayed-auto`: arranca automáticamente unos 30 s después del
  inicio, cuando com0com ya cargó. Si el COM aún no está, reintenta con backoff
  sin detenerse.
- El servicio fija su directorio de trabajo en el del ejecutable, así las rutas
  relativas de `data\` y `logs\` no acaban en `C:\Windows\System32`.

## Logs y rotación

- Archivo: `logs\server.log` y `logs\agent.log` (configurable).
- Rotación con lumberjack: `max_size_mb` (20), `max_backups` (10),
  `max_age_days` (30), `compress` (zip de los antiguos).
- Nivel: `debug | info | warn | error`. `debug` en el agente registra además los
  bytes en hexadecimal (`bridge.log_hex_payloads`).
- Cada minuto el servidor escribe una línea `métricas` con documentos, comandos,
  errores, duplicados, latencia promedio/máxima, profundidad de cola y estado.

## Permisos del token y de la auditoría

El YAML de configuración contiene el token compartido. Al arrancar, el servicio
restringe la ACL del archivo y del directorio `data\` a SYSTEM, Administradores y
el usuario que ejecuta el proceso (en Windows usa `icacls` con SIDs, así funciona
en equipos en español o inglés). Si el proceso no puede cambiar la ACL, deja una
advertencia en el log y continúa funcionando.

```powershell
icacls C:\SpoolerFiscal\configs\server.yaml /inheritance:r /grant:r "*S-1-5-18:F" "*S-1-5-32-544:F"
icacls C:\SpoolerFiscal\data /inheritance:r /grant:r "*S-1-5-18:F" "*S-1-5-32-544:F"
```

Los scripts `install-server.ps1` e `install-agent.ps1` aplican esta ACL. El modo
consola debe ejecutarse como administrador (o como el usuario que instaló),
porque el token ya no es legible por el grupo `Usuarios`.

## Seguridad: token por estación y mTLS

- `token`: token compartido para la API externa y para las estaciones que no
  tengan uno propio.
- `station_tokens`: un token por estación. Con token propio, esa estación **sólo**
  se representa con su token: una petición que pida otra estación recibe `403`, y
  el token compartido deja de servirle. Una caja comprometida no puede facturar
  como otra.
- `tls.enabled` con `cert_file`/`key_file`: HTTPS.
- `tls.ca_file`: **mTLS**. El servidor exige certificado de cliente firmado por
  esa CA; el agente configura `cert_file`/`key_file` (los dos juntos).

Comprobación rápida (con `token-caja-01` de `CAJA-01`):

```powershell
# debe responder 403: el token de CAJA-01 no emite como CAJA-02
curl.exe -i -X POST http://127.0.0.1:8080/v1/commands `
  -H "Authorization: Bearer token-caja-01" `
  -H "Content-Type: application/json" `
  -d "{\"station\":\"CAJA-02\",\"command\":\"S2\"}"
```

## Capacidad y tiempos de espera

La impresora es el cuello de botella: un documento por vez. Medición del circuito
completo (10 estaciones, 5 facturas cada una, 50 documentos) con impresora
emulada y 8 ms por comando: **5,0 s, ~10 facturas/s, 101 ms por factura**. Una
impresora fiscal real tarda del orden de 1-3 s por factura, así que la capacidad
práctica es **0,3-1 factura por segundo en total**, sin importar cuántas cajas
haya.

- Con 10 cajas y ráfagas simultáneas, la última factura espera
  `nº de facturas en cola × 2 s`. Suba `queue.default_timeout_ms` (30 s por
  omisión) a 60000-90000 si hay ráfagas.
- `queue.max_pending: 256` es holgado para 10 cajas; por encima de 30 cajas
  revise la memoria y los timeouts.
- El turno por factura es exclusivo: la caja que abre una factura no lo suelta
  hasta cerrarla. Evita que dos facturas se mezclen en el papel; a cambio, las
  demás esperan (no se rechazan).
- Un comando que vence en la cola **no se imprime**: se descarta y el cliente
  recibe `DeadlineExceeded`. Una factura nunca se completa con ítems viejos.

## API REST

`GET /v1/health` sirve para monitoreo y
`GET /v1/status` devuelve cola, documento abierto y motivo de bloqueo. La emisión
externa usa `POST /v1/invoices` (líneas TFHKA) o `/v1/invoices/structured` (ítems
JSON); cada factura retiene el turno hasta el `101`,
igual que una caja. Errores frecuentes: `409 printer_blocked` (papel o memoria),
`504 timeout` (no se imprimió) y `429 queue_full`. Detalle en
[`API.md`](API.md).

## Panel de operación

`http://<servidor>:8080/admin` sirve un panel para el operador:

- estado de la impresora, cola, documento abierto y motivo de bloqueo;
- últimos trabajos con **Cancelar** (si no llegó a papel) y **Reintentar** (si
  está pausado sin líneas impresas);
- últimos documentos emitidos.

La página no contiene secretos: pide el token y lo guarda sólo en la pestaña.
Las mismas operaciones están en `POST /v1/jobs/{id}/cancel` y `/retry`. Si el
operador no debe ver el panel, bloquéelo en el firewall o no lo publique.

## Qué registrar en la auditoría

Tabla `documents` (nunca se purga):

| Columna | Contenido |
| --- | --- |
| `sequence` | secuencia global del spooler |
| `doc_type` / `doc_number` | tipo y número asignado por la impresora |
| `station_id` / `session_id` | estación de origen y su sesión |
| `issued_at` | fecha/hora del registro |
| `request_sha256` / `response_sha256` | huellas del comando y la respuesta |
| `response_text` | respuesta cruda de la impresora |

Tabla `command_log`: hashes, latencia, resultado y error de cada comando
(retención configurable con `database.retention_days`).

Consultas útiles (con cualquier visor SQLite, por ejemplo DB Browser for SQLite):

```sql
SELECT sequence, doc_type, doc_number, station_id, issued_at
FROM documents ORDER BY sequence DESC LIMIT 20;

SELECT doc_number, station_id, issued_at FROM documents
WHERE doc_type = 'invoice' AND date(issued_at) = date('now') ORDER BY doc_number;

SELECT station_id, COUNT(*) FROM documents GROUP BY station_id;
```

## Respaldo

Con el servicio detenido, copie `data\spooler.db` (y `-wal`/`-shm` si existen).
También puede copiarlos en caliente: SQLite en WAL mantiene consistencia, pero el
respaldo en frío es el más simple. **La base es la evidencia del correlativo**:
respáldela antes de cualquier actualización.

## Diagnóstico

| Síntoma | Causa probable | Solución |
| --- | --- | --- |
| `no se pudo abrir el COM virtual` en el agente | com0com no instalado, par no creado o puerto usado por otra app | `setupc list`; cierre la app que lo usa; revise `virtual_com.name` |
| Profit Plus dice "impresora fiscal no responde" | agente caído, servidor inalcanzable o token distinto | `sc query SpoolerFiscalAgent`; pruebe `telnet IP 8080`; compare tokens |
| `token de estación inválido` | token distinto entre agente y servidor | iguale `server.token` o `SPOOLER_TOKEN` |
| `estación no autorizada` | `allowed_stations` no incluye el `station_id` | agréguelo o vacíe la lista |
| `emisión bloqueada: PAPER_OUT` | la impresora reportó fin de papel | cargue papel; el bloqueo se libera con la siguiente respuesta limpia |
| `emisión bloqueada: FISCAL_MEMORY_FULL` | memoria fiscal llena | cierre Z / reemplace la memoria según normativa |
| `la impresora no respondió` | cable, puerto o impresora apagada | revise `printer.port.name`, baudios y cable; el comando **no** se reenvió |
| `cola de impresión llena` | impresora muy lenta o caída | suba `queue.max_pending`; revise el estado de la impresora |
| `doble emisión detectada` | la impresora reinició su numeración | audite la factura; no se sobrescribió el registro original |
| `versión de protocolo incompatible` | binarios de distinta versión | actualice agente y servidor juntos |
| Latencia alta | muchas estaciones o `read_gap_ms` grande | ajuste `read_gap_ms` (30 ms típico) y revise `métricas` |

## Windows Defender y los binarios

Defender puede marcar los ejecutables Go sin firmar como «virus o software
potencialmente no deseado» (falso positivo). Los síntomas son:

- `go build` termina con `No se pudo completar la operación porque el archivo
  contiene un virus o software potencialmente no deseado`;
- `bin\server.exe` desaparece después de compilarlo o responde `Permission
  denied` al ejecutarlo.

`scripts\build.ps1` lo detecta y reintenta la compilación **sin** `-s -w`, que
suele bastar porque cambia la firma del binario. Para forzarlo desde el inicio:

```powershell
.\scripts\build.ps1 -NoStrip
```

Si vuelve a ocurrir, revise **Seguridad de Windows → Protección antivirus y
contra amenazas → Historial de protección** y, en un equipo de desarrollo,
excluya sólo la carpeta del proyecto (requiere PowerShell como administrador):

```powershell
Add-MpPreference -ExclusionPath "C:\Proyectos\GO\SpoolerFiscal"
```

Para revertirlo:

```powershell
Remove-MpPreference -ExclusionPath "C:\Proyectos\GO\SpoolerFiscal"
```

Excluir una carpeta reduce la protección del equipo: hágalo sólo con la carpeta
del proyecto (nunca `C:\` ni el perfil completo) y, en producción, prefiera
firmar los binarios o usar la lista blanca del antivirus corporativo.

## Firma de los binarios

Windows SmartScreen y algunos antivirus valoran la firma Authenticode. Un
binario sin firmar o firmado con un certificado autofirmado puede mostrar
advertencias; un certificado de una CA pública (OV/EV) o Azure Trusted Signing
es lo que realmente evita el aviso al usuario final.

`scripts\sign.ps1` firma `bin\server.exe` y `bin\agent.exe` con SHA-256 y sello
de tiempo, y `scripts\build.ps1 -Sign` lo hace al final de la compilación.

Con el certificado en el almacén (`CurrentUser\My`):

```powershell
.\scripts\build.ps1 -Sign -Thumbprint 0123ABCD...
```

Desde un PFX (se importa al almacén durante la firma):

```powershell
.\scripts\build.ps1 -Sign -PfxPath .\spooler.pfx -PfxPassword (Read-Host -AsSecureString)
```

Sin `-Thumbprint` ni `-PfxPath`, el script usa el primer certificado de firma de
código con clave privada que encuentre. El sello de tiempo
(`http://timestamp.digicert.com`) mantiene la firma válida cuando el certificado
expira; sin Internet, el script firma igualmente sin sello.

Verificar:

```powershell
Get-AuthenticodeSignature .\bin\server.exe | Format-List Status, StatusMessage, SignerCertificate
```

`Valid` significa que la cadena de confianza es correcta. `UnknownError` con un
certificado autofirmado es normal: la firma existe, pero la CA no es de
confianza pública.

El autofirmado sólo sirve para pruebas o para equipos donde se distribuya la CA
interna por GPO; no elimina los avisos de SmartScreen en equipos de clientes.
Al firmar desde un PFX, el certificado queda importado en `CurrentUser\My`;
puede eliminarlo con `Remove-Item Cert:\CurrentUser\My\<huella>` si no desea
conservarlo. No guarde el `.pfx` ni su contraseña en el repositorio: use el
almacén de Windows o un gestor de secretos.

## Actualización

1. Detenga los servicios.
2. Respalde `data\` y `configs\`.
3. Reemplace `bin\server.exe` y `bin\agent.exe` (misma versión en todos lados),
   o vuelva a ejecutar `INSTALAR.bat` sobre la instalación existente: conserva la
   configuración y solo actualiza el binario y el servicio.
4. Arranque primero el servidor y luego los agentes.
5. Ejecute la verificación de la impresora y compare con la línea base
   ([`PUESTA_EN_MARCHA.md`](PUESTA_EN_MARCHA.md)):
   `sc stop SpoolerFiscalServer`, `.\bin\verificar\verificar.exe -config .\configs\server.yaml -out .\data\verificacion-post.json`,
   `sc start SpoolerFiscalServer`.
6. Verifique `Ping` en los logs (`conectado al spooler … version_servidor`).

## Límites y decisiones operativas

- **Un solo spooler por impresora.** Dos servidores sobre el mismo puerto físico
  se pelearían por el COM; el diseño asume un único escritor.
- **No hay reimpresión automática.** Si se corta la comunicación después de
  escribir, la factura pudo imprimirse: revise el papel antes de reintentar.
- **El correlativo legal lo asigna la impresora.** El spooler lo audita y detecta
  duplicados, pero no lo inventa.
- **Cambiar la numeración de la impresora invalida la auditoría**: si se reemplaza
  la máquina fiscal, documente el cambio y respalde la base.
