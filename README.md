# RMS-Gate

[English](README.md) | [中文](README_CN.md)

A customized [Gate](https://github.com/minekube/gate) Minecraft proxy server for RMS Server, featuring remote whitelist validation and intelligent load balancing.

## Features

### 🔐 Remote Whitelist Validation

Instead of local whitelist files, RMS-Gate validates players against a remote HTTP API:

- **Real-time validation** - Every login request is verified against the central API
- **UUID + Username check** - Both identifiers are sent for validation
- **Graceful error handling** - Configurable messages for denied access and server errors

```
Player Login → Gate Proxy → HTTP POST to API → Allow/Deny
```

### ⚖️ Intelligent Load Balancing

Distribute players across multiple backend servers with health-aware routing:

**Strategies:**
- `health-score` - Weighted scoring based on latency, jitter, connections, and historical data (recommended)
- `round-robin` - Simple rotation
- `least-connections` - Route to server with fewest players
- `sequential` - Always try first available server
- `random` - Random selection

**Health Monitoring:**
- Minecraft protocol ping (not just TCP) for accurate server status
- Sliding window latency tracking with jitter calculation
- Automatic unhealthy/healthy state transitions
- Trust coefficient for gradual recovery after failures
- Historical performance tracking with EMA (Exponential Moving Average)

**Per-Backend Features:**
- Connection limits
- Manual enable/disable via commands
- Real-time player tracking

### 🚀 Dynamic Server Management

Auto-start servers on demand via MCSManager API:

- Start servers when players connect
- Auto-shutdown after idle timeout
- Protection periods to prevent premature shutdown
- Per-server auto-shutdown toggle

### 🛡️ Permission Management

Control command access based on remote permission levels:

- Cached permission lookups
- Configurable admin commands list
- Integration with external permission API

## Installation

```bash
# Clone
git clone https://github.com/RMS-Server/RMS-Gate.git
cd RMS-Gate

# Build
go build -o rms-gate .

# Run
./rms-gate
```

## Configuration

Configuration is stored in `plugins/rms_whitelist/config.json`:

```json
{
  "apiUrl": "https://your-api.example.com",
  "timeoutSeconds": 10,
  "msgNotInWhitelist": "You are not whitelisted",
  "msgServerError": "Server error, please contact admin",

  "loadBalancer": {
    "enabled": true,
    "healthCheck": {
      "intervalSeconds": 5,
      "windowSize": 20,
      "unhealthyAfterFailures": 3,
      "healthyAfterSuccesses": 3,
      "jitterThreshold": 0.5,
      "dialTimeoutSeconds": 5
    },
    "servers": {
      "survival": {
        "strategy": "health-score",
        "backends": [
          { "addr": "192.168.1.10:25565", "maxConnections": 50 },
          { "addr": "192.168.1.11:25565", "maxConnections": 50 }
        ]
      }
    }
  },

  "mcsManager": {
    "baseUrl": "https://mcsm.example.com/api",
    "apiKey": "your-api-key",
    "daemonId": "your-daemon-id"
  },

  "dynamicServer": {
    "serverUuidMap": {
      "creative": "mcsm-instance-uuid"
    },
    "autoStartServers": ["creative"],
    "startupTimeoutSeconds": 60,
    "idleShutdownSeconds": 300
  },

  "permission": {
    "enabled": true,
    "cacheTtlSeconds": 300,
    "adminCommands": ["send", "glist", "server", "lb"]
  }
}
```

## Commands

### Load Balancer
- `/lb status` - Show all load balanced servers
- `/lb status <server>` - Show detailed backend status
- `/lb disable <server> <backend>` - Disable a backend
- `/lb enable <server> <backend>` - Enable a backend

### Dynamic Server
- `/dserver delay <server> <time>` - Set protection period (e.g., `5m`, `2h`)
- `/dserver delay <server> off` - Clear protection period
- `/dserver autoshutdown <server> <on|off>` - Toggle auto-shutdown

## Project Structure

```
RMS-Gate/
├── main.go                          # Plugin entry, commands
├── internal/
│   ├── config/                      # Configuration management
│   ├── minecraft/                   # MC protocol utilities
│   ├── whitelist/                   # Whitelist API client
│   ├── permission/                  # Permission management
│   ├── mcsmanager/                  # MCSManager API client
│   ├── dynamicserver/               # Server lifecycle management
│   └── loadbalancer/                # Load balancing system
│       ├── backend.go               # Backend state & metrics
│       ├── strategy.go              # LB strategies
│       ├── history.go               # Historical tracking
│       ├── server_info.go           # Gate ServerInfo impl
│       └── loadbalancer.go          # Main orchestrator
└── go.mod
```

## API Requirements

### Whitelist API

```
POST /api/whitelist
Content-Type: application/json

{
  "username": "PlayerName",
  "uuid": "player-uuid-string"
}

Response:
- 200 OK: Player allowed
- 403 Forbidden: Not whitelisted
- 5xx: Server error
```

### Permission API

```
GET /api/mcdr/permission

Response:
{
  "success": true,
  "users": [
    { "username": "Admin", "permission_level": 4 }
  ]
}
```

## License

MIT License - See [LICENSE](LICENSE) for details.

## Credits

- [Gate](https://github.com/minekube/gate) - The underlying Minecraft proxy
- [MCSManager](https://github.com/MCSManager/MCSManager) - Server management panel integration
