package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/zhoushoujianwork/bbclaw/adapter/internal/buildinfo"
	"github.com/zhoushoujianwork/bbclaw/adapter/internal/homeadapter"
	"github.com/zhoushoujianwork/bbclaw/adapter/internal/obs"
	"github.com/zhoushoujianwork/bbclaw/adapter/internal/openclaw"
	"github.com/zhoushoujianwork/bbclaw/adapter/internal/pipeline"
)

func main() {
	if buildinfo.ShouldPrintVersion(os.Args[1:]) {
		fmt.Println(buildinfo.String("bbclaw-home-adapter"))
		return
	}

	logger := obs.NewLogger()
	metrics := obs.NewMetrics()

	cfg, err := homeadapter.LoadFromEnv()
	if err != nil {
		logger.Errorf("load config failed: %v", err)
		os.Exit(1)
	}

	sink := pipeline.Wrap(openclaw.NewClient(cfg.OpenClawURL, cfg.HTTPTimeout, openclaw.Options{
		NodeID:             cfg.OpenClawNodeID,
		AuthToken:          cfg.OpenClawAuthToken,
		DeviceIdentityPath: cfg.OpenClawIdentityPath,
		ReplyWaitTimeout:   cfg.OpenClawReplyWait,
	}), logger, metrics)
	adapter := homeadapter.New(cfg, sink, logger, metrics)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	logger.Infof("%s", buildinfo.String("bbclaw-home-adapter"))
	logger.Infof("starting bbclaw-home-adapter home_site=%s cloud=%s openclaw=%s",
		cfg.HomeSiteID, cfg.CloudWSURL, cfg.OpenClawURL)
	if err := adapter.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		logger.Errorf("adapter stopped: %v", err)
		os.Exit(1)
	}
}
