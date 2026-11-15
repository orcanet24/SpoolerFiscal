# Detiene y elimina los servicios del spooler fiscal.
#   .\scripts\uninstall.ps1                 (ambos servicios)
#   .\scripts\uninstall.ps1 -Only server
param(
    [ValidateSet("both", "server", "agent")]
    [string]$Only = "both"
)

$ErrorActionPreference = "Stop"

$identity = [Security.Principal.WindowsIdentity]::GetCurrent()
$principal = New-Object Security.Principal.WindowsPrincipal($identity)
if (-not $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
    throw "Ejecute este script como administrador."
}

$services = @()
if ($Only -eq "both" -or $Only -eq "server") { $services += "SpoolerFiscalServer" }
if ($Only -eq "both" -or $Only -eq "agent")  { $services += "SpoolerFiscalAgent" }

foreach ($svc in $services) {
    Write-Host "Deteniendo $svc ..." -ForegroundColor Cyan
    sc.exe stop $svc | Out-Null
    Start-Sleep -Seconds 2
    sc.exe delete $svc | Out-Null
    if ($LASTEXITCODE -eq 0) {
        Write-Host "Servicio $svc eliminado." -ForegroundColor Green
    } else {
        Write-Host "El servicio $svc no estaba instalado." -ForegroundColor Yellow
    }
}

Write-Host ""
Write-Host "Los datos (data\spooler.db) y los logs NO se borran. Elimínelos manualmente si corresponde." -ForegroundColor Yellow
