package main

import (
	"context"
	"os"
	"path/filepath"

	"github.com/go-logr/logr"
	"github.com/robinbraemer/event"
	"go.minekube.com/common/minecraft/component"
	"go.minekube.com/gate/cmd/gate"
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
	ctx     context.Context
	proxy   *proxy.Proxy
	log     logr.Logger
	config  *Config
	checker *WhitelistChecker
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

	event.Subscribe(r.proxy.Event(), 0, r.onLogin)

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
