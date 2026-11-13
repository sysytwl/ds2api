@echo off
setlocal
cd /d "%~dp0"
if not exist "config.json" copy /y "config.example.json" "config.json" >nul 2>nul
echo Starting elm2api: http://127.0.0.1:8787
echo Press Ctrl+C to stop.
elm2api.exe -config "%~dp0config.json"
echo.
echo elm2api exited.
pause
