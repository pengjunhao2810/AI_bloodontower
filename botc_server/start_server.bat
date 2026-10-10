@echo off
chcp 65001 >nul
title Blood on the Clocktower - Go Server (8741)
cd /d %~dp0

if not exist botc-server.exe (
    echo [build] building botc-server.exe ...
    go build -o botc-server.exe .
    if errorlevel 1 (
        echo [error] build failed, please check Go environment
        pause
        exit /b 1
    )
)

echo [start] Go game server at http://127.0.0.1:8741
botc-server.exe
pause
