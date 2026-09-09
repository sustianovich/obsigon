@echo off
setlocal
powershell.exe -NoProfile -ExecutionPolicy Bypass -File "%~dp0iniciar.ps1"
set "OBSIGON_EXIT=%ERRORLEVEL%"
if not "%OBSIGON_EXIT%"=="0" pause
exit /b %OBSIGON_EXIT%
