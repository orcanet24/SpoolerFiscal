# Compila los binarios Windows amd64 del spooler y del agente.
#   .\scripts\build.ps1
#   .\scripts\build.ps1 -NoStrip    # sin -s -w (si Defender marca el ejecutable)
param(
    [string]$Root = (Split-Path -Parent $PSScriptRoot),
    [switch]$NoStrip,
    [switch]$Sign,
    [string]$Thumbprint,
    [string]$PfxPath,
    [securestring]$PfxPassword,
    [string]$TimestampUrl = "http://timestamp.digicert.com"
)

$ErrorActionPreference = "Stop"
$bin = Join-Path $Root "bin"
New-Item -ItemType Directory -Force -Path $bin | Out-Null

function Build-Binary {
    param(
        [string]$Name,
        [string]$Package,
        [string[]]$Ldflags
    )

    $out = Join-Path $bin $Name
    Remove-Item $out -ErrorAction SilentlyContinue

    $arguments = @("build", "-trimpath")
    if ($Ldflags.Count -gt 0) {
        $arguments += @("-ldflags", ($Ldflags -join " "))
    }
    $arguments += @("-o", $out, $Package)

    # Un fallo del compilador no debe abortar el script: queremos reintentar.
    $prev = $ErrorActionPreference
    $ErrorActionPreference = "Continue"
    & go @arguments 2>&1 | ForEach-Object { Write-Host $_ }
    $code = $LASTEXITCODE
    $ErrorActionPreference = $prev

    if ($code -ne 0) { return $false }

    # Defender puede poner el archivo en cuarentena justo después del enlace.
    Start-Sleep -Milliseconds 300
    if (-not (Test-Path $out)) { return $false }
    if ((Get-Item $out).Length -eq 0) { return $false }
    return $true
}

Push-Location $Root
try {
    $env:GOOS = "windows"
    $env:GOARCH = "amd64"
    $env:CGO_ENABLED = "0"    # SQLite puro Go (modernc.org/sqlite)

    # El manual se embebe en el servidor; docs\ es la copia para abrir en el navegador.
    Write-Host "Sincronizando manual y OpenAPI..." -ForegroundColor Cyan
    Copy-Item (Join-Path $Root "internal\server\web\manual.html") (Join-Path $Root "docs\manual.html") -Force
    Copy-Item (Join-Path $Root "api\openapi.yaml") (Join-Path $Root "internal\server\web\openapi.yaml") -Force

    Write-Host "Generando diccionario del código..." -ForegroundColor Cyan
    go run ./cmd/diccionario
    if ($LASTEXITCODE -ne 0) { throw "Falló la generación del diccionario del código" }

    $strip = @()
    if (-not $NoStrip) { $strip = @("-s", "-w") }

    $targets = @(
        @{ Name = "server.exe";    Package = "./cmd/server";    Label = "servidor" },
        @{ Name = "agent.exe";     Package = "./cmd/agent";     Label = "agente" },
        @{ Name = "verificar.exe"; Package = "./cmd/verificar"; Label = "verificador" }
    )

    foreach ($target in $targets) {
        Write-Host "Compilando $($target.Label)..." -ForegroundColor Cyan
        $ok = Build-Binary -Name $target.Name -Package $target.Package -Ldflags $strip
        if (-not $ok -and -not $NoStrip) {
            Write-Warning "La compilación con -s -w falló o el ejecutable desapareció: probable cuarentena de Windows Defender."
            Write-Warning "Reintentando $($target.Label) sin strip (-s -w). El binario funciona igual y pesa más."
            $ok = Build-Binary -Name $target.Name -Package $target.Package -Ldflags @()
        }
        if (-not $ok) {
            throw "Falló la compilación del $($target.Label). Revise el historial de protección de Windows Defender y docs\OPERACION.md."
        }
    }

    if ($Sign) {
        Write-Host "Firmando binarios..." -ForegroundColor Cyan
        $signArgs = @{ Root = $Root }
        if ($Thumbprint) { $signArgs.Thumbprint = $Thumbprint }
        if ($PfxPath) {
            $signArgs.PfxPath = $PfxPath
            $signArgs.PfxPassword = $PfxPassword
        }
        $signArgs.TimestampUrl = $TimestampUrl
        & (Join-Path $PSScriptRoot "sign.ps1") @signArgs
        if ($LASTEXITCODE -ne 0) { throw "Falló la firma de los binarios" }
    }

    Get-ChildItem $bin -Filter *.exe | Select-Object Name, Length, LastWriteTime | Format-Table -AutoSize
    Write-Host "Listo. Si Defender vuelve a marcar un binario, agregue la carpeta del proyecto a las exclusiones (ver docs\OPERACION.md)." -ForegroundColor Yellow
} finally {
    Pop-Location
}
