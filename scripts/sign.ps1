# Firma los binarios con un certificado de código (Authenticode).
#   .\scripts\sign.ps1 -Thumbprint 0123ABCD...
#   .\scripts\sign.ps1 -PfxPath .\certificado.pfx -PfxPassword (Read-Host -AsSecureString)
param(
    [string]$Root = (Split-Path -Parent $PSScriptRoot),
    [string]$Thumbprint,
    [string]$PfxPath,
    [securestring]$PfxPassword,
    [string]$TimestampUrl = "http://timestamp.digicert.com",
    [string[]]$Files = @()
)

$ErrorActionPreference = "Stop"
$bin = Join-Path $Root "bin"

if ($Files.Count -eq 0) {
    $Files = @(
        (Join-Path $bin "server.exe"),
        (Join-Path $bin "agent.exe")
    )
}

function Find-SignTool {
    $cmd = Get-Command signtool.exe -ErrorAction SilentlyContinue
    if ($cmd) { return $cmd.Source }

    $kits = "${env:ProgramFiles(x86)}\Windows Kits\10\bin"
    if (Test-Path $kits) {
        $found = Get-ChildItem $kits -Recurse -Filter signtool.exe -ErrorAction SilentlyContinue |
            Where-Object { $_.FullName -match '\\x64\\' } |
            Sort-Object FullName -Descending |
            Select-Object -First 1
        if ($found) { return $found.FullName }
    }
    return $null
}

function Get-SigningCertificate {
    if ($PfxPath) {
        if (-not (Test-Path $PfxPath)) { throw "No existe el PFX: $PfxPath" }
        if (-not $PfxPassword) { throw "Indique -PfxPassword (Read-Host -AsSecureString)." }
        $imported = Import-PfxCertificate -FilePath $PfxPath -CertStoreLocation Cert:\CurrentUser\My -Password $PfxPassword
        return Get-Item "Cert:\CurrentUser\My\$($imported.Thumbprint)"
    }
    if ($Thumbprint) {
        $path = "Cert:\CurrentUser\My\$Thumbprint"
        if (-not (Test-Path $path)) { throw "No hay certificado $Thumbprint en CurrentUser\My." }
        return Get-Item $path
    }
    $found = Get-ChildItem Cert:\CurrentUser\My -CodeSigningCert -ErrorAction SilentlyContinue |
        Where-Object { $_.HasPrivateKey } |
        Select-Object -First 1
    if (-not $found) {
        throw "No hay certificado de firma de código. Instale uno o use -Thumbprint / -PfxPath. Ver docs\OPERACION.md."
    }
    return $found
}

$cert = Get-SigningCertificate
if (-not $cert.HasPrivateKey) { throw "El certificado $($cert.Thumbprint) no tiene clave privada." }

$signtool = Find-SignTool
Write-Host "Certificado: $($cert.Subject)" -ForegroundColor Cyan
Write-Host "Herramienta: $(if ($signtool) { $signtool } else { 'Set-AuthenticodeSignature' })" -ForegroundColor Cyan

$failed = @()
foreach ($exe in $Files) {
    if (-not (Test-Path $exe)) {
        Write-Warning "No existe $exe"
        $failed += $exe
        continue
    }
    Write-Host "Firmando $(Split-Path -Leaf $exe)..." -ForegroundColor Cyan

    if ($signtool) {
        $toolArgs = @("sign", "/sha1", $cert.Thumbprint, "/fd", "sha256")
        if ($TimestampUrl) { $toolArgs += @("/tr", $TimestampUrl, "/td", "sha256") }
        $toolArgs += $exe

        $prev = $ErrorActionPreference
        $ErrorActionPreference = "Continue"
        & $signtool @toolArgs
        $code = $LASTEXITCODE
        $ErrorActionPreference = $prev

        if ($code -ne 0) {
            if ($TimestampUrl) {
                Write-Warning "Falló con sello de tiempo (¿sin Internet?). Firmando sin sello..."
                & $signtool sign /sha1 $cert.Thumbprint /fd sha256 $exe
                $code = $LASTEXITCODE
            }
            if ($code -ne 0) {
                $failed += $exe
                continue
            }
        }
    } else {
        try {
            $sig = Set-AuthenticodeSignature -FilePath $exe -Certificate $cert -HashAlgorithm SHA256 -TimestampServer $TimestampUrl
        } catch {
            Write-Warning "Falló con sello de tiempo: $($_.Exception.Message). Firmando sin sello..."
            $sig = Set-AuthenticodeSignature -FilePath $exe -Certificate $cert -HashAlgorithm SHA256
        }
        if ($sig.Status -notin @("Valid", "UnknownError")) {
            $failed += $exe
            continue
        }
    }

    $check = Get-AuthenticodeSignature $exe
    Write-Host ("  {0}: {1} - {2}" -f (Split-Path -Leaf $exe), $check.Status, $check.SignerCertificate.Subject)
}

if ($failed.Count -gt 0) { throw "No se pudieron firmar: $($failed -join ', ')" }
Write-Host "Firma completada." -ForegroundColor Green
