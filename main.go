package main

import (
	"context"
	"flag"
	"os"
	"os/signal"
	"syscall"

	"github.com/jehuda-ruzinski/container-supervisor/internal/config"
	"github.com/jehuda-ruzinski/container-supervisor/internal/logging"
	"github.com/jehuda-ruzinski/container-supervisor/internal/supervisor"
	"github.com/rs/zerolog"
)

const defaultConfigPath = "/container-supervisor/config.yml"

func main() {
	mlog := logging.Supervisor()

	var configPath string
	flag.StringVar(&configPath, "config", defaultConfigPath, "path to the config file")
	flag.StringVar(&configPath, "c", defaultConfigPath, "path to the config file (shorthand)")
	flag.Parse()

	cfg, err := config.Load(configPath)
	if err != nil {
		mlog.Fatal().Err(err).Str("path", configPath).Msg("load config")
	}

	level, err := zerolog.ParseLevel(cfg.LogLevel)
	if err != nil {
		mlog.Fatal().Err(err).Str("loglevel", cfg.LogLevel).Msg("invalid log level")
	}
	zerolog.SetGlobalLevel(level)

	logging.Configure(cfg.LogOutputFormat)
	mlog = logging.Supervisor()

	mlog.Info().Int("processes", len(cfg.Processes)).Str("config", configPath).Msg("loaded config")
	mlog.Debug().Interface("config", cfg).Msg("config contents")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := supervisor.Run(ctx, cfg, mlog); err != nil {
		mlog.Fatal().Err(err).Msg("run processes")
	}
}
