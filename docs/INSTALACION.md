# Manual de instalación

Arquitectura: **una PC servidor** con la impresora fiscal conectada y **una PC por
estación** con Profit Plus. Todas en la misma red.

## Instalación rápida (recomendada)

Doble clic en `build.bat` y luego en **`INSTALAR.bat`**: el asistente instala el
servicio de Windows (inicio automático), genera la configuración, restringe
permisos y comprueba que arranca. Guía: [INSTALAR.md](INSTALAR.md).

El resto de este manual describe la instalación manual paso a paso; es útil para
entender qué hace el instalador o para casos especiales.

## 0. Requisitos

- Windows 10/11 o Server 2019/2022, x64.
- Go 1.26 o superior **sólo en la máquina de compilación** (los ejecutables no lo
  necesitan).
- Puerto serie físico para la impresora (o adaptador USB-serie).
- Firewall: el puerto HTTP del servidor (8080 por omisión) accesible desde las
  estaciones.
- Permisos de administrador para instalar servicios y com0com.

## 1. Compilar

```powershell
cd C:\Proyectos\GO\SpoolerFiscal
.\scripts\build.ps1
```

Genera `bin\server.exe` y `bin\agent.exe`. También puede compilar a mano:

```powershell
go build -o bin\server.exe .\cmd\server
go build -o bin\agent.exe   .\cmd\agent
```

## 2. Servidor (PC con la impresora)

1. Copie a la PC servidor: `bin\server.exe`, `configs\server.yaml` y una carpeta
   `data\` escribible. Estructura sugerida: `C:\SpoolerFiscal\`.
2. Edite `configs\server.yaml`:
   - `http.listen`: `0.0.0.0:8080` (único puerto; lo usan agentes y sistemas externos).
   - `token`: un secreto largo y único (o use la variable `SPOOLER_TOKEN`).
   - `station_tokens`: opcional, un token por estación. Una estación con token
     propio ya no se representa con el compartido y no puede emitir como otra.
   - `printer.port.name`: el COM real de la impresora (ej. `COM1`).
   - `printer.port.baud_rate`: 9600 o 115200 según la impresora.
   - `allowed_stations`: opcional, lista de estaciones autorizadas.
3. Pruebe en consola antes de instalar:

   ```powershell
   .\bin\server.exe -console -config .\configs\server.yaml
   ```

   Debe registrar `spooler fiscal escuchando` y el driver configurado.
4. Instale el servicio (inicio automático + reinicio ante fallos):

   ```powershell
   .\scripts\install-server.ps1 -Root C:\SpoolerFiscal
   ```

   Equivalente manual:

   ```cmd
   sc create SpoolerFiscalServer binPath= "C:\SpoolerFiscal\bin\server.exe -config C:\SpoolerFiscal\configs\server.yaml" start= auto
   sc start SpoolerFiscalServer
   ```

5. Abra el puerto en el firewall:

   ```powershell
   New-NetFirewallRule -DisplayName "Spooler Fiscal HTTP" -Direction Inbound -Protocol TCP -LocalPort 8080 -Action Allow
   ```

   La REST comparte el token (o usa `station_tokens`). Si cruza una red no
   confiable, active `tls.enabled` (HTTPS). Con `tls.ca_file` el servidor exige
   certificado de cliente (mTLS): el agente configura `cert_file`/`key_file`.
   No exponga la API a Internet.

## 3. Estación (PC con Profit Plus)

### 3.1 Crear el par de puertos virtuales

1. Instale com0com (driver firmado, gratuito): <https://sourceforge.net/projects/com0com/>.
2. Créelo con el script incluido, en PowerShell **como administrador**:

   ```powershell
   .\scripts\setup-com0com.ps1 -ProfitPort COM3 -AgentPort COM4
   ```

   O manualmente:

   ```
   "C:\Program Files (x86)\com0com\setupc.exe" install PortName=COM3 PortName=COM4
   ```

3. Verifique en el Administrador de dispositivos que aparecen `COM3` y `COM4`
   emparejados.

> `COM3` queda para Profit Plus; `COM4` lo abre el agente. Si esos números están
> ocupados, use otros (por ejemplo `COM11`/`COM12`) y ajuste la configuración.

### 3.2 Configurar el agente

1. Copie a la estación: `bin\agent.exe`, `configs\agent.yaml` y una carpeta
   `logs\`. Estructura sugerida: `C:\SpoolerFiscal\`.
2. Edite `configs\agent.yaml`:
   - `station_id`: único por estación (`CAJA-01`, `CAJA-02`, …). **No lo repita**:
     es la identidad que queda en la auditoría.
   - `server.address`: `http://IP-del-servidor:8080`.
   - `server.token`: el mismo del servidor.
   - `virtual_com.name`: `COM4` (el extremo del agente).
   - `virtual_com`: los mismos parámetros que la impresora (9600 8N1 típico).
   - `logging.console`: `false` cuando corra como servicio.
3. Pruebe en consola:

   ```powershell
   .\bin\agent.exe -console -config .\configs\agent.yaml
   ```

   Debe mostrar `COM virtual abierto` y `conectado al spooler`.
4. Instale el servicio:

   ```powershell
   .\scripts\install-agent.ps1 -Root C:\SpoolerFiscal
   ```

### 3.3 Apuntar Profit Plus al puerto virtual

Ver [`PROFITPLUS.md`](PROFITPLUS.md). En resumen: impresora fiscal **TFHKA /
The Factory**, puerto **COM3**, 9600 8N1, sin control de flujo.

## 4. Verificación de extremo a extremo

0. **Puesta en marcha contra la impresora física.** Antes de operar, ejecute el
   diagnóstico (detenga el servicio para liberar el puerto):

   ```powershell
   sc stop SpoolerFiscalServer
   .\bin\verificar\verificar.exe -config .\configs\server.yaml -out .\data\verificacion-inicial.json
   ```

   Debe dar `Resultado: OK` y mostrar el serial y el RIF reales de la máquina.
   El procedimiento completo (papel agotado, reporte X, primera venta) está en
   [`PUESTA_EN_MARCHA.md`](PUESTA_EN_MARCHA.md).

1. En el servidor, revise `logs\server.log`: debe aparecer `sesión abierta` con el
   `station_id` al abrir Profit Plus.
2. Emita la primera **venta real** desde Profit Plus (no una factura de prueba:
   cada factura fiscal consume un correlativo y es un documento real).
3. En el servidor debe aparecer `documento fiscal registrado` con tipo, número y
   estación; en la estación, `documentos registrados` por el spooler.
4. En la base: `data\spooler.db`, tabla `documents`.
5. API HTTP:

   ```bash
   curl http://127.0.0.1:8080/v1/health
   curl -H "Authorization: Bearer CAMBIAR-ESTE-TOKEN" http://127.0.0.1:8080/v1/status
   ```

   Una factura completa desde un sistema externo:

   ```bash
   curl -X POST http://127.0.0.1:8080/v1/invoices \
     -H "Authorization: Bearer CAMBIAR-ESTE-TOKEN" \
     -H "Content-Type: application/json" \
     -H "Idempotency-Key: pedido-1001" \
     -d '{"station":"ECOM-01","lines":["!   1.000     10.00CAFE","101"]}'
   ```

   Detalle en [`API.md`](API.md) y contrato en [`../api/openapi.yaml`](../api/openapi.yaml).

Prueba sin impresora física (laboratorio):

```powershell
# impresora fiscal emulada (proyecto SimuladorFiscalGO)
.\simuladorfiscal.exe serve --model tfhka --addr 127.0.0.1:2011

# servidor apuntando al emulador: printer.transport = tcp, tcp_address = 127.0.0.1:2011

# prueba automática contra el emulador
$env:SPOOLER_E2E_PRINTER_ADDR = "127.0.0.1:2011"
go test ./internal/e2e -run TestAgainstExternalPrinter -v
```

### Papel real en una térmica común (simprint)

`simprint` (en `bin\simprint\simprint.exe`, incluido por `build.bat`) es un
emulador de impresora fiscal TFHKA que **imprime con papel real** en una
térmica de 80 mm (ESC/POS): hace de impresora fiscal en el protocolo y de
térmica en la salida. No emite documentos fiscales válidos: es para probar el
circuito completo (correlativo, cola, pausa por papel, reintentos, formato).

```powershell
# térmica instalada en Windows (USB00x), como una Xprinter XP-80
.\bin\simprint\simprint.exe -listen 127.0.0.1:2011 -win-printer "Xprinter XP-80"

# térmica de red (puerto 9100)
.\bin\simprint\simprint.exe -listen 127.0.0.1:2011 -escpos-tcp 192.168.1.50:9100

# térmica por puerto COM (modo CDC/serie)
.\bin\simprint\simprint.exe -listen 127.0.0.1:2011 -com COM5

# solo texto en consola (vista previa, sin hardware)
.\bin\simprint\simprint.exe -listen 127.0.0.1:2011
```

Con el servidor apuntado a `printer.transport = tcp` y
`printer.tcp_address = 127.0.0.1:2011`, cada factura del spooler se imprime en
la térmica. La tecla `p` + Enter alterna el sensor de papel para probar la
pausa y reanudación de la cola; `estado` muestra los correlativos; `salir`
cierra guardando el estado en `data\simprint_state.json`.

### Ver la factura en el panel de SimuladorFiscalGO (puente TCP→panel)

Con `-panel`, `simprint` recibe el protocolo por TCP y lo reenvía por HTTP al
panel web de **SimuladorFiscalGO**: la factura se ve en el papel virtual del
navegador (`http://127.0.0.1:8787`) con el mismo recorrido que una impresora
real (pre-vuelo S1, ítems, cierre y conciliación).

```powershell
# 1) panel del simulador (una sola instancia, la que se ve)
& C:\Proyectos\GO\SimuladorFiscalGO\simuladorfiscal.exe panel --host 127.0.0.1 --port 8787 --no-browser

# 2) puente: spooler <-TCP:2011- simprint -HTTP-> panel :8787
.\bin\simprint\simprint.exe -listen 127.0.0.1:2011 -panel http://127.0.0.1:8787

# 3) servidor: printer.transport = tcp, printer.tcp_address = 127.0.0.1:2011
# 4) factura por curl -> se imprime en el panel
curl.exe -X POST http://127.0.0.1:8080/v1/invoices -H "Authorization: Bearer CAMBIAR-ESTE-TOKEN" `
  -H "Content-Type: application/json" -H "Idempotency-Key: pedido-1" `
  -d '{\"station\":\"CAJA-01\",\"lines\":[\"!   1.000     10.00CAFE\",\"101\"]}'
```

El agente de estación (`bin\cliente`) se conecta al servidor por HTTP con el
`.env` (`SPOOLER_SERVER_ADDR=http://127.0.0.1:8080`, `SPOOLER_TOKEN` y
`SPOOLER_STATION_ID`); el puente COM de la estación requiere `com0com`.


## 5. Desinstalar

```powershell
.\scripts\uninstall.ps1            # ambos servicios
.\scripts\uninstall.ps1 -Only agent
```

Los datos (`data\spooler.db`) y los logs no se borran.
