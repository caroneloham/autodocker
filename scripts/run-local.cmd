@echo off
setlocal
cd /d "%~dp0.."
if not exist ".local\state" mkdir ".local\state"

set "AUTODOCKER_ROOT=%CD%\.local\state"
set "BIND_HOST=127.0.0.1"
set "PORT=18743"
set "AUTODOCKER_GENERATOR_ONLY=0"
set "AUTODOCKER_ACCOUNT_OWNER=admin"
set "AUTODOCKER_LOCAL_BOOTSTRAP=1"
set "AUTODOCKER_LOCAL_HTTP=1"
set "AUTH_BYPASS=0"
set "DEMO=0"
set "AUTODOCKER_OPEN_BROWSER=1"

set "AUTODOCKER_BOOTSTRAP_TOKEN="
for /f "usebackq delims=" %%K in (`powershell.exe -NoLogo -NoProfile -NonInteractive -Command "$rng=[Security.Cryptography.RandomNumberGenerator]::Create(); $bytes=New-Object byte[] 32; $rng.GetBytes($bytes); $rng.Dispose(); [BitConverter]::ToString($bytes).Replace('-','').ToLowerInvariant()"`) do set "AUTODOCKER_BOOTSTRAP_TOKEN=%%K"
if not defined AUTODOCKER_BOOTSTRAP_TOKEN (
  echo Impossible de preparer le premier lancement securise.
  exit /b 1
)

go run .
if errorlevel 1 (
  echo.
  echo AutoDocker s'est arrete avec une erreur. Consultez le message ci-dessus.
  pause
)
