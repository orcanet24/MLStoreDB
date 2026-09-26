@echo off
REM ============================================================
REM  MLStoreDB (Memory-Log Store Database) - iniciar servidor
REM
REM  Arranca vacio: las colecciones se crean al insertar por wire
REM  (mongosh/Compass/Navicat), consola web o admin web.
REM  Detener: Ctrl+C
REM ============================================================
setlocal enabledelayedexpansion
cd /d "%~dp0"

REM ---- configuracion ------------------------------------------
set ADDR=127.0.0.1:28917
set WEB=127.0.0.1:28918
set DBDIR=databases
set DBKEY=
REM set DBKEY=tu-clave-32-bytes!!           <-- descomentar para cifrado
REM set DBUSER=admin
REM set DBPASS=secreto
REM -----------------------------------------------------------

if not exist bin mkdir bin

echo.
echo [1/2] Compilando mls-server...
go build -o bin\mls-server.exe .\tools\mls-server
if errorlevel 1 (
    echo ERROR: fallo la compilacion.
    pause
    exit /b 1
)

echo.
echo [2/2] Iniciando servidor...
echo.
echo   Wire Mongo  : %ADDR%   (Compass / Navicat / mongosh)
echo   Admin web   : http://%WEB%
echo   Directorio  : %DBDIR%\   (BDs se crean desde la web o al insertar)
echo.
if defined DBKEY (
    echo   Cifrado     : activado
) else (
    echo   Cifrado     : desactivado (configura DBKEY para cifrar)
)
echo.
REM Sin -path: el servidor arranca con store vacio en memoria.
REM Las colecciones se crean al insertar documentos por wire o web.
bin\mls-server.exe -addr %ADDR% -web %WEB% -dbdir "%DBDIR%" -key "%DBKEY%"

echo.
echo Servidor detenido.
pause
