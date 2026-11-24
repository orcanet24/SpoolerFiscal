package main

const agentUsage = `Uso: agent.exe [opciones]

Agente de estación: expone la impresora fiscal en un puerto COM virtual local
y reenvía los bytes al spooler central. Profit Plus cree que habla con una
impresora; el agente transporta los bytes sin interpretarlos.

Opciones:
  -config <archivo>       Configuración YAML (por omisión configs\agent.yaml)
  -console                Ejecutar en primer plano (recomendado para probar)
  -install                Instalar como servicio de Windows (inicio automático)
  -uninstall              Detener y desinstalar el servicio
  -debug                  Ejecutar el servicio adjunto a la consola
  -docs                   Guía rápida y rutas del manual
  -help                   Esta ayuda
  -install-com0com        Descargar e instalar com0com + crear par COM virtual
  -profit-port <COM>      Puerto para Profit Plus (por omisión COM3)
  -agent-port <COM>       Puerto para el agente (por omisión COM4)

Ejemplos:
  agent.exe -console -config configs\agent.yaml
  agent.exe -install -config C:\SpoolerFiscal\configs\agent.yaml
  agent.exe -install-com0com -profit-port COM3 -agent-port COM4
  agent.exe -install-com0com                (usa virtual_com del agent.yaml)

Documentación:
  docs\manual.html  o  http://<servidor>:8080/docs
`

const agentDocs = `Guía rápida — Agente de estación
================================

1. Instale com0com y cree un par de puertos (OPCIÓN AUTOMÁTICA):
   agent.exe -install-com0com -profit-port COM3 -agent-port COM4
   # O si ya tiene agent.yaml configurado:
   agent.exe -install-com0com

   Requiere: PowerShell/CMD como Administrador.
   Crea el par COM virtual en el kernel (COM3 <-> COM4).

2. Edite configs\agent.yaml:
   - station_id: identificador único (por ejemplo CAJA-01)
   - server.address: URL del spooler (por ejemplo http://192.168.1.10:8080)
   - server.token: el mismo token del servidor
   - virtual_com.name: COM4 (debe coincidir con -agent-port)
   - virtual_com: 9600 8N1, sin control de flujo

3. Arranque en consola:
   agent.exe -console -config configs\agent.yaml
   El log debe mostrar "COM virtual abierto".

4. En Profit Plus configure la impresora fiscal:
   marca The Factory, puerto COM3, 9600 8N1, sin control de flujo.

5. Emita una factura de prueba y confirme en el servidor:
   logs\server.log -> "documento fiscal registrado".

6. Instale como servicio:
   agent.exe -install -config C:\SpoolerFiscal\configs\agent.yaml
   sc start SpoolerFiscalAgent

Si el agente repite "Serial port not found", revise com0com y que COM4 no esté
ocupado por otro programa.

Manual completo:
   docs\manual.html  o  http://<servidor>:8080/docs
`
