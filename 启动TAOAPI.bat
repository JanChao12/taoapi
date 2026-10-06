@echo off
rem Switch console to UTF-8 so wbapi's Chinese log lines show correctly.
rem (Default code page on this machine is 936/GBK, but Go writes UTF-8
rem  -> the service log would appear as garbled text.)
chcp 65001 >nul
title TAOAPI

cd /d "%~dp0"

if not exist "wbapi.exe" (
    echo [ERROR] wbapi.exe not found in this folder.
    pause
    exit /b 1
)

echo.
echo ============================================================
echo   TAOAPI starting...
echo ============================================================
echo.
echo   Panel: http://127.0.0.1:8787/panel/
echo.
echo   KEEP THIS WINDOW OPEN. Closing it stops the service.
echo      To stop: close this window, or press Ctrl+C.
echo.
echo ------------------------------------------------------------
echo.

start "" cmd /c "timeout /t 3 /nobreak >nul & start http://127.0.0.1:8787/panel/"

wbapi.exe serve

echo.
echo ------------------------------------------------------------
echo   Service exited.
echo ------------------------------------------------------------
pause
