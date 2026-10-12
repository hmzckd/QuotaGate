# PowerShell 7; observable decisions and real outages in an isolated QG-09 project.
param([string]$EnvFile='.env.compose',[string]$ProjectName='quotagate-qg09')
$ErrorActionPreference='Stop'
Push-Location -LiteralPath (Split-Path -Parent $PSScriptRoot)
$qgIDs=[System.Collections.Generic.List[string]]::new()
$qgRequests=[System.Collections.Generic.List[object]]::new()
$qgRedisStopped=$false
$qgDBStopped=$false

function Invoke-QGCompose {
    param([string[]]$Arguments)
    & docker compose -p $ProjectName --env-file $EnvFile -f compose.yaml -f compose.qg08.yaml @Arguments
    if ($LASTEXITCODE -ne 0) { throw 'QG-09 Compose command failed' }
}
function Invoke-QGSQL {
    param([string]$SQL)
    return Invoke-QGCompose @('exec','-T','db','psql','-v','ON_ERROR_STOP=1','-U','quotagate','-d','quotagate','-At','-c',$SQL)
}
function Get-QGCounts {
    $qgList=($qgIDs | ForEach-Object { "'$_'" }) -join ','
    return Invoke-QGSQL "SELECT (SELECT COALESCE(sum(used),0) FROM quotagate.daily_usage WHERE customer_id IN ($qgList)),count(*),count(*) FILTER(WHERE allowed),count(*) FILTER(WHERE reason='daily_quota'),count(*) FILTER(WHERE reason='rate_limited') FROM quotagate.decisions WHERE customer_id IN ($qgList)"
}
function Invoke-QGHTTP {
    param([string]$Method,[string]$URL,[string]$Token,[string]$Body,[int]$Expected)
    $qgArgs=@{ Method=$Method; Uri=$URL; SkipHttpErrorCheck=$true; TimeoutSec=10; Headers=@{ 'X-Request-ID'=$qgSettings.QG_ADMIN_TOKEN } }
    if ($Token) { $qgArgs.Headers.Authorization="Bearer $Token" }
    if ($Body) { $qgArgs.Body=$Body; $qgArgs.ContentType='application/json' }
    $qgResponse=Invoke-WebRequest @qgArgs
    if ([int]$qgResponse.StatusCode -ne $Expected) { throw "$Method returned $($qgResponse.StatusCode), expected $Expected" }
    $qgCorrelation=[string]$qgResponse.Headers['X-Request-ID'][0]
    if ($qgCorrelation -notmatch '^[a-f0-9]{32}$' -or $qgCorrelation -eq $qgSettings.QG_ADMIN_TOKEN) { throw 'Missing/generated correlation ID mismatch' }
    $qgContent=if ([string]$qgResponse.Headers['Content-Type'] -match 'application/json') { $qgResponse.Content | ConvertFrom-Json } else { $qgResponse.Content }
    return [pscustomobject]@{ Body=$qgContent; Correlation=$qgCorrelation }
}
function Invoke-QGDecision {
    param([int]$Copy,[string]$Key,[string]$ID,[string]$Outcome,[string]$Dependency='')
    $qgStatus=if ($Outcome -eq 'unavailable') { 503 } else { 200 }
    $qgResponse=Invoke-QGHTTP POST "$($qgOrigins[$Copy])/v1/decisions" $Key (@{ decision_id=$ID; operation='job.create' } | ConvertTo-Json -Compress) $qgStatus
    if ($qgStatus -eq 200 -and ($qgResponse.Body.reason -ne $Outcome -or $qgResponse.Body.allowed -ne ($Outcome -eq 'allowed'))) { throw 'Decision outcome mismatch' }
    $qgRequests.Add([pscustomobject]@{ Copy=$Copy; ID=$ID; Correlation=$qgResponse.Correlation; Outcome=$Outcome; Status=$qgStatus; Dependency=$Dependency })
}
function Get-QGMetrics {
    param([int]$Copy)
    return (Invoke-QGHTTP GET "$($qgOrigins[$Copy])/metrics" '' '' 200).Body
}
function Get-QGMetric {
    param([string]$Text,[string]$Name,[hashtable]$Labels=@{})
    $qgSum=0.0
    foreach ($qgLine in ($Text -split "`n")) {
        if ($qgLine -notmatch ('^'+[regex]::Escape($Name)+'(?:\{([^}]*)\})? ([-+0-9.eE]+)$')) { continue }
        $qgValue=[double]::Parse($Matches[2],[Globalization.CultureInfo]::InvariantCulture)
        $qgActual=@{}
        foreach ($qgLabel in [regex]::Matches([string]$Matches[1],'([a-z_]+)="([^"]*)"')) { $qgActual[$qgLabel.Groups[1].Value]=$qgLabel.Groups[2].Value }
        $qgMatch=$true
        foreach ($qgLabel in $Labels.Keys) { if ($qgActual[$qgLabel] -ne $Labels[$qgLabel]) { $qgMatch=$false } }
        if ($qgMatch) { $qgSum+=$qgValue }
    }
    return $qgSum
}
function Assert-QGReadiness {
    param([int]$Postgres,[int]$Redis)
    $qgExpected=if ($Postgres -eq 1 -and $Redis -eq 1) { 200 } else { 503 }
    foreach ($qgCopy in 0..1) {
        $null=Invoke-QGHTTP GET "$($qgOrigins[$qgCopy])/health/live" '' '' 200
        $null=Invoke-QGHTTP GET "$($qgOrigins[$qgCopy])/health/ready" '' '' $qgExpected
        $qgText=Get-QGMetrics $qgCopy
        if ((Get-QGMetric $qgText qg_dependency_ready @{ dependency='postgres' }) -ne $Postgres -or
            (Get-QGMetric $qgText qg_dependency_ready @{ dependency='redis' }) -ne $Redis -or
            (Get-QGMetric $qgText qg_ready) -ne [int]($qgExpected -eq 200)) { throw 'Readiness dependency gauge mismatch' }
    }
}
function Wait-QGReady {
    $qgDeadline=[DateTimeOffset]::UtcNow.AddSeconds(20)
    do {
        try { Assert-QGReadiness 1 1; return } catch { Start-Sleep -Milliseconds 250 }
    } while ([DateTimeOffset]::UtcNow -lt $qgDeadline)
    throw 'QG-09 readiness did not recover'
}
function Get-QGAppStarts {
    $qgContainers=@(Invoke-QGCompose @('ps','-q','api','api-2'))
    if ($qgContainers.Count -ne 2) { throw 'Expected two API copies' }
    $qgStarts=& docker inspect --format '{{.Id}}|{{.State.StartedAt}}|{{.RestartCount}}' @qgContainers
    if ($LASTEXITCODE -ne 0) { throw 'API process check failed' }
    return (@($qgStarts | Sort-Object) -join "`n")
}
function Get-QGHash {
    param([string]$Value)
    return [Convert]::ToHexString([Security.Cryptography.SHA256]::HashData([Text.Encoding]::UTF8.GetBytes($Value))).ToLowerInvariant()
}

try {
    $qgSettings=@{}
    foreach ($qgLine in Get-Content -LiteralPath $EnvFile) { if ($qgLine -match '^([A-Z_0-9]+)=(.*)$') { $qgSettings[$Matches[1]]=$Matches[2] } }
    if (-not $qgSettings.QG_ADMIN_TOKEN) { throw 'QG_ADMIN_TOKEN is required' }
    $qgPort=if ($qgSettings.QG_HTTP_PORT) { $qgSettings.QG_HTTP_PORT } else { '18080' }
    $qgPort2=if ($qgSettings.QG_HTTP_2_PORT) { $qgSettings.QG_HTTP_2_PORT } else { '18082' }
    $qgOrigins=@("http://127.0.0.1:$qgPort","http://127.0.0.1:$qgPort2")
    Assert-QGReadiness 1 1
    $qgAppStarts=Get-QGAppStarts
    $qgEmpty=Invoke-QGSQL 'SELECT (SELECT count(*) FROM quotagate.customers),(SELECT count(*) FROM quotagate.daily_usage),(SELECT count(*) FROM quotagate.decisions)'
    $qgRedisEmpty=Invoke-QGCompose @('exec','-T','redis','redis-cli','DBSIZE')
    foreach ($qgCopy in 0..1) { if ((Get-QGMetric (Get-QGMetrics $qgCopy) qg_decision_responses_total) -ne 0) { throw 'Use freshly started QG-09 API containers' } }
    if ($qgEmpty -ne '0|0|0' -or $qgRedisEmpty -ne '0') { throw 'Use an empty isolated QG-09 project' }
    $qgCustomers=@()
    foreach ($qgPolicy in @(@{ Name='QG-09 rate'; Daily=100; Rate=1 },@{ Name='QG-09 daily'; Daily=1; Rate=100 },@{ Name='QG-09 recovery'; Daily=10; Rate=100 })) {
        $qgCreated=(Invoke-QGHTTP POST "$($qgOrigins[0])/v1/admin/customers" $qgSettings.QG_ADMIN_TOKEN (@{ name=$qgPolicy.Name } | ConvertTo-Json -Compress) 201).Body
        if ($qgCreated.id -notmatch '^cus_[a-f0-9]{32}$') { throw 'Unexpected generated customer ID' }
        $qgIDs.Add($qgCreated.id)
        $null=Invoke-QGHTTP PUT "$($qgOrigins[0])/v1/admin/customers/$($qgCreated.id)/policies/job.create" $qgSettings.QG_ADMIN_TOKEN (@{ daily_limit=$qgPolicy.Daily; rate_limit_per_minute=$qgPolicy.Rate } | ConvertTo-Json -Compress) 200
        $qgCustomers+=$qgCreated
    }
    $qgRun=[Guid]::NewGuid().ToString('N')
    # Start the rate pair away from the real Redis minute boundary.
    $qgTime=@(Invoke-QGCompose @('exec','-T','redis','redis-cli','--raw','TIME'))
    $qgRemaining=60000-([int64]$qgTime[0]*1000+[int64][Math]::Floor(([int64]$qgTime[1])/1000))%60000
    if ($qgRemaining -lt 10000) { Start-Sleep -Milliseconds ($qgRemaining+50) }
    Invoke-QGDecision 0 $qgCustomers[0].api_key ($qgRun+'_saved') allowed
    Invoke-QGDecision 1 $qgCustomers[0].api_key ($qgRun+'_saved') allowed
    Invoke-QGDecision 1 $qgCustomers[0].api_key ($qgRun+'_rate') rate_limited
    Invoke-QGDecision 1 $qgCustomers[1].api_key ($qgRun+'_daily_allow') allowed
    Invoke-QGDecision 0 $qgCustomers[1].api_key ($qgRun+'_daily_deny') daily_quota
    Invoke-QGDecision 0 $qgCustomers[2].api_key ($qgRun+'_recovery_before') allowed
    if ((Get-QGCounts) -ne '3|5|3|1|1') { throw 'Initial durable reconciliation mismatch' }
    Write-Output 'PASS allow/rate/daily and replay responses from both copies; durable usage/decisions/allowed/daily/rate=3|5|3|1|1.'

    $qgRedisStopped=$true
    Invoke-QGCompose @('stop','redis') | Out-Null
    Assert-QGReadiness 1 0
    foreach ($qgCopy in 0..1) { Invoke-QGDecision $qgCopy $qgCustomers[2].api_key ($qgRun+"_redis_down_$qgCopy") unavailable redis }
    Invoke-QGDecision 1 $qgCustomers[0].api_key ($qgRun+'_saved') allowed
    foreach ($qgCopy in 0..1) {
        $qgText=Get-QGMetrics $qgCopy
        if ((Get-QGMetric $qgText qg_dependency_errors_total @{ dependency='redis'; operation='check' }) -ne 1 -or
            (Get-QGMetric $qgText qg_dependency_errors_total @{ dependency='redis'; operation='ping' }) -lt 1 -or
            (Get-QGMetric $qgText qg_dependency_errors_total @{ dependency='postgres' }) -ne 0) { throw 'Redis outage attribution mismatch' }
    }
    if ((Get-QGCounts) -ne '3|5|3|1|1') { throw 'Redis outage changed durable counts' }
    Invoke-QGCompose @('up','-d','--no-deps','--wait','redis') | Out-Null
    $qgRedisStopped=$false
    Wait-QGReady
    Invoke-QGDecision 0 $qgCustomers[2].api_key ($qgRun+'_redis_recovery') allowed
    $qgBeforeDB=Get-QGCounts
    Write-Output 'PASS Redis outage: metrics stay available; ready postgres/redis/overall=1/0/0; 503 attributed only to Redis; durable replay allowed; recovery=1/1/1.'

    $qgDBStopped=$true
    Invoke-QGCompose @('stop','db') | Out-Null
    Assert-QGReadiness 0 1
    foreach ($qgCopy in 0..1) { Invoke-QGDecision $qgCopy $qgCustomers[2].api_key ($qgRun+"_postgres_down_$qgCopy") unavailable postgres }
    Invoke-QGDecision 1 $qgCustomers[0].api_key ($qgRun+'_saved') unavailable postgres
    foreach ($qgCopy in 0..1) {
        $qgText=Get-QGMetrics $qgCopy
        $qgAuthErrors=if ($qgCopy -eq 0) { 1 } else { 2 }
        if ((Get-QGMetric $qgText qg_dependency_errors_total @{ dependency='postgres'; operation='authenticate' }) -ne $qgAuthErrors -or
            (Get-QGMetric $qgText qg_dependency_errors_total @{ dependency='postgres'; operation='ping' }) -lt 1 -or
            (Get-QGMetric $qgText qg_dependency_errors_total @{ dependency='redis'; operation='check' }) -ne 1) { throw 'PostgreSQL outage attribution mismatch' }
    }
    Invoke-QGCompose @('up','-d','--no-deps','--wait','db') | Out-Null
    $qgDBStopped=$false
    Wait-QGReady
    if ((Get-QGCounts) -ne $qgBeforeDB) { throw 'PostgreSQL restart changed durable counts' }
    Invoke-QGDecision 1 $qgCustomers[2].api_key ($qgRun+'_postgres_recovery') allowed
    Invoke-QGDecision 0 $qgCustomers[0].api_key ($qgRun+'_saved') allowed
    $null=Invoke-QGHTTP GET "$($qgOrigins[0])/missing/$($qgCustomers[0].id)?token=$($qgCustomers[0].api_key)" '' '' 404
    Assert-QGReadiness 1 1
    if ((Get-QGAppStarts) -ne $qgAppStarts -or (Get-QGCounts) -ne '5|7|5|1|1') { throw 'Final process/durable reconciliation mismatch' }
    $qgAllText=@()
    foreach ($qgCopy in 0..1) {
        $qgText=Get-QGMetrics $qgCopy
        $qgAllText+=$qgText
        $qgOwn=@($qgRequests | Where-Object Copy -eq $qgCopy)
        foreach ($qgOutcome in @('allowed','rate_limited','daily_quota','unavailable')) {
            if ((Get-QGMetric $qgText qg_decision_responses_total @{ outcome=$qgOutcome }) -ne @($qgOwn | Where-Object Outcome -eq $qgOutcome).Count) { throw 'Decision response counter mismatch' }
        }
        if ((Get-QGMetric $qgText qg_http_request_duration_seconds_count @{ route='/v1/decisions'; method='POST' }) -ne $qgOwn.Count -or
            (Get-QGMetric $qgText qg_http_request_duration_seconds_bucket @{ route='/v1/decisions'; method='POST'; le='+Inf' }) -ne $qgOwn.Count -or
            (Get-QGMetric $qgText qg_http_request_duration_seconds_sum @{ route='/v1/decisions'; method='POST' }) -le 0) { throw 'Decision duration histogram mismatch' }
        if ((Get-QGMetric $qgText qg_http_requests_total @{ route='/v1/decisions'; method='POST'; status='503' }) -ne @($qgOwn | Where-Object Status -eq 503).Count) { throw 'HTTP 503 counter mismatch' }
    }
    $qgLogs=(Invoke-QGCompose @('logs','--no-color','--no-log-prefix','api','api-2')) -join "`n"
    $qgEvents=@($qgLogs -split "`n" | Where-Object { $_.StartsWith('{') } | ForEach-Object { $_ | ConvertFrom-Json } | Where-Object msg -eq http_request)
    foreach ($qgRequest in $qgRequests) {
        $qgEvent=@($qgEvents | Where-Object correlation_id -eq $qgRequest.Correlation)
        if ($qgEvent.Count -ne 1 -or $qgEvent[0].decision_outcome -ne $qgRequest.Outcome -or $qgEvent[0].status -ne $qgRequest.Status -or
            $qgEvent[0].route -ne '/v1/decisions' -or $qgEvent[0].postgres_error -ne ($qgRequest.Dependency -eq 'postgres') -or
            $qgEvent[0].redis_error -ne ($qgRequest.Dependency -eq 'redis')) { throw 'JSON log correlation/outcome/dependency mismatch' }
    }
    $qgTelemetry=($qgAllText -join "`n")+"`n"+$qgLogs
    foreach ($qgSecret in @($qgSettings.QG_ADMIN_TOKEN,$qgSettings.QG_DB_PASSWORD)+@($qgCustomers.api_key)+@($qgIDs)+@($qgRequests.ID)) {
        if ($qgSecret -and $qgTelemetry.Contains($qgSecret)) { throw 'Secret/customer/decision ID found in telemetry' }
    }
    Write-Output 'PASS PostgreSQL outage: metrics stay available; ready=0/1/0; 503 attributed to authentication; Redis check failures unchanged; recovery=1/1/1 without API restart.'
    Write-Output 'PASS counters/histogram/logs: 15 decision responses = 8 allowed / 1 rate / 1 daily / 5 unavailable; durable totals=5|7|5|1|1; all 15 generated correlation IDs match JSON logs; secrets and customer/decision IDs absent.'
} finally {
    try {
        try { if ($qgDBStopped) { Invoke-QGCompose @('up','-d','--no-deps','--wait','db') | Out-Null } }
        finally { if ($qgRedisStopped) { Invoke-QGCompose @('up','-d','--no-deps','--wait','redis') | Out-Null } }
    } finally {
        try {
            try {
                foreach ($qgID in $qgIDs) {
                    $qgKeys=@(Invoke-QGCompose @('exec','-T','redis','redis-cli','--scan','--pattern',('qg:rate:'+(Get-QGHash $qgID)+':*')))
                    if ($qgKeys.Count -gt 0) { Invoke-QGCompose (@('exec','-T','redis','redis-cli','DEL')+$qgKeys) | Out-Null }
                }
            } finally {
                if ($qgIDs.Count -gt 0) {
                    $qgList=($qgIDs | ForEach-Object { "'$_'" }) -join ','
                    Invoke-QGSQL "DELETE FROM quotagate.customers WHERE id IN ($qgList)" | Out-Null
                    if ((Get-QGCounts) -ne '0|0|0|0|0') { throw 'QG-09 customer cleanup failed' }
                    Write-Output 'PASS cleanup: generated customers/Redis fields removed; stopped dependencies restored.'
                }
            }
        } finally { Pop-Location }
    }
}
