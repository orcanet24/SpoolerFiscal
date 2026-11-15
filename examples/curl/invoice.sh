#!/usr/bin/env bash
# Emite una factura de prueba contra la API REST del spooler fiscal.
#
#   SPOOLER_TOKEN=mi-token ./invoice.sh
#   SPOOLER_URL=http://192.168.1.10:8080 STATION=ECOM-01 ./invoice.sh
set -euo pipefail

SPOOLER_URL="${SPOOLER_URL:-http://127.0.0.1:8080}"
STATION="${STATION:-ECOM-01}"
ORDER_ID="${ORDER_ID:-pedido-1001}"
: "${SPOOLER_TOKEN:?Defina SPOOLER_TOKEN con el token del spooler}"

payload=$(cat <<JSON
{
  "station": "${STATION}",
  "client_ref": "${ORDER_ID}",
  "items": [
    {"description": "CAFE", "quantity": 1, "unit_price": 10.00, "tax": "general"},
    {"description": "PAN",  "quantity": 2, "unit_price": 5.00,  "tax": "reduced"}
  ]
}
JSON
)

# -w separa el cuerpo del código HTTP en la última línea.
response=$(curl -sS -w '\n%{http_code}' -X POST "${SPOOLER_URL}/v1/invoices/structured" \
  -H "Authorization: Bearer ${SPOOLER_TOKEN}" \
  -H "Content-Type: application/json" \
  -H "Idempotency-Key: ${ORDER_ID}" \
  -d "${payload}")

status="${response##*$'\n'}"
body="${response%$'\n'*}"

if [ "${status}" != "201" ]; then
  echo "ERROR ${status}: ${body}" >&2
  exit 1
fi

if command -v jq >/dev/null 2>&1; then
  echo "${body}" | jq -r '"Factura \(.invoice_number) emitida: total \(.totals.total) (secuencia \(.sequence))"'
  echo "${body}" | jq -r '.document.response_text // empty' | sed 's/^/Impresora: /'
else
  echo "${body}"
fi
