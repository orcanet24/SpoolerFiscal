# Instalación con el instalador

`INSTALAR.bat` deja el spooler funcionando como **servicio de Windows** en dos
pasos: compilar y ejecutar el instalador. El servicio arranca solo con el equipo,
se reinicia si falla y los datos quedan en una carpeta con permisos restringidos.

## Requisitos

- Windows 10/11 o Windows Server 2019/2022, x64.
- Permisos de administrador (el instalador los pide por UAC).
- Servidor: la impresora fiscal encendida y conectada (COM o TCP).
- Estación: Profit Plus y, para el COM virtual, **com0com** instalado.
- Los binarios compilados (`build.bat` o `.\scripts\build.ps1`).

## 1. Compilar

Doble clic en `build.bat` en la raíz del proyecto (o, desde PowerShell,
`.\scripts\build.ps1`). Se generan `server.exe`, `agent.exe`/`cliente.exe` y
`verificar.exe` dentro de `bin\`.

## 2. Instalar

Doble clic en **`INSTALAR.bat`** y elija:

```
  1) Instalar servidor (PC con la impresora)
  2) Instalar estación (PC con Profit Plus)
  3) Verificar impresora (detiene el servicio)
  4) Desinstalar servicios
  5) Salir
```

Al terminar, la ventana queda abierta con el resumen (servicio, rutas, panel y,
en el servidor, el token que deben usar las estaciones).

## Qué hace el instalador

En ambos roles:

1. Copia el binario a `C:\SpoolerFiscal` (configurable con `-TargetDir`).
2. Genera `configs\server.yaml` o `configs\agent.yaml` (con respaldo si ya
   existía) con rutas absolutas para datos y logs.
3. Restringe los permisos de la configuración y de `data\` a SYSTEM y
   Administradores (el YAML contiene el token).
4. Crea el servicio de Windows con **inicio automático** y **reinicio ante
   fallos** (5 s, 10 s y 30 s; contador diario).
5. Lo arranca y comprueba que responde (API en el servidor, COM virtual en la
   estación).
6. Servidor: abre el puerto TCP en el firewall (perfiles Dominio y Privado) y
   copia `verificar.exe` para la puesta en marcha.

## Servidor

Preguntas del asistente:

| Pregunta | Ejemplo | Nota |
| --- | --- | --- |
| Driver | `tfhka` | `tfhka`, `hasar`, `bixolon` o `epson` |
| Transporte | `serial` | `serial` o `tcp` |
| Puerto | `COM1` | el de la impresora física |
| Baudios | `9600` | 9600 u 115200 según el equipo |
| Puerto HTTP | `8080` | API y panel; ábralo en el firewall |
| Token | (vacío = generar) | las estaciones deben usar el mismo |

Al finalizar:

- API: `http://127.0.0.1:8080/v1/health`
- Panel: `http://127.0.0.1:8080/admin`
- Servicio: `SpoolerFiscalServer`

**Siguiente paso obligatorio:** la puesta en marcha contra la impresora real
([`PUESTA_EN_MARCHA.md`](PUESTA_EN_MARCHA.md)). El instalador deja el comando
listo en el resumen.

## Estación

Preguntas del asistente:

| Pregunta | Ejemplo | Nota |
| --- | --- | --- |
| Estación | `CAJA-01` | letras, números y `- _ . :` |
| Servidor | `http://192.168.1.10:8080` | si omite `http://`, se añade |
| Token | el del servidor | si no lo tiene, pídalo al administrador |
| COM del agente | `COM4` | extremo del par com0com que abre el agente |
| COM de Profit Plus | `COM3` | el otro extremo del par |

Si com0com está instalado, el asistente ofrece crear el par. Si no, avisa y el
agente queda esperando el puerto (reintenta solo). Profit Plus debe configurarse
con el puerto `COM3` (o el que haya elegido).

Servicio: `SpoolerFiscalAgent`, con inicio **automático diferido** para dar
tiempo a que com0com cargue tras el reinicio.

## Instalación silenciosa (varias estaciones)

Para desplegar por GPO, SCCM o un script:

```powershell
powershell -NoProfile -ExecutionPolicy Bypass -File .\scripts\instalar.ps1 `
  -Role server -Silent -TargetDir C:\SpoolerFiscal `
  -Token "TOKEN-LARGO" -PrinterPort COM1 -BaudRate 9600 -HttpPort 8080

powershell -NoProfile -ExecutionPolicy Bypass -File .\scripts\instalar.ps1 `
  -Role agent -Silent -TargetDir C:\SpoolerFiscal `
  -Token "TOKEN-LARGO" -StationId CAJA-02 `
  -ServerAddr http://192.168.1.10:8080 -AgentPort COM4 -ProfitPort COM3
```

> Si la política de ejecución de PowerShell bloquea los scripts, use siempre
> `powershell -NoProfile -ExecutionPolicy Bypass -File ...`; `INSTALAR.bat` ya lo
> hace.

Notas del modo silencioso:

- El agente exige `-Token` y `-ServerAddr`; el servidor genera el token si no se
  lo pasan y lo muestra en el resumen.
- Crea el servicio y arranca; **no** crea el par com0com (use
  `scripts\setup-com0com.ps1` o el modo interactivo).
- El servidor abre el firewall salvo `-NoFirewall`.
- Re-ejecutarlo conserva la configuración existente salvo `-Force`; con
  `-ConfigPath` puede indicar un YAML ya preparado.
- Devuelve código 0 si todo fue bien; distinto de 0 si un paso falló.

Para revisar qué haría sin tocar el equipo: agregue `-DryRun`.

## Servicio: arranque tras reinicio y recuperación

```powershell
Get-Service SpoolerFiscalServer, SpoolerFiscalAgent
sc.exe qc SpoolerFiscalServer      # START_TYPE : AUTO_START
sc.exe qfailure SpoolerFiscalServer
```

- `AUTO_START`: arranca solo al iniciar Windows.
- `delayed-auto` (agente): arranca ~30 s después, cuando com0com ya cargó.
- `sc failure`: reinicia el servicio a los 5, 10 y 30 segundos si termina con
  error, con contador diario.

Si un servicio no arranca, el Visor de eventos (Registro de Windows > Sistema)
y `logs\server.log` / `logs\agent.log` dicen por qué.

## Actualizar

1. Detenga el servicio.
2. Respalde `data\` y `configs\`.
3. Copie los binarios nuevos (o vuelva a ejecutar el instalador; conserva la
   configuración).
4. Arranque el servicio.
5. Ejecute la verificación comparando con la línea base
   ([`PUESTA_EN_MARCHA.md`](PUESTA_EN_MARCHA.md)).

## Desinstalar

`INSTALAR.bat` > opción 4, o `.\scripts\uninstall.ps1`.

- Detiene y elimina los servicios y la regla de firewall.
- **Conserva** los binarios, la configuración y `data\spooler.db` (la auditoría
  fiscal). Pregunta antes de borrar `data\`; en modo silencioso sólo lo hace con
  `-RemoveData`.

## Problemas frecuentes

| Síntoma | Causa | Solución |
| --- | --- | --- |
| "No se encontró server.exe" | Falta compilar | Ejecute `build.bat` |
| El servicio se detiene al arrancar | Configuración inválida o puerto HTTP ocupado | `server.exe -console -config ...` para ver el error |
| La estación no conecta | Token o dirección distintos | Compare `token` y `server.address` con el servidor |
| "acceso denegado" al COM | Otra aplicación usa el puerto (Profit Plus abierto) | Cierre Profit Plus; el agente reintenta |
| El panel no carga desde otra PC | Firewall o perfil de red Público | Use perfiles Dominio/Privado o abra el puerto a mano |
| `sc query` dice STOPPED | Revise el Visor de eventos y el log | Corrija la causa y `sc start ...` |
