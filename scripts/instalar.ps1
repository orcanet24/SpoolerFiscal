# Instalador del Spooler Fiscal.
#
#   .\scripts\instalar.ps1                     (menú interactivo)
#   .\scripts\instalar.ps1 -Role server        (servidor, con preguntas)
#   .\scripts\instalar.ps1 -Role agent         (estación, con preguntas)
#   .\scripts\instalar.ps1 -Role server -Silent -Token XXX -PrinterPort COM1
#   .\scripts\instalar.ps1 -Role uninstall
#
# Deja el binario como servicio de Windows con inicio automático y reinicio
# ante fallos. Los datos y los logs quedan bajo la carpeta de instalación.
[CmdletBinding()]
param(
    [string]$Role = "",
    [string]$TargetDir = "C:\SpoolerFiscal",
    [string]$ServiceName = "",
    [string]$SourceRoot = "",
    [string]$ConfigPath = "",
    [switch]$Silent,
    [switch]$DryRun,
    [switch]$Force,

    # Servidor
    [string]$Driver = "tfhka",
    [string]$Transport = "serial",
    [string]$PrinterPort = "COM1",
    [int]$BaudRate = 9600,
    [string]$TcpAddress = "127.0.0.1:2011",
    [int]$HttpPort = 8080,
    [string]$Token = "",
    [switch]$NoFirewall,

    # Estación
    [string]$ServerAddr = "",
    [string]$StationId = "",
    [string]$AgentPort = "COM4",
    [string]$ProfitPort = "COM3",
    [switch]$SkipCom0com,

    # Desinstalar
    [switch]$RemoveData
)

$ErrorActionPreference = "Stop"

# ---------------------------------------------------------------- estado
$script:DryRun = [bool]$DryRun
$script:Silent = [bool]$Silent
$script:Force = [bool]$Force
$script:TargetDir = [IO.Path]::GetFullPath($TargetDir)
$script:RepoRoot = if ($SourceRoot) { [IO.Path]::GetFullPath($SourceRoot) } else { Split-Path -Parent $PSScriptRoot }
$script:Token = $Token

# ---------------------------------------------------------------- salida
function Write-Step([string]$Message) {
    Write-Host ""
    Write-Host "== $Message" -ForegroundColor Cyan
}
function Write-Ok([string]$Message) {
    if ($script:DryRun) {
        Write-Host "   [dry-run] $Message" -ForegroundColor DarkGray
    } else {
        Write-Host "   [OK] $Message" -ForegroundColor Green
    }
}
function Write-Warn([string]$Message) {
    Write-Host "   [!]  $Message" -ForegroundColor Yellow
}
function Write-Dry([string]$Message) {
    Write-Host "   [dry-run] $Message" -ForegroundColor DarkGray
}

# ---------------------------------------------------------------- ayudas
function Assert-Admin {
    if ($script:DryRun) { return }
    $id = [Security.Principal.WindowsIdentity]::GetCurrent()
    $pr = New-Object Security.Principal.WindowsPrincipal($id)
    if (-not $pr.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
        throw "Ejecute este instalador como administrador (clic derecho > Ejecutar como administrador)."
    }
}

function Read-Text([string]$Label, [string]$Default = "") {
    if ($script:Silent) { return $Default }
    $suffix = if ([string]::IsNullOrWhiteSpace($Default)) { "" } else { " [$Default]" }
    $v = Read-Host "$Label$suffix"
    if ([string]::IsNullOrWhiteSpace($v)) { return $Default }
    return $v.Trim()
}

function Read-Required([string]$Label, [string]$Default = "") {
    while ($true) {
        $v = Read-Text $Label $Default
        if (-not [string]::IsNullOrWhiteSpace($v)) { return $v }
        if ($script:Silent) { throw "$Label es obligatorio en modo -Silent." }
        Write-Warn "Valor obligatorio."
    }
}

function Read-YesNo([string]$Label, [bool]$Default = $true) {
    if ($script:Silent) { return $Default }
    $d = if ($Default) { "S" } else { "N" }
    $v = Read-Host "$Label [$d]"
    if ([string]::IsNullOrWhiteSpace($v)) { return $Default }
    return ($v.Trim().ToLower() -in @("s", "si", "sí", "y", "yes"))
}

function New-Token {
    return [guid]::NewGuid().ToString("N")
}

function Get-DefaultStationId {
    $n = $env:COMPUTERNAME
    if ([string]::IsNullOrWhiteSpace($n)) { return "CAJA-01" }
    $n = $n.ToUpper() -replace "[^A-Z0-9\-_.:]", "-"
    if ($n.Length -gt 64) { $n = $n.Substring(0, 64) }
    return $n
}

function Get-ComPorts {
    try { return @([System.IO.Ports.SerialPort]::GetPortNames() | Sort-Object) } catch { return @() }
}

function Find-Binary([string[]]$Names) {
    $roots = @(
        (Join-Path $script:RepoRoot "bin"),
        (Join-Path $script:RepoRoot "bin\server"),
        (Join-Path $script:RepoRoot "bin\cliente"),
        (Join-Path $script:RepoRoot "bin\agent"),
        (Join-Path $script:RepoRoot "bin\verificar"),
        $script:RepoRoot,
        $PSScriptRoot
    )
    foreach ($root in $roots) {
        foreach ($name in $Names) {
            $candidate = Join-Path $root $name
            if (Test-Path -LiteralPath $candidate -PathType Leaf) {
                return (Resolve-Path -LiteralPath $candidate).Path
            }
        }
    }
    return $null
}

function Find-Com0com {
    $candidates = @()
    if (${env:ProgramFiles(x86)}) { $candidates += (Join-Path ${env:ProgramFiles(x86)} "com0com\setupc.exe") }
    if ($env:ProgramFiles) { $candidates += (Join-Path $env:ProgramFiles "com0com\setupc.exe") }
    foreach ($c in $candidates) {
        if (Test-Path -LiteralPath $c) { return $c }
    }
    return $null
}

function Write-TextNoBom([string]$Path, [string]$Text) {
    $dir = Split-Path -Parent $Path
    if ($dir) { New-Item -ItemType Directory -Force -Path $dir | Out-Null }
    if ($script:DryRun) {
        Write-Host ""
        Write-Host "--- contenido de $Path ---" -ForegroundColor DarkGray
        Write-Host $Text -ForegroundColor DarkGray
        Write-Host "--- fin ---" -ForegroundColor DarkGray
        return
    }
    [IO.File]::WriteAllText($Path, $Text, (New-Object System.Text.UTF8Encoding($false)))
}

function Backup-File([string]$Path) {
    if (-not (Test-Path -LiteralPath $Path)) { return $null }
    $bak = "$Path.bak-$(Get-Date -Format 'yyyyMMdd-HHmmss')"
    if ($script:DryRun) { Write-Dry "respaldar $Path -> $bak"; return $bak }
    Copy-Item -LiteralPath $Path -Destination $bak -Force
    return $bak
}

function Copy-Binary([string]$Source, [string]$Destination) {
    if (-not $Source) { return }
    if ($script:DryRun) { Write-Dry "copiar $Source -> $Destination"; return }
    $same = $false
    if (Test-Path -LiteralPath $Destination) {
        $same = ((Get-Item -LiteralPath $Source).FullName -eq (Get-Item -LiteralPath $Destination).FullName)
    }
    if (-not $same) { Copy-Item -LiteralPath $Source -Destination $Destination -Force }
}

function Protect-Path([string]$Path, [switch]$Directory) {
    if ($Directory -and -not $script:DryRun) {
        New-Item -ItemType Directory -Force -Path $Path | Out-Null
    }
    if ($script:DryRun) {
        Write-Dry "icacls `"$Path`" /inheritance:r /grant:r *S-1-5-18:F *S-1-5-32-544:F"
        return
    }
    & icacls $Path /inheritance:r /grant:r "*S-1-5-18:F" "*S-1-5-32-544:F" | Out-Null
}

# ---------------------------------------------------------------- servicios
function Get-SpoolerService([string]$Name) {
    return Get-Service -Name $Name -ErrorAction SilentlyContinue
}

function Wait-ServiceState([string]$Name, [string]$State, [int]$TimeoutSec) {
    $deadline = (Get-Date).AddSeconds($TimeoutSec)
    while ((Get-Date) -lt $deadline) {
        $svc = Get-SpoolerService $Name
        if (-not $svc) { return ($State -eq "Absent") }
        if ($svc.Status.ToString() -eq $State) { return $true }
        Start-Sleep -Milliseconds 300
    }
    return $false
}

function Remove-ServiceIfExists([string]$Name) {
    $svc = Get-SpoolerService $Name
    if (-not $svc) { return }
    if ($script:DryRun) {
        Write-Dry "sc.exe stop $Name"
        Write-Dry "sc.exe delete $Name"
        return
    }
    Write-Host "   Deteniendo el servicio $Name ..."
    & sc.exe stop $Name | Out-Null
    [void](Wait-ServiceState $Name "Stopped" 20)
    & sc.exe delete $Name | Out-Null
    [void](Wait-ServiceState $Name "Absent" 15)
}

function Install-SpoolerService([string]$Name, [string]$Display, [string]$Description,
                                [string]$Exe, [string]$Config, [switch]$Delayed) {
    $binPath = '"{0}" -config "{1}"' -f $Exe, $Config
    if ($script:DryRun) {
        Write-Dry "sc.exe create $Name binPath= $binPath start= auto DisplayName= `"$Display`""
        Write-Dry "sc.exe description $Name `"$Description`""
        Write-Dry "sc.exe failure $Name reset= 86400 actions= restart/5000/restart/10000/restart/30000"
        if ($Delayed) { Write-Dry "sc.exe config $Name start= delayed-auto" }
        Write-Dry "sc.exe start $Name"
        return $true
    }
    Remove-ServiceIfExists $Name

    $created = $false
    for ($i = 0; $i -lt 3 -and -not $created; $i++) {
        & sc.exe create $Name binPath= $binPath start= auto DisplayName= $Display | Out-Null
        if ($LASTEXITCODE -eq 0) { $created = $true } else { Start-Sleep -Seconds 1 }
    }
    if (-not $created) { throw "No se pudo crear el servicio $Name (código $LASTEXITCODE)." }

    & sc.exe description $Name $Description | Out-Null
    & sc.exe failure $Name reset= 86400 actions= restart/5000/restart/10000/restart/30000 | Out-Null
    if ($Delayed) { & sc.exe config $Name start= delayed-auto | Out-Null }
    & sc.exe start $Name | Out-Null
    return (Wait-ServiceState $Name "Running" 30)
}

function Test-ServerHealth([int]$Port, [int]$TimeoutSec) {
    if ($script:DryRun) { Write-Dry "GET http://127.0.0.1:$Port/v1/health"; return $true }
    $url = "http://127.0.0.1:$Port/v1/health"
    $deadline = (Get-Date).AddSeconds($TimeoutSec)
    while ((Get-Date) -lt $deadline) {
        try {
            $r = Invoke-WebRequest -Uri $url -UseBasicParsing -TimeoutSec 2
            if ($r.StatusCode -eq 200) { return $true }
        } catch { }
        Start-Sleep -Milliseconds 400
    }
    return $false
}

function Test-AgentBridge([string]$LogPath, [int]$TimeoutSec) {
    if ($script:DryRun) { Write-Dry "revisar $LogPath en busca de 'COM virtual abierto'"; return $true }
    $deadline = (Get-Date).AddSeconds($TimeoutSec)
    while ((Get-Date) -lt $deadline) {
        if (Test-Path -LiteralPath $LogPath) {
            $tail = Get-Content -LiteralPath $LogPath -Encoding UTF8 -Tail 100 -ErrorAction SilentlyContinue
            if (($tail -join "`n") -match "COM virtual abierto") { return $true }
        }
        Start-Sleep -Milliseconds 500
    }
    return $false
}

function Add-FirewallRule([int]$Port) {
    $name = "Spooler Fiscal HTTP ($Port)"
    if ($script:DryRun) {
        Write-Dry "New-NetFirewallRule -DisplayName `"$name`" -Direction Inbound -Protocol TCP -LocalPort $Port -Action Allow"
        return
    }
    Get-NetFirewallRule -DisplayName $name -ErrorAction SilentlyContinue | Remove-NetFirewallRule -ErrorAction SilentlyContinue
    New-NetFirewallRule -DisplayName $name -Direction Inbound -Protocol TCP -LocalPort $Port `
        -Action Allow -Profile Domain,Private | Out-Null
    Write-Ok "Regla de firewall creada para TCP $Port (perfiles Dominio y Privado)."
}

function Remove-FirewallRules {
    if ($script:DryRun) { Write-Dry "quitar reglas de firewall 'Spooler Fiscal HTTP (*)'"; return }
    Get-NetFirewallRule -DisplayName "Spooler Fiscal HTTP (*)" -ErrorAction SilentlyContinue |
        Remove-NetFirewallRule -ErrorAction SilentlyContinue
}

# ---------------------------------------------------------------- configuración
function New-ServerYaml {
    param(
        [string]$Driver, [string]$Transport, [string]$TcpAddress, [string]$PrinterPort,
        [int]$BaudRate, [int]$HttpPort, [string]$Token, [string]$TargetDir
    )
    $databasePath = Join-Path $TargetDir "data\spooler.db"
    $logPath = Join-Path $TargetDir "logs\server.log"
    $lines = @(
        "# Generado por instalar.ps1 el $(Get-Date -Format 'yyyy-MM-dd HH:mm').",
        "# Reconfigurar: .\scripts\instalar.ps1 -Role server -Force",
        "token: $Token",
        "",
        "http:",
        "  listen: 0.0.0.0:$HttpPort",
        "",
        "printer:",
        "  driver: $Driver",
        "  transport: $Transport"
    )
    if ($Transport -eq "tcp") {
        $lines += "  tcp_address: $TcpAddress"
    } else {
        $lines += @(
            "  port:",
            "    name: $PrinterPort",
            "    baud_rate: $BaudRate",
            "    data_bits: 8",
            "    parity: none",
            '    stop_bits: "1"',
            "    flow_control: none"
        )
    }
    $lines += @(
        "  read_gap_ms: 30",
        "  response_timeout_ms: 20000",
        "  status_probe: S2",
        '  command_terminator: "\r\n"',
        "",
        "queue:",
        "  status_probe_interval_seconds: 15",
        "",
        "database:",
        "  path: '$databasePath'",
        "",
        "logging:",
        "  level: info",
        "  file: '$logPath'",
        "  console: false",
        ""
    )
    return ($lines -join "`r`n")
}

function New-AgentYaml {
    param(
        [string]$StationId, [string]$ServerAddr, [string]$Token,
        [string]$AgentPort, [int]$BaudRate, [string]$TargetDir
    )
    $logPath = Join-Path $TargetDir "logs\agent.log"
    $lines = @(
        "# Generado por instalar.ps1 el $(Get-Date -Format 'yyyy-MM-dd HH:mm').",
        "# Reconfigurar: .\scripts\instalar.ps1 -Role agent -Force",
        "station_id: $StationId",
        "",
        "server:",
        "  address: $ServerAddr",
        "  token: $Token",
        "  request_timeout_ms: 30000",
        "",
        "virtual_com:",
        "  name: $AgentPort",
        "  baud_rate: $BaudRate",
        "  data_bits: 8",
        "  parity: none",
        '  stop_bits: "1"',
        "  flow_control: none",
        "",
        "bridge:",
        "  read_gap_ms: 30",
        "  response_timeout_ms: 15000",
        "  max_frame_bytes: 16384",
        "  write_settle_ms: 0",
        "  reopen_delay_ms: 1000",
        "  reopen_max_delay_ms: 30000",
        "  local_queue_size: 16",
        "  max_send_backoff_ms: 2000",
        "",
        "logging:",
        "  level: info",
        "  file: '$logPath'",
        "  console: false",
        ""
    )
    return ($lines -join "`r`n")
}

function Confirm-Reconfigure([string]$ConfigPath, [string]$Label) {
    if ($script:Force -or -not (Test-Path -LiteralPath $ConfigPath)) { return $true }
    if ($script:Silent) { return $false }
    return (Read-YesNo "Ya existe $ConfigPath. ¿Reconfigurar $Label? (se respalda el anterior)" $false)
}

# ---------------------------------------------------------------- servidor
function Install-ServerRole {
    Write-Step "Instalación del SERVIDOR (PC con la impresora)"
    Assert-Admin

    $source = Find-Binary @("server.exe")
    if (-not $source -and -not $script:DryRun) {
        throw "No se encontró server.exe. Compile primero con build.bat o .\scripts\build.ps1."
    }
    if ($source) { Write-Host "   Origen: $source" }

    $exe = Join-Path $script:TargetDir "server.exe"
    $configDir = Join-Path $script:TargetDir "configs"
    $cfgPath = if ($ConfigPath) { [IO.Path]::GetFullPath($ConfigPath) } else { Join-Path $configDir "server.yaml" }
    $service = if ($ServiceName) { $ServiceName } else { "SpoolerFiscalServer" }

    if (-not $script:DryRun) {
        New-Item -ItemType Directory -Force -Path $script:TargetDir, $configDir,
            (Join-Path $script:TargetDir "data"), (Join-Path $script:TargetDir "logs") | Out-Null
    }

    $reconfigure = if ($ConfigPath) {
        if (-not (Test-Path -LiteralPath $cfgPath) -and -not $script:DryRun) {
            throw "No existe la configuración indicada: $cfgPath"
        }
        $false
    } else {
        Confirm-Reconfigure $cfgPath "el servidor"
    }
    $tokenKnown = $false
    if ($reconfigure) {
        $Driver = (Read-Text "Driver de impresora (tfhka|hasar|bixolon|epson)" $Driver).ToLower()
        if ($Driver -notin @("tfhka", "hasar", "bixolon", "epson")) { throw "Driver inválido: $Driver" }
        $Transport = (Read-Text "Transporte (serial|tcp)" $Transport).ToLower()
        if ($Transport -notin @("serial", "tcp")) { throw "Transporte inválido: $Transport" }
        if ($Transport -eq "serial") {
            $ports = Get-ComPorts
            if ($ports.Count -gt 0) { Write-Host "   Puertos detectados: $($ports -join ', ')" }
            $PrinterPort = Read-Text "Puerto de la impresora (ej. COM1)" $PrinterPort
            $BaudRate = [int](Read-Text "Baudios" "$BaudRate")
        } else {
            $TcpAddress = Read-Required "Dirección TCP de la impresora (ip:puerto)" $TcpAddress
        }
        $HttpPort = [int](Read-Text "Puerto HTTP de la API" "$HttpPort")
        if ([string]::IsNullOrWhiteSpace($script:Token)) {
            $script:Token = New-Token
            $tokenKnown = $true
        } else {
            $script:Token = Read-Text "Token compartido (vacío = generar uno nuevo)" $script:Token
            if ([string]::IsNullOrWhiteSpace($script:Token)) {
                $script:Token = New-Token
                $tokenKnown = $true
            }
        }

        $yaml = New-ServerYaml -Driver $Driver -Transport $Transport -TcpAddress $TcpAddress `
            -PrinterPort $PrinterPort -BaudRate $BaudRate -HttpPort $HttpPort `
            -Token $script:Token -TargetDir $script:TargetDir
        $bak = Backup-File $cfgPath
        if ($bak) { Write-Ok "Configuración anterior respaldada en $bak" }
        Write-TextNoBom $cfgPath $yaml
        Write-Ok "Configuración escrita en $cfgPath"
        $tokenKnown = $true
    } else {
        Write-Ok "Se conserva la configuración existente: $cfgPath"
        $envFile = Join-Path $script:TargetDir ".env"
        if (Test-Path -LiteralPath $envFile) {
            Write-Warn "Existe ${envFile}: sus variables (p. ej. SPOOLER_TOKEN) tienen prioridad sobre el YAML."
        }
    }

    Copy-Binary $source $exe
    $verifier = Find-Binary @("verificar.exe")
    if ($verifier) { Copy-Binary $verifier (Join-Path $script:TargetDir "verificar.exe") }

    Protect-Path $cfgPath
    Protect-Path (Join-Path $script:TargetDir "data") -Directory
    Write-Ok "Permisos restringidos (SYSTEM y Administradores) sobre config y data."

    $running = Install-SpoolerService -Name $service -Display "Spooler Fiscal Central" `
        -Description "Serializa la emisión fiscal de todas las estaciones sobre una única impresora fiscal y mantiene el correlativo centralizado." `
        -Exe $exe -Config $cfgPath
    if (-not $running) { throw "El servicio $service no llegó a RUNNING. Revise el Visor de eventos y $(Join-Path $script:TargetDir 'logs\server.log')." }
    Write-Ok "Servicio $service instalado (inicio automático + reinicio ante fallos) y en ejecución."

    if (-not $NoFirewall) {
        $addRule = if ($script:Silent) { $true } else { Read-YesNo "¿Abrir el puerto TCP $HttpPort en el firewall (perfiles Dominio/Privado)?" $true }
        if ($addRule) { Add-FirewallRule $HttpPort }
    }

    $healthy = Test-ServerHealth $HttpPort 20
    if ($healthy) { Write-Ok "API respondiendo en http://127.0.0.1:$HttpPort/v1/health" }
    else { Write-Warn "La API no respondió aún. Revise $script:TargetDir\logs\server.log" }

    Write-Step "Servidor instalado"
    Write-Host "   Servicio: $service (automático)"
    Write-Host "   Binario:  $exe"
    Write-Host "   Config:   $cfgPath"
    Write-Host "   Datos:    $(Join-Path $script:TargetDir 'data')"
    Write-Host "   Panel:    http://127.0.0.1:$HttpPort/admin"
    if ($tokenKnown) {
        Write-Host ""
        Write-Host "   TOKEN COMPARTIDO: $($script:Token)" -ForegroundColor Yellow
        Write-Host "   Úselo en cada estación (instalador del agente)." -ForegroundColor Yellow
    }
    Write-Host ""
    Write-Host "   Puesta en marcha con la impresora real:" -ForegroundColor Cyan
    Write-Host "     sc stop $service"
    Write-Host "     & `"$(Join-Path $script:TargetDir 'verificar.exe')`" -config `"$cfgPath`" -out `"$(Join-Path $script:TargetDir 'data\verificacion-inicial.json')`""
    Write-Host "     sc start $service"
}

# ---------------------------------------------------------------- agente
function Install-AgentRole {
    Write-Step "Instalación de la ESTACIÓN (PC con Profit Plus)"
    Assert-Admin

    $source = Find-Binary @("agent.exe", "cliente.exe")
    if (-not $source -and -not $script:DryRun) {
        throw "No se encontró agent.exe ni cliente.exe. Compile primero con build.bat o .\scripts\build.ps1."
    }
    if ($source) { Write-Host "   Origen: $source" }

    $exe = Join-Path $script:TargetDir "agent.exe"
    $configDir = Join-Path $script:TargetDir "configs"
    $cfgPath = if ($ConfigPath) { [IO.Path]::GetFullPath($ConfigPath) } else { Join-Path $configDir "agent.yaml" }
    $service = if ($ServiceName) { $ServiceName } else { "SpoolerFiscalAgent" }

    if (-not $script:DryRun) {
        New-Item -ItemType Directory -Force -Path $script:TargetDir, $configDir,
            (Join-Path $script:TargetDir "logs") | Out-Null
    }

    $reconfigure = if ($ConfigPath) {
        if (-not (Test-Path -LiteralPath $cfgPath) -and -not $script:DryRun) {
            throw "No existe la configuración indicada: $cfgPath"
        }
        $false
    } else {
        Confirm-Reconfigure $cfgPath "la estación"
    }
    if ($reconfigure) {
        if ([string]::IsNullOrWhiteSpace($StationId)) { $StationId = Get-DefaultStationId }
        $StationId = Read-Required "Identificador de la estación" $StationId
        if ($StationId -notmatch "^[A-Za-z0-9._:\-]{1,64}$") {
            throw "station_id inválido: use letras, números y - _ . : (máx. 64)."
        }
        if ([string]::IsNullOrWhiteSpace($ServerAddr) -and -not $script:Silent) {
            $ServerAddr = "http://127.0.0.1:8080"
        }
        $ServerAddr = Read-Required "Dirección del servidor (http://ip:8080)" $ServerAddr
        if ($ServerAddr -notmatch "^https?://") { $ServerAddr = "http://$ServerAddr" }
        if ([string]::IsNullOrWhiteSpace($script:Token)) {
            throw "Falta el token del servidor. Use -Token o péguelo cuando se lo pida."
        }
        $script:Token = Read-Required "Token de la estación (el mismo del servidor)" $script:Token

        $ports = Get-ComPorts
        if ($ports.Count -gt 0) { Write-Host "   Puertos detectados: $($ports -join ', ')" }
        $AgentPort = Read-Required "Puerto COM del agente (par com0com)" $AgentPort
        $ProfitPort = Read-Required "Puerto COM para Profit Plus (el otro del par)" $ProfitPort

        $setupc = Find-Com0com
        if (-not $SkipCom0com) {
            if ($setupc) {
                $create = if ($script:Silent) { $false } else {
                    Read-YesNo "¿Crear el par com0com $ProfitPort <-> $AgentPort ahora?" $true
                }
                if ($create) {
                    Write-Host "   Creando par com0com $ProfitPort <-> $AgentPort ..."
                    & $setupc install "PortName=$ProfitPort" "PortName=$AgentPort" | Out-Null
                    if ($LASTEXITCODE -ne 0) {
                        Write-Warn "com0com devolvió $LASTEXITCODE (puede que el par ya exista). Verifique con: `"$setupc`" list"
                    } else {
                        Write-Ok "Par com0com creado: Profit Plus en $ProfitPort, agente en $AgentPort."
                    }
                }
            } else {
                Write-Warn "com0com no está instalado. El agente esperará el puerto $AgentPort."
                Write-Warn "Instálelo desde https://sourceforge.net/projects/com0com/ y cree el par $ProfitPort <-> $AgentPort."
            }
        }

        $yaml = New-AgentYaml -StationId $StationId -ServerAddr $ServerAddr -Token $script:Token `
            -AgentPort $AgentPort -BaudRate $BaudRate -TargetDir $script:TargetDir
        $bak = Backup-File $cfgPath
        if ($bak) { Write-Ok "Configuración anterior respaldada en $bak" }
        Write-TextNoBom $cfgPath $yaml
        Write-Ok "Configuración escrita en $cfgPath"
    } else {
        Write-Ok "Se conserva la configuración existente: $cfgPath"
    }

    Copy-Binary $source $exe
    Protect-Path $cfgPath
    Write-Ok "Permisos restringidos (SYSTEM y Administradores) sobre la configuración."

    # delayed-auto: deja que com0com cargue antes de abrir el puerto.
    $running = Install-SpoolerService -Name $service -Display "Spooler Fiscal (agente de estación)" `
        -Description "Emula la impresora fiscal en un puerto COM local y reenvía los comandos al spooler central." `
        -Exe $exe -Config $cfgPath -Delayed
    if (-not $running) { throw "El servicio $service no llegó a RUNNING. Revise $(Join-Path $script:TargetDir 'logs\agent.log')." }
    Write-Ok "Servicio $service instalado (inicio automático diferido + reinicio ante fallos) y en ejecución."

    $logPath = Join-Path $script:TargetDir "logs\agent.log"
    if (Test-AgentBridge $logPath 20) {
        Write-Ok "COM virtual abierto; el agente está listo."
    } else {
        Write-Warn "El agente no ha abierto el COM todavía. Revise $logPath"
        if (Test-Path -LiteralPath $logPath) {
            Write-Host "   Últimas líneas del log:" -ForegroundColor Yellow
            Get-Content -LiteralPath $logPath -Encoding UTF8 -Tail 5 | ForEach-Object { Write-Host "     $_" }
        }
    }

    Write-Step "Estación instalada"
    Write-Host "   Servicio: $service (automático diferido)"
    Write-Host "   Binario:  $exe"
    Write-Host "   Config:   $cfgPath"
    Write-Host "   Profit Plus debe usar el puerto $ProfitPort (el agente abre $AgentPort)."
}

# ---------------------------------------------------------------- verificar / desinstalar
function Verify-Role {
    $verifier = Join-Path $script:TargetDir "verificar.exe"
    if (-not (Test-Path -LiteralPath $verifier)) {
        $found = Find-Binary @("verificar.exe")
        if (-not $found) { throw "No se encontró verificar.exe. Instale el servidor o compile el proyecto." }
        $verifier = $found
    }
    $cfgPath = if ($ConfigPath) { [IO.Path]::GetFullPath($ConfigPath) } else { Join-Path $script:TargetDir "configs\server.yaml" }
    if (-not (Test-Path -LiteralPath $cfgPath)) { throw "No existe $cfgPath." }

    $service = if ($ServiceName) { $ServiceName } else { "SpoolerFiscalServer" }
    $svc = Get-SpoolerService $service
    $wasRunning = $svc -and $svc.Status.ToString() -eq "Running"
    if ($wasRunning) {
        Write-Host "   Deteniendo $service para liberar el puerto de la impresora ..."
        if (-not $script:DryRun) {
            & sc.exe stop $service | Out-Null
            [void](Wait-ServiceState $service "Stopped" 20)
        }
    }
    try {
        if ($script:DryRun) { Write-Dry "& `"$verifier`" -config `"$cfgPath`""; return 0 }
        & $verifier -config $cfgPath | Out-Host
        $code = $LASTEXITCODE
    } finally {
        if ($wasRunning) {
            Write-Host "   Reiniciando $service ..."
            if (-not $script:DryRun) {
                & sc.exe start $service | Out-Null
                [void](Wait-ServiceState $service "Running" 30)
            }
        }
    }
    return $code
}

function Uninstall-Role {
    Write-Step "Desinstalación"
    Assert-Admin
    $names = if ($ServiceName) { @($ServiceName) } else { @("SpoolerFiscalServer", "SpoolerFiscalAgent") }
    foreach ($n in $names) {
        Remove-ServiceIfExists $n
        if (Get-SpoolerService $n) { Write-Warn "El servicio $n sigue existiendo; reinicie el equipo y repita." }
        else { Write-Ok "Servicio $n eliminado." }
    }
    Remove-FirewallRules
    $dataDir = Join-Path $script:TargetDir "data"
    $removeData = $RemoveData
    if (-not $removeData -and -not $script:Silent) {
        $removeData = Read-YesNo "¿Borrar la auditoría fiscal en $dataDir? NO se puede deshacer" $false
    }
    if ($removeData) {
        if ($script:DryRun) { Write-Dry "Remove-Item -Recurse -Force $dataDir" }
        elseif (Test-Path -LiteralPath $dataDir) {
            Remove-Item -LiteralPath $dataDir -Recurse -Force
            Write-Ok "Datos eliminados: $dataDir"
        }
    } else {
        Write-Host "   Los datos se conservan en $dataDir"
    }
    Write-Host "   Binarios y configuración se conservan en $script:TargetDir"
}

# ---------------------------------------------------------------- menú
function Show-Menu {
    Write-Host ""
    Write-Host "=============================================" -ForegroundColor Cyan
    Write-Host "  Spooler Fiscal - Instalador" -ForegroundColor Cyan
    Write-Host "=============================================" -ForegroundColor Cyan
    Write-Host "  1) Instalar servidor (PC con la impresora)"
    Write-Host "  2) Instalar estación (PC con Profit Plus)"
    Write-Host "  3) Verificar impresora (detiene el servicio)"
    Write-Host "  4) Desinstalar servicios"
    Write-Host "  5) Salir"
    $op = Read-Host "Opción"
    switch ($op) {
        "1" { return "server" }
        "2" { return "agent" }
        "3" { return "verify" }
        "4" { return "uninstall" }
        "5" { return "" }
        default { return "invalid" }
    }
}

# ---------------------------------------------------------------- principal
if (-not $Role) {
    if ($script:Silent) { throw "En modo -Silent debe indicar -Role server|agent|verify|uninstall." }
    do {
        $Role = Show-Menu
        if ($Role -eq "invalid") { Write-Warn "Opción inválida." }
    } while ($Role -eq "invalid")
    if (-not $Role) { return }
}

$Role = $Role.ToLower()
if ($Role -notin @("server", "agent", "verify", "uninstall")) {
    throw "Rol inválido: $Role (server|agent|verify|uninstall)."
}

if (-not $script:DryRun) {
    Write-Host "   Destino: $script:TargetDir"
}

switch ($Role) {
    "server"    { Install-ServerRole }
    "agent"     { Install-AgentRole }
    "verify"    { exit (Verify-Role) }
    "uninstall" { Uninstall-Role }
}
