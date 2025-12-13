package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/go-logr/logr"
	"github.com/robinbraemer/event"
	"go.minekube.com/brigodier"
	"go.minekube.com/common/minecraft/color"
	"go.minekube.com/common/minecraft/component"
	"go.minekube.com/gate/cmd/gate"
	"go.minekube.com/gate/pkg/command"
	"go.minekube.com/gate/pkg/edition/java/proxy"

	"github.com/RMS-Server/RMS-Gate/internal/config"
	"github.com/RMS-Server/RMS-Gate/internal/dynamicserver"
	"github.com/RMS-Server/RMS-Gate/internal/loadbalancer"
	"github.com/RMS-Server/RMS-Gate/internal/mcsmanager"
	"github.com/RMS-Server/RMS-Gate/internal/permission"
	"github.com/RMS-Server/RMS-Gate/internal/whitelist"
)

var pluginCtx context.Context

func main() {
	proxy.Plugins = append(proxy.Plugins, proxy.Plugin{
		Name: "RMSWhitelist",
		Init: func(ctx context.Context, p *proxy.Proxy) error {
			pluginCtx = ctx
			return newRMSWhitelist(ctx, p).init()
		},
	})

	gate.Execute()
}

type RMSWhitelist struct {
	ctx           context.Context
	proxy         *proxy.Proxy
	log           logr.Logger
	config        *config.Config
	checker       *whitelist.Checker
	mcsClient     *mcsmanager.Client
	dynamicServer *dynamicserver.Manager
	permission    *permission.Manager
	loadBalancer  *loadbalancer.LoadBalancer
}

func newRMSWhitelist(ctx context.Context, p *proxy.Proxy) *RMSWhitelist {
	return &RMSWhitelist{
		ctx:   ctx,
		proxy: p,
		log:   logr.FromContextOrDiscard(ctx).WithName("rms-whitelist"),
	}
}

func (r *RMSWhitelist) init() error {
	r.log.Info("Initializing RMS Whitelist Plugin...")

	configDir := filepath.Join(getPluginDataDir(), "rms_whitelist")
	r.config = config.LoadConfig(configDir, r.log)
	r.checker = whitelist.NewChecker(r.log)

	if r.config.MCSManager != nil && r.config.DynamicServer != nil {
		mcsCfg := &mcsmanager.Config{
			BaseURL:  r.config.MCSManager.BaseURL,
			APIKey:   r.config.MCSManager.APIKey,
			DaemonID: r.config.MCSManager.DaemonID,
		}
		r.mcsClient = mcsmanager.NewClient(r.log, mcsCfg)

		dsCfg := &dynamicserver.Config{
			ServerUUIDMap:              r.config.DynamicServer.ServerUUIDMap,
			AutoStartServers:           r.config.DynamicServer.AutoStartServers,
			StartupTimeoutSeconds:      r.config.DynamicServer.StartupTimeoutSeconds,
			PollIntervalSeconds:        r.config.DynamicServer.PollIntervalSeconds,
			ConnectivityTimeoutSeconds: r.config.DynamicServer.ConnectivityTimeoutSeconds,
			IdleShutdownSeconds:        r.config.DynamicServer.IdleShutdownSeconds,
			MsgStarting:                r.config.DynamicServer.MsgStarting,
			MsgStartupTimeout:          r.config.DynamicServer.MsgStartupTimeout,
		}
		r.dynamicServer = dynamicserver.NewManager(r.ctx, r.log, r.proxy, r.mcsClient, dsCfg)
		r.log.Info("Dynamic server management enabled")
	}

	if r.config.Permission != nil && r.config.Permission.Enabled {
		r.permission = permission.NewManager(r.log, r.config.APIUrl, r.config.Permission.CacheTTLSeconds, r.config.Permission.AdminCommands)
		r.log.Info("Permission management enabled", "adminCommands", r.config.Permission.AdminCommands)
	}

	if r.config.LoadBalancer != nil && r.config.LoadBalancer.Enabled {
		lbCfg := convertLoadBalancerConfig(r.config.LoadBalancer)
		lb := loadbalancer.NewLoadBalancer(r.ctx, r.log, r.proxy, lbCfg, configDir)
		if err := lb.Start(); err != nil {
			r.log.Error(err, "Failed to start load balancer")
		} else {
			r.loadBalancer = lb
			r.log.Info("Load balancer enabled")
		}
	}

	event.Subscribe(r.proxy.Event(), 0, r.onLogin)
	event.Subscribe(r.proxy.Event(), -100, r.onServerPreConnect)
	event.Subscribe(r.proxy.Event(), -100, r.onCommandExecute)

	r.registerCommands()

	r.log.Info("RMS Whitelist Plugin initialized successfully")
	return nil
}

func convertLoadBalancerConfig(cfg *config.LoadBalancerConfig) *loadbalancer.Config {
	servers := make(map[string]*loadbalancer.ServerConfig)
	for name, srv := range cfg.Servers {
		backends := make([]*loadbalancer.BackendConfig, len(srv.Backends))
		for i, b := range srv.Backends {
			backends[i] = &loadbalancer.BackendConfig{
				Addr:           b.Addr,
				MaxConnections: b.MaxConnections,
			}
		}
		servers[name] = &loadbalancer.ServerConfig{
			Strategy: srv.Strategy,
			Backends: backends,
		}
	}

	return &loadbalancer.Config{
		Enabled: cfg.Enabled,
		HealthCheck: &loadbalancer.HealthCheckConfig{
			IntervalSeconds:        cfg.HealthCheck.IntervalSeconds,
			WindowSize:             cfg.HealthCheck.WindowSize,
			UnhealthyAfterFailures: cfg.HealthCheck.UnhealthyAfterFailures,
			HealthyAfterSuccesses:  cfg.HealthCheck.HealthyAfterSuccesses,
			JitterThreshold:        cfg.HealthCheck.JitterThreshold,
			DialTimeoutSeconds:     cfg.HealthCheck.DialTimeoutSeconds,
		},
		Servers: servers,
	}
}

func (r *RMSWhitelist) onLogin(e *proxy.LoginEvent) {
	player := e.Player()
	username := player.Username()
	uuid := player.ID().String()

	result := r.checker.Check(r.ctx, username, uuid, r.config.APIUrl, r.config.TimeoutSeconds)

	switch result {
	case whitelist.Allowed:
		r.log.Info("User is whitelisted", "username", username, "uuid", uuid)
	case whitelist.NotInWhitelist:
		r.log.Info("User is not in whitelist", "username", username, "uuid", uuid)
		e.Deny(&component.Text{Content: r.config.MsgNotInWhitelist})
	case whitelist.ServerError:
		r.log.Error(nil, "Whitelist check failed", "username", username, "uuid", uuid)
		e.Deny(&component.Text{Content: r.config.MsgServerError})
	}
}

func (r *RMSWhitelist) onServerPreConnect(e *proxy.ServerPreConnectEvent) {
	r.log.Info("ServerPreConnectEvent triggered")

	if r.dynamicServer == nil {
		r.log.Info("dynamicServer is nil, skipping")
		return
	}

	server := e.Server()
	if server == nil {
		r.log.Info("server is nil, skipping")
		return
	}

	serverName := server.ServerInfo().Name()
	player := e.Player()

	r.log.Info("Checking server", "server", serverName, "player", player.Username())

	if !r.dynamicServer.IsAutoStartServer(serverName) {
		r.log.Info("Server is not in auto-start list, skipping", "server", serverName)
		return
	}

	r.log.Info("Player attempting to connect to auto-start server", "player", player.Username(), "server", serverName)

	instanceUUID := r.config.DynamicServer.ServerUUIDMap[serverName]
	status, err := r.mcsClient.GetInstanceStatus(r.ctx, instanceUUID)
	r.log.Info("Checking instance status via MCSManager", "server", serverName, "status", status, "err", err)

	if status == 3 {
		r.log.Info("Server is already running (MCSManager status=3)", "server", serverName)
		return
	}

	r.log.Info("Server is offline, starting it", "server", serverName, "player", player.Username())

	msg := fmt.Sprintf(r.config.DynamicServer.MsgStarting, serverName)
	player.SendMessage(&component.Text{Content: msg})

	started := r.dynamicServer.EnsureServerRunning(serverName)
	if !started {
		r.log.Error(nil, "Failed to start server", "server", serverName, "player", player.Username())
		timeoutMsg := fmt.Sprintf(r.config.DynamicServer.MsgStartupTimeout, serverName)
		player.SendMessage(&component.Text{Content: timeoutMsg})
		e.Deny()
	} else {
		r.log.Info("Server started successfully", "server", serverName, "player", player.Username())
	}
}

func isServerOnline(addr net.Addr) bool {
	conn, err := net.DialTimeout(addr.Network(), addr.String(), 3*time.Second)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

func (r *RMSWhitelist) onCommandExecute(e *proxy.CommandExecuteEvent) {
	if r.permission == nil {
		return
	}

	player, ok := e.Source().(proxy.Player)
	if !ok {
		return
	}

	cmd := e.Command()
	username := player.Username()

	if !r.permission.CanExecute(r.ctx, username, cmd) {
		e.SetAllowed(false)
		r.log.Info("Command blocked due to insufficient permission", "player", username, "command", cmd)
		player.SendMessage(&component.Text{
			Content: r.config.Permission.MsgNoPermission,
			S:       component.Style{Color: color.Red},
		})
	}
}

func (r *RMSWhitelist) registerCommands() {
	r.proxy.Command().Register(brigodier.Literal("dserver").
		Then(brigodier.Literal("delay").
			Then(brigodier.Argument("server", brigodier.String).
				Then(brigodier.Argument("time", brigodier.String).
					Executes(command.Command(func(ctx *command.Context) error {
						return r.cmdDelay(ctx)
					}))))).
		Then(brigodier.Literal("autoshutdown").
			Then(brigodier.Argument("server", brigodier.String).
				Then(brigodier.Argument("toggle", brigodier.String).
					Executes(command.Command(func(ctx *command.Context) error {
						return r.cmdAutoShutdown(ctx)
					}))))).
		Executes(command.Command(func(ctx *command.Context) error {
			return r.cmdHelp(ctx)
		})))

	r.proxy.Command().Register(brigodier.Literal("lb").
		Then(brigodier.Literal("status").
			Then(brigodier.Argument("server", brigodier.String).
				Executes(command.Command(func(ctx *command.Context) error {
					return r.cmdLBStatus(ctx)
				}))).
			Executes(command.Command(func(ctx *command.Context) error {
				return r.cmdLBStatusAll(ctx)
			}))).
		Then(brigodier.Literal("disable").
			Then(brigodier.Argument("server", brigodier.String).
				Then(brigodier.Argument("backend", brigodier.String).
					Executes(command.Command(func(ctx *command.Context) error {
						return r.cmdLBDisable(ctx)
					}))))).
		Then(brigodier.Literal("enable").
			Then(brigodier.Argument("server", brigodier.String).
				Then(brigodier.Argument("backend", brigodier.String).
					Executes(command.Command(func(ctx *command.Context) error {
						return r.cmdLBEnable(ctx)
					}))))).
		Executes(command.Command(func(ctx *command.Context) error {
			return r.cmdLBHelp(ctx)
		})))
}

func (r *RMSWhitelist) cmdHelp(ctx *command.Context) error {
	ctx.Source.SendMessage(&component.Text{Content: "Dynamic Server Management Commands:", S: component.Style{Color: color.Gold}})
	ctx.Source.SendMessage(&component.Text{Content: "  /dserver delay <server> <time|off> - Set/clear protection period", S: component.Style{Color: color.Yellow}})
	ctx.Source.SendMessage(&component.Text{Content: "    Time format: 10s, 5m, 2h or plain seconds", S: component.Style{Color: color.Gray}})
	ctx.Source.SendMessage(&component.Text{Content: "  /dserver autoshutdown <server> <on|off> - Toggle auto-shutdown", S: component.Style{Color: color.Yellow}})
	return nil
}

func (r *RMSWhitelist) cmdDelay(ctx *command.Context) error {
	if r.dynamicServer == nil {
		ctx.Source.SendMessage(&component.Text{Content: "Dynamic server management is not enabled", S: component.Style{Color: color.Red}})
		return nil
	}

	serverName := ctx.String("server")
	timeArg := ctx.String("time")

	if !r.dynamicServer.IsAutoStartServer(serverName) {
		ctx.Source.SendMessage(&component.Text{Content: fmt.Sprintf("Server '%s' is not a managed dynamic server", serverName), S: component.Style{Color: color.Red}})
		return nil
	}

	if timeArg == "off" {
		r.dynamicServer.ClearProtectionPeriod(serverName)
		ctx.Source.SendMessage(&component.Text{Content: fmt.Sprintf("Cleared protection period for server '%s'", serverName), S: component.Style{Color: color.Green}})
		return nil
	}

	seconds, err := parseTimeString(timeArg)
	if err != nil || seconds < 0 {
		ctx.Source.SendMessage(&component.Text{Content: fmt.Sprintf("Invalid time format: %s", timeArg), S: component.Style{Color: color.Red}})
		ctx.Source.SendMessage(&component.Text{Content: "Use format like: 30s, 5m, 2h or plain seconds", S: component.Style{Color: color.Gray}})
		return nil
	}

	r.dynamicServer.SetShutdownDelay(serverName, seconds)
	endTime := time.Now().Add(time.Duration(seconds) * time.Second)
	ctx.Source.SendMessage(&component.Text{Content: fmt.Sprintf("Set protection period for server '%s' to %s", serverName, formatDuration(seconds)), S: component.Style{Color: color.Green}})
	ctx.Source.SendMessage(&component.Text{Content: fmt.Sprintf("Protection ends at: %s", endTime.Format("2006-01-02 15:04:05")), S: component.Style{Color: color.Gray}})
	return nil
}

func (r *RMSWhitelist) cmdAutoShutdown(ctx *command.Context) error {
	if r.dynamicServer == nil {
		ctx.Source.SendMessage(&component.Text{Content: "Dynamic server management is not enabled", S: component.Style{Color: color.Red}})
		return nil
	}

	serverName := ctx.String("server")
	toggle := ctx.String("toggle")

	if !r.dynamicServer.IsAutoStartServer(serverName) {
		ctx.Source.SendMessage(&component.Text{Content: fmt.Sprintf("Server '%s' is not a managed dynamic server", serverName), S: component.Style{Color: color.Red}})
		return nil
	}

	var enabled bool
	switch toggle {
	case "on", "true":
		enabled = true
	case "off", "false":
		enabled = false
	default:
		ctx.Source.SendMessage(&component.Text{Content: fmt.Sprintf("Invalid value: %s. Use 'on' or 'off'", toggle), S: component.Style{Color: color.Red}})
		return nil
	}

	r.dynamicServer.SetAutoShutdownEnabled(serverName, enabled)
	state := "disabled"
	if enabled {
		state = "enabled"
	}
	ctx.Source.SendMessage(&component.Text{Content: fmt.Sprintf("Auto-shutdown %s for server '%s'", state, serverName), S: component.Style{Color: color.Green}})
	return nil
}

func parseTimeString(s string) (int, error) {
	if len(s) == 0 {
		return 0, fmt.Errorf("empty string")
	}

	lastChar := s[len(s)-1]
	var multiplier int
	var numStr string

	switch lastChar {
	case 's':
		multiplier = 1
		numStr = s[:len(s)-1]
	case 'm':
		multiplier = 60
		numStr = s[:len(s)-1]
	case 'h':
		multiplier = 3600
		numStr = s[:len(s)-1]
	default:
		multiplier = 1
		numStr = s
	}

	var num int
	_, err := fmt.Sscanf(numStr, "%d", &num)
	if err != nil {
		return 0, err
	}
	return num * multiplier, nil
}

func formatDuration(seconds int) string {
	if seconds >= 3600 && seconds%3600 == 0 {
		return fmt.Sprintf("%d hour(s)", seconds/3600)
	} else if seconds >= 60 && seconds%60 == 0 {
		return fmt.Sprintf("%d minute(s)", seconds/60)
	}
	return fmt.Sprintf("%d second(s)", seconds)
}

func getPluginDataDir() string {
	if dir := os.Getenv("GATE_PLUGIN_DATA_DIR"); dir != "" {
		return dir
	}
	exe, err := os.Executable()
	if err != nil {
		return "plugins"
	}
	return filepath.Join(filepath.Dir(exe), "plugins")
}

func (r *RMSWhitelist) cmdLBHelp(ctx *command.Context) error {
	ctx.Source.SendMessage(&component.Text{Content: "Load Balancer Commands:", S: component.Style{Color: color.Gold}})
	ctx.Source.SendMessage(&component.Text{Content: "  /lb status [server] - Show backend status and health scores", S: component.Style{Color: color.Yellow}})
	ctx.Source.SendMessage(&component.Text{Content: "  /lb disable <server> <backend> - Disable a backend", S: component.Style{Color: color.Yellow}})
	ctx.Source.SendMessage(&component.Text{Content: "  /lb enable <server> <backend> - Enable a backend", S: component.Style{Color: color.Yellow}})
	return nil
}

func (r *RMSWhitelist) cmdLBStatusAll(ctx *command.Context) error {
	if r.loadBalancer == nil {
		ctx.Source.SendMessage(&component.Text{Content: "Load balancer is not enabled", S: component.Style{Color: color.Red}})
		return nil
	}

	servers := r.loadBalancer.GetAllServers()
	if len(servers) == 0 {
		ctx.Source.SendMessage(&component.Text{Content: "No load balanced servers configured", S: component.Style{Color: color.Yellow}})
		return nil
	}

	ctx.Source.SendMessage(&component.Text{Content: "Load Balanced Servers:", S: component.Style{Color: color.Gold}})
	for name, server := range servers {
		backends := server.Backends()
		availableCount := 0
		for _, b := range backends {
			if b.IsAvailable() {
				availableCount++
			}
		}
		ctx.Source.SendMessage(&component.Text{
			Content: fmt.Sprintf("  %s: %d/%d backends available, strategy: %s",
				name, availableCount, len(backends), server.Strategy().Name()),
			S: component.Style{Color: color.Yellow},
		})
	}
	return nil
}

func (r *RMSWhitelist) cmdLBStatus(ctx *command.Context) error {
	if r.loadBalancer == nil {
		ctx.Source.SendMessage(&component.Text{Content: "Load balancer is not enabled", S: component.Style{Color: color.Red}})
		return nil
	}

	serverName := ctx.String("server")
	stats := r.loadBalancer.GetServerStats(serverName)
	if stats == nil {
		ctx.Source.SendMessage(&component.Text{Content: fmt.Sprintf("Server '%s' not found", serverName), S: component.Style{Color: color.Red}})
		return nil
	}

	server := r.loadBalancer.GetServer(serverName)
	ctx.Source.SendMessage(&component.Text{
		Content: fmt.Sprintf("Server '%s' (strategy: %s):", serverName, server.Strategy().Name()),
		S:       component.Style{Color: color.Gold},
	})

	for _, stat := range stats {
		statusColor := color.Green
		statusText := "OK"
		if stat.Disabled {
			statusColor = color.Gray
			statusText = "DISABLED"
		} else if !stat.Healthy {
			statusColor = color.Red
			statusText = "UNHEALTHY"
		}

		score := 0
		for _, b := range server.Backends() {
			if b.Addr == stat.Addr {
				score = b.HealthScore(r.loadBalancer.Config().HealthCheck.JitterThreshold)
				break
			}
		}

		ctx.Source.SendMessage(&component.Text{
			Content: fmt.Sprintf("  %s [%s] - %d player(s)", stat.Addr, statusText, stat.CurrentConns),
			S:       component.Style{Color: statusColor},
		})
		ctx.Source.SendMessage(&component.Text{
			Content: fmt.Sprintf("    Score: %d | Max: %d | Latency: %.1fms | Jitter: %.1fms | Fails: %d",
				score, stat.MaxConnections, stat.AvgLatency, stat.Jitter, stat.FailCount),
			S: component.Style{Color: color.Gray},
		})
		if len(stat.Players) > 0 {
			playerList := ""
			for i, p := range stat.Players {
				if i > 0 {
					playerList += ", "
				}
				playerList += p
			}
			ctx.Source.SendMessage(&component.Text{
				Content: fmt.Sprintf("    Players: %s", playerList),
				S:       component.Style{Color: color.Aqua},
			})
		}
	}
	return nil
}

func (r *RMSWhitelist) cmdLBDisable(ctx *command.Context) error {
	if r.loadBalancer == nil {
		ctx.Source.SendMessage(&component.Text{Content: "Load balancer is not enabled", S: component.Style{Color: color.Red}})
		return nil
	}

	serverName := ctx.String("server")
	backendAddr := ctx.String("backend")

	if r.loadBalancer.DisableBackend(serverName, backendAddr) {
		ctx.Source.SendMessage(&component.Text{
			Content: fmt.Sprintf("Backend '%s' disabled for server '%s'", backendAddr, serverName),
			S:       component.Style{Color: color.Green},
		})
	} else {
		ctx.Source.SendMessage(&component.Text{
			Content: fmt.Sprintf("Backend '%s' not found for server '%s'", backendAddr, serverName),
			S:       component.Style{Color: color.Red},
		})
	}
	return nil
}

func (r *RMSWhitelist) cmdLBEnable(ctx *command.Context) error {
	if r.loadBalancer == nil {
		ctx.Source.SendMessage(&component.Text{Content: "Load balancer is not enabled", S: component.Style{Color: color.Red}})
		return nil
	}

	serverName := ctx.String("server")
	backendAddr := ctx.String("backend")

	if r.loadBalancer.EnableBackend(serverName, backendAddr) {
		ctx.Source.SendMessage(&component.Text{
			Content: fmt.Sprintf("Backend '%s' enabled for server '%s'", backendAddr, serverName),
			S:       component.Style{Color: color.Green},
		})
	} else {
		ctx.Source.SendMessage(&component.Text{
			Content: fmt.Sprintf("Backend '%s' not found for server '%s'", backendAddr, serverName),
			S:       component.Style{Color: color.Red},
		})
	}
	return nil
}
