@echo off
setlocal

set "ROOT=%~dp0"
set "BIN=%ROOT%bin"
set "CLIENTE=%BIN%\cliente"
set "SERVER=%BIN%\server"
set "SIMPRINT=%BIN%\simprint"
set "VERIFICAR=%BIN%\verificar"

where go >nul 2>nul
if errorlevel 1 (
    echo ERROR: Go no esta instalado o no esta disponible en PATH.
    exit /b 1
)

if not exist "%CLIENTE%\configs" mkdir "%CLIENTE%\configs"
if errorlevel 1 goto :error
if not exist "%SERVER%\configs" mkdir "%SERVER%\configs"
if errorlevel 1 goto :error
if not exist "%SIMPRINT%" mkdir "%SIMPRINT%"
if errorlevel 1 goto :error
if not exist "%VERIFICAR%" mkdir "%VERIFICAR%"
if errorlevel 1 goto :error

if not exist "%CLIENTE%\configs\agent.yaml" copy "%ROOT%configs\agent.yaml" "%CLIENTE%\configs\agent.yaml" >nul
if errorlevel 1 goto :error
if not exist "%SERVER%\configs\server.yaml" copy "%ROOT%configs\server.yaml" "%SERVER%\configs\server.yaml" >nul
if errorlevel 1 goto :error

if not exist "%CLIENTE%\.env" (
    >"%CLIENTE%\.env" echo # Ajuste estos valores para esta estacion.
    >>"%CLIENTE%\.env" echo SPOOLER_TOKEN=CAMBIAR-ESTE-TOKEN
    >>"%CLIENTE%\.env" echo SPOOLER_STATION_ID=CAJA-01
    >>"%CLIENTE%\.env" echo SPOOLER_SERVER_ADDR=http://192.168.1.10:8080
    >>"%CLIENTE%\.env" echo SPOOLER_VIRTUAL_COM=COM4
)
if errorlevel 1 goto :error

if not exist "%SERVER%\.env" (
    >"%SERVER%\.env" echo # Ajuste estos valores para el servidor fiscal.
    >>"%SERVER%\.env" echo SPOOLER_TOKEN=CAMBIAR-ESTE-TOKEN
    >>"%SERVER%\.env" echo SPOOLER_PRINTER_PORT=COM1
)
if errorlevel 1 goto :error

set "GOOS=windows"
set "GOARCH=amd64"
set "CGO_ENABLED=0"

echo Compilando agente (cliente.exe + agent.exe)...
pushd "%ROOT%"
go build -trimpath -ldflags "-s -w" -o "%CLIENTE%\agent.exe" .\cmd\agent
if errorlevel 1 (
    popd
    goto :error
)
copy /Y "%CLIENTE%\agent.exe" "%CLIENTE%\cliente.exe" >nul
if errorlevel 1 (
    popd
    goto :error
)

echo Compilando servidor...
go build -trimpath -ldflags "-s -w" -o "%SERVER%\server.exe" .\cmd\server
if errorlevel 1 (
    popd
    goto :error
)

echo Compilando simulador de impresion (simprint)...
go build -trimpath -ldflags "-s -w" -o "%SIMPRINT%\simprint.exe" .\cmd\simprint
if errorlevel 1 (
    popd
    goto :error
)

echo Compilando verificador de puesta en marcha...
go build -trimpath -ldflags "-s -w" -o "%VERIFICAR%\verificar.exe" .\cmd\verificar
if errorlevel 1 (
    popd
    goto :error
)
popd

echo.
echo Compilacion completada:
echo   Agente:   %CLIENTE%\agent.exe    (principal, para -install-com0com)
echo   Alias:    %CLIENTE%\cliente.exe  (compatibilidad legacy)
echo   Config:   %CLIENTE%\configs\agent.yaml
echo   Entorno:  %CLIENTE%\.env
echo   Servidor: %SERVER%\server.exe
echo   Config:   %SERVER%\configs\server.yaml
echo   Entorno:  %SERVER%\.env
echo   Simprint: %SIMPRINT%\simprint.exe  (emulador fiscal con salida a termica)
echo   Verificar: %VERIFICAR%\verificar.exe  (puesta en marcha contra la impresora)
echo.
echo Revise y personalice los archivos .env antes de ejecutar.
exit /b 0

:error
echo.
echo ERROR: No se pudo preparar o compilar el proyecto.
exit /b 1
