# Compatibilidad: instala el servidor con el instalador unico (scripts\instalar.ps1).
#   .\scripts\install-server.ps1 -Root C:\SpoolerFiscal
#   .\scripts\install-server.ps1 -Root C:\SpoolerFiscal -ConfigPath C:\...\server.yaml
#
# Recomendado: INSTALAR.bat (menu) o scripts\instalar.ps1 -Role server.
param(
    [string]$Root = (Split-Path -Parent $PSScriptRoot),
    [string]$ServiceName = "SpoolerFiscalServer",
    [string]$ConfigPath = ""
)

$ErrorActionPreference = "Stop"
$installer = Join-Path $PSScriptRoot "instalar.ps1"
if (-not (Test-Path -LiteralPath $installer)) { throw "No se encontro $installer" }

$argLine = '-NoProfile -ExecutionPolicy Bypass -NoExit -File "{0}" -Role server -TargetDir "{1}" -ServiceName "{2}"' -f $installer, $Root, $ServiceName
if ($ConfigPath) { $argLine += ' -ConfigPath "{0}"' -f $ConfigPath }

$ps = Join-Path $env:SystemRoot "System32\WindowsPowerShell\v1.0\powershell.exe"
Start-Process -FilePath $ps -ArgumentList $argLine -Verb RunAs
exit 0
