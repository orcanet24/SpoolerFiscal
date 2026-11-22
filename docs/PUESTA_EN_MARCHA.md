# Puesta en marcha con la impresora fiscal real

Esta guía es el procedimiento para dejar operativo el spooler con una impresora
física y para volver a validarlo después de cada actualización o cambio de
equipo. No es una prueba de software: es la comprobación de que **esta**
impresora, **este** cable y **este** firmware responden como el spooler espera.

> Con equipo fiscal no se "prueba en producción": o emite bien o no emite. Por
> eso el procedimiento separa lo que **no** imprime (diagnóstico), lo que imprime
> **sin** consumir correlativo (reporte X, documento no fiscal) y lo que **sí** es
> fiscal (la primera venta real, que debe ser una venta de verdad).

## 1. Antes de empezar

| Elemento | Comprobación |
| --- | --- |
| Impresora | Encendida, con papel, cable serie/USB-serie conectado al servidor |
| Puerto | El COM de la impresora es el de `printer.port.name` en `server.yaml` |
| Servicio | **Detenido**: `sc stop SpoolerFiscalServer` (el puerto es exclusivo) |
| Profit Plus | Cerrado en todas las estaciones |
| Config | `printer.driver`, `printer.transport` y el puerto/token ya ajustados |
| Datos | `data\spooler.db` respaldado antes de tocar el equipo |

El verificador abre el puerto directamente: si el servicio del spooler sigue
corriendo, fallará con "acceso denegado" o "puerto ocupado". Deténgalo primero.

## 2. Diagnóstico sin imprimir (obligatorio)

```powershell
cd C:\SpoolerFiscal
.\bin\verificar\verificar.exe -config .\bin\server\configs\server.yaml `
  -out .\data\verificacion-inicial.json
```

Hace tres sondas de sólo lectura (`S1` identificación, `S2` estado, `S5` memoria
fiscal) y consulta el estado interpretado por el driver. No imprime nada.

Salida esperada:

```
Verificación de impresora fiscal
  driver:    tfhka
  transporte: serial COM1 @ 9600 8N1

  [OK   ] identificacion_s1     0|MLTFHKA001|J-12345678-9|...
  [OK   ] estado_s2             ...
  [OK   ] memoria_fiscal_s5     ...
  [OK   ] estado_interpretado
  [OK   ] identificacion

  Impresora
    marca/modelo: tfhka TFHKA
    serial:       MLTFHKA001
    RIF:          J-12345678-9
    memoria:      4 MB (libre 3.6 MB)
    última factura: 1523 (NC 41, ND 7)

  Estado: online=true papel_agotado=false memoria_llena=false documento_abierto=false
  Informe: .\data\verificacion-inicial.json
Resultado: OK
```

Qué mirar:

- **serial y RIF** deben ser los de la máquina física (no un valor de ejemplo).
- **última factura** debe coincidir con el papel/cierre del día. Si no coincide
  con lo que la empresa cree, **deténgase**: hay documentos emitidos fuera del
  spooler y hay que conciliar antes de seguir.
- **memoria libre**: si está casi llena, planifique el cierre (Z) o el reemplazo
  según la normativa, antes de operar.
- Si el JSON sale con `"ok": false`, revise la sección 7.

Guarde el informe: es la línea base del equipo.

## 3. Prueba de impresión no fiscal (opcional pero recomendada)

```powershell
.\bin\verificar\verificar.exe -config .\bin\server\configs\server.yaml `
  -x -nonfiscal -yes -out .\data\verificacion-impresion.json
```

- `-x`: reporte X (lectura; **no** reinicia acumulados).
- `-nonfiscal`: imprime un documento no fiscal de prueba.

Ambas imprimen papel real, pero **no** consumen correlativo fiscal. Sin `-yes`
el programa pide confirmación.

## 4. Prueba de papel agotado

1. Retire el rollo de papel.
2. Ejecute de nuevo el diagnóstico (sección 2). Debe fallar con `fin de papel`
   y `Resultado: FALLA`.
3. Vuelva a colocar el papel y repita: debe volver a `OK`.

Esto valida el sensor y el guardián: con papel agotado el spooler **rechaza**
emisiones (`409 printer_blocked`) en vez de imprimir a medias.

## 5. Primera emisión fiscal real

La primera emisión debe ser una **venta real** atendida por el cajero, no una
factura de prueba. Recomendado: hágalo desde Profit Plus con el agente y el
servidor ya en marcha.

1. Arranque el servidor y el agente:
   `sc start SpoolerFiscalServer` y, en la estación, `sc start SpoolerFiscalAgent`.
2. Verifique la API: `GET /v1/health` y `GET /v1/status?probe=true`.
3. Emita la venta desde Profit Plus.
4. Compruebe, en este orden:
   - el papel muestra el número de factura esperado;
   - `GET /v1/documents?limit=1` devuelve ese mismo número con la estación de
     origen;
   - `GET /v1/status` ya no muestra documento abierto;
   - el contador `duplicates_detected` sigue en cero.

Si el número del papel no coincide con el registrado, no siga emitiendo: revise
`data\spooler.db` y la sección 7.

## 6. Reinicio y concurrencia

- **Reinicio:** detenga y arranque el servicio; `GET /v1/status` debe conservar
  la última factura y el historial (SQLite). Emita una venta más y compruebe que
  el correlativo continúa.
- **Concurrencia:** con dos o tres cajas, emita una venta en cada una casi a la
  vez. Las facturas deben salir completas y con correlativos únicos: el turno
  exclusivo impide que se entrelacen.

## 7. Fallos frecuentes

| Síntoma | Causa probable | Qué hacer |
| --- | --- | --- |
| `no se pudo abrir la impresora: ... acceso denegado` | El servicio del spooler sigue usando el puerto | `sc stop SpoolerFiscalServer` y repetir |
| `sin respuesta dentro del tiempo configurado` | Cable, puerto equivocado, impresora apagada o `command_terminator` distinto de CRLF | Revise cable/COM; el terminador debe ser `"\r\n"` |
| `fin de papel` | Sin rollo o sensor | Cargue papel; repetir |
| `memoria fiscal llena` | Memoria llena | Cierre Z / reemplace según normativa; el spooler bloqueará emisiones |
| `Comando invalido (0x50)` en `S1`/`S5` | Firmware que no implementa la sonda o driver equivocado | Confirme modelo/firmware y driver (`tfhka`) |
| Serial/RIF vacíos | El firmware no devuelve el bloque esperado | Deténgase: el `Describe` del driver debe ajustarse al Anexo 1/2 del equipo |
| `documento_abierto=true` sin operador | Factura colgada de una sesión anterior | El spooler intentará el cierre de recuperación; si no, revise el papel |

## 8. Después de cada actualización

1. Detenga el servicio.
2. Respalde `data\` y `configs\`.
3. Reemplace los binarios (misma versión en servidor y estaciones).
4. Repita la sección 2 comparando con la línea base:

   ```powershell
   .\bin\verificar\verificar.exe -config .\bin\server\configs\server.yaml `
     -out .\data\verificacion-post.json -compare .\data\verificacion-inicial.json
   ```

   Debe terminar con `Comparación con la línea base: sin diferencias`. Si el
   serial, el RIF o el correlativo cambiaron, el programa falla con el detalle:
   eso significa que se cambió de máquina o que la numeración retrocedió, y hay
   que detenerse antes de emitir.

   > Si compiló con `scripts\build.ps1`, el ejecutable queda en
   > `.\bin\verificar.exe` (sin subcarpeta).
5. Arranque el servicio y haga una venta de control.

Los informes JSON de `data\verificacion-*.json` son evidencia: guárdelos junto al
registro de despliegue. Si algún día hay una discrepancia de correlativo, esa
línea base es lo que permite reconstruir qué pasó.

## 9. Qué no hacer

- No emita facturas de prueba en producción: cada una consume un correlativo
  fiscal y es un documento real.
- No "cierre" a mano un documento abierto con `101` si hay una venta en curso:
  el spooler ya lo hace cuando la sesión desaparece, y hacerlo antes puede
  cortar la factura a mitad.
- No exponga la API a Internet. En red local use el token por estación y, si
  cruza segmentos, TLS/mTLS ([Operación](OPERACION.md)).
