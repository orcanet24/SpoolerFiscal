@echo off
setlocal
rem Lanza el probador TFHKA con la primera Python 3 que encuentre.
where py >nul 2>nul
if %errorlevel%==0 (
    py -3 "%~dp0tfhka_gui.py"
    goto :eof
)
where python >nul 2>nul
if %errorlevel%==0 (
    python "%~dp0tfhka_gui.py"
    goto :eof
)
echo No se encontro Python 3. Instale Python desde https://www.python.org/
pause
