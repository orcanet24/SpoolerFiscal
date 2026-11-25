# Decisiones de arquitectura y confirmaciones requeridas

Este documento responde los cinco puntos que la solicitud pide confirmar antes de
empezar. Cada decisión se tomó con lo disponible en el equipo de desarrollo y
queda aislada detrás de una interfaz para poder cambiarla sin tocar el resto.

## 1. Modelo de impresora fiscal: dos familias de protocolo

**Decisión:** están implementadas dos familias, elegidas con `printer.driver`:

- `tfhka` (The Factory HKA): conjunto de comandos ASCII documentado en
  [`PROTOCOLO_TFHKA.md`](PROTOCOLO_TFHKA.md): `S1`…`S8P`, `101`, `3`, `100/103`,
  `80$`/`81`, `d1`/`d2`, `U0X`/`U0Z`, ítems con prefijo `!`, `"`, `#`, espacio.
- `hasar`, `bixolon`, `epson` (IxBatch): `@OpenFiscalReceipt`,
  `@CustomerData`, `@PrintLineItem`, `@AddPayment`, `@Close`, `@Status`,
  `@PrintXReport`/`@PrintZReport`. Epson antepone `0xESTADO|0xERROR|` a cada
  respuesta.

**Por qué:** TFHKA es la marca más difundida en Venezuela y la que ya emula el
`SimuladorFiscalGO` del mismo autor (referencia byte a byte). Hasar SMH/P-615F,
Bixolon SRP-270 y Epson TM2000 comparten IxBatch en esa referencia, de modo que
una sola implementación cubre las tres. Validar todo el circuito sin hardware
fiscal es requisito del proyecto.

**Cómo cambiarla:** `internal/fiscal/printer.go` define `Printer`
(`Open/Close/SendCommand/ReadResponse/GetStatus/GetInfo/Describe/Step`) y el
registro de marcas. `internal/fiscal/New(driver, opts)` selecciona la
implementación. Para una marca de otra familia se agrega un archivo con su
protocolo y se registra con `fiscal.RegisterDriver`; ni el agente, ni la cola, ni
la persistencia cambian. El agente de estación transporta bytes sin interpretar
el protocolo, así que la marca es una decisión **del servidor**.

**Diferencias entre familias que el spooler ya contempla:**

- Cierre de recuperación: `101` en TFHKA, `@Close` en IxBatch
  (`fiscal.DocumentCloser`).
- Sonda de estado: `S2` en TFHKA, `@Status` en IxBatch (el driver corrige el
  valor del YAML).
- Correlativo: TFHKA lo concilia antes de abrir factura (`S1`); en IxBatch el
  número llega en la respuesta del cierre y se audita en ese momento. No se
  inventa un pre-vuelo que el protocolo no ofrece.
- `/v1/invoices/structured` compila TFHKA; con otro driver responde `501` para
  no enviar una secuencia sin validar.

**A confirmar con el cliente (crítico):** marca, modelo exacto y firmware. Si el
firmware devuelve un bloque de estado distinto al Anexo 1/2, se ajusta
`Describe` en `internal/fiscal/tfhka.go` o `internal/fiscal/ixbatch.go`.

## 2. Puerto COM virtual: par de com0com (driver existente, sin código de kernel propio)

**Decisión:** el agente abre un extremo de un par de com0com y Profit Plus abre el
otro. El puente (Go) lee bytes del extremo del agente y los reenvía; la respuesta
real de la impresora vuelve por el mismo camino. **No se escribe ningún driver
ni emulación de kernel**: com0com es un driver firmado de terceros, y todo el
comportamiento fiscal vive en el binario Go.

```
Profit Plus  ──COM3──┐            ┌──COM4──  agent.exe  ──HTTP───  server.exe  ──COM1──  Impresora
                     └── com0com ──┘
```

**Por qué com0com y no otra opción:**

| Opción | Veredicto |
| --- | --- |
| com0com (par de puertos) | **Elegida.** Driver firmado, gratuito, estándar de facto para POS en Windows; crea pares reales que Profit Plus ve como COM normales; instalación con `setupc.exe`. |
| Librería Go "nativa" de COM virtual | No existe una que cree puertos visibles para aplicaciones de terceros sin driver; las que hay dependen de un driver de todos modos. |
| Named pipe "disfrazado" de COM | Windows no expone un named pipe como `COMx`; Profit Plus sólo abre nombres COM, así que hace falta un driver intermedio de todas formas. |
| Driver propio | Prohibido por la solicitud y además exigiría firma de kernel y mantenimiento por versión de Windows. |
| Puerto serie físico + cable nulo | Válido en una sola estación, no permite compartir una impresora entre varias. |

**Instalación:** [`../scripts/setup-com0com.ps1`](../scripts/setup-com0com.ps1)
(`setupc install PortName=COM3 PortName=COM4`), documentada paso a paso en
[`INSTALACION.md`](INSTALACION.md). El agente acepta cualquier nombre de puerto,
así que también funciona con otros drivers de COM virtual o con un par hardware.

**Limitación del driver:** com0com ignora la velocidad; los parámetros del puerto
virtual existen para que Profit Plus y el agente declaren la misma trama (8N1,
9600 o 115200). La velocidad real la fija el puerto físico del servidor.

## 3. Profit Plus: versión y configuración del puerto

**Decisión:** la emulación es transparente al programa, porque el agente no
interpreta ni reescribe un solo byte: delimita tramas por silencio y las
transporta. Cualquier versión de Profit Plus que hable el protocolo TFHKA por un
puerto serie funciona igual.

**Procedimiento:** [`PROFITPLUS.md`](PROFITPLUS.md): configurar la impresora fiscal
como TFHKA/The Factory, puerto `COM3`, 9600 8N1 (o lo que use la impresora real),
sin control de flujo.

**A confirmar con el cliente:** versión de Profit Plus y captura de la pantalla
de configuración de impresora fiscal (nombre del modelo y parámetros del puerto).
Si Profit Plus exige un modelo distinto de TFHKA, el driver debe ser otro.

## 4. Sistema operativo: Windows 10/11 y Server 2019/2022, x64

**Decisión:** binarios `windows/amd64`, sin cgo, sin dependencias de runtime
(sólo el `.exe`). Servicio nativo vía `golang.org/x/sys/windows/svc`, instalable
con `sc create` (scripts incluidos) o con el flag `-install`.

**Consideraciones:** el servicio del agente debe iniciar **después** de que exista
el COM virtual; si com0com tarda en cargar, el agente reintenta abrir el puerto
con backoff (configurable) sin terminar. En el servidor, el puerto COM físico debe
existir antes de emitir; si no, las peticiones esperan en cola hasta el timeout.

**A confirmar:** versiones exactas. No se probó en Windows Server 2016 ni en
ediciones ARM.

## 5. Persistencia: SQLite embebido con `modernc.org/sqlite` (puro Go)

**Decisión:** SQLite en modo WAL, una sola conexión, esquema con `documents`,
`command_log` y `printer_state`.

**Por qué:**

| Opción | Veredicto |
| --- | --- |
| modernc.org/sqlite | **Elegida.** Puro Go (sin cgo, sin DLL), SQL para auditoría y reportes, transacciones, índice único que impide duplicados a nivel de motor. |
| mattn/go-sqlite3 | Requiere cgo y toolchain C en Windows; complica el build y el despliegue. |
| BoltDB/bbolt | Clave-valor: habría que construir índices y consultas a mano para auditar por estación, fecha o número. |

**Qué se guarda:** cada documento emitido (tipo, número, fecha/hora, estación,
sesión, hashes SHA-256 del comando y la respuesta, texto de la respuesta) y una
bitácora de comandos con latencia y resultado. La retención de la bitácora es
configurable; **los documentos no se purgan**.

## 6. API HTTP integrada en el servidor, no un gateway aparte

Los agentes de estación y los sistemas externos (ecommerce, ERP web, CRM) usan la
misma API HTTP del servidor. Se descartó un proceso intermedio: duplica despliegue,
token y ciclo de vida, y dos gateways podrían partir el turno por factura en dos
colas.

La API HTTP vive en `server.exe` (`internal/server/httpapi.go`) y usa el mismo
`Spooler`: una cola, un correlativo, un turno exclusivo y una auditoría. Se
configura con `http.listen`; el contrato es `api/openapi.yaml`.

Dos decisiones de seguridad y fiscalidad:

- **Idempotencia por cabecera**: un reintento con la misma `Idempotency-Key` no
  vuelve a imprimir; se guarda la respuesta en SQLite y se purga por retención.
- **Dos niveles de emisión**: `/v1/invoices` recibe líneas TFHKA ya formateadas
  (vía canónica, el servidor no interpreta fiscalidad) y
  `/v1/invoices/structured` compila la venta completa: datos del cliente,
  ítems, subtotal, descuento/recargo, formas de pago y cierre, en el orden
  canónico de la referencia. Valida el total antes de imprimir y exige que los
  pagos cubran el neto. No cubre la referencia a una factura original para
  notas de crédito: eso se envía por `/v1/invoices` con `iF*`/`iD*`/`iI*`.
  El compilador sólo emite TFHKA; con otro driver responde `501` para no enviar
  una secuencia sin validar.

## 7. Seguridad de la API: token por estación y mTLS

**Decisión:** el token compartido se mantiene para la API externa, y se agrega
`station_tokens` para atar cada estación a su propio token. Con token propio, la
estación sólo puede emitir en su nombre (`403` si pide otra) y el compartido deja
de representarla. La comparación es en tiempo constante.

**mTLS:** `tls.ca_file` activa `RequireAndVerifyClientCert`; el agente presenta
`cert_file`/`key_file`. Sin `ca_file`, TLS es de un solo sentido y el token
autentica al cliente.

**Por qué:** una caja comprometida no debe poder facturar como otra, y el token
compartido en un `.env` de estación es un secreto que viaja a todos lados. La
alternativa (un único token) era simple pero no distinguía estaciones.

**El panel `/admin` no se autentica por sí mismo:** la página es pública y no
contiene secretos; pide el token y lo guarda sólo en la sesión del navegador.
Las operaciones sensibles (cancelar/reintentar) siguen exigiendo token.

Los comandos externos se normalizan con `printer.command_terminator` (por
omisión `\r\n`): Profit Plus ya envía el suyo y no se duplica. Con `\r` solo, la
TFHKA no responde (verificado contra el simulador de referencia).

## Decisiones de comportamiento fiscal

1. **Un solo escritor.** Todas las peticiones pasan por un planificador con un
   worker dedicado (`internal/server/spooler.go`); no hay dos comandos en el
   puerto.
2. **Turno exclusivo por factura.** El planificador atiende una estación por vez
   y, cuando una abre una factura (primera línea de ítem), **retiene el turno
   hasta que la cierra**. Sin esto, dos cajas podrían intercalar ítems en el
   mismo documento impreso. Las sondas de estado sí pueden correr (sólo leen).
   Si Profit Plus desaparece a mitad de una factura, el turno se libera después
   de `queue.document_lock_timeout_ms` y queda un error crítico en el log.
3. **Sin reenvío automático tras escribir.** Si el comando salió y la respuesta no
   llegó, el spooler devuelve error y **no** reintenta: reintentar podría imprimir
   dos veces la misma factura. El reintento sólo ocurre *antes* de escribir
   (puerto ocupado/impresora caída) o cuando el servidor rechazó la sesión antes
   de encolar.
4. **Doble emisión detectada, no silenciada.** Si la impresora reinicia su
   numeración y repite un número, el spooler registra el evento, incrementa el
   contador `duplicates_detected` y devuelve `duplicate=true`; la factura ya está
   impresa, así que no se oculta el hecho.
5. **Guardián de estado.** Con papel agotado, memoria fiscal llena o error
   físico/fiscal, el spooler rechaza la emisión (`409 printer_blocked`) y lo
   informa por HTTP. Los errores de comando (p. ej. `0x50`) no bloquean.
6. **Respuestas íntegras.** Se devuelven los bytes exactos de la impresora; el
   agente los escribe tal cual en el COM virtual. Si el servidor no responde, no
   se inventa una respuesta fiscal: Profit Plus ve un timeout (o los bytes que el
   operador configure en `offline_response_hex`).
7. **Correlativo.** La numeración la asigna la impresora (es la fuente legal). El
   spooler mantiene la secuencia global de ejecución y el historial persistido;
   al arrancar carga el último número por tipo y la última secuencia, y lo
   reporta en `OpenSession`.

## Limitación conocida: control de flujo RTS/CTS

`go.bug.st/serial` no expone el handshake del driver. `flow_control: none`
(default) y `xonxoff` (implementado en la capa de stream) funcionan; `rtscts`
debe activarse en el driver del puerto (Administrador de dispositivos → Puerto →
Configuración → Avanzado) y el servicio lo advierte al arrancar. Las impresoras
fiscales típicas usan 9600 8N1 sin control de flujo.
