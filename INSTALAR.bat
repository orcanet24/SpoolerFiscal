@echo off
setlocal
title Spooler Fiscal - Instalador
echo ============================================================
echo   Spooler Fiscal - Instalador
echo ============================================================
echo.
echo Solicitando permisos de administrador...
set "PS=%SystemRoot%\System32\WindowsPowerShell\v1.0\powershell.exe"
"%PS%" -NoProfile -ExecutionPolicy Bypass -Command "Start-Process -FilePath '%PS%' -Verb RunAs -ArgumentList @('-NoProfile','-ExecutionPolicy','Bypass','-NoExit','-File','%~dp0scripts\instalar.ps1')"
if errorlevel 1 (
    echo.
    echo No se pudo iniciar el instalador con permisos elevados.
    echo Haga clic derecho en INSTALAR.bat y elija "Ejecutar como administrador".
    pause
)
endlocal
