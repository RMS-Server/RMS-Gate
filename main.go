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
	config        *Config
	checker       *WhitelistChecker
	mcsClient     *MCSManagerClient
	dynamicServer *DynamicServerManager
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
	r.config = loadConfig(configDir, r.log)
	r.checker = NewWhitelistChecker(r.log)

	if r.config.MCSManager != nil && r.config.DynamicServer != nil {
		r.mcsClient = NewMCSManagerClient(r.log, r.config.MCSManager)
		r.dynamicServer = NewDynamicServerManager(r.ctx, r.log, r.proxy, r.mcsClient, r.config.DynamicServer)
		r.log.Info("Dynamic server management enabled")
	}

	event.Subscribe(r.proxy.Event(), 0, r.onLogin)
	event.Subscribe(r.proxy.Event(), -100, r.onServerPreConnect)

	r.registerCommands()

	r.log.Info("RMS Whitelist Plugin initialized successfully")
	return nil
}

func (r *RMSWhitelist) onLogin(e *proxy.LoginEvent) {
	player := e.Player()
	username := player.Username()
	uuid := player.ID().String()

	result := r.checker.Check(r.ctx, username, uuid, r.config.APIUrl, r.config.TimeoutSeconds)

	switch result {
	case Allowed:
		r.log.Info("User is whitelisted", "username", username, "uuid", uuid)
	case NotInWhitelist:
		r.log.Info("User is not in whitelist", "username", username, "uuid", uuid)
		e.Deny(&component.Text{Content: r.config.MsgNotInWhitelist})
	case ServerError:
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
