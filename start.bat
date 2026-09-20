@echo off
cd /d "%~dp0"
title xm2api console

where node >nul 2>nul
if errorlevel 1 goto nonode

node menu.mjs %*
set "CODE=%ERRORLEVEL%"

echo.
if not "%CODE%"=="0" echo [xm2api] menu exited with code %CODE%
echo Press any key to close this window . . .
pause >nul
exit /b %CODE%

:nonode
echo.
echo [xm2api] ERROR: node not found.
echo            Install Node 22 or newer, or add node to PATH.
echo.
pause
exit /b 1
