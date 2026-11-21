package main

const serverUsage = `Uso: server.exe [opciones]

Spooler fiscal central: recibe los comandos de todas las estaciones y los
ejecuta en orden sobre una única impresora fiscal. Mantiene el correlativo y la
auditoría, y expone una API HTTP.

Opciones:
  -config <archivo>  Configuración YAML (por omisión configs\server.yaml)
  -console           Ejecutar en primer plano (recomendado para probar)
  -install           Instalar como servicio de Windows (inicio automático)
  -uninstall         Detener y desinstalar el servicio
  -debug             Ejecutar el servicio adjunto a la consola
  -docs              Guía rápida y rutas del manual
  -help              Esta ayuda

Ejemplos:
  server.exe -console -config configs\server.yaml
  server.exe -install -config C:\SpoolerFiscal\configs\server.yaml

Documentación:
  http://127.0.0.1:8080/docs
  http://127.0.0.1:8080/docs/openapi.yaml
  docs\manual.html
`

const serverDocs = `Guía rápida — Servidor fiscal
=============================

1. Edite configs\server.yaml:
   - token: el mismo que usarán todos los agentes
   - printer.port.name: puerto real de la impresora (por ejemplo COM1)
   - printer.command_terminator: "\r\n"  (con \r solo la TFHKA no responde)
   - http.listen: 0.0.0.0:8080 (único puerto; lo usan agentes y sistemas externos)

2. Arranque en consola:
   server.exe -console -config configs\server.yaml

3. Verifique:
   curl http://127.0.0.1:8080/v1/health
   curl -H "Authorization: Bearer <token>" http://127.0.0.1:8080/v1/status

4. Emita una factura de prueba:
   curl -X POST http://127.0.0.1:8080/v1/invoices/structured ^
     -H "Authorization: Bearer <token>" ^
     -H "Content-Type: application/json" ^
     -d "{\"station\":\"ECOM-01\",\"items\":[{\"description\":\"CAFE\",\"quantity\":1,\"unit_price\":10.00}]}"

5. Instale como servicio:
   server.exe -install -config C:\SpoolerFiscal\configs\server.yaml
   sc start SpoolerFiscalServer

Manual completo:
   docs\manual.html  o  http://<servidor>:8080/docs

Ejemplos por lenguaje (curl, PHP, Go, Python, Node.js, Rust):
   examples\
`
