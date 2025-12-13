# RMS-Gate

[English](README.md) | [中文](README_CN.md)

为 RMS 服务器定制的 [Gate](https://github.com/minekube/gate) Minecraft 代理服务器，支持远程白名单验证和智能负载均衡。

## 核心功能

### 🔐 远程白名单验证

抛弃本地白名单文件，通过远程 HTTP API 验证玩家：

- **实时验证** - 每次登录请求都会向中央 API 验证
- **UUID + 用户名双重检查** - 同时发送两个标识符进行验证
- **优雅的错误处理** - 可配置拒绝访问和服务器错误的提示消息

```
玩家登录 → Gate 代理 → HTTP POST 到 API → 允许/拒绝
```

### ⚖️ 智能负载均衡

将玩家分配到多个后端服务器，支持健康感知路由：

**负载均衡策略：**
- `health-score` - 基于延迟、抖动、连接数和历史数据的加权评分（推荐）
- `round-robin` - 简单轮询
- `least-connections` - 路由到玩家最少的服务器
- `sequential` - 始终尝试第一个可用服务器
- `random` - 随机选择

**健康监控：**
- 使用 Minecraft 协议 ping（不仅仅是 TCP）获取准确的服务器状态
- 滑动窗口延迟跟踪，计算抖动
- 自动健康/不健康状态转换
- 信任系数机制，故障恢复后逐步提升权重
- 使用 EMA（指数移动平均）进行历史性能跟踪

**后端服务器特性：**
- 连接数限制
- 通过命令手动启用/禁用
- 实时玩家跟踪

### 🚀 动态服务器管理

通过 MCSManager API 按需启动服务器：

- 玩家连接时自动启动服务器
- 空闲超时后自动关闭
- 保护期机制，防止过早关闭
- 每个服务器可单独开关自动关闭

### 🛡️ 权限管理

基于远程权限等级控制命令访问：

- 缓存权限查询结果
- 可配置管理员命令列表
- 与外部权限 API 集成

## 安装

```bash
# 克隆
git clone https://github.com/RMS-Server/RMS-Gate.git
cd RMS-Gate

# 构建
go build -o rms-gate .

# 运行
./rms-gate
```

## 配置

配置文件位于 `plugins/rms_whitelist/config.json`：

```json
{
  "apiUrl": "https://your-api.example.com",
  "timeoutSeconds": 10,
  "msgNotInWhitelist": "您不在白名单中",
  "msgServerError": "服务器错误，请联系管理员",

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

## 命令

### 负载均衡
- `/lb status` - 显示所有负载均衡服务器
- `/lb status <服务器>` - 显示详细的后端状态
- `/lb disable <服务器> <后端>` - 禁用某个后端
- `/lb enable <服务器> <后端>` - 启用某个后端

### 动态服务器
- `/dserver delay <服务器> <时间>` - 设置保护期（如 `5m`、`2h`）
- `/dserver delay <服务器> off` - 清除保护期
- `/dserver autoshutdown <服务器> <on|off>` - 开关自动关闭

## 项目结构

```
RMS-Gate/
├── main.go                          # 插件入口、命令处理
├── internal/
│   ├── config/                      # 配置管理
│   ├── minecraft/                   # MC 协议工具
│   ├── whitelist/                   # 白名单 API 客户端
│   ├── permission/                  # 权限管理
│   ├── mcsmanager/                  # MCSManager API 客户端
│   ├── dynamicserver/               # 服务器生命周期管理
│   └── loadbalancer/                # 负载均衡系统
│       ├── backend.go               # 后端状态与指标
│       ├── strategy.go              # 负载均衡策略
│       ├── history.go               # 历史数据跟踪
│       ├── server_info.go           # Gate ServerInfo 实现
│       └── loadbalancer.go          # 主协调器
└── go.mod
```

## API 要求

### 白名单 API

```
POST /api/whitelist
Content-Type: application/json

{
  "username": "玩家名",
  "uuid": "玩家-uuid-字符串"
}

响应：
- 200 OK: 玩家允许进入
- 403 Forbidden: 不在白名单
- 5xx: 服务器错误
```

### 权限 API

```
GET /api/mcdr/permission

响应：
{
  "success": true,
  "users": [
    { "username": "Admin", "permission_level": 4 }
  ]
}
```

## 许可证

MIT License - 详见 [LICENSE](LICENSE)

## 致谢

- [Gate](https://github.com/minekube/gate) - 底层 Minecraft 代理
- [MCSManager](https://github.com/MCSManager/MCSManager) - 服务器管理面板集成
