@echo off
chcp 65001 >nul
title Blood on the Clocktower - Launcher
cd /d %~dp0

echo ============================================
echo  Blood on the Clocktower - one click start
echo ============================================

echo [1/3] Starting AI service (8742) ...
start "BOTC-AI" cmd /c "cd /d %~dp0ai_service && start_ai.bat"

echo [2/3] Starting Go server (8741) ...
cd /d %~dp0botc_server
if not exist botc-server.exe (
    echo       building botc-server.exe ...
    go build -o botc-server.exe .
    if errorlevel 1 (
        echo       [error] build failed, please check Go environment
        pause
        exit /b 1
    )
)
start "BOTC-Server" cmd /c botc-server.exe
cd /d %~dp0

timeout /t 4 /nobreak >nul
echo [3/3] Opening browser ...
start http://127.0.0.1:8741

echo.
echo [done] Game: http://127.0.0.1:8741
echo        Close the two service windows to stop.
echo.
pause
