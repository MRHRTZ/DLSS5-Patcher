@echo off
setlocal EnableDelayedExpansion

rem ============================================================
rem  DLSS 5 Patcher - build + packaging
rem
rem  Version is hardcoded below. Used for:
rem    - Exe name           : DLSS 5 Patcher v<VERSION>.exe
rem    - Main zip           : DLSS 5 Patcher v<VERSION>.zip
rem                             (exe + config.json + data\)
rem    - Per-dataset zips   : ^<data-folder-name^> v<VERSION>.zip
rem                             (dlss5, feeder, reshade, optiscaler,
rem                              dgvoodoo, reshade-setup, ...)
rem  All outputs go to    : build\<VERSION>\
rem ============================================================

set VERSION=1.2.1
set APPNAME=DLSS 5 Patcher

set ROOT=%~dp0
set SRC=%ROOT%src
set BUILT_EXE=%SRC%\build\bin\%APPNAME%.exe
set OUTDIR=%ROOT%build\%VERSION%
set STAGE=%OUTDIR%\stage
set VERSIONED_EXE=%APPNAME% v%VERSION%.exe
set MAIN_ZIP=%OUTDIR%\%APPNAME% v%VERSION%.zip

echo ============================================
echo  %APPNAME% - build v%VERSION%
echo ============================================

rem ---- [1/5] Prepare output folder ----
if exist "%STAGE%" rmdir /s /q "%STAGE%"
if not exist "%OUTDIR%" mkdir "%OUTDIR%"
if errorlevel 1 (
  echo [FAILED] Cannot create folder %OUTDIR%
  exit /b 1
)

rem ---- [2/5] Wails build from src ----
where wails >nul 2>&1
if errorlevel 1 (
  echo [FAILED] 'wails' is not on PATH.
  exit /b 1
)
echo [2/5] wails build ...
pushd "%SRC%"
call wails build
if errorlevel 1 (
  echo [FAILED] wails build failed.
  popd
  exit /b 1
)
popd
if not exist "%BUILT_EXE%" (
  echo [FAILED] Build output not found: %BUILT_EXE%
  exit /b 1
)

rem ---- [3/5] Copy versioned exe ----
echo [3/5] Copying exe ...
copy /y "%BUILT_EXE%" "%OUTDIR%\%VERSIONED_EXE%" >nul
if errorlevel 1 (
  echo [FAILED] Cannot copy versioned exe.
  exit /b 1
)
rem Dev launcher at root (per AGENTS.md)
copy /y "%BUILT_EXE%" "%ROOT%%APPNAME%.exe" >nul

rem ---- [4/5] Main zip: exe + config.json + data\ ----
echo [4/5] Preparing main zip ...
mkdir "%STAGE%"
copy /y "%OUTDIR%\%VERSIONED_EXE%" "%STAGE%\" >nul
copy /y "%ROOT%config.json" "%STAGE%\" >nul
if not exist "%ROOT%config.json" (
  echo [FAILED] config.json not found at root.
  exit /b 1
)
rem Copy data\ (skip the "New folder" junk dir).
robocopy "%ROOT%data" "%STAGE%\data" /E /XD "New folder" >nul
if %ERRORLEVEL% GEQ 8 (
  echo [FAILED] Cannot copy data folder.
  exit /b 1
)
powershell -NoProfile -NonInteractive -Command "Compress-Archive -Path '%STAGE%\*' -DestinationPath '%MAIN_ZIP%' -Force"
if errorlevel 1 (
  echo [FAILED] Cannot create %MAIN_ZIP%
  exit /b 1
)
rem Remove stage (retry: Defender sometimes locks DLL files briefly).
for /l %%R in (1,1,5) do (
  rmdir /s /q "%STAGE%" 2>nul
  if not exist "%STAGE%" goto stage_done
  timeout /t 2 /nobreak >nul
)
:stage_done

rem ---- [5/5] Per-data-subfolder zips ----
echo [5/5] Per-dataset zips ...
for /d %%D in ("%ROOT%data\*") do (
  echo   - %%~nxD v%VERSION%.zip
  powershell -NoProfile -NonInteractive -Command "Compress-Archive -Path '%%D\*' -DestinationPath '%OUTDIR%\%%~nxD v%VERSION%.zip' -Force"
  if !ERRORLEVEL! NEQ 0 (
    echo [FAILED] Cannot create zip for %%~nxD
    exit /b 1
  )
)

echo ============================================
echo  DONE - outputs in %OUTDIR%
dir /b "%OUTDIR%"
echo ============================================
exit /b 0
