@echo off
REM ============================================================
REM  MLStoreDB - iniciar servidor Mongo-compatible (M8)
REM
REM  Conecta con:  mongodb://127.0.0.1:28917
REM  Clientes:     Navicat / MongoDB Compass / mongosh
REM
REM  Detener:      Ctrl+C  (hace flush final y cierra limpio)
REM ============================================================
setlocal
cd /d "%~dp0"

REM ---- configuracion (edita estas lineas) ---------------------
set ADDR=127.0.0.1:28917
set DBNAME=demo
set DBFILE=data\demo.mlstore
set DBKEY=cambia-esta-clave-maestra-32b!
REM Para habilitar auth SCRAM-SHA-256, quita REM de estas 2 lineas:
REM set DBUSER=admin
REM set DBPASS=secreto
REM -------------------------------------------------------------

if not exist data mkdir data
if not exist bin mkdir bin

echo [1/2] Compilando mls-server...
go build -o bin\mls-server.exe .\tools\mls-server
if errorlevel 1 (
    echo ERROR: fallo la compilacion.
    pause
    exit /b 1
)

echo [2/2] Iniciando servidor en %ADDR%
echo.
echo   Base de datos : %DBNAME%
echo   Archivo       : %DBFILE%   (persistente, cifrado)
echo   URI           : mongodb://%ADDR%
echo.
if defined DBUSER (
    echo   Auth          : SCRAM-SHA-256 usuario "%DBUSER%"
    echo   URI con auth  : mongodb://%DBUSER%:%DBPASS%@%ADDR%
    echo.
    bin\mls-server.exe -addr %ADDR% -path %DBFILE% -key "%DBKEY%" -db %DBNAME% -user %DBUSER% -pass %DBPASS%
) else (
    bin\mls-server.exe -addr %ADDR% -path %DBFILE% -key "%DBKEY%" -db %DBNAME%
)

echo.
echo Servidor detenido. Los datos quedaron guardados en %DBFILE%
pause
