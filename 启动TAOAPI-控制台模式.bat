@echo off
rem ============================================================
rem  TAOAPI launcher (optional)
rem
rem  NOTE: this .bat is NO LONGER REQUIRED.
rem  Just double-click taoapi.exe directly -- it starts the
rem  service with a tray icon and shows the panel address.
rem
rem  This file only exists for the case where you want a
rem  console-visible run (for troubleshooting).
rem
rem  What it does: run `taoapi.exe serve` in the foreground,
rem  so logs are visible in this window. Closing the window
rem  stops the service.
rem ============================================================

rem UTF-8 console so the service's Chinese log lines display correctly
rem (default code page here is 936/GBK, but Go writes UTF-8).
chcp 65001 >nul
title TAOAPI (console mode)

cd /d "%~dp0"

if not exist "taoapi.exe" (
    echo.
    echo [ERROR] taoapi.exe not found in this folder.
    echo.
    pause
    exit /b 1
)

echo.
echo ============================================================
echo   TAOAPI starting (console mode)...
echo ============================================================
echo.
echo   KEEP THIS WINDOW OPEN. Closing it stops the service.
echo      To stop: close this window, or press Ctrl+C.
echo.
echo   Tip: for the normal (tray, no console) experience,
echo        just double-click taoapi.exe instead.
echo.
echo ------------------------------------------------------------
echo.

taoapi.exe serve

echo.
echo ------------------------------------------------------------
echo   Service exited.
echo ------------------------------------------------------------
pause
