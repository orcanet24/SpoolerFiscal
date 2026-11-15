# Crea el par de puertos COM virtuales con com0com.
# Profit Plus usará el primer puerto del par; el agente abre el segundo.
#
#   .\scripts\setup-com0com.ps1 -ProfitPort COM3 -AgentPort COM4
#
# Requiere: com0com instalado (driver firmado) y PowerShell como administrador.
param(
    [string]$ProfitPort = "COM3",
    [string]$AgentPort = "COM4",
    [string]$SetupC = "${env:ProgramFiles(x86)}\com0com\setupc.exe"
)

$ErrorActionPreference = "Stop"

$identity = [Security.Principal.WindowsIdentity]::GetCurrent()
$principal = New-Object Security.Principal.WindowsPrincipal($identity)
if (-not $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
    throw "Ejecute este script como administrador."
}
if (-not (Test-Path $SetupC)) {
    throw "No se encontró com0com en '$SetupC'. Instálelo primero (https://sourceforge.net/projects/com0com/)."
}

Write-Host "Pares existentes:" -ForegroundColor Cyan
& $SetupC list

Write-Host "Creando par $ProfitPort <-> $AgentPort ..." -ForegroundColor Cyan
& $SetupC install "PortName=$ProfitPort" "PortName=$AgentPort"
if ($LASTEXITCODE -ne 0) { throw "com0com devolvió el código $LASTEXITCODE" }

Write-Host "Verificando..." -ForegroundColor Cyan
& $SetupC list

Write-Host ""
Write-Host "Listo. Configure en Profit Plus el puerto $ProfitPort y en configs\agent.yaml virtual_com.name = $AgentPort." -ForegroundColor Green
