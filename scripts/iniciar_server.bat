@echo off
cd /d "%~dp0..\bin\server"
echo ============================================================
echo   SpoolerFiscal - Servidor
echo   gRPC: 5050   REST: 8080
echo   Ctrl+C para detener.
echo ============================================================
server.exe -console -config configs\server.yaml
pause
