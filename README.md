# SpoolerFiscal

SpoolerFiscal es un **servicio de impresión fiscal centralizado para Windows**.
Conecta una impresora fiscal **The Factory HKA (TFHKA)** con varias estaciones
Profit Plus y, opcionalmente, con sistemas externos —como ecommerce, ERP, CRM,
aplicaciones web o móviles— mediante una API REST/JSON.

Su objetivo técnico es recibir solicitudes de emisión, serializarlas para que no
se mezclen documentos, enviarlas a la impresora fiscal real y conservar una
auditoría consultable. La impresora asigna el correlativo fiscal; el spooler
registra su respuesta y el número devuelto.

> **Importante: integración técnica no equivale a homologación tributaria.**
> Este repositorio implementa un driver TFHKA, una API y un flujo de impresión.
> No incluye autorización del SENIAT, homologación del software, conexión de
> reporte al SENIAT, firma electrónica, imprenta digital autorizada, generación
> de PDF ni certificación de que una factura concreta cumpla todos los requisitos
> aplicables al contribuyente.

## Utilidad y escenarios de uso

- **Varias cajas y una impresora fiscal:** permite que Profit Plus en distintas
  estaciones comparta una impresora TFHKA sin que los comandos de dos facturas
  se intercalen.
- **Ecommerce y sistemas web:** un servidor puede enviar una factura a imprimir
  cuando el pedido esté listo, guardar el número fiscal devuelto y adjuntar la
  impresión al paquete, sujeto a las obligaciones fiscales aplicables al emisor.
- **ERP, CRM y aplicaciones propias:** integra cualquier cliente HTTP que pueda
  enviar JSON y autenticarse con un token.
- **Auditoría y conciliación:** conserva documentos, correlativos observados,
  estación de origen, secuencia del spooler, respuestas de la impresora y una
  bitácora opcional de comandos.
- **Operación en red local:** centraliza impresora, cola, configuración y
  supervisión en un solo servidor Windows.

SpoolerFiscal **no** es un POS completo, un sistema de inventario, una pasarela
de pagos, un generador de guías de despacho, un servicio de correo, una
herramienta para diseñar facturas o una solución de facturación digital
conectada directamente al SENIAT. Su interfaz REST structured cubre líneas,
alícuotas y validación de totales; no cubre por sí sola todos los datos de
cliente, pagos, descuentos u otros documentos.

## Cómo funciona

```text
Profit Plus ── COM virtual ── agent.exe ── gRPC ──┐
                                                  ├── server.exe ── COM/serie ── TFHKA
Ecommerce / ERP / app ───────── REST/JSON ────────┘
```

- **`server.exe`** vive en el equipo que tiene conectada la impresora. Mantiene
  la cola serializada, el turno exclusivo de cada factura, el guardián de estado,
  la base SQLite y la API REST opcional.
- **`agent.exe`** se instala en cada estación Profit Plus. Comunica el COM
  virtual local con el servidor por gRPC; transporta las tramas sin interpretarlas.
- **Impresora:** el único driver implementado es `tfhka` (The Factory HKA,
  protocolo ASCII). La conexión serie es para uso real; TCP se admite para
  laboratorio o un puente serie/red.
- **API REST:** vive dentro de `server.exe`; REST y estaciones comparten la
  misma impresora, cola, correlativo observado y auditoría.
- **Persistencia:** SQLite en `data/spooler.db`. Los documentos no se purgan;
  la retención de la bitácora de comandos es configurable.

Cada factura REST se ejecuta de forma atómica bajo un turno exclusivo. La
impresora fiscal es la fuente del número del documento; el spooler no lo inventa.
La idempotencia permite reintentar solicitudes sin volver a imprimir una factura
ya completada cuando se reutiliza la misma clave y el mismo cuerpo.

## Situación fiscal y SENIAT

La normativa cambia y la autorización depende del contribuyente, su actividad,
el medio de emisión y la versión concreta del software. Esta sección es una
referencia de orientación, **no asesoría legal o tributaria**.

**Consulta realizada el 14 de noviembre de 2026:**

1. El [SENIAT mantiene una relación de software y versiones autorizadas para
   emitir facturas y otros documentos fiscales](https://seniatenlinea.seniat.gob.ve/tributos-software-facturacion/).
   Esa página identifica la Providencia Administrativa
   **SNAT/2024/000121** y advierte que versiones no incluidas en la lista no
   están autorizadas. Verifique la versión exacta del sistema que emite, no sólo
   la marca de su impresora.
2. El portal también publica la relación de [imprentas digitales
   autorizadas](https://seniatenlinea.seniat.gob.ve/tributos-imprenta-digital/)
   (Providencia **SNAT/2024/0102**) y de [proveedores y modelos de máquinas
   fiscales](https://seniatenlinea.seniat.gob.ve/tributos-maquinas-fiscales/).
   Son categorías distintas; usar una máquina fiscal no convierte a cualquier
   software conectado a ella en software autorizado.
3. La Providencia **SNAT/2026/00093**, publicada en la Gaceta Oficial N.º
   **43.484** del 21 de octubre de 2026, establece un nuevo marco para la emisión
   de facturas y otros documentos. Su disposición final prevé una vacatio legis
   de 180 días continuos; con esa fecha de publicación, la entrada en vigencia
   corresponde al **19 de abril de 2027**; a la fecha de consulta todavía no
   está en vigor. Consulte el [texto de la Gaceta
   Oficial N.º 43.484](https://www.grantthornton.com.ve/globalassets/1.-member-firms/venezuela/2026/gaceta-43.484.pdf)
   (copia alojada por Grant Thornton) y confirme la aplicación de sus artículos
   y disposiciones transitorias con su asesor tributario o el SENIAT. Para
   contexto, consulte también los resúmenes de
   [HLB Marambio](https://www.marambio-hlb.com/seniat-dicta-la-providencia-snat-2026-00093-sobre-emision-de-facturas-y-documentos-fiscales-gaceta-oficial-n-43-484/?lang=es)
   y [The Factory HKA](https://imprenta.thefactoryhka.com.ve/facturacion-digital/providencia-snat-2026-00093-cambios-en-la-facturacion-en-venezuela/);
   son análisis de terceros, no sustituyen el texto de la Gaceta ni una consulta
   tributaria.

### ¿Puede usarse para facturas físicas que acompañen paquetes?

**Técnicamente**, la integración REST puede recibir una factura y enviarla a una
impresora TFHKA conectada al servidor; si la impresora la emite correctamente,
el sistema que integra puede guardar el número fiscal y usar la copia impresa en
su flujo logístico. Consulte [API y ejemplos de `curl`](#integración-rápida-con-curl).

**Tributariamente**, no asuma que la impresión de un pedido web es suficiente
para cumplir con el régimen aplicable al comercio electrónico. La nueva
Providencia 2026/00093 introduce cambios cuya vigencia está prevista para el
19-04-2027; proveedores del sector resumen que habrá requisitos particulares
para ventas en plataformas digitales y que los talonarios físicos quedan para
contingencias. El alcance concreto debe confirmarse en el texto oficial y según
la actividad del contribuyente.

Este proyecto no está en la lista del SENIAT de software y versiones
autorizadas, ni presta el servicio de una imprenta digital autorizada. Por
tanto:

- No lo presente por sí solo como autorización para emitir facturas fiscales o
  digitales.
- Antes de producción, confirme si el emisor puede usar el software, la máquina
  y el flujo de emisión seleccionados, y si necesita un software homologado o un
  proveedor de facturación digital autorizado.
- Si se factura digitalmente a través de un proveedor autorizado, confirme con
  dicho proveedor cómo generar una **representación impresa** para el paquete;
  no emita un segundo documento fiscal con este spooler para reproducirla.
- Revise las normas y listas vigentes antes de desplegar y ante cada cambio
  regulatorio. Las tasas tributarias del compilador también son valores
  configurados en el código, no una actualización normativa automática.

Referencias oficiales adicionales: [Normativa legal del
SENIAT](https://seniatenlinea.seniat.gob.ve/normativa-legal/) y [búsqueda de
Gacetas de la Imprenta Nacional](https://www.gacetaoficial.gob.ve/gacetas/filtro-avanzado).

## Requisitos

- **Servidor y estaciones:** Windows 10/11 o Windows Server 2019/2022, x64.
- **Compilación:** Go **1.26 o superior**, conforme al `go.mod`. Go sólo hace
  falta en la máquina de compilación; los ejecutables no requieren instalarlo.
- **Conexión fiscal:** impresora TFHKA compatible, conectada por puerto serie o
  adaptador USB-serie. Ajuste COM y parámetros según el modelo y firmware.
- **Profit Plus:** puerto COM virtual por estación; la guía del proyecto utiliza
  com0com.
- **Red:** el servidor debe aceptar gRPC desde los agentes (5050 por omisión).
  Si se habilita REST, los clientes también necesitan acceso al puerto HTTP
  configurado (8080 por omisión).

## Instalación resumida

Consulte la [guía de instalación completa](docs/INSTALACION.md) antes de
desplegar en producción. El flujo básico es:

1. Compile los binarios:

   ```powershell
   .\scripts\build.ps1
   ```

   Se generan `bin\server.exe` y `bin\agent.exe`.
2. En la PC con la impresora, configure `configs\server.yaml`: token único,
   puerto real de la impresora, lista de estaciones permitidas y opciones REST.
3. Pruebe el servidor en consola:

   ```powershell
   .\bin\server.exe -console -config .\configs\server.yaml
   ```
4. En cada estación, cree un par de puertos COM virtuales con com0com. Configure
   `configs\agent.yaml` con un `station_id` único, dirección/token del servidor y
   el puerto que abrirá el agente. Profit Plus usa el otro puerto del par.
5. Pruebe el agente y luego configure Profit Plus como TFHKA/The Factory.
6. Instale y opere los servicios con los scripts documentados en
   [instalación](docs/INSTALACION.md) y [operación](docs/OPERACION.md).

El servidor trae habilitada la API REST en `configs/server.yaml`; verifique el
valor `http.enabled` y el token antes de iniciar. Cambie las credenciales de
ejemplo. **No exponga la API a Internet.**

## Integración rápida con `curl`

Los ejemplos son para Bash/cURL. En Windows PowerShell use `curl.exe` para
evitar el alias `curl` de PowerShell. Defina la URL y el token del mismo
`server.yaml`:

```bash
export SPOOLER_URL="http://127.0.0.1:8080"
export SPOOLER_TOKEN="REEMPLAZAR-POR-UN-TOKEN-SEGURO"
```

### 1. Comprobar servicio y estado de la impresora

`/v1/health` no requiere token:

```bash
curl -sS "$SPOOLER_URL/v1/health"
```

Para consultar la impresora, la cola y el documento abierto:

```bash
curl -sS "$SPOOLER_URL/v1/status?probe=true" \
  -H "Authorization: Bearer $SPOOLER_TOKEN"
```

### 2. Emitir una factura desde ítems JSON

`/v1/invoices/structured` compila ítems JSON a comandos TFHKA, compara los
totales enviados con los calculados y entrega el trabajo a la impresora. El
ejemplo usa precios con IVA incluido, según el formato que maneja la TFHKA:

```bash
curl -i -X POST "$SPOOLER_URL/v1/invoices/structured" \
  -H "Authorization: Bearer $SPOOLER_TOKEN" \
  -H "Content-Type: application/json" \
  -H "Idempotency-Key: pedido-1001" \
  -d '{
    "station": "ECOM-01",
    "client_ref": "pedido-1001",
    "items": [
      {"description": "CAFE", "quantity": 1, "unit_price": 10.00, "tax": "general"},
      {"description": "PAN",  "quantity": 2, "unit_price": 5.00,  "tax": "reduced"}
    ],
    "totals": {"subtotal": 17.88, "tax": 2.12, "total": 20.00}
  }'
```

Una emisión terminada responde `201 Created`. La respuesta incluye el
`invoice_number` asignado por la impresora, `issued_at`, el `job_id`, líneas
compiladas, totales y respuesta del dispositivo. Guarde estos datos para
conciliar la operación con el pedido; sólo marque la factura como emitida cuando
la respuesta confirme que se completó.

Clases de impuesto implementadas en el compilador:

| `tax` | Prefijo TFHKA | Tasa codificada |
| --- | --- | ---: |
| `general` (por omisión) | `!` | 16 % |
| `reduced` | `"` | 8 % |
| `additional` | `#` | 30 % |
| `exempt` | espacio | 0 % |

Estas tasas son las reglas del compilador actual, **no una tabla fiscal
actualizada automáticamente ni una determinación de la tasa que corresponda a
cada producto**. Verifique las alícuotas, exenciones y el tratamiento del precio
con su responsable tributario.

### 3. Enviar líneas TFHKA ya preparadas

Use `/v1/invoices` si su sistema ya genera las líneas del protocolo. La primera
línea debe abrir la factura con un ítem y la última debe cerrarla con `101`;
el servidor ejecuta todas las líneas bajo el mismo turno exclusivo.

```bash
curl -i -X POST "$SPOOLER_URL/v1/invoices" \
  -H "Authorization: Bearer $SPOOLER_TOKEN" \
  -H "Content-Type: application/json" \
  -H "Idempotency-Key: pedido-1002" \
  -d '{
    "station": "ECOM-01",
    "client_ref": "pedido-1002",
    "lines": [
      "!   1.000     10.00CAFE",
      "101"
    ]
  }'
```

No agregue CR/LF a las líneas JSON: el servidor normaliza el terminador
configurado. Use caracteres y anchos compatibles con el protocolo y el modelo
real de impresora.

### 4. Envío asíncrono y consulta del trabajo

Para no mantener abierta la conexión mientras se imprime, agregue `?async=1`.
El servidor responde `202 Accepted` con un `job_id`:

```bash
curl -i -X POST "$SPOOLER_URL/v1/invoices/structured?async=1" \
  -H "Authorization: Bearer $SPOOLER_TOKEN" \
  -H "Content-Type: application/json" \
  -H "Idempotency-Key: pedido-1003" \
  -d '{
    "station": "ECOM-01",
    "client_ref": "pedido-1003",
    "items": [
      {"description": "LIBRO", "quantity": 1, "unit_price": 12.50, "tax": "general"}
    ]
  }'
```

Consulte el trabajo y espere su estado final antes de conciliar el pedido:

```bash
curl -sS "$SPOOLER_URL/v1/jobs/JOB_ID" \
  -H "Authorization: Bearer $SPOOLER_TOKEN"
```

Los estados pueden incluir `queued`, `printing`, `paused`, `aborted`, `done` y
`canceled`. Un trabajo `paused` puede reanudarse cuando vuelva la impresora; un
trabajo abortado requiere revisión y una nueva decisión de negocio, no un
reintento ciego.

### Reintentos y seguridad de emisión

- Envíe un `Idempotency-Key` único por pedido/factura. La misma clave y el mismo
  cuerpo reproducen la respuesta guardada; la misma clave con otro cuerpo
  responde `409 idempotency_conflict`.
- La idempotencia reduce duplicados por reintento HTTP, pero no sustituye la
  conciliación cuando se pierde la conexión o hay un fallo físico durante la
  emisión. Compruebe estado, trabajo y papel antes de volver a emitir.
- Una falla a mitad de factura puede dejar un trabajo abortado y parte del
  contenido ya impreso. No cree una segunda factura automáticamente sin revisión
  operativa.
- El token es compartido con las estaciones. Use HTTPS/TLS al cruzar redes no
  confiables, limite el acceso mediante firewall y `allowed_stations`, y no
  publique el puerto en Internet.

## API disponible

La API REST es opcional, HTTP/JSON y se configura en la sección `http` de
[`configs/server.yaml`](configs/server.yaml). Las rutas de negocio, excepto
`GET /v1/health`, exigen `Authorization: Bearer <token>` o
`X-Spooler-Token: <token>`.

| Método y ruta | Función |
| --- | --- |
| `GET /v1/health` | Salud y versión del servidor; sin autenticación. |
| `GET /v1/status` | Estado de impresora, cola, documento abierto y contadores. |
| `GET /v1/printer` | Modelo, serial, RIF fiscal, firmware y memoria fiscal. |
| `GET /v1/documents?limit=50` | Historial de documentos registrado por el spooler. |
| `POST /v1/commands` | Ejecuta un comando TFHKA individual. |
| `POST /v1/invoices` | Imprime una factura atómica a partir de líneas TFHKA. |
| `POST /v1/invoices/structured` | Compila e imprime una factura a partir de ítems JSON. |
| `GET /v1/jobs` | Lista trabajos REST y permite filtrar por estado. |
| `GET /v1/jobs/{id}` | Consulta el detalle y resultado de un trabajo. |

También se sirven la [documentación HTML](http://127.0.0.1:8080/docs) y el
[contrato OpenAPI](http://127.0.0.1:8080/docs/openapi.yaml), si HTTP está activo.
Consulte la [guía de API](docs/API.md) para esquemas, límites, respuestas y
códigos de error completos.

### Límites del endpoint structured

- `unit_price` incluye IVA; hasta dos decimales. `quantity` admite hasta tres.
- Descripción ASCII imprimible de hasta 40 caracteres; no admite `|` ni
  saltos de línea.
- Total de ítems y ancho de línea limitados por el protocolo TFHKA.
- Si se envían `totals`, el total se compara con el calculado; una diferencia
  fuera de tolerancia devuelve `400 totals_mismatch` y no se imprime.
- No incluye identificación del cliente ni formas de pago. Use
  `/v1/commands` sólo si conoce el protocolo TFHKA y el flujo correcto para el
  hardware instalado.

## Operación, capacidad y datos

- La impresora trabaja un documento a la vez. El turno por factura impide que
  estaciones distintas mezclen comandos en una misma factura.
- La cola y el guardián bloquean emisiones ante condiciones configuradas, como
  falta de papel, memoria fiscal llena o errores del dispositivo.
- La velocidad está limitada por la impresora; la capacidad observada con el
  equipo físico puede variar según el modelo, firmware, conexión y contenido.
- Los documentos registrados no se purgan automáticamente. La bitácora de
  comandos sí puede tener retención configurada.
- Si se pierde la respuesta después de enviar datos a la impresora, el estado
  físico puede requerir conciliación. El software evita el reenvío automático
  cuando éste podría generar una segunda impresión.

Para instalación de servicios, permisos, respaldo, diagnóstico, actualización y
detalles de capacidad, consulte [Operación](docs/OPERACION.md). Detalles del
protocolo en [TFHKA](docs/PROTOCOLO_TFHKA.md).

## Ejemplos y documentación

- [API REST: referencia y ejemplos adicionales](docs/API.md)
- [Manual HTML completo](docs/manual.html) — también se sirve en
  `/docs` cuando la API está activa
- [Instalación](docs/INSTALACION.md)
- [Operación y diagnóstico](docs/OPERACION.md)
- [Configuración de Profit Plus](docs/PROFITPLUS.md)
- [Protocolo TFHKA](docs/PROTOCOLO_TFHKA.md)
- [Ejemplos ejecutables por lenguaje](examples/README.md): Bash/cURL,
  PowerShell, Go, Node.js, PHP, Python y Rust
- [Contrato OpenAPI](api/openapi.yaml)

## Pruebas y compilación

```powershell
go test ./...
.\scripts\build.ps1
```

Las pruebas unitarias no requieren una impresora fiscal real. Las pruebas
end-to-end contra un emulador externo son opcionales; ver las instrucciones en
[Instalación](docs/INSTALACION.md).

## Licencia

Consulte [`LICENSE`](LICENSE).
