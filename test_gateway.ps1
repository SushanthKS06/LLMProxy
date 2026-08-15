param (
    [Parameter(Mandatory=$true)]
    [string]$Prompt
)

$uri = "http://localhost:8081/v1/chat/completions"
$headers = @{
    "Content-Type" = "application/json"
    "X-Gateway-API-Key" = "key1"
    "X-Gateway-Team" = "engineering"
}

$body = @{
    messages = @(
        @{
            role = "user"
            content = $Prompt
        }
    )
} | ConvertTo-Json -Depth 10

Write-Host "----------------------------------------" -ForegroundColor Cyan
Write-Host "Sending Prompt: " -NoNewline; Write-Host $Prompt -ForegroundColor White

$res = Invoke-WebRequest -Method Post -Uri $uri -Headers $headers -Body $body -UseBasicParsing

$cacheHeader = $res.Headers["X-Gateway-Cache"]
$modelHeader = $res.Headers["X-Gateway-Model"]
$jsonResponse = $res.Content | ConvertFrom-Json
$answer = $jsonResponse.choices[0].message.content

Write-Host ""
if ($cacheHeader -eq "miss") {
    Write-Host "Status: " -NoNewline; Write-Host "Routed to LLM" -ForegroundColor Yellow
} elseif ($cacheHeader -eq "redis_hit") {
    Write-Host "Status: " -NoNewline; Write-Host "Cache Hit (Redis - Exact Match)" -ForegroundColor Green
} elseif ($cacheHeader -eq "semantic_hit") {
    Write-Host "Status: " -NoNewline; Write-Host "Cache Hit (PostgreSQL - Semantic Match)" -ForegroundColor Green
} else {
    Write-Host "Status: " -NoNewline; Write-Host "Unknown ($cacheHeader)" -ForegroundColor Red
}

Write-Host "Model:  " -NoNewline; Write-Host $modelHeader -ForegroundColor Magenta
Write-Host ""
Write-Host "Answer: " -ForegroundColor Cyan
Write-Host $answer -ForegroundColor White
Write-Host "----------------------------------------" -ForegroundColor Cyan
