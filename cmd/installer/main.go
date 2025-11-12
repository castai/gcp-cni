package main

import (
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/spf13/pflag"

	"github.com/castai/gcp-cni/internal/installer"
)

const (
	defaultCNIBinDir     = "/opt/cni/bin"
	defaultCNIConfDir    = "/etc/cni/net.d"
	gcpCNIConfName       = "10-containerd-net.conflist"
	defaultHostRoot      = "/host"
	maxCopyBytes         = 100 * 1024 * 1024
	checkIntervalSeconds = 30
)

var (
	cniBinDir   = pflag.String("cni-bin-dir", defaultCNIBinDir, "CNI binary directory on the host")
	cniConfDir  = pflag.String("cni-conf-dir", defaultCNIConfDir, "CNI configuration directory on the host")
	cniConfName = pflag.String("cni-conf-name", gcpCNIConfName, "GCP CNI configuration file name")
	hostRoot    = pflag.String("host-root", defaultHostRoot, "Host root mount point")
	logLevel    = pflag.String("log-level", "info", "Log level (debug, info, warn, error)")
)

func main() {
	pflag.Parse()

	level := parseLogLevel(*logLevel)
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(logger)

	logger.Info("Starting GCP CNI installer daemon",
		slog.String("cni_bin_dir", *cniBinDir),
		slog.String("cni_conf_dir", *cniConfDir),
		slog.String("host_root", *hostRoot),
	)

	if err := runInstallation(logger); err != nil {
		logger.Error("Installation check failed", slog.String("error", err.Error()))
	}

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)

	sig := <-signals
	logger.Info("Received termination signal, exiting", slog.String("signal", sig.String()))

	logger.Info("Reverting CNI configuration to use host-local IPAM")
	reconfigureCNIIPAMConf(logger, "host-local")
}

func runInstallation(logger *slog.Logger) error {
	if err := installHostBinary(logger, "gcp-ipam"); err != nil {
		return fmt.Errorf("failed to install gcp-ipam binary: %w", err)
	}

	if err := reconfigureCNIIPAMConf(logger, "gcp-ipam"); err != nil {
		return fmt.Errorf("failed to reconfigure CNI: %w", err)
	}

	return nil
}

func reconfigureCNIIPAMConf(logger *slog.Logger, ipamType string) error {
	confDir := filepath.Join(*hostRoot, *cniConfDir)
	confPath := filepath.Join(confDir, *cniConfName)

	if _, err := os.Stat(confPath); os.IsNotExist(err) {
		logger.Warn("GCP CNI configuration not found, skipping reconfiguration",
			slog.String("path", confPath))
		return nil
	}

	logger.Info("Reading CNI configuration", slog.String("path", confPath))

	data, err := os.ReadFile(confPath)
	if err != nil {
		return fmt.Errorf("failed to read CNI config: %w", err)
	}

	updatedData, err := installer.UpdateCNIIPAM(data, ipamType, logger)
	if err != nil {
		return err
	}

	tmpPath := confPath + ".tmp"
	if err := os.WriteFile(tmpPath, updatedData, 0o644); err != nil {
		return fmt.Errorf("failed to write temporary config: %w", err)
	}

	if err := os.Rename(tmpPath, confPath); err != nil {
		return fmt.Errorf("failed to rename config file: %w", err)
	}

	logger.Info("CNI configuration updated successfully", slog.String("path", confPath))
	return nil
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
