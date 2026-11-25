# Puesta en marcha con la impresora fiscal real (actualizado 2026-11-24)

> Esta guía es el procedimiento para dejar operativo el spooler con una impresora
> física y para volver a validarlo después de cada actualización o cambio de
> equipo. No es una prueba de software: es la comprobación de que **esta**
> impresora, **este** cable y **este** firmware responden como el spooler espera.
>
> **Estado actual (2026-11-24):**
> - ✅ Documentos no fiscales (80$ / 80! / 81) → **funcionan e imprimen papel real**
> - ✅ Consultas S1/S2/S3/S4/S5 → **devuelven datos reales** (serial, RIF, correlativos, memoria)
> - ❌ Facturas fiscales (ítems `!`/`"`/`#` + `101`) → **NAK (0x15)** — pendiente configuración HKA80 (ver §7)

---

## 1. Antes de empezar

| Elemento | Comprobación |
|---|---|
| Impresora | Encendida, con papel, cable serie/USB-serie conectado al servidor |
| Puerto | El COM de la impresora es el de `printer.port.name` en `server.yaml` (`COM1`) |
| Servicio | **Detenido**: `sc stop SpoolerFiscalServer` (el puerto es exclusivo) |
| Profit Plus | Cerrado en todas las estaciones |
| Config | `printer.driver: tfhka`, `printer.transport: serial`, puerto/token ya ajustados |
| Datos | `data\spooler.db` respaldado antes de tocar el equipo |

Si no sabe en qué puerto está la impresora, ejecute primero el detector (no imprime ni consume correlativo):

```cmd
C:\job\spooler\detectar.exe
```

Si el detector informa que Windows no expone ningún puerto serie, no es un problema del spooler: falta el controlador del puerto o el cable. En el Administrador de dispositivos, un "Puerto serie PCI" con error 28 indica que hay que instalar el controlador de chipset/serie del fabricante y reiniciar.

El verificador abre el puerto directamente: si el servicio del spooler sigue corriendo, fallará con "acceso denegado" o "puerto ocupado". Deténgalo primero.

---

## 2. Diagnóstico sin imprimir (obligatorio)

```cmd
C:\job\spooler\verificar.cmd
```

Hace tres sondas de sólo lectura (`S1` identificación, `S2` estado, `S5` memoria fiscal) y consulta el estado interpretado por el driver. No imprime nada.

**Salida esperada (ejemplo real):**

```
--- Servicio ---
NOMBRE_SERVICIO: SpoolerFiscalServer
        ESTADO             : 4  RUNNING

--- API ---
{"driver":"tfhka","protocol_version":1,"status":"ok","uptime_seconds":2}

--- Impresora (sonda S2) ---
{"printer":{"online":true,"status_byte":0,"error_byte":0,"status_text":"","error_text":"","paper_out":false,"fiscal_memory_full":false,"fiscal_memory_almost_full":false,"raw":"S2 00000000000000000\n 00000000000000000\n 00000000000000000\n00000000000000000\n 00000000000000000\n0000\n0","checked_at_unix":1795571715},"online":true,"blocked":false,"queue_depth":0}

--- Ultimos numeros de control ---
{"brand":"tfhka","firmware":"n/d","fiscal_memory_free_mb":1384,"fiscal_memory_total_mb":1908,"fiscal_rif":"J-503895241","last_credit_note":259,"last_debit_note":0,"last_invoice_number":10850,"last_non_fiscal_doc":921,"last_z_number":893,"model":"TFHKA","port":"COM1 @ 9600 8N1","serial":"Z7C7022079"}
```

### Qué mirar:

- **serial y RIF** deben ser los de la máquina física (no un valor de ejemplo).
- **última factura** debe coincidir con el papel/cierre del día. Si no coincide con lo que la empresa cree, **deténgase**: hay documentos emitidos fuera del spooler y hay que conciliar antes de seguir.
- **memoria libre**: si está casi llena, planifique el cierre (Z) o el reemplazo según la normativa, antes de operar.
- Si el JSON sale con `"ok": false` (en el verificador completo), revise la sección 7.

Guarde el informe: es la línea base del equipo.

---

## 3. Prueba de impresión no fiscal (recomendada)

```cmd
# 80$ abre documento no fiscal
curl -X POST http://127.0.0.1:8080/v1/commands -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" -d "{\"station\":\"TEST\",\"command_base64\":\"ODAk\"}"

# 80! escribe línea
curl -X POST ... -d "{\"station\":\"TEST\",\"command_base64\":\"ODAhUFJVRUJBIFRlc3QK\"}"

# 81 cierra e imprime
curl -X POST ... -d "{\"station\":\"TEST\",\"command_base64\":\"ODE=\"}"
```

Debe imprimir papel real con "PRUEBA Test". **No consume correlativo fiscal**.

---

## 4. Prueba de papel agotado

1. Retire el rollo de papel.
2. Ejecute de nuevo `verificar.cmd`. Debe fallar con `paper_out: true` y `fiscal_memory_full: false`.
3. Vuelva a colocar el papel y repita: debe volver a `online: true`, `paper_out: false`.

Esto valida el sensor y el guardián: con papel agotado el spooler **rechaza** emisiones (`409 printer_blocked`) en vez de imprimir a medias.

---

## 5. Primera emisión fiscal real

> **Hoy devuelve NAK (0x15)**. Ver sección 7 para desbloquear.

La primera emisión debe ser una **venta real** atendida por el cajero, no una factura de prueba.

1. Arranque el servidor y el agente:
   `sc start SpoolerFiscalServer` y, en la estación, `sc start SpoolerFiscalAgent`.
2. Verifique la API: `GET /v1/health` y `GET /v1/status?probe=true`.
3. Emita la venta desde Profit Plus.
4. Compruebe, en este orden:
   - el papel muestra el número de factura esperado;
   - `GET /v1/documents?limit=1` devuelve ese mismo número con la estación de origen;
   - `GET /v1/status` ya no muestra documento abierto;
   - el contador `duplicates_detected` sigue en cero.

Si el número del papel no coincide con el registrado, no siga emitiendo: revise `data\spooler.db` y la sección 7.

---

## 6. Reinicio y concurrencia

- **Reinicio:** detenga y arranque el servicio; `GET /v1/status` debe conservar la última factura y el historial (SQLite). Emita una venta más y compruebe que el correlativo continúa.
- **Concurrencia:** con dos o tres cajas, emita una venta en cada una casi a la vez. Las facturas deben salir completas y con correlativos únicos: el turno exclusivo impide que se entrelacen.

---

## 7. Desbloqueo de facturas fiscales (NAK 0x15)

La impresora acepta no-fiscal pero rechaza fiscal. **No es bug del spooler** (entrega bytes exactos y devuelve respuesta cruda). Posibles causas en la HKA80:

### 7.1 Ver modo actual
`S2` ya devuelve `status_byte: 0` = "Modo fiscal - en espera" ✅. Si fuera `0x01`-`0x03` → modo prueba.

### 7.2 Probar apertura de jornada / modo fiscal
```cmd
# U2 - estado extendido
curl -X POST .../v1/commands -d "{\"station\":\"TEST\",\"command_base64\":\"VTI=\"}"

# U4 - modo fiscal
curl -X POST .../v1/commands -d "{\"station\":\"TEST\",\"command_base64\":\"VTQ=\"}"
```

### 7.3 Enviar cliente ANTES del ítem (obligatorio en TFHKA)
```cmd
# iR*RIF
curl -X POST .../v1/commands -d "{\"station\":\"TEST\",\"command_base64\":\"aVIqSjEyMzQ1Njc4LTkK\"}"

# iS*NOMBRE
curl -X POST .../v1/commands -d "{\"station\":\"TEST\",\"command_base64\":\"aVMqQ09OU1VNSURPUiBGSU5BTAo=\"}"
```

### 7.4 Luego el ítem + cierre
```cmd
# ! 1.000 10.00CAFE
curl -X POST .../v1/commands -d "{\"station\":\"TEST\",\"command_base64\":\"IT0gIDEuMDAwICAgMTAuMDBDQUZFCg==\"}"

# 101
curl -X POST .../v1/commands -d "{\"station\":\"TEST\",\"command_base64\":\"MTAx\"}"
```

### 7.5 Si sigue NAK
Revisar en la impresora física (teclado/display):
- Menú **"Modo Fiscal"** → debe estar activo, no en prueba
- **"Apertura Jornada"** → ejecutar si no se hizo hoy
- **"Configuración Tasas"** → verificar que las alícuotas (16%, 8%, 30%) están cargadas
- Manual del SDK: `C:\Users\Usuario10\Desktop\TFHKA Venezuela - VB SDK\Library\TfhkaNet Español Venezuela Ver1.3.pdf`

---

## 8. Fallos frecuentes

| Síntoma | Causa probable | Qué hacer |
|---|---|---|
| `no se pudo abrir la impresora: ... acceso denegado` | El servicio del spooler sigue usando el puerto | `sc stop SpoolerFiscalServer` y repetir |
| `sin respuesta dentro del tiempo configurado` | Cable, puerto equivocado, impresora apagada o `command_terminator` distinto de CRLF | Revise cable/COM; el terminador debe ser `"\r\n"` (el driver envuelve en STX/ETX/LRC) |
| `fin de papel` | Sin rollo o sensor | Cargue papel; repetir |
| `memoria fiscal llena` | Memoria llena | Cierre Z / reemplace según normativa; el spooler bloqueará emisiones |
| `Comando invalido (0x50)` en `S1`/`S5` | Firmware que no implementa la sonda o driver equivocado | Confirme modelo/firmware y driver (`tfhka`) |
| Serial/RIF vacíos | El firmware no devuelve el bloque esperado | Deténgase: el `Describe` del driver debe ajustarse al Anexo 1/2 del equipo |
| `documento_abierto=true` sin operador | Factura colgada de una sesión anterior | El spooler intentará el cierre de recuperación; si no, revise el papel |

---

## 9. Después de cada actualización

1. Detenga el servicio.
2. Respalde `data\` y `configs\`.
3. Reemplace los binarios (misma versión en servidor y estaciones).
4. Repita la **sección 2** comparando con la línea base:

```cmd
C:\job\spooler\verificar.cmd
# Compare serial, RIF, última factura, último Z con la línea base guardada
```

Debe coincidir todo. Si el serial, el RIF o el correlativo cambiaron, **deténgase**: significa que se cambió de máquina o que la numeración retrocedió.

5. Arranque el servicio (`instalar.cmd`) y haga una venta de control (no fiscal si fiscal sigue bloqueada).

Los informes de `verificar.cmd` son evidencia: guárdelos junto al registro de despliegue. Si algún día hay una discrepancia de correlativo, esa línea base es lo que permite reconstruir qué pasó.

---

## 10. Qué no hacer

- No emita facturas de prueba en producción: cada una consume un correlativo fiscal y es un documento real.
- No "cierre" a mano un documento abierto con `101` si hay una venta en curso: el spooler ya lo hace cuando la sesión desaparece, y hacerlo antes puede cortar la factura a mitad.
- No exponga la API a Internet. En red local use el token por estación y, si cruza segmentos, TLS/mTLS ([Operación](OPERACION.md)).