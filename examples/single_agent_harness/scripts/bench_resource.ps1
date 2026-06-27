# bench_resource.ps1
# So sanh resource usage giua SAH Go vs Python EI Core o trang thai idle.
#
# Usage:
#   .\scripts\bench_resource.ps1                    # chi do SAH Go
#   .\scripts\bench_resource.ps1 -WithEI            # do ca hai (can EI container dang chay)
#   .\scripts\bench_resource.ps1 -EIContainer ei-agent   # ten container EI tuy chinh
#
# Prereqs:
#   - Docker Desktop running
#   - SAH image built: docker build -f examples/single_agent_harness/Dockerfile -t sah:latest .
#   - .env file present in examples/single_agent_harness/

param(
    [switch]$WithEI,
    [string]$EIContainer = "ei-agent",   # name/id of the running Python EI container
    [string]$SAHImage    = "sah:latest",
    [string]$SAHEnvFile  = "$PSScriptRoot\..\env",
    [int]$WarmupSec      = 15,           # wait after start before sampling
    [int]$SampleSec      = 10            # duration to average docker stats
)

$ErrorActionPreference = "Stop"

# ── helpers ───────────────────────────────────────────────────────────────────

function Get-ImageSize([string]$image) {
    $raw = docker image inspect $image --format '{{.Size}}' 2>$null
    if (-not $raw) { return "N/A" }
    $bytes = [long]$raw
    if ($bytes -ge 1GB) { return "$([math]::Round($bytes/1GB, 2)) GB" }
    return "$([math]::Round($bytes/1MB, 1)) MB"
}

function Sample-Stats([string]$container, [int]$durationSec) {
    # docker stats --no-stream returns one snapshot; we average multiple samples.
    $samples = @()
    $deadline = (Get-Date).AddSeconds($durationSec)
    while ((Get-Date) -lt $deadline) {
        $raw = docker stats $container --no-stream --format "{{.MemUsage}}|{{.CPUPerc}}" 2>$null
        if ($raw) {
            $parts = $raw -split '\|'
            # MemUsage = "12.3MiB / 15.6GiB"  → take left side
            $memStr = ($parts[0] -split '/')[0].Trim()
            $cpuStr = $parts[1].Trim().TrimEnd('%')
            $memMiB = switch -Regex ($memStr) {
                '(\d+\.?\d*)GiB' { [double]$Matches[1] * 1024 }
                '(\d+\.?\d*)MiB' { [double]$Matches[1] }
                '(\d+\.?\d*)kB'  { [double]$Matches[1] / 1024 }
                default           { 0 }
            }
            $samples += [pscustomobject]@{ MemMiB = $memMiB; CPU = [double]$cpuStr }
        }
        Start-Sleep -Milliseconds 500
    }
    if ($samples.Count -eq 0) { return $null }
    $avgMem = ($samples | Measure-Object -Property MemMiB -Average).Average
    $avgCPU = ($samples | Measure-Object -Property CPU     -Average).Average
    return [pscustomobject]@{
        MemMiB   = [math]::Round($avgMem, 1)
        MemLabel = if ($avgMem -ge 1024) { "$([math]::Round($avgMem/1024,2)) GB" } else { "$([math]::Round($avgMem,1)) MB" }
        CPU      = [math]::Round($avgCPU, 2)
    }
}

# ── build check ───────────────────────────────────────────────────────────────

Write-Host ""
Write-Host "=== SAH Go Resource Benchmark ===" -ForegroundColor Cyan
Write-Host ""

$imageExists = docker image inspect $SAHImage 2>$null
if (-not $imageExists) {
    Write-Host "[!] Image '$SAHImage' not found. Building..." -ForegroundColor Yellow
    $repoRoot = Resolve-Path "$PSScriptRoot\..\..\..\"
    docker build -f "$PSScriptRoot\..\Dockerfile" -t $SAHImage $repoRoot
}

Write-Host "[image] $SAHImage = $(Get-ImageSize $SAHImage)" -ForegroundColor Green

# ── start SAH container ───────────────────────────────────────────────────────

$envFileArg = if (Test-Path $SAHEnvFile) { @("--env-file", $SAHEnvFile) } else {
    Write-Host "[warn] .env not found at $SAHEnvFile — container may exit if API keys missing" -ForegroundColor Yellow
    @()
}

$sahContainer = "sah-bench-$(Get-Random)"
Write-Host "[start] launching $SAHImage as $sahContainer ..." -ForegroundColor Cyan
docker run -d --rm --name $sahContainer -p 18080:8080 @envFileArg $SAHImage | Out-Null

Write-Host "[wait]  warming up for ${WarmupSec}s ..." -ForegroundColor Gray
Start-Sleep -Seconds $WarmupSec

# Verify it's still running
$running = docker ps --filter "name=$sahContainer" --format "{{.Names}}"
if (-not $running) {
    Write-Host "[error] Container exited early. Check logs:" -ForegroundColor Red
    docker logs $sahContainer 2>&1 | Select-Object -Last 20
    exit 1
}

# ── sample SAH stats ──────────────────────────────────────────────────────────

Write-Host "[sample] collecting ${SampleSec}s of stats for $sahContainer ..." -ForegroundColor Cyan
$sahStats = Sample-Stats $sahContainer $SampleSec

docker stop $sahContainer 2>$null | Out-Null
Write-Host "[stop]  $sahContainer stopped" -ForegroundColor Gray

# ── optional: sample EI Python stats ─────────────────────────────────────────

$eiStats = $null
if ($WithEI) {
    $eiRunning = docker ps --filter "name=$EIContainer" --format "{{.Names}}"
    if ($eiRunning) {
        Write-Host "[sample] collecting ${SampleSec}s of stats for EI Python ($EIContainer) ..." -ForegroundColor Cyan
        $eiStats = Sample-Stats $EIContainer $SampleSec
        $eiImageId = docker inspect $EIContainer --format '{{.Image}}' 2>$null
        $eiImageTags = docker inspect $eiImageId --format '{{index .RepoTags 0}}' 2>$null
        Write-Host "[image] EI Python ($eiImageTags) = $(Get-ImageSize $eiImageTags)"
    } else {
        Write-Host "[warn] EI container '$EIContainer' not running — skipping EI comparison" -ForegroundColor Yellow
    }
}

# ── report ────────────────────────────────────────────────────────────────────

Write-Host ""
Write-Host "╔══════════════════════════════════════════════════════╗" -ForegroundColor Cyan
Write-Host "║          IDLE RESOURCE USAGE COMPARISON              ║" -ForegroundColor Cyan
Write-Host "╠══════════════════════════╦═══════════╦═══════════════╣" -ForegroundColor Cyan
Write-Host "║ Metric                   ║ SAH (Go)  ║ EI Core (Py)  ║" -ForegroundColor Cyan
Write-Host "╠══════════════════════════╬═══════════╬═══════════════╣" -ForegroundColor Cyan

$sahMem  = if ($sahStats) { $sahStats.MemLabel } else { "N/A" }
$sahCPU  = if ($sahStats) { "$($sahStats.CPU)%" } else { "N/A" }
$eiMem   = if ($eiStats)  { $eiStats.MemLabel  } else { "–" }
$eiCPU   = if ($eiStats)  { "$($eiStats.CPU)%" } else { "–" }
$sahImg  = Get-ImageSize $SAHImage

$fmt = "║ {0,-24} ║ {1,-9} ║ {2,-13} ║"
Write-Host ($fmt -f "RAM (idle avg)",        $sahMem,  $eiMem)  -ForegroundColor White
Write-Host ($fmt -f "CPU (idle avg)",        $sahCPU,  $eiCPU)  -ForegroundColor White
Write-Host ($fmt -f "Image size",            $sahImg,  "see above") -ForegroundColor White
Write-Host ($fmt -f "Startup (approx)",      "${WarmupSec}s wait","~5-10s") -ForegroundColor White
Write-Host ($fmt -f "Runtime",               "Go static","Python 3.12") -ForegroundColor White

if ($sahStats -and $eiStats) {
    $memRatio = [math]::Round($eiStats.MemMiB / [math]::Max($sahStats.MemMiB, 0.1), 1)
    Write-Host "╠══════════════════════════╩═══════════╩═══════════════╣" -ForegroundColor Cyan
    Write-Host ("║  Go uses ~{0}x less RAM than Python EI Core              ║" -f $memRatio) -ForegroundColor Green
}

Write-Host "╚══════════════════════════════════════════════════════╝" -ForegroundColor Cyan
Write-Host ""
Write-Host "Tip: run with -WithEI to include the live EI container in the comparison." -ForegroundColor DarkGray
Write-Host "     docker ps | grep ei   # find the EI container name" -ForegroundColor DarkGray
Write-Host ""
