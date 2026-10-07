# PowerShell 7; real outages in the already-started, isolated QG-08 Compose project.
param([string]$EnvFile = '.env.compose', [string]$ProjectName = 'quotagate-qg08')
$ErrorActionPreference = 'Stop'
Push-Location -LiteralPath (Split-Path -Parent $PSScriptRoot)
$qgIDs = [System.Collections.Generic.List[string]]::new()
$qgRedisStopped = $false
$qgDBStopped = $false

function Invoke-QGCompose {
    param([string[]]$Arguments)
    & docker compose -p $ProjectName --env-file $EnvFile -f compose.yaml -f compose.qg08.yaml @Arguments
    if ($LASTEXITCODE -ne 0) { throw 'QG-08 Compose command failed' }
}
function Invoke-QGHTTP {
    param([string]$Method,[string]$URL,[string]$Token,[string]$Body,[int]$Expected)
    $qgRequest = @{ Method=$Method; Uri=$URL; SkipHttpErrorCheck=$true; TimeoutSec=10 }
    if ($Token) { $qgRequest.Headers = @{ Authorization="Bearer $Token" } }
    if ($Body) { $qgRequest.Body=$Body; $qgRequest.ContentType='application/json' }
    $qgResponse = Invoke-WebRequest @qgRequest
    if ([int]$qgResponse.StatusCode -ne $Expected) { throw "$Method returned $($qgResponse.StatusCode), expected $Expected" }
    return ($qgResponse.Content | ConvertFrom-Json)
}
function Invoke-QGDecision {
    param([int]$Copy,[string]$Key,[string]$ID,[int]$Expected=200)
    return Invoke-QGHTTP POST "$($qgOrigins[$Copy])/v1/decisions" $Key (@{ decision_id=$ID; operation='job.create' } | ConvertTo-Json -Compress) $Expected
}
function Assert-QGHealth {
    param([int]$ReadyStatus)
    foreach ($qgOrigin in $qgOrigins) {
        foreach ($qgProbe in @(@{ Path='live'; Status=200 },@{ Path='ready'; Status=$ReadyStatus })) {
            $qgResponse = Invoke-WebRequest -Uri "$qgOrigin/health/$($qgProbe.Path)" -SkipHttpErrorCheck -TimeoutSec 3
            if ([int]$qgResponse.StatusCode -ne $qgProbe.Status) { throw "Health $($qgProbe.Path) returned $($qgResponse.StatusCode), expected $($qgProbe.Status)" }
        }
    }
}
function Wait-QGReady {
    $qgDeadline = [DateTimeOffset]::UtcNow.AddSeconds(20)
    do {
        $qgReady = $true
        foreach ($qgOrigin in $qgOrigins) {
            try {
                $qgResponse = Invoke-WebRequest -Uri "$qgOrigin/health/ready" -SkipHttpErrorCheck -TimeoutSec 3
                if ([int]$qgResponse.StatusCode -ne 200) { $qgReady = $false }
            } catch { $qgReady = $false }
        }
        if ($qgReady) { return }
        Start-Sleep -Milliseconds 250
    } while ([DateTimeOffset]::UtcNow -lt $qgDeadline)
    throw 'QG-08 readiness did not recover on both copies'
}
function Get-QGCalls {
    return (Invoke-QGHTTP GET "$qgExample/demo/stats" '' '' 200).handler_calls
}
function Invoke-QGSQL {
    param([string]$SQL)
    return Invoke-QGCompose -Arguments @('exec','-T','db','psql','-v','ON_ERROR_STOP=1','-U','quotagate','-d','quotagate','-At','-c',$SQL)
}
function Get-QGCounts {
    $qgIDList = ($qgIDs | ForEach-Object { "'$_'" }) -join ','
    return Invoke-QGSQL "SELECT (SELECT COALESCE(sum(used),0) FROM quotagate.daily_usage WHERE customer_id IN ($qgIDList)),count(*),count(*) FILTER(WHERE allowed),count(*) FILTER(WHERE reason='daily_quota'),count(*) FILTER(WHERE reason='rate_limited'),count(DISTINCT decision_id) FROM quotagate.decisions WHERE customer_id IN ($qgIDList)"
}
function Get-QGHash {
    param([string]$Value)
    return [Convert]::ToHexString([Security.Cryptography.SHA256]::HashData([Text.Encoding]::UTF8.GetBytes($Value))).ToLowerInvariant()
}
function Get-QGRateUsed {
    param([string]$CustomerID)
    $qgBucket = 'qg:rate:' + (Get-QGHash $CustomerID) + ':bucket:' + (Get-QGHash 'job.create')
    return Invoke-QGCompose -Arguments @('exec','-T','redis','redis-cli','--raw','HGET',$qgBucket,'used')
}
function Get-QGRedisDecisions {
    param([string]$CustomerID)
    $qgPattern = 'qg:rate:' + (Get-QGHash $CustomerID) + ':decision:*'
    return @(Invoke-QGCompose -Arguments @('exec','-T','redis','redis-cli','--scan','--pattern',$qgPattern)).Count
}
function Wait-QGWindow {
    $qgTime = @(Invoke-QGCompose -Arguments @('exec','-T','redis','redis-cli','--raw','TIME'))
    $qgNowMS = [int64]$qgTime[0]*1000 + [int64][Math]::Floor(([int64]$qgTime[1])/1000)
    $qgRemaining = 60000 - $qgNowMS % 60000
    if ($qgRemaining -lt 15000) { Start-Sleep -Milliseconds ($qgRemaining+50) }
}
function Get-QGAppStarts {
    $qgContainers = @(Invoke-QGCompose -Arguments @('ps','-q','api','api-2','example-api'))
    if ($qgContainers.Count -ne 3) { throw 'Expected two API copies and one example API' }
    $qgStarts = & docker inspect --format '{{.Id}}|{{.State.StartedAt}}|{{.RestartCount}}' @qgContainers
    if ($LASTEXITCODE -ne 0) { throw 'Application process check failed' }
    return (@($qgStarts | Sort-Object) -join "`n")
}
function Invoke-QGConcurrent {
    param([string]$Key,[int]$Count,[string]$Prefix)
    $qgOriginsForCall = $qgOrigins
    return @(0..($Count-1) | ForEach-Object -ThrottleLimit 20 -Parallel {
        $qgOrigin = ($using:qgOriginsForCall)[$_ % 2]
        $qgID = "$($using:Prefix)_$_"
        $qgResponse = Invoke-WebRequest -Method POST -Uri "$qgOrigin/v1/decisions" -Headers @{ Authorization="Bearer $($using:Key)" } -Body (@{ decision_id=$qgID; operation='job.create' } | ConvertTo-Json -Compress) -ContentType 'application/json' -SkipHttpErrorCheck -TimeoutSec 10
        if ([int]$qgResponse.StatusCode -ne 200) { throw 'Concurrent final decision did not return 200' }
        $qgResult = $qgResponse.Content | ConvertFrom-Json
        [pscustomobject]@{ allowed=$qgResult.allowed; reason=$qgResult.reason; utc_day=$qgResult.utc_day; daily_used=$qgResult.daily_used; daily_limit=$qgResult.daily_limit; policy_version=$qgResult.policy_version; decision_id=$qgID; origin=$qgOrigin }
    })
}

try {
    $qgSettings = @{}
    foreach ($qgLine in Get-Content -LiteralPath $EnvFile) {
        if ($qgLine -match '^([A-Z_0-9]+)=(.*)$') { $qgSettings[$Matches[1]]=$Matches[2] }
    }
    if (-not $qgSettings.QG_ADMIN_TOKEN) { throw 'QG_ADMIN_TOKEN is required' }
    $qgPort = if ($qgSettings.QG_HTTP_PORT) { $qgSettings.QG_HTTP_PORT } else { '18080' }
    $qgPort2 = if ($qgSettings.QG_HTTP_2_PORT) { $qgSettings.QG_HTTP_2_PORT } else { '18082' }
    $qgExamplePort = if ($qgSettings.QG_EXAMPLE_PORT) { $qgSettings.QG_EXAMPLE_PORT } else { '18081' }
    $qgOrigins = @("http://127.0.0.1:$qgPort","http://127.0.0.1:$qgPort2")
    $qgExample = "http://127.0.0.1:$qgExamplePort"
    Assert-QGHealth 200
    $qgAppStarts = Get-QGAppStarts
    # Redis restart deliberately loses this isolated demo's ephemeral data.
    $qgEmpty = Invoke-QGSQL 'SELECT (SELECT count(*) FROM quotagate.customers),(SELECT count(*) FROM quotagate.daily_usage),(SELECT count(*) FROM quotagate.decisions)'
    $qgEmptyRedis = Invoke-QGCompose -Arguments @('exec','-T','redis','redis-cli','-n','0','DBSIZE')
    if ($qgEmpty -ne '0|0|0' -or $qgEmptyRedis -ne '0') { throw 'Run QG-08 only in an empty isolated demo project' }
    $qgBaseline = Get-QGCalls
    $qgRun = [Guid]::NewGuid().ToString('N')
    $qgCustomers = @()
    foreach ($qgItem in @(@{ Name='QG-08 shared rate'; Daily=1000; Rate=7 },@{ Name='QG-08 shared daily'; Daily=3; Rate=1000 },@{ Name='QG-08 recovery'; Daily=4; Rate=1000 })) {
        $qgCreated = Invoke-QGHTTP POST "$($qgOrigins[0])/v1/admin/customers" $qgSettings.QG_ADMIN_TOKEN (@{ name=$qgItem.Name } | ConvertTo-Json -Compress) 201
        if ($qgCreated.id -notmatch '^cus_[a-f0-9]{32}$') { throw 'Unexpected generated customer ID' }
        $qgIDs.Add($qgCreated.id)
        $null = Invoke-QGHTTP PUT "$($qgOrigins[0])/v1/admin/customers/$($qgCreated.id)/policies/job.create" $qgSettings.QG_ADMIN_TOKEN (@{ daily_limit=$qgItem.Daily; rate_limit_per_minute=$qgItem.Rate } | ConvertTo-Json -Compress) 200
        $qgCustomers += $qgCreated
    }
    Wait-QGWindow
    $qgRateResults = Invoke-QGConcurrent $qgCustomers[0].api_key 60 ($qgRun+'_rate')
    if ($qgRateResults.Count -ne 60 -or @($qgRateResults | Where-Object { $_.allowed -and $_.reason -eq 'allowed' }).Count -ne 7 -or
        @($qgRateResults | Where-Object { -not $_.allowed -and $_.reason -eq 'rate_limited' }).Count -ne 53 -or
        @($qgRateResults.origin | Select-Object -Unique).Count -ne 2 -or (Get-QGRateUsed $qgCustomers[0].id) -ne '7') { throw 'Shared rate limit mismatch or window boundary crossed' }
    $qgSaved = $qgRateResults | Where-Object allowed | Select-Object -First 1
    Wait-QGWindow
    $qgDailyResults = Invoke-QGConcurrent $qgCustomers[1].api_key 40 ($qgRun+'_daily')
    if ($qgDailyResults.Count -ne 40 -or @($qgDailyResults | Where-Object { $_.allowed -and $_.reason -eq 'allowed' }).Count -ne 3 -or
        @($qgDailyResults | Where-Object { -not $_.allowed -and $_.reason -eq 'daily_quota' }).Count -ne 37 -or
        @($qgDailyResults.origin | Select-Object -Unique).Count -ne 2) { throw 'Shared daily limit mismatch' }
    $null = Invoke-QGHTTP POST "$qgExample/jobs" $qgCustomers[2].api_key '' 201
    if ((Get-QGCalls) -ne $qgBaseline+1 -or (Get-QGCounts) -ne '11|101|11|37|53|101') { throw 'Initial handler/durable counts mismatch' }
    Write-Output 'PASS two copies: rate customer 60=7 allowed/53 rate_limited; daily customer 40=3 allowed/37 daily_quota; handler baseline +1.'

    $qgBeforeRedis = Get-QGCounts
    $qgRedisStopped = $true
    Invoke-QGCompose -Arguments @('stop','redis') | Out-Null
    Assert-QGHealth 503
    foreach ($qgCopy in 0..1) { $null = Invoke-QGDecision $qgCopy $qgCustomers[2].api_key ($qgRun+"_redis_down_$qgCopy") 503 }
    $null = Invoke-QGHTTP POST "$qgExample/jobs" $qgCustomers[2].api_key '' 503
    foreach ($qgCopy in 0..1) {
        $qgReplay = Invoke-QGDecision $qgCopy $qgCustomers[0].api_key $qgSaved.decision_id
        foreach ($qgField in @('allowed','reason','utc_day','daily_used','daily_limit','policy_version')) {
            if ($qgReplay.$qgField -ne $qgSaved.$qgField) { throw 'Redis outage changed a durable replay' }
        }
    }
    if ((Get-QGCalls) -ne $qgBaseline+1 -or (Get-QGCounts) -ne $qgBeforeRedis) { throw 'Redis outage ran handler or changed durable usage/decisions' }
    Invoke-QGCompose -Arguments @('up','-d','--no-deps','--wait','redis') | Out-Null
    $qgRedisStopped = $false
    Wait-QGReady
    foreach ($qgCopy in 0..1) { $null = Invoke-QGDecision $qgCopy $qgCustomers[0].api_key $qgSaved.decision_id }
    $qgRedisAfterRestart = Invoke-QGCompose -Arguments @('exec','-T','redis','redis-cli','-n','0','DBSIZE')
    if ($qgRedisAfterRestart -ne '0' -or (Get-QGCounts) -ne $qgBeforeRedis) { throw 'Redis restart/replay changed durable usage or recharged rate capacity' }
    $null = Invoke-QGHTTP POST "$qgExample/jobs" $qgCustomers[2].api_key '' 201
    if ((Get-QGCalls) -ne $qgBaseline+2 -or (Get-QGCounts) -ne '12|102|12|37|53|102') { throw 'Redis recovery did not permit a fresh request' }
    Write-Output 'PASS Redis stop: live=200/ready=503 on both copies; new decisions and example=503; handler/durable counts unchanged; saved replay=200. Restart loses ephemeral Redis data; saved replay does not recharge; fresh example=201.'

    $qgBeforeDB = Get-QGCounts
    $qgBeforeDBRedis = Get-QGRedisDecisions $qgCustomers[2].id
    if ($qgBeforeDBRedis -ne 1) { throw 'Unexpected recovery Redis pre-result count' }
    $qgDBStopped = $true
    Invoke-QGCompose -Arguments @('stop','db') | Out-Null
    Assert-QGHealth 503
    foreach ($qgCopy in 0..1) { $null = Invoke-QGDecision $qgCopy $qgCustomers[2].api_key ($qgRun+"_db_down_$qgCopy") 503 }
    $null = Invoke-QGHTTP POST "$qgExample/jobs" $qgCustomers[2].api_key '' 503
    $null = Invoke-QGDecision 1 $qgCustomers[0].api_key $qgSaved.decision_id 503
    if ((Get-QGCalls) -ne $qgBaseline+2 -or (Get-QGRedisDecisions $qgCustomers[2].id) -ne $qgBeforeDBRedis) { throw 'PostgreSQL outage ran handler or created Redis pre-results' }
    Invoke-QGCompose -Arguments @('up','-d','--no-deps','--wait','db') | Out-Null
    $qgDBStopped = $false
    Wait-QGReady
    if ((Get-QGCounts) -ne $qgBeforeDB) { throw 'PostgreSQL outage changed persisted usage/decisions' }
    $null = Invoke-QGDecision 1 $qgCustomers[0].api_key $qgSaved.decision_id
    $null = Invoke-QGHTTP POST "$qgExample/jobs" $qgCustomers[2].api_key '' 201
    $qgRecoveredCopy = Invoke-QGDecision 1 $qgCustomers[2].api_key ($qgRun+'_copy_two_recovery')
    if (-not $qgRecoveredCopy.allowed -or $qgRecoveredCopy.daily_used -ne 4) { throw 'Second copy did not recover with shared daily usage' }
    $null = Invoke-QGHTTP POST "$qgExample/jobs" $qgCustomers[2].api_key '' 429
    if ((Get-QGCalls) -ne $qgBaseline+3 -or (Get-QGCounts) -ne '14|105|14|38|53|105' -or (Get-QGRedisDecisions $qgCustomers[2].id) -ne 4) { throw 'Recovery handler/usage/decision reconciliation failed' }
    foreach ($qgIndex in 0..2) {
        $qgUsage = Invoke-QGHTTP GET "$($qgOrigins[1])/v1/usage?operation=job.create" $qgCustomers[$qgIndex].api_key '' 200
        $qgExpectedUsage = @(7,3,4)[$qgIndex]
        if ($qgUsage.daily_used -ne $qgExpectedUsage) { throw 'Tenant daily usage mismatch after outages' }
    }
    Assert-QGHealth 200
    if ((Get-QGAppStarts) -ne $qgAppStarts) { throw 'Application containers restarted instead of reconnecting' }
    $qgLogs = (Invoke-QGCompose -Arguments @('logs','--no-color','api','api-2','example-api')) -join "`n"
    foreach ($qgSecret in @($qgSettings.QG_ADMIN_TOKEN,$qgSettings.QG_DB_PASSWORD)+@($qgCustomers.api_key)) {
        if ($qgSecret -and $qgLogs.Contains($qgSecret)) { throw 'Credential found in application logs' }
    }
    Write-Output 'PASS PostgreSQL stop: live=200/ready=503; new/replay/example=503; handler unchanged; no new Redis pre-results. Restart preserves durable counts; both copies recover without application restarts.'
    Write-Output 'PASS final daily/decisions/allowed/daily_denied/rate_denied/distinct_ids=14|105|14|38|53|105; tenant daily usage=7/3/4; handler increment=3; final denial=429; secrets absent from logs.'
} finally {
    try {
        try {
            if ($qgDBStopped) { Invoke-QGCompose -Arguments @('up','-d','--no-deps','--wait','db') | Out-Null }
        } finally {
            if ($qgRedisStopped) { Invoke-QGCompose -Arguments @('up','-d','--no-deps','--wait','redis') | Out-Null }
        }
    } finally {
        try {
            try {
                foreach ($qgID in $qgIDs) {
                    $qgKeys = @(Invoke-QGCompose -Arguments @('exec','-T','redis','redis-cli','--scan','--pattern',('qg:rate:'+(Get-QGHash $qgID)+':*')))
                    if ($qgKeys.Count -gt 0) { Invoke-QGCompose -Arguments (@('exec','-T','redis','redis-cli','DEL')+$qgKeys) | Out-Null }
                }
            } finally {
                if ($qgIDs.Count -gt 0) {
                    $qgIDList = ($qgIDs | ForEach-Object { "'$_'" }) -join ','
                    Invoke-QGSQL "DELETE FROM quotagate.customers WHERE id IN ($qgIDList)" | Out-Null
                    if ((Get-QGCounts) -ne '0|0|0|0|0|0') { throw 'Smoke customer cleanup failed' }
                    Write-Output 'PASS cleanup: only generated QG-08 customers and their Redis fields removed; stopped dependencies restored.'
                }
            }
        } finally { Pop-Location }
    }
}
