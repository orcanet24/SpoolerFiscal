# Instala (o reinstala) el agente fiscal como servicio de Windows en la estación.
#   .\scripts\install-agent.ps1 -Root C:\SpoolerFiscal -ConfigPath C:\SpoolerFiscal\configs\agent.yaml
param(
    [string]$Root = (Split-Path -Parent $PSScriptRoot),
    [string]$ServiceName = "SpoolerFiscalAgent",
    [string]$ConfigPath = ""
)

$ErrorActionPreference = "Stop"

$identity = [Security.Principal.WindowsIdentity]::GetCurrent()
$principal = New-Object Security.Principal.WindowsPrincipal($identity)
if (-not $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
    throw "Ejecute este script como administrador."
}

if ([string]::IsNullOrWhiteSpace($ConfigPath)) { $ConfigPath = Join-Path $Root "configs\agent.yaml" }
$exe = Join-Path $Root "bin\agent.exe"

if (-not (Test-Path $exe)) { throw "No existe $exe. Copie la carpeta bin y compile/copie el agente." }
if (-not (Test-Path $ConfigPath)) { throw "No existe la configuración $ConfigPath" }

$binPath = '"{0}" -config "{1}"' -f $exe, (Resolve-Path $ConfigPath)

# El YAML contiene el token compartido: sólo SYSTEM y Admins.
Write-Host "Restringiendo permisos del archivo de configuración..." -ForegroundColor Cyan
icacls (Resolve-Path $ConfigPath) /inheritance:r /grant:r "*S-1-5-18:F" "*S-1-5-32-544:F" | Out-Null

sc.exe stop $ServiceName | Out-Null
Start-Sleep -Seconds 1
sc.exe delete $ServiceName | Out-Null
Start-Sleep -Seconds 1

Write-Host "Creando servicio $ServiceName ..." -ForegroundColor Cyan
sc.exe create $ServiceName binPath= $binPath start= auto DisplayName= "Spooler Fiscal (agente de estación)"
if ($LASTEXITCODE -ne 0) { throw "sc create falló con código $LASTEXITCODE" }

sc.exe description $ServiceName "Emula la impresora fiscal en un puerto COM local y reenvía los comandos al spooler central."
sc.exe failure $ServiceName reset= 86400 actions= restart/5000/restart/10000/restart/30000
sc.exe start $ServiceName

Write-Host ""
sc.exe query $ServiceName
Write-Host ""
Write-Host "Verifique el COM virtual con:  .\scripts\setup-com0com.ps1 -ProfitPort COM3 -AgentPort COM4" -ForegroundColor Yellow
