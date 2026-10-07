# PowerShell 7 + Go; run only against the isolated, already-started QG-07 project.
# The test-only host proxy drops/stalls replies from the real Compose API copies.
param([string]$EnvFile = '.env.compose', [string]$ProjectName = 'quotagate-qg07')
$ErrorActionPreference = 'Stop'
Push-Location -LiteralPath (Split-Path -Parent $PSScriptRoot)
$qgPreviousEnv = @{}
$qgEnvNames = @('QG_TEST_DATABASE_URL','QG_TEST_REDIS_URL','QG_TEST_API_URL','QG_TEST_API_URL_2','QG_TEST_ADMIN_TOKEN','GOCACHE')
foreach ($qgName in $qgEnvNames) { $qgPreviousEnv[$qgName] = [Environment]::GetEnvironmentVariable($qgName,'Process') }

function Invoke-QGCompose {
    param([string[]]$Arguments)
    & docker compose -p $ProjectName --env-file $EnvFile -f compose.yaml -f compose.qg05.yaml -f compose.qg06.yaml @Arguments
    if ($LASTEXITCODE -ne 0) { throw 'QG-07 Compose command failed' }
}

try {
    $qgSettings = @{}
    foreach ($qgLine in Get-Content -LiteralPath $EnvFile) {
        if ($qgLine -match '^([A-Z_0-9]+)=(.*)$') { $qgSettings[$Matches[1]] = $Matches[2] }
    }
    if (-not $qgSettings.QG_ADMIN_TOKEN -or -not $qgSettings.QG_DB_PASSWORD) { throw 'Local Compose credentials are required' }
    $qgPort = if ($qgSettings.QG_HTTP_PORT) { $qgSettings.QG_HTTP_PORT } else { '18080' }
    $qgPort2 = if ($qgSettings.QG_HTTP_2_PORT) { $qgSettings.QG_HTTP_2_PORT } else { '18082' }
    $qgDBPort = if ($qgSettings.QG_DB_PORT) { $qgSettings.QG_DB_PORT } else { '55432' }
    $qgRedisPort = if ($qgSettings.QG_REDIS_PORT) { $qgSettings.QG_REDIS_PORT } else { '56379' }
    $qgPassword = [Uri]::EscapeDataString($qgSettings.QG_DB_PASSWORD)
    $env:QG_TEST_DATABASE_URL = "postgres://quotagate:${qgPassword}@127.0.0.1:$qgDBPort/quotagate?sslmode=disable"
    # Real Compose decisions use Redis DB 0; the general integration suite uses 15.
    $env:QG_TEST_REDIS_URL = "redis://127.0.0.1:$qgRedisPort/0"
    $env:QG_TEST_API_URL = "http://127.0.0.1:$qgPort"
    $env:QG_TEST_API_URL_2 = "http://127.0.0.1:$qgPort2"
    $env:QG_TEST_ADMIN_TOKEN = $qgSettings.QG_ADMIN_TOKEN
    $env:GOCACHE = Join-Path (Get-Location) '.cache/go-build'
    foreach ($qgOrigin in @($env:QG_TEST_API_URL,$env:QG_TEST_API_URL_2)) {
        $qgReady = Invoke-WebRequest -Uri "$qgOrigin/health/ready" -TimeoutSec 5
        if ([int]$qgReady.StatusCode -ne 200) { throw 'QG-07 API copy is not ready' }
    }
    & go test ./examples/api -run '^TestQG07ComposeRetry$' -count=1 -timeout=60s -v
    if ($LASTEXITCODE -ne 0) { throw 'QG-07 live retry acceptance failed' }
    $qgCounts = Invoke-QGCompose -Arguments @('exec','-T','db','psql','-U','quotagate','-d','quotagate','-At','-c','SELECT (SELECT count(*) FROM quotagate.customers),(SELECT count(*) FROM quotagate.daily_usage),(SELECT count(*) FROM quotagate.decisions)')
    $qgRedisCount = Invoke-QGCompose -Arguments @('exec','-T','redis','redis-cli','-n','0','DBSIZE')
    if ($qgCounts -ne '0|0|0' -or $qgRedisCount -ne '0') { throw 'Expected empty isolated demo after test-owned cleanup' }
    $qgLogs = (Invoke-QGCompose -Arguments @('logs','--no-color','api','api-2','example-api')) -join "`n"
    if ($qgLogs.Contains($qgSettings.QG_ADMIN_TOKEN) -or $qgLogs.Contains($qgSettings.QG_DB_PASSWORD) -or $qgLogs -match 'qg_[a-f0-9]{64}') { throw 'Credential found in application logs' }
    Write-Output 'PASS live Compose retry acceptance, PostgreSQL cleanup=0|0|0, Redis=0, credentials absent from logs.'
} finally {
    foreach ($qgName in $qgEnvNames) { [Environment]::SetEnvironmentVariable($qgName,$qgPreviousEnv[$qgName],'Process') }
    Pop-Location
}
