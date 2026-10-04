# PowerShell 7; run only against the isolated, already-started QG-05 Compose demo.
param([string]$EnvFile = '.env.compose', [string]$ProjectName = 'quotagate-qg05')
$ErrorActionPreference = 'Stop'
Push-Location -LiteralPath (Split-Path -Parent $PSScriptRoot)
$qgIDs = [System.Collections.Generic.List[string]]::new()

function Invoke-QGCompose {
    param([string[]]$Arguments)
    & docker compose -p $ProjectName --env-file $EnvFile -f compose.yaml -f compose.qg05.yaml @Arguments
    if ($LASTEXITCODE -ne 0) { throw 'QG-05 Compose command failed' }
}
function Invoke-QGHTTP {
    param([string]$Method, [string]$URL, [string]$Token, [string]$Body, [int]$Expected)
    $qgRequest = @{ Method = $Method; Uri = $URL; Headers = @{ Authorization = "Bearer $Token" }; SkipHttpErrorCheck = $true; TimeoutSec = 10 }
    if ($Body) { $qgRequest.Body = $Body; $qgRequest.ContentType = 'application/json' }
    $qgResponse = Invoke-WebRequest @qgRequest
    if ([int]$qgResponse.StatusCode -ne $Expected) { throw "$Method returned $($qgResponse.StatusCode), expected $Expected" }
    return ($qgResponse.Content | ConvertFrom-Json)
}
function Get-QGHash {
    param([string]$Value)
    return [Convert]::ToHexString([System.Security.Cryptography.SHA256]::HashData([System.Text.Encoding]::UTF8.GetBytes($Value))).ToLowerInvariant()
}
function Wait-QGWindow {
    $qgTime = @(Invoke-QGCompose -Arguments @('exec', '-T', 'redis', 'redis-cli', '--raw', 'TIME'))
    $qgNowMS = [int64]$qgTime[0] * 1000 + [int64][Math]::Floor(([int64]$qgTime[1]) / 1000)
    $qgRemaining = 60000 - $qgNowMS % 60000
    if ($qgRemaining -lt 20000) { Start-Sleep -Milliseconds ($qgRemaining + 50) }
}
function Invoke-QGConcurrent {
    param([string]$Key, [int]$Count, [string]$Prefix, [bool]$Duplicate)
    $qgOriginsForCall = $qgOrigins
    return @(0..($Count - 1) | ForEach-Object -ThrottleLimit 20 -Parallel {
        $qgIndex = $_
        $qgOrigin = ($using:qgOriginsForCall)[$qgIndex % 2]
        $qgDecisionID = if ($using:Duplicate) { $using:Prefix } else { "$($using:Prefix)_$qgIndex" }
        $qgBody = @{ decision_id = $qgDecisionID; operation = 'job.create' } | ConvertTo-Json -Compress
        $qgResponse = Invoke-WebRequest -Method POST -Uri "$qgOrigin/demo/rate-checks" -Headers @{ Authorization = "Bearer $($using:Key)" } -Body $qgBody -ContentType 'application/json' -SkipHttpErrorCheck -TimeoutSec 10
        if ([int]$qgResponse.StatusCode -ne 200) { throw 'Concurrent rate check did not return 200' }
        $qgResult = $qgResponse.Content | ConvertFrom-Json
        [pscustomobject]@{ allowed = $qgResult.allowed; reason = $qgResult.reason; rate_used = $qgResult.rate_used; rate_limit = $qgResult.rate_limit; window_start_utc = $qgResult.window_start_utc; resets_at_utc = $qgResult.resets_at_utc; decision_id = $qgDecisionID; origin = $qgOrigin }
    })
}

try {
    $qgSettings = @{}
    foreach ($qgLine in Get-Content -LiteralPath $EnvFile) {
        if ($qgLine -match '^([A-Z_0-9]+)=(.*)$') { $qgSettings[$Matches[1]] = $Matches[2] }
    }
    if (-not $qgSettings.QG_ADMIN_TOKEN) { throw 'QG_ADMIN_TOKEN is required' }
    $qgPort = if ($qgSettings.QG_HTTP_PORT) { $qgSettings.QG_HTTP_PORT } else { '18080' }
    $qgPort2 = if ($qgSettings.QG_HTTP_2_PORT) { $qgSettings.QG_HTTP_2_PORT } else { '18082' }
    $qgOrigins = @("http://127.0.0.1:$qgPort", "http://127.0.0.1:$qgPort2")
    $qgRun = [Guid]::NewGuid().ToString('N')
    $qgCustomers = @()
    foreach ($qgItem in @(@{ Name = 'QG-05 A'; Rate = 7 }, @{ Name = 'QG-05 B'; Rate = 1 })) {
        $qgCreated = Invoke-QGHTTP POST "$($qgOrigins[0])/v1/admin/customers" $qgSettings.QG_ADMIN_TOKEN (@{ name = $qgItem.Name } | ConvertTo-Json -Compress) 201
        if ($qgCreated.id -notmatch '^cus_[a-f0-9]{32}$') { throw 'Unexpected generated customer ID' }
        $qgIDs.Add($qgCreated.id)
        foreach ($qgOperation in @('job.create', 'job.other')) {
            $null = Invoke-QGHTTP PUT "$($qgOrigins[0])/v1/admin/customers/$($qgCreated.id)/policies/$qgOperation" $qgSettings.QG_ADMIN_TOKEN (@{ daily_limit = 1000; rate_limit_per_minute = $qgItem.Rate } | ConvertTo-Json -Compress) 200
        }
        $qgCustomers += $qgCreated
    }
    Wait-QGWindow
    $qgResults = Invoke-QGConcurrent $qgCustomers[0].api_key 60 $qgRun $false
    $qgAllowed = @($qgResults | Where-Object { $_.allowed -eq $true -and $_.reason -eq 'rate_allowed' }).Count
    $qgDenied = @($qgResults | Where-Object { $_.allowed -eq $false -and $_.reason -eq 'rate_limited' -and $_.rate_used -eq 7 }).Count
    if ($qgResults.Count -ne 60 -or $qgAllowed -ne 7 -or $qgDenied -ne 53 -or
        @($qgResults.window_start_utc | Select-Object -Unique).Count -ne 1 -or
        @($qgResults.origin | Select-Object -Unique).Count -ne 2) { throw 'Two-copy shared rate limit mismatch or window boundary crossed' }
    $qgBucket = 'qg:rate:' + (Get-QGHash $qgCustomers[0].id) + ':bucket:' + (Get-QGHash 'job.create')
    $qgUsed = Invoke-QGCompose -Arguments @('exec', '-T', 'redis', 'redis-cli', '--raw', 'HGET', $qgBucket, 'used')
    $qgExpiry = Invoke-QGCompose -Arguments @('exec', '-T', 'redis', 'redis-cli', '--raw', 'PEXPIRETIME', $qgBucket)
    $qgExpectedExpiry = ([DateTimeOffset]$qgResults[0].resets_at_utc).ToUnixTimeMilliseconds()
    if ($qgUsed -ne '7' -or [int64]$qgExpiry -ne $qgExpectedExpiry) { throw "Redis counter/expiry mismatch: used=$qgUsed, expiry=$qgExpiry, expected=$qgExpectedExpiry" }
    $qgRecord = 'qg:rate:' + (Get-QGHash $qgCustomers[0].id) + ':decision:' + (Get-QGHash $qgResults[0].decision_id)
    $qgRecordTTL = Invoke-QGCompose -Arguments @('exec', '-T', 'redis', 'redis-cli', '--raw', 'PTTL', $qgRecord)
    if ([int64]$qgRecordTTL -lt 86340000 -or [int64]$qgRecordTTL -gt 86400000) { throw '24-hour pre-result TTL mismatch' }
    Write-Output 'PASS two copies: 60 unique requests = 7 rate_allowed / 53 rate_limited; Redis counter=7; absolute window expiry and 24h pre-result TTL match.'

    Wait-QGWindow
    $qgDuplicateID = $qgRun + '_duplicate'
    $qgDuplicates = Invoke-QGConcurrent $qgCustomers[1].api_key 20 $qgDuplicateID $true
    if ($qgDuplicates.Count -ne 20 -or @($qgDuplicates | Where-Object { -not $_.allowed -or $_.rate_used -ne 1 -or $_.rate_limit -ne 1 }).Count -ne 0) { throw 'Concurrent duplicate consumed more than one rate allowance' }
    # A replay can cross a real minute without consuming the new window. Prime
    # the current window before testing a fresh denial, then check immediately.
    Wait-QGWindow
    $qgCurrent = Invoke-QGHTTP POST "$($qgOrigins[0])/demo/rate-checks" $qgCustomers[1].api_key (@{ decision_id = $qgRun + '_current'; operation = 'job.create' } | ConvertTo-Json -Compress) 200
    if ($qgCurrent.rate_used -ne 1) { throw 'Current B window was not primed to one rate allowance' }
    $qgDeniedID = $qgRun + '_denied'
    $qgDeniedResult = Invoke-QGHTTP POST "$($qgOrigins[0])/demo/rate-checks" $qgCustomers[1].api_key (@{ decision_id = $qgDeniedID; operation = 'job.create' } | ConvertTo-Json -Compress) 200
    if ($qgDeniedResult.allowed -or $qgDeniedResult.rate_used -ne 1 -or $qgDeniedResult.window_start_utc -ne $qgCurrent.window_start_utc) { throw 'B should have exhausted rate limit 1 in the same current window' }
    $null = Invoke-QGHTTP PUT "$($qgOrigins[0])/v1/admin/customers/$($qgCustomers[1].id)/policies/job.create" $qgSettings.QG_ADMIN_TOKEN '{"daily_limit":1000,"rate_limit_per_minute":5}' 200
    foreach ($qgReplayID in @($qgDuplicateID, $qgDeniedID)) {
        $qgReplay = Invoke-QGHTTP POST "$($qgOrigins[1])/demo/rate-checks" $qgCustomers[1].api_key (@{ decision_id = $qgReplayID; operation = 'job.create' } | ConvertTo-Json -Compress) 200
        if ($qgReplay.rate_limit -ne 1 -or $qgReplay.rate_used -ne 1 -or ($qgReplay.allowed -ne ($qgReplayID -eq $qgDuplicateID))) { throw 'Policy update changed a stored pre-result' }
    }
    $null = Invoke-QGHTTP POST "$($qgOrigins[1])/demo/rate-checks" $qgCustomers[1].api_key (@{ decision_id = $qgDuplicateID; operation = 'job.other' } | ConvertTo-Json -Compress) 409
    $qgOther = Invoke-QGHTTP POST "$($qgOrigins[1])/demo/rate-checks" $qgCustomers[1].api_key (@{ decision_id = $qgRun + '_other'; operation = 'job.other' } | ConvertTo-Json -Compress) 200
    if (-not $qgOther.allowed -or $qgOther.rate_used -ne 1) { throw 'Operation counters were not isolated' }
    $null = Invoke-QGHTTP POST "$($qgOrigins[0])/demo/rate-checks" 'wrong' '{"decision_id":"invalid_auth_00001","operation":"job.create"}' 401
    $null = Invoke-QGHTTP POST "$($qgOrigins[0])/demo/rate-checks" $qgCustomers[0].api_key '{"decision_id":"foreign_customer_001","operation":"job.create","customer_id":"foreign"}' 400
    foreach ($qgCustomer in $qgCustomers) {
        $qgUsage = Invoke-QGHTTP GET "$($qgOrigins[0])/v1/usage?operation=job.create" $qgCustomer.api_key '' 200
        if ($qgUsage.daily_used -ne 0) { throw 'Rate pre-check changed PostgreSQL daily usage' }
    }
    $qgIDList = ($qgIDs | ForEach-Object { "'$_'" }) -join ','
    $qgDBCounts = Invoke-QGCompose -Arguments @('exec', '-T', 'db', 'psql', '-U', 'quotagate', '-d', 'quotagate', '-At', '-c', "SELECT (SELECT COUNT(*) FROM quotagate.daily_usage WHERE customer_id IN ($qgIDList)), (SELECT COUNT(*) FROM quotagate.decisions WHERE customer_id IN ($qgIDList))")
    if ($qgDBCounts -ne '0|0') { throw 'Rate pre-check wrote final daily decisions' }
    $qgLogs = (Invoke-QGCompose -Arguments @('logs', '--no-color', 'api', 'api-2')) -join "`n"
    foreach ($qgSecret in @($qgSettings.QG_ADMIN_TOKEN, $qgCustomers[0].api_key, $qgCustomers[1].api_key)) {
        if ($qgLogs.Contains($qgSecret)) { throw 'Credential found in QuotaGate logs' }
    }
    Write-Output 'PASS 20 concurrent duplicates=one rate allowance; allow/deny replays keep original policy result; operation/customer scopes isolated; conflict=409.'
    Write-Output 'PASS auth=401, foreign customer body=400; PostgreSQL usage/final decisions=0|0; secrets absent from logs.'
} finally {
    try {
        foreach ($qgID in $qgIDs) {
            $qgPattern = 'qg:rate:' + (Get-QGHash $qgID) + ':*'
            $qgKeys = @(Invoke-QGCompose -Arguments @('exec', '-T', 'redis', 'redis-cli', '--scan', '--pattern', $qgPattern))
            if ($qgKeys.Count -gt 0) { Invoke-QGCompose -Arguments (@('exec', '-T', 'redis', 'redis-cli', 'DEL') + $qgKeys) | Out-Null }
        }
    } finally {
        try {
            if ($qgIDs.Count -gt 0) {
                $qgIDList = ($qgIDs | ForEach-Object { "'$_'" }) -join ','
                Invoke-QGCompose -Arguments @('exec', '-T', 'db', 'psql', '-U', 'quotagate', '-d', 'quotagate', '-c', "DELETE FROM quotagate.customers WHERE id IN ($qgIDList)") | Out-Null
                Write-Output 'PASS cleanup: only the generated QG-05 customers and their Redis keys were removed.'
            }
        } finally { Pop-Location }
    }
}
