package main

import (
	"context"
	"log/slog"
	"os"
	"time"

	"github.com/spf13/pflag"

	"github.com/castai/gcp-cni/internal/provisioner"
)

var (
	secondaryRangeName = pflag.String("secondary-range-name", "live", "Name for the secondary IP range")
	rangeSizeBits      = pflag.Int("range-size-bits", 16, "Size of the secondary range in bits (e.g., 16 for /16)")
	logLevel           = pflag.String("log-level", "info", "Log level (debug, info, warn, error)")
	dryRun             = pflag.Bool("dry-run", false, "Dry run mode - don't make any changes")
)

func main() {
	pflag.Parse()

	level := parseLogLevel(*logLevel)
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(logger)

	logger.Info("Starting GCP CNI cluster provisioner",
		slog.String("secondary_range_name", *secondaryRangeName),
		slog.Int("range_size_bits", *rangeSizeBits),
		slog.Bool("dry_run", *dryRun),
	)

	ctx := context.Background()

	provisioner, err := provisioner.NewProvisioner(ctx, logger)
	if err != nil {
		logger.Error("Failed to create provisioner", slog.String("error", err.Error()))
		os.Exit(1)
	}

	err = provisioner.Provision(ctx, secondaryRangeName)
	if err != nil {
		logger.Error("Cluster provisioning failed", slog.String("error", err.Error()))
		os.Exit(1)
	}

	logger.Info("Cluster provisioning completed successfully")
	logger.Info("Entering sleep mode - provisioner will run indefinitely")

	time.Sleep(24 * time.Hour)
}

func parseLogLevel(level string) slog.Level {
	switch level {
	case "debug":
		return slog.LevelDebug
	case "info":
		return slog.LevelInfo
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
