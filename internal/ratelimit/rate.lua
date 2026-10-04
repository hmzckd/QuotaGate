-- KEYS[1]: customer/operation bucket; KEYS[2]: customer/decision pre-result.
-- ARGV[1]: operation; ARGV[2]: limit. A replay keeps its first limit and window.
local saved = redis.call('GET', KEYS[2])
if saved then
    local result = cjson.decode(saved)
    if result.operation ~= ARGV[1] then
        return redis.error_reply('QG_DECISION_CONFLICT')
    end
    return {result.allowed, result.used, result.limit, result.start_ms, result.end_ms}
end

local now = redis.call('TIME')
local now_ms = tonumber(now[1]) * 1000 + math.floor(tonumber(now[2]) / 1000)
local start_ms = math.floor(now_ms / 60000) * 60000
local end_ms = start_ms + 60000
local limit = tonumber(ARGV[2])
local used = 0
local stored_start = tonumber(redis.call('HGET', KEYS[1], 'start_ms'))
if stored_start == start_ms then
    used = tonumber(redis.call('HGET', KEYS[1], 'used')) or 0
end
local allowed = 0
if used < limit then
    used = used + 1
    allowed = 1
end
redis.call('HSET', KEYS[1], 'start_ms', start_ms, 'used', used)
redis.call('PEXPIREAT', KEYS[1], end_ms)
redis.call('SET', KEYS[2], cjson.encode({
    operation = ARGV[1], allowed = allowed, used = used, limit = limit,
    start_ms = start_ms, end_ms = end_ms
}), 'PX', 86400000)
return {allowed, used, limit, start_ms, end_ms}
