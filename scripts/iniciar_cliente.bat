@echo off
cd /d "%~dp0..\bin\cliente"
echo ============================================================
echo   SpoolerFiscal - Agente de estacion
echo   Se conecta al servidor por HTTP (SPOOLER_SERVER_ADDR del .env).
echo   Ctrl+C para detener.
echo ============================================================
cliente.exe -console -config configs\agent.yaml
pause
