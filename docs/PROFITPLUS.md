# Configurar Profit Plus en cada estación

Objetivo: que Profit Plus abra el puerto **COM3** (extremo de Profit Plus del par
com0com) como si fuera la impresora fiscal real. Del otro lado, el agente
(`COM4`) reenvía los bytes al servidor.

## Antes de empezar

1. El servicio `SpoolerFiscalAgent` está corriendo en la estación:

   ```powershell
   sc query SpoolerFiscalAgent
   ```

2. El log `logs\agent.log` muestra `COM virtual abierto` y `conectado al spooler`.
3. El par de puertos existe (`setupc list`) y **ninguna otra aplicación** tiene
   abierto `COM3` ni `COM4`.

## Pasos en Profit Plus

Los nombres de menú varían según la versión; el procedimiento es el habitual para
una impresora fiscal serie.

1. Entre a **Configuración → Impresoras / Impresoras fiscales**.
2. Cree o edite la impresora fiscal:
   - **Modelo/marca:** `The Factory HKA` (TFHKA). Es el protocolo implementado.
   - **Puerto:** `COM3` (el que dejó para Profit Plus).
   - **Velocidad:** la misma de la impresora real — `9600` u `115200`.
   - **Trama:** `8N1` (8 bits de datos, sin paridad, 1 bit de parada).
   - **Control de flujo:** ninguno.
   - **Timeout de comunicaciones:** 15–30 s (el agente espera al servidor).
3. Marque la impresora como **fiscal / serial** y asígnela a la caja o sucursal.
4. Guarde y cierre la configuración.

> Si Profit Plus tiene una opción de "detectar impresora", puede usarla: el
> agente responderá exactamente lo que responda la impresora del servidor.

## Prueba de emisión

1. Emita una **factura de prueba** con un ítem gravado.
2. El papel debe salir de la impresora física del servidor.
3. Verifique:
   - En la estación: `logs\agent.log` → `documentos registrados` (tipo y número).
   - En el servidor: `logs\server.log` → `documento fiscal registrado` con el
     `station_id` correcto.
   - En Profit Plus: la factura queda con el número fiscal que devolvió la
     impresora (el agente no lo modifica).
4. Emita otra factura: debe salir con el número consecutivo siguiente, aunque la
   haya emitido otra caja.

## Si Profit Plus reporta error de comunicaciones

| Verificación | Comando |
| --- | --- |
| Servicio activo | `sc query SpoolerFiscalAgent` |
| Puerto virtual existe | `"C:\Program Files (x86)\com0com\setupc.exe" list` |
| Puerto libre | cierre el panel del simulador u otra app que use `COM3`/`COM4` |
| Servidor alcanzable | `Test-NetConnection IP-SERVIDOR -Port 5050` |
| Token correcto | compare `server.token` con el del servidor |

El agente **no inventa respuestas** si el servidor no contesta: Profit Plus verá
un timeout de comunicaciones, que es el comportamiento correcto (no debe asumir
que una factura se emitió). Revise el papel antes de reintentar.

## Varias cajas en la misma PC

Cada instancia de Profit Plus necesita su propio par de puertos (por ejemplo
`COM3/COM4` para la caja 1 y `COM5/COM6` para la caja 2) y un `station_id`
distinto. No comparta un par entre dos procesos: el segundo no podrá abrirlo.
