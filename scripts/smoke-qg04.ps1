# Requires PowerShell 7 and the running isolated QuotaGate Compose project.
param(
    [string]$ProjectName = 'quotagate-qg01',
    [string]$EnvFile = '.env.compose'
)

$ErrorActionPreference = 'Stop'
$qgRoot = Split-Path -Parent $PSScriptRoot
Push-Location -LiteralPath $qgRoot
$qgIDs = [System.Collections.Generic.List[string]]::new()
$qgStopped = $false

function Invoke-QGCompose {
    param([string[]]$Arguments)
    & docker compose -p $ProjectName --env-file $EnvFile @Arguments
    if ($LASTEXITCODE -ne 0) { throw 'QuotaGate Compose command failed' }
}
function Invoke-QGHTTP {
    param([string]$Method, [string]$URL, [string]$Token, [string]$Body, [int]$Expected)
    $qgHeaders = @{}
    if ($Token) { $qgHeaders.Authorization = "Bearer $Token" }
    # Even repeated application requests with this header must get fresh IDs.
    $qgHeaders['Idempotency-Key'] = 'qg04-same-inbound-header'
    $qgRequest = @{
        Method = $Method; Uri = $URL; Headers = $qgHeaders
        SkipHttpErrorCheck = $true; TimeoutSec = 10
    }
    if ($Body) { $qgRequest.Body = $Body; $qgRequest.ContentType = 'application/json' }
    $qgResponse = Invoke-WebRequest @qgRequest
    if ([int]$qgResponse.StatusCode -ne $Expected) {
        throw "$Method returned $($qgResponse.StatusCode), expected $Expected"
    }
    return ($qgResponse.Content | ConvertFrom-Json)
}

function Get-QGCalls {
    return (Invoke-QGHTTP GET "$qgExample/demo/stats" '' '' 200).handler_calls
}

try {
    $qgSettings = @{}
    foreach ($qgLine in Get-Content -LiteralPath $EnvFile) {
        if ($qgLine -match '^([A-Z_]+)=(.*)$') { $qgSettings[$Matches[1]] = $Matches[2] }
    }
    if (-not $qgSettings.QG_ADMIN_TOKEN) { throw 'QG_ADMIN_TOKEN is required in the local env file' }
    $qgPort = if ($qgSettings.QG_HTTP_PORT) { $qgSettings.QG_HTTP_PORT } else { '18080' }
    $qgExamplePort = if ($qgSettings.QG_EXAMPLE_PORT) { $qgSettings.QG_EXAMPLE_PORT } else { '18081' }
    $qgAPI = "http://127.0.0.1:$qgPort"
    $qgExample = "http://127.0.0.1:$qgExamplePort"
    $qgBaseline = Get-QGCalls
    $qgCustomers = @()
    foreach ($qgItem in @(@{ Name = 'QG-04 A'; Limit = 2 }, @{ Name = 'QG-04 B'; Limit = 3 })) {
        $qgCustomer = Invoke-QGHTTP POST "$qgAPI/v1/admin/customers" $qgSettings.QG_ADMIN_TOKEN (
            @{ name = $qgItem.Name } | ConvertTo-Json -Compress
        ) 201
        if ($qgCustomer.id -notmatch '^cus_[a-f0-9]{32}$') { throw 'Unexpected generated customer ID' }
        $qgIDs.Add($qgCustomer.id)
        $null = Invoke-QGHTTP PUT "$qgAPI/v1/admin/customers/$($qgCustomer.id)/policies/job.create" $qgSettings.QG_ADMIN_TOKEN (
            @{ daily_limit = $qgItem.Limit; rate_limit_per_minute = 100 } | ConvertTo-Json -Compress
        ) 200
        $qgCustomers += $qgCustomer
    }
    foreach ($qgStatus in @(201, 201, 429)) {
        $null = Invoke-QGHTTP POST "$qgExample/jobs" $qgCustomers[0].api_key '' $qgStatus
    }
    foreach ($qgStatus in @(201, 201)) {
        $null = Invoke-QGHTTP POST "$qgExample/jobs" $qgCustomers[1].api_key '' $qgStatus
    }
    $null = Invoke-QGHTTP POST "$qgExample/jobs" 'wrong' '' 401
    if ((Get-QGCalls) -ne ($qgBaseline + 4)) { throw 'Allow/denial handler count mismatch' }

    $qgStopped = $true
    Invoke-QGCompose -Arguments @('stop', 'api') | Out-Null
    $null = Invoke-QGHTTP POST "$qgExample/jobs" $qgCustomers[1].api_key '' 503
    if ((Get-QGCalls) -ne ($qgBaseline + 4)) { throw 'Handler ran during QuotaGate outage' }
    Invoke-QGCompose -Arguments @('up', '-d', '--wait', 'api') | Out-Null
    $qgStopped = $false
    $null = Invoke-QGHTTP POST "$qgExample/jobs" $qgCustomers[1].api_key '' 201
    $null = Invoke-QGHTTP POST "$qgExample/jobs" $qgCustomers[1].api_key '' 429
    if ((Get-QGCalls) -ne ($qgBaseline + 5)) { throw 'Recovery handler count mismatch' }
    $qgAUsage = Invoke-QGHTTP GET "$qgAPI/v1/usage?operation=job.create" $qgCustomers[0].api_key '' 200
    $qgBUsage = Invoke-QGHTTP GET "$qgAPI/v1/usage?operation=job.create" $qgCustomers[1].api_key '' 200
    if ($qgAUsage.daily_used -ne 2 -or $qgAUsage.daily_limit -ne 2 -or
        $qgBUsage.daily_used -ne 3 -or $qgBUsage.daily_limit -ne 3) { throw 'Customer usage isolation mismatch' }

    $qgIDList = ($qgIDs | ForEach-Object { "'$_'" }) -join ','
    $qgQuery = "SELECT (SELECT SUM(used) FROM quotagate.daily_usage WHERE customer_id IN ($qgIDList)), COUNT(*), COUNT(*) FILTER (WHERE allowed), COUNT(*) FILTER (WHERE NOT allowed), COUNT(DISTINCT decision_id) FROM quotagate.decisions WHERE customer_id IN ($qgIDList)"
    $qgReconciled = Invoke-QGCompose -Arguments @('exec', '-T', 'db', 'psql', '-U', 'quotagate', '-d', 'quotagate', '-At', '-c', $qgQuery)
    if ($qgReconciled -ne '5|7|5|2|7') { throw 'PostgreSQL usage/decision reconciliation mismatch' }
    # Verify exact current secrets in logs without printing matched lines.
    $qgLogs = Invoke-QGCompose -Arguments @('logs', '--no-color', 'api', 'example-api')
    $qgLogText = $qgLogs -join "`n"
    foreach ($qgSecret in @($qgSettings.QG_ADMIN_TOKEN, $qgCustomers[0].api_key, $qgCustomers[1].api_key)) {
        if ($qgLogText.Contains($qgSecret)) { throw 'Credential found in application logs' }
    }
    Write-Output 'PASS QG-04: A=201,201,429; B=201,201 then recovery=201,429; outage=503; invalid key=401.'
    Write-Output 'PASS handler calls: 4 before outage, unchanged during outage, 5 after recovery.'
    Write-Output 'PASS PostgreSQL: usage=5, decisions=7, allowed=5, denied=2, distinct IDs=7; secrets absent from logs.'
} finally {
    try {
        try {
            if ($qgStopped) { Invoke-QGCompose -Arguments @('up', '-d', '--wait', 'api') | Out-Null }
        } finally {
            if ($qgIDs.Count -gt 0) {
                $qgIDList = ($qgIDs | ForEach-Object { "'$_'" }) -join ','
                Invoke-QGCompose -Arguments @('exec', '-T', 'db', 'psql', '-U', 'quotagate', '-d', 'quotagate', '-c', "DELETE FROM quotagate.customers WHERE id IN ($qgIDList)") | Out-Null
                $qgRemaining = Invoke-QGCompose -Arguments @('exec', '-T', 'db', 'psql', '-U', 'quotagate', '-d', 'quotagate', '-At', '-c', "SELECT COUNT(*) FROM quotagate.customers WHERE id IN ($qgIDList)")
                if ($qgRemaining -ne '0') { throw 'Smoke-test customer cleanup failed' }
                Write-Output 'PASS cleanup: only the two generated smoke-test customers and their counters/decisions were removed.'
            }
        }
    } finally {
        Pop-Location
    }
}
