# Instala (o reinstala) el spooler fiscal como servicio de Windows.
#   .\scripts\install-server.ps1 -Root C:\SpoolerFiscal
param(
    [string]$Root = (Split-Path -Parent $PSScriptRoot),
    [string]$ServiceName = "SpoolerFiscalServer",
    [string]$ConfigPath = ""
)

$ErrorActionPreference = "Stop"

$identity = [Security.Principal.WindowsIdentity]::GetCurrent()
$principal = New-Object Security.Principal.WindowsPrincipal($identity)
if (-not $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
    throw "Ejecute este script como administrador."
}

if ([string]::IsNullOrWhiteSpace($ConfigPath)) { $ConfigPath = Join-Path $Root "configs\server.yaml" }
$exe = Join-Path $Root "bin\server.exe"

if (-not (Test-Path $exe)) { throw "No existe $exe. Compile con .\scripts\build.ps1" }
if (-not (Test-Path $ConfigPath)) { throw "No existe la configuración $ConfigPath" }

$binPath = '"{0}" -config "{1}"' -f $exe, (Resolve-Path $ConfigPath)

# El YAML contiene el token y data\ la auditoría fiscal: sólo SYSTEM y Admins.
Write-Host "Restringiendo permisos del token y de la auditoría..." -ForegroundColor Cyan
icacls (Resolve-Path $ConfigPath) /inheritance:r /grant:r "*S-1-5-18:F" "*S-1-5-32-544:F" | Out-Null
$dataDir = Join-Path $Root "data"
New-Item -ItemType Directory -Force -Path $dataDir | Out-Null
icacls $dataDir /inheritance:r /grant:r "*S-1-5-18:F" "*S-1-5-32-544:F" | Out-Null

Write-Host "Deteniendo instancias previas..." -ForegroundColor Cyan
sc.exe stop $ServiceName | Out-Null
Start-Sleep -Seconds 1
sc.exe delete $ServiceName | Out-Null
Start-Sleep -Seconds 1

Write-Host "Creando servicio $ServiceName ..." -ForegroundColor Cyan
sc.exe create $ServiceName binPath= $binPath start= auto DisplayName= "Spooler Fiscal Central"
if ($LASTEXITCODE -ne 0) { throw "sc create falló con código $LASTEXITCODE" }

sc.exe description $ServiceName "Serializa la emisión fiscal de todas las estaciones sobre una única impresora fiscal y mantiene el correlativo centralizado."
# Reinicio automático ante caídas: 5s, 10s, 30s; contador diario.
sc.exe failure $ServiceName reset= 86400 actions= restart/5000/restart/10000/restart/30000
# Arrancar más rápido tras reiniciar el equipo no es necesario, pero el
# servicio depende de que el puerto COM exista; sc lo inicia después del boot.
sc.exe start $ServiceName

Write-Host ""
sc.exe query $ServiceName
Write-Host ""
Write-Host "Revise los logs en $(Join-Path $Root 'logs\server.log')" -ForegroundColor Green
