# PowerShell 7; run against the isolated, already-started QG-06 Compose project.
param([string]$EnvFile = '.env.compose', [string]$ProjectName = 'quotagate-qg06')
$ErrorActionPreference = 'Stop'
Push-Location -LiteralPath (Split-Path -Parent $PSScriptRoot)
$qgIDs = [System.Collections.Generic.List[string]]::new()
$qgFaultConstraint = ''

function Invoke-QGCompose {
    param([string[]]$Arguments)
    & docker compose -p $ProjectName --env-file $EnvFile -f compose.yaml -f compose.qg05.yaml -f compose.qg06.yaml @Arguments
    if ($LASTEXITCODE -ne 0) { throw 'QG-06 Compose command failed' }
}
function Invoke-QGHTTP {
    param([string]$Method, [string]$URL, [string]$Token, [string]$Body, [int]$Expected)
    $qgRequest = @{ Method = $Method; Uri = $URL; Headers = @{ Authorization = "Bearer $Token" }; SkipHttpErrorCheck = $true; TimeoutSec = 10 }
    if ($Body) { $qgRequest.Body = $Body; $qgRequest.ContentType = 'application/json' }
    $qgResponse = Invoke-WebRequest @qgRequest
    if ([int]$qgResponse.StatusCode -ne $Expected) { throw "$Method returned $($qgResponse.StatusCode), expected $Expected" }
    return ($qgResponse.Content | ConvertFrom-Json)
}
function Invoke-QGDecision {
    param([int]$Copy, [string]$Key, [string]$ID, [string]$Operation = 'job.create', [int]$Expected = 200)
    return Invoke-QGHTTP POST "$($qgOrigins[$Copy])/v1/decisions" $Key (@{ decision_id = $ID; operation = $Operation } | ConvertTo-Json -Compress) $Expected
}
function Get-QGHash {
    param([string]$Value)
    return [Convert]::ToHexString([System.Security.Cryptography.SHA256]::HashData([System.Text.Encoding]::UTF8.GetBytes($Value))).ToLowerInvariant()
}
function Get-QGRateUsed {
    param([string]$CustomerID)
    $qgBucket = 'qg:rate:' + (Get-QGHash $CustomerID) + ':bucket:' + (Get-QGHash 'job.create')
    return Invoke-QGCompose -Arguments @('exec', '-T', 'redis', 'redis-cli', '--raw', 'HGET', $qgBucket, 'used')
}
function Wait-QGWindow {
    $qgTime = @(Invoke-QGCompose -Arguments @('exec', '-T', 'redis', 'redis-cli', '--raw', 'TIME'))
    $qgNowMS = [int64]$qgTime[0] * 1000 + [int64][Math]::Floor(([int64]$qgTime[1]) / 1000)
    $qgRemaining = 60000 - $qgNowMS % 60000
    if ($qgRemaining -lt 25000) { Start-Sleep -Milliseconds ($qgRemaining + 50) }
}
function Invoke-QGConcurrent {
    param([string]$Key, [int]$Count, [string]$Prefix, [bool]$Duplicate)
    $qgOriginsForCall = $qgOrigins
    return @(0..($Count - 1) | ForEach-Object -ThrottleLimit 20 -Parallel {
        $qgIndex = $_
        $qgOrigin = ($using:qgOriginsForCall)[$qgIndex % 2]
        $qgDecisionID = if ($using:Duplicate) { $using:Prefix } else { "$($using:Prefix)_$qgIndex" }
        $qgBody = @{ decision_id = $qgDecisionID; operation = 'job.create' } | ConvertTo-Json -Compress
        $qgResponse = Invoke-WebRequest -Method POST -Uri "$qgOrigin/v1/decisions" -Headers @{ Authorization = "Bearer $($using:Key)" } -Body $qgBody -ContentType 'application/json' -SkipHttpErrorCheck -TimeoutSec 10
        if ([int]$qgResponse.StatusCode -ne 200) { throw 'Concurrent final decision did not return 200' }
        $qgResult = $qgResponse.Content | ConvertFrom-Json
        [pscustomobject]@{ allowed = $qgResult.allowed; reason = $qgResult.reason; daily_used = $qgResult.daily_used; daily_limit = $qgResult.daily_limit; policy_version = $qgResult.policy_version; decision_id = $qgDecisionID; origin = $qgOrigin }
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
    $qgExamplePort = if ($qgSettings.QG_EXAMPLE_PORT) { $qgSettings.QG_EXAMPLE_PORT } else { '18081' }
    $qgOrigins = @("http://127.0.0.1:$qgPort", "http://127.0.0.1:$qgPort2")
    $qgExample = "http://127.0.0.1:$qgExamplePort"
    $qgRun = [Guid]::NewGuid().ToString('N')
    $qgCustomers = @()
    foreach ($qgItem in @(@{ Name = 'QG-06 A'; Daily = 3; Rate = 7 }, @{ Name = 'QG-06 B'; Daily = 1; Rate = 2 }, @{ Name = 'QG-06 rate handler'; Daily = 1000; Rate = 1 }, @{ Name = 'QG-06 daily handler'; Daily = 1; Rate = 1000 })) {
        $qgCreated = Invoke-QGHTTP POST "$($qgOrigins[0])/v1/admin/customers" $qgSettings.QG_ADMIN_TOKEN (@{ name = $qgItem.Name } | ConvertTo-Json -Compress) 201
        if ($qgCreated.id -notmatch '^cus_[a-f0-9]{32}$') { throw 'Unexpected generated customer ID' }
        $qgIDs.Add($qgCreated.id)
        $null = Invoke-QGHTTP PUT "$($qgOrigins[0])/v1/admin/customers/$($qgCreated.id)/policies/job.create" $qgSettings.QG_ADMIN_TOKEN (@{ daily_limit = $qgItem.Daily; rate_limit_per_minute = $qgItem.Rate } | ConvertTo-Json -Compress) 200
        $qgCustomers += $qgCreated
    }
    Wait-QGWindow
    $qgResults = Invoke-QGConcurrent $qgCustomers[0].api_key 60 $qgRun $false
    if ($qgResults.Count -ne 60 -or @($qgResults | Where-Object { $_.reason -eq 'allowed' }).Count -ne 3 -or
        @($qgResults | Where-Object { $_.reason -eq 'daily_quota' }).Count -ne 4 -or
        @($qgResults | Where-Object { $_.reason -eq 'rate_limited' }).Count -ne 53 -or
        @($qgResults.origin | Select-Object -Unique).Count -ne 2 -or (Get-QGRateUsed $qgCustomers[0].id) -ne '7') { throw 'Shared rate/daily decisions mismatch or window boundary crossed' }
    Write-Output 'PASS two copies: 60 unique = 3 allowed / 4 daily_quota / 53 rate_limited; Redis=7, daily=3.'

    Wait-QGWindow
    # Reuse an A decision ID with B's key to verify tenant isolation as well.
    $qgDuplicateID = $qgRun + '_0'
    $qgDuplicates = Invoke-QGConcurrent $qgCustomers[1].api_key 20 $qgDuplicateID $true
    if ($qgDuplicates.Count -ne 20 -or @($qgDuplicates | Where-Object { -not $_.allowed -or $_.daily_used -ne 1 -or $_.daily_limit -ne 1 }).Count -ne 0 -or
        (Get-QGRateUsed $qgCustomers[1].id) -ne '1') { throw 'Duplicate decisions consumed more than one allowance' }
    $qgFaultConstraint = 'qg06_smoke_' + $qgRun
    $qgFaultSQL = "ALTER TABLE quotagate.decisions ADD CONSTRAINT $qgFaultConstraint CHECK (customer_id <> '$($qgCustomers[1].id)') NOT VALID"
    Invoke-QGCompose -Arguments @('exec', '-T', 'db', 'psql', '-v', 'ON_ERROR_STOP=1', '-U', 'quotagate', '-d', 'quotagate', '-c', $qgFaultSQL) | Out-Null
    $qgDailyID = $qgRun + '_db_fault'
    $null = Invoke-QGDecision 0 $qgCustomers[1].api_key $qgDailyID 'job.create' 503
    $qgFaultCounts = Invoke-QGCompose -Arguments @('exec', '-T', 'db', 'psql', '-U', 'quotagate', '-d', 'quotagate', '-At', '-c', "SELECT (SELECT sum(used) FROM quotagate.daily_usage WHERE customer_id='$($qgCustomers[1].id)'), count(*) FROM quotagate.decisions WHERE customer_id='$($qgCustomers[1].id)'")
    if ($qgFaultCounts -ne '1|1' -or (Get-QGRateUsed $qgCustomers[1].id) -ne '2') { throw 'PostgreSQL fault changed durable state or refunded Redis' }
    Invoke-QGCompose -Arguments @('exec', '-T', 'db', 'psql', '-v', 'ON_ERROR_STOP=1', '-U', 'quotagate', '-d', 'quotagate', '-c', "ALTER TABLE quotagate.decisions DROP CONSTRAINT $qgFaultConstraint") | Out-Null
    $qgFaultConstraint = ''
    $qgDaily = Invoke-QGDecision 1 $qgCustomers[1].api_key $qgDailyID
    $qgRateID = $qgRun + '_rate'
    $qgRate = Invoke-QGDecision 0 $qgCustomers[1].api_key $qgRateID
    if ($qgDaily.reason -ne 'daily_quota' -or $qgRate.reason -ne 'rate_limited' -or (Get-QGRateUsed $qgCustomers[1].id) -ne '2') { throw 'Partial failure retry or denial reasons mismatch' }
    $null = Invoke-QGHTTP PUT "$($qgOrigins[0])/v1/admin/customers/$($qgCustomers[1].id)/policies/job.create" $qgSettings.QG_ADMIN_TOKEN '{"daily_limit":10,"rate_limit_per_minute":10}' 200
    foreach ($qgReplayID in @($qgDuplicateID, $qgDailyID, $qgRateID)) {
        $qgReplay = Invoke-QGDecision 1 $qgCustomers[1].api_key $qgReplayID
        $qgReason = if ($qgReplayID -eq $qgDuplicateID) { 'allowed' } elseif ($qgReplayID -eq $qgDailyID) { 'daily_quota' } else { 'rate_limited' }
        if ($qgReplay.reason -ne $qgReason -or $qgReplay.daily_limit -ne 1 -or $qgReplay.policy_version -ne 1) { throw 'Final replay changed after policy update' }
    }
    $null = Invoke-QGDecision 1 $qgCustomers[1].api_key $qgDuplicateID 'job.other' 409
    $null = Invoke-QGDecision 0 $qgCustomers[1].api_key ($qgRun + '_unknown') 'job.other' 403
    $null = Invoke-QGDecision 0 'wrong' ($qgRun + '_bad_auth') 'job.create' 401
    $null = Invoke-QGHTTP POST "$($qgOrigins[0])/v1/decisions" $qgCustomers[0].api_key '{"decision_id":"foreign_customer_001","operation":"job.create","customer_id":"foreign"}' 400
    $null = Invoke-QGHTTP POST "$($qgOrigins[0])/v1/decisions?customer_id=foreign" $qgCustomers[0].api_key '{"decision_id":"foreign_customer_002","operation":"job.create"}' 400
    Write-Output 'PASS 20 duplicates=one daily/rate allowance; injected PostgreSQL write fault=503, Redis remains consumed; retry and final replays preserve results; conflict=409, auth=401, policy=403, foreign customer=400.'

    Wait-QGWindow
    $qgBefore = Invoke-QGHTTP GET "$qgExample/demo/stats" '' '' 200
    foreach ($qgCustomer in @($qgCustomers[2], $qgCustomers[3])) {
        $null = Invoke-QGHTTP POST "$qgExample/jobs" $qgCustomer.api_key '{"payload":"local demo"}' 201
        $null = Invoke-QGHTTP POST "$qgExample/jobs" $qgCustomer.api_key '{"payload":"local demo"}' 429
    }
    $qgAfter = Invoke-QGHTTP GET "$qgExample/demo/stats" '' '' 200
    if ($qgAfter.handler_calls -ne $qgBefore.handler_calls + 2) { throw 'A rate/daily denied request ran the handler' }
    foreach ($qgIndex in 0..3) {
        $qgUsage = Invoke-QGHTTP GET "$($qgOrigins[0])/v1/usage?operation=job.create" $qgCustomers[$qgIndex].api_key '' 200
        $qgExpectedUsage = if ($qgIndex -eq 0) { 3 } else { 1 }
        if ($qgUsage.daily_used -ne $qgExpectedUsage) { throw 'Tenant usage mismatch' }
    }
    $qgIDList = ($qgIDs | ForEach-Object { "'$_'" }) -join ','
    $qgCounts = Invoke-QGCompose -Arguments @('exec', '-T', 'db', 'psql', '-U', 'quotagate', '-d', 'quotagate', '-At', '-c', "SELECT (SELECT sum(used) FROM quotagate.daily_usage WHERE customer_id IN ($qgIDList)), count(*), count(*) FILTER(WHERE allowed), count(*) FILTER(WHERE reason='daily_quota'), count(*) FILTER(WHERE reason='rate_limited') FROM quotagate.decisions WHERE customer_id IN ($qgIDList)")
    if ($qgCounts -ne '6|67|6|6|55') { throw "PostgreSQL reconciliation mismatch: $qgCounts" }
    $qgLogs = (Invoke-QGCompose -Arguments @('logs', '--no-color', 'api', 'api-2', 'example-api')) -join "`n"
    foreach ($qgSecret in @($qgSettings.QG_ADMIN_TOKEN) + @($qgCustomers.api_key)) {
        if ($qgLogs.Contains($qgSecret)) { throw 'Credential found in application logs' }
    }
    Write-Output 'PASS middleware rate/daily denials=429; handler increment=2; daily/decisions/allowed/daily_denied/rate_denied=6|67|6|6|55; secrets absent from logs.'
} finally {
    try {
        if ($qgFaultConstraint) {
            Invoke-QGCompose -Arguments @('exec', '-T', 'db', 'psql', '-v', 'ON_ERROR_STOP=1', '-U', 'quotagate', '-d', 'quotagate', '-c', "ALTER TABLE quotagate.decisions DROP CONSTRAINT IF EXISTS $qgFaultConstraint") | Out-Null
        }
        foreach ($qgID in $qgIDs) {
            $qgKeys = @(Invoke-QGCompose -Arguments @('exec', '-T', 'redis', 'redis-cli', '--scan', '--pattern', ('qg:rate:' + (Get-QGHash $qgID) + ':*')))
            if ($qgKeys.Count -gt 0) { Invoke-QGCompose -Arguments (@('exec', '-T', 'redis', 'redis-cli', 'DEL') + $qgKeys) | Out-Null }
        }
    } finally {
        try {
            if ($qgIDs.Count -gt 0) {
                $qgIDList = ($qgIDs | ForEach-Object { "'$_'" }) -join ','
                Invoke-QGCompose -Arguments @('exec', '-T', 'db', 'psql', '-v', 'ON_ERROR_STOP=1', '-U', 'quotagate', '-d', 'quotagate', '-c', "DELETE FROM quotagate.customers WHERE id IN ($qgIDList)") | Out-Null
                Write-Output 'PASS cleanup: generated QG-06 customers and Redis keys removed.'
            }
        } finally { Pop-Location }
    }
}
