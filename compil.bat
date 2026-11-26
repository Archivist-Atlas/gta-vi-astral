@echo off
chcp 65001 >nul
setlocal
cd /d "%~dp0"

echo ============================================
echo   GTA VI Astral — Compilation de astral.exe
echo ============================================
echo.

where go >nul 2>&1
if errorlevel 1 (
    echo [ERREUR] Go n'est pas installe ou absent du PATH.
    echo Telechargez Go sur : https://go.dev/dl/
    echo.
    pause
    exit /b 1
)

for /f "tokens=*" %%v in ('go version') do echo [INFO] %%v
echo [INFO] Compilation en cours (sans fenetre console)...

go build -ldflags="-H windowsgui" -o astral.exe ./cmd/astral
if errorlevel 1 (
    echo.
    echo [ERREUR] La compilation a echoue.
    pause
    exit /b 1
)

echo.
echo [OK] astral.exe compile avec succes !
echo      Vous pouvez maintenant lancer astral.exe.
echo.
pause
endlocal
