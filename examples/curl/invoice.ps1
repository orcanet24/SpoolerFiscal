# Emite una factura de prueba contra la API REST del spooler fiscal.
#
#   $env:SPOOLER_TOKEN = "mi-token"
#   .\invoice.ps1
#   $env:SPOOLER_URL = "http://192.168.1.10:8080"
param(
    [string]$SpoolerUrl = $(if ($env:SPOOLER_URL) { $env:SPOOLER_URL } else { "http://127.0.0.1:8080" }),
    [string]$Station    = $(if ($env:STATION) { $env:STATION } else { "ECOM-01" }),
    [string]$OrderId    = $(if ($env:ORDER_ID) { $env:ORDER_ID } else { "pedido-1001" })
)

if (-not $env:SPOOLER_TOKEN) {
    Write-Error "Defina SPOOLER_TOKEN con el token del spooler."
    exit 1
}

$payload = @{
    station    = $Station
    client_ref = $OrderId
    items      = @(
        @{ description = "CAFE"; quantity = 1; unit_price = 10.00; tax = "general" }
        @{ description = "PAN";  quantity = 2; unit_price = 5.00;  tax = "reduced" }
    )
} | ConvertTo-Json -Depth 5

$headers = @{
    Authorization    = "Bearer $($env:SPOOLER_TOKEN)"
    "Idempotency-Key" = $OrderId
}

try {
    $factura = Invoke-RestMethod -Method Post -Uri "$SpoolerUrl/v1/invoices/structured" `
        -Headers $headers -ContentType "application/json; charset=utf-8" `
        -Body ([System.Text.Encoding]::UTF8.GetBytes($payload)) -TimeoutSec 60
} catch {
    $detalle = $_.ErrorDetails.Message
    Write-Error "No se pudo emitir la factura: $($_.Exception.Message) $detalle"
    exit 1
}

Write-Host "Factura $($factura.invoice_number) emitida: total $($factura.totals.total)"
if ($factura.document.response_text) {
    Write-Host "Impresora: $($factura.document.response_text)"
}
