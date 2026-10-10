@echo off
chcp 65001 >nul
title Blood on the Clocktower - AI Service (8742)
cd /d %~dp0

if not exist venv (
    echo [init] creating Python venv ...
    python -m venv venv
    venv\Scripts\python.exe -m pip install -r requirements.txt
)

echo [start] AI service at http://127.0.0.1:8742
venv\Scripts\python.exe main.py
pause
