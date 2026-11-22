# Desinstala los servicios del Spooler Fiscal (por compatibilidad).
#   .\scripts\uninstall.ps1                 (ambos servicios)
#   .\scripts\uninstall.ps1 -Only server
#   .\scripts\uninstall.ps1 -Only agent -RemoveData
#
# Recomendado: INSTALAR.bat, opcion 4.
param(
    [ValidateSet("both", "server", "agent")]
    [string]$Only = "both",
    [switch]$RemoveData
)

$ErrorActionPreference = "Stop"
$installer = Join-Path $PSScriptRoot "instalar.ps1"
if (-not (Test-Path -LiteralPath $installer)) { throw "No se encontro $installer" }

$argLine = '-NoProfile -ExecutionPolicy Bypass -NoExit -File "{0}" -Role uninstall' -f $installer
if ($Only -eq "server") { $argLine += ' -ServiceName "SpoolerFiscalServer"' }
if ($Only -eq "agent") { $argLine += ' -ServiceName "SpoolerFiscalAgent"' }
if ($RemoveData) { $argLine += ' -RemoveData' }

$ps = Join-Path $env:SystemRoot "System32\WindowsPowerShell\v1.0\powershell.exe"
Start-Process -FilePath $ps -ArgumentList $argLine -Verb RunAs
exit 0
