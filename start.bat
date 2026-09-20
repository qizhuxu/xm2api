@echo off
rem xm2api 控制台入口（双击即可）。逻辑都在 menu.mjs 里，这里只做转发。
cd /d "%~dp0"
where node >nul 2>nul
if errorlevel 1 (
  echo [xm2api] 找不到 node，请先安装 Node ^>= 22 或把 node 加进 PATH。
  pause
  exit /b 1
)
node menu.mjs %*
if errorlevel 1 pause
