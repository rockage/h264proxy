<#
.SYNOPSIS
  One-click end-to-end test for H264TunnelLab (Windows / PowerShell).

.DESCRIPTION
  Checks Go, builds, generates a key and a random test file, starts the server,
  waits for readiness, runs the client, transfers over a real WebRTC/SRTP
  H.264 media channel, and verifies the SHA-256. Exits non-zero on failure.

.EXAMPLE
  powershell -ExecutionPolicy Bypass -File scripts\run.ps1
  $env:SIZE=104857600; powershell -ExecutionPolicy Bypass -File scripts\run.ps1
  $env:MODE="raw"; powershell -ExecutionPolicy Bypass -File scripts\run.ps1
#>
$ErrorActionPreference = "Stop"

# Move to repo root (parent of scripts/).
Set-Location (Join-Path $PSScriptRoot "..")
$Root = (Get-Location).Path

$Size  = if ($env:SIZE)  { [int64]$env:SIZE } else { 4194304 }
$Mode  = if ($env:MODE)  { $env:MODE }  else { "aead" }
$Chunk = if ($env:CHUNK) { $env:CHUNK } else { "900" }
$Rate  = if ($env:RATE)  { $env:RATE }  else { "20000000" }
$Port  = if ($env:PORT)  { $env:PORT }  else { "8080" }
$Addr  = "127.0.0.1:$Port"

$Build = Join-Path $Root "build"
$Logs  = Join-Path $Build "logs"
$Data  = Join-Path $Build "data"
New-Item -ItemType Directory -Force -Path $Build,$Logs,$Data | Out-Null

$Key        = Join-Path $Data "key.bin"
$InFile     = Join-Path $Data "input.bin"
$OutFile    = Join-Path $Data "output.bin"
$SrvReport  = Join-Path $Logs "server_report.json"
$CliReport  = Join-Path $Logs "client_report.json"
$ServerExe  = Join-Path $Build "server.exe"
$ClientExe  = Join-Path $Build "client.exe"
$GenkeyExe  = Join-Path $Build "genkey.exe"

function Info($m){ Write-Host "[*] $m" -ForegroundColor Cyan }
function Ok($m){ Write-Host $m -ForegroundColor Green }
function Err($m){ Write-Host $m -ForegroundColor Red }

# 1. Check Go.
Info "Checking Go toolchain..."
if (-not (Get-Command go -ErrorAction SilentlyContinue)) {
  Err "Go is not installed or not on PATH. Install Go 1.21+ from https://go.dev/dl/"
  exit 1
}
go version

# 2. Build.
Info "Downloading dependencies and building..."
go build ./... | Out-Null
go build -o $GenkeyExe ./cmd/genkey
go build -o $ServerExe ./cmd/server
go build -o $ClientExe ./cmd/client
Ok "Build OK"

# 3. Key.
if ($Mode -eq "aead") {
  Info "Generating 32-byte key..."
  & $GenkeyExe -out $Key -force
}

# 4. Test file.
Info "Generating $Size-byte random test file..."
$rng = [System.Security.Cryptography.RandomNumberGenerator]::Create()
$fs  = [System.IO.File]::Open($InFile,[System.IO.FileMode]::Create)
try {
  $buf = New-Object byte[] 1048576
  $remaining = $Size
  while ($remaining -gt 0) {
    $n = [math]::Min($buf.Length, $remaining)
    $rng.GetBytes($buf)
    $fs.Write($buf, 0, $n)
    $remaining -= $n
  }
} finally { $fs.Close() }
$SrcSha = (Get-FileHash -Algorithm SHA256 $InFile).Hash.ToLower()
Info "source SHA-256: $SrcSha"

# 5. Start server.
Info "Starting server on $Addr..."
$srv = Start-Process -FilePath $ServerExe -PassThru -NoNewWindow `
  -ArgumentList @("-http",$Addr,"-key",$Key,"-in",$InFile,"-mode",$Mode,
                  "-chunk",$Chunk,"-rate",$Rate,"-log",(Join-Path $Logs "server.log"),
                  "-report",$SrvReport,"-timeout","600s") `
  -RedirectStandardOutput (Join-Path $Logs "server.stdout") `
  -RedirectStandardError  (Join-Path $Logs "server.stderr")

# 6. Wait for readiness.
Info "Waiting for server to be ready..."
$ready = $false
for ($i = 0; $i -lt 100; $i++) {
  if ($srv.HasExited) { Err "Server exited early."; Get-Content (Join-Path $Logs "server.stderr") -Tail 20; exit 1 }
  try {
    Invoke-WebRequest -UseBasicParsing -Uri "http://$Addr/offer" -TimeoutSec 2 | Out-Null
    $ready = $true; break
  } catch { Start-Sleep -Milliseconds 200 }
}
if (-not $ready) { Err "Server did not become ready in time"; exit 1 }
Ok "Server ready"

# 7-12. Client.
Info "Starting client..."
$cli = Start-Process -FilePath $ClientExe -PassThru -NoNewWindow -Wait `
  -ArgumentList @("-http",$Addr,"-key",$Key,"-out",$OutFile,"-mode",$Mode,
                  "-log",(Join-Path $Logs "client.log"),"-report",$CliReport,"-timeout","600s") `
  -RedirectStandardOutput (Join-Path $Logs "client.stdout") `
  -RedirectStandardError  (Join-Path $Logs "client.stderr")
$ClientExit = $cli.ExitCode

if (-not $srv.HasExited) { $srv.WaitForExit() }
$SrvExit = $srv.ExitCode

# 13. Verify.
$DstSha = if (Test-Path $OutFile) { (Get-FileHash -Algorithm SHA256 $OutFile).Hash.ToLower() } else { "MISSING" }
Write-Host ""
Info "server exit=$SrvExit client exit=$ClientExit"
Info "source    SHA-256: $SrcSha"
Info "recovered SHA-256: $DstSha"
Info "reports: $SrvReport , $CliReport"

if ($ClientExit -eq 0 -and $SrvExit -eq 0 -and $SrcSha -eq $DstSha) {
  Ok "SUCCESS: file transferred over real WebRTC H.264 SRTP channel and recovered byte-for-byte."
  exit 0
} else {
  Err "FAILURE: transfer did not verify. See logs in $Logs."
  if (Test-Path (Join-Path $Logs "client.stderr")) { Write-Host "--- client stderr tail ---"; Get-Content (Join-Path $Logs "client.stderr") -Tail 20 }
  exit 1
}
