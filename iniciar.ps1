[CmdletBinding()]
param()

$ErrorActionPreference = "Stop"

# Windows PowerShell 5.1 interpreta los .ps1 sin BOM con la codificación ANSI
# del sistema, lo que parte los acentos ("está" -> "estÃ¡") en los mensajes.
# Este archivo se guarda como UTF-8 con BOM; además forzamos la salida de
# consola a UTF-8 para que los avisos se lean bien aunque se redirijan.
try {
    [Console]::OutputEncoding = [Text.UTF8Encoding]::new($false)
    $OutputEncoding = [Text.UTF8Encoding]::new($false)
} catch {
}

$projectDir = $PSScriptRoot
Set-Location -LiteralPath $projectDir

if (-not (Test-Path -LiteralPath ".env")) {
    if (-not (Test-Path -LiteralPath ".env.example")) {
        throw "No se encuentra .env.example junto al lanzador."
    }
    Copy-Item -LiteralPath ".env.example" -Destination ".env"
    Write-Host "Se ha creado .env a partir de .env.example."
}

function Read-DotEnvValue([string]$Name, [string]$Fallback = "") {
    $match = Get-Content -LiteralPath ".env" | Where-Object {
        $_ -match "^\s*$([regex]::Escape($Name))\s*="
    } | Select-Object -Last 1
    if ($null -eq $match) { return $Fallback }
    return (($match -split "=", 2)[1]).Trim().Trim('"').Trim("'")
}

$appHost = Read-DotEnvValue "APP_HOST" "127.0.0.1"
$appPortText = Read-DotEnvValue "APP_PORT" "8000"
$appPort = 0
if (-not [int]::TryParse($appPortText, [ref]$appPort) -or $appPort -lt 1 -or $appPort -gt 65535) {
    throw "APP_PORT debe ser un puerto válido entre 1 y 65535."
}

$listener = $null
try {
    $address = if ($appHost -eq "localhost") {
        [Net.IPAddress]::Loopback
    } else {
        [Net.IPAddress]::Parse($appHost)
    }
    $listener = [Net.Sockets.TcpListener]::new($address, $appPort)
    $listener.Start()
} catch {
    throw "El puerto $appHost`:$appPort ya está ocupado. Cierra el proceso que lo usa o cambia APP_PORT en .env."
} finally {
    if ($null -ne $listener) { $listener.Stop() }
}

$ollama = Get-Command "ollama" -ErrorAction SilentlyContinue
if ($null -eq $ollama) {
    Write-Warning "No se encuentra el comando ollama. Instálalo y arranca el servicio antes de indexar o consultar."
} else {
    $ollamaBase = Read-DotEnvValue "OLLAMA_BASE_URL" "http://127.0.0.1:11434"
    try {
        Invoke-RestMethod -Uri "$($ollamaBase.TrimEnd('/'))/api/version" -TimeoutSec 2 | Out-Null
    } catch {
        Write-Warning "Ollama está instalado, pero su servicio local no responde en $ollamaBase."
    }
}

foreach ($modelKey in @("OLLAMA_CHAT_MODEL", "OLLAMA_EMBEDDING_MODEL")) {
    if ([string]::IsNullOrWhiteSpace((Read-DotEnvValue $modelKey))) {
        Write-Warning "$modelKey no está configurado en .env. Obsigon no descargará modelos automáticamente."
    }
}

$binary = @("obsigon-server.exe", "obsigon-windows-amd64.exe") |
    Where-Object { Test-Path -LiteralPath $_ } |
    Select-Object -First 1

if ($null -ne $binary) {
    $server = Start-Process -FilePath (Join-Path $projectDir $binary) -NoNewWindow -PassThru
} else {
    $go = Get-Command "go" -ErrorAction SilentlyContinue
    if ($null -eq $go) {
        throw "No se encontró un binario de Obsigon ni el comando go. Copia obsigon-windows-amd64.exe junto al lanzador o instala Go 1.23."
    }
    $server = Start-Process -FilePath $go.Source -ArgumentList @("run", "./cmd/server") -WorkingDirectory $projectDir -NoNewWindow -PassThru
}

$url = "http://$appHost`:$appPort"
try {
    $ready = $false
    # Un `go run` con la caché fría puede tardar bastante más de 10 segundos
    # en compilar aunque el servidor esté sano.
    for ($attempt = 0; $attempt -lt 240; $attempt++) {
        if ($server.HasExited) { throw "El servidor terminó antes de estar listo (código $($server.ExitCode))." }
        try {
            Invoke-WebRequest -UseBasicParsing -Uri "$url/health" -TimeoutSec 1 | Out-Null
            $ready = $true
            break
        } catch {
            Start-Sleep -Milliseconds 250
        }
    }
    if (-not $ready) { throw "El servidor no respondió en $url tras 60 segundos." }
    Start-Process $url
    Write-Host "Obsigon está disponible en $url. Cierra esta ventana para detenerlo."
    Wait-Process -Id $server.Id
} finally {
    if (-not $server.HasExited) { Stop-Process -Id $server.Id }
}
