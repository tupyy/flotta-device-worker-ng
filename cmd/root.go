/*
Copyright © 2022 NAME HERE <EMAIL ADDRESS>

*/
package cmd

import (
	"context"
	"os"
	"os/signal"

	"github.com/spf13/cobra"
	config "github.com/tupyy/device-worker-ng/configuration"
	"github.com/tupyy/device-worker-ng/internal/certificate"
	httpClient "github.com/tupyy/device-worker-ng/internal/client/http"
	"github.com/tupyy/device-worker-ng/internal/configuration"
	"github.com/tupyy/device-worker-ng/internal/edge"
	"github.com/tupyy/device-worker-ng/internal/executor"
	"github.com/tupyy/device-worker-ng/internal/profile"
	"github.com/tupyy/device-worker-ng/internal/resources"
	"github.com/tupyy/device-worker-ng/internal/scheduler"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

var (
	configFile            string
	caRoot                string
	certFile              string
	privateKey            string
	server                string
	namespace             string
	logLevel              string
	profileManagerEnabled bool
)

const (
	flottaSlice = "flotta"
)

var rootCmd = &cobra.Command{
	Use:   "device-worker-ng",
	Short: "Device worker",
	Run: func(cmd *cobra.Command, args []string) {
		logger := setupLogger()
		defer logger.Sync()

		undo := zap.ReplaceGlobals(logger)
		defer undo()

		config.InitConfiguration(cmd, configFile)

		certManager, err := initCertificateManager(caRoot, certFile, privateKey)
		if err != nil {
			panic(err)
		}

		// httpClient is a wrapper around http client which implements yggdrasil API.
		httpClient, err := httpClient.New(config.GetServerAddress(), certManager)
		if err != nil {
			panic(err)
		}

		confManager := configuration.New(profileManagerEnabled)
		executor, err := executor.New()
		if err != nil {
			panic(err)
		}

		controller := edge.New(httpClient, confManager, certManager)
		var profileManager *profile.Manager
		if profileManagerEnabled {
			profileManager = profile.New(confManager.StateManagerCh)
		}
		resourceManager := resources.New()
		// setup scheduler
		scheduler := scheduler.New(executor, resourceManager)
		//	confManager.SetWorkloadStatusReader(scheduler)

		// this should be the last step, in order to avoid data races.
		// starting in right order the controller, scheduler and profile manager
		ctx, cancel := context.WithCancel(context.Background())
		controller.Start(ctx)
		if profileManagerEnabled {
			scheduler.Start(ctx, confManager.SchedulerCh, profileManager.OutputCh)
			profileManager.Start(ctx)
		} else {
			scheduler.Start(ctx, confManager.SchedulerCh, nil)
		}

		done := make(chan os.Signal, 1)
		signal.Notify(done, os.Interrupt, os.Kill)

		<-done

		cancel()
		controller.Shutdown(ctx)
		if profileManagerEnabled {
			profileManager.Shutdown(ctx)
		}
	},
}

func Execute() {
	err := rootCmd.Execute()
	if err != nil {
		os.Exit(1)
	}
}

func init() {
	rootCmd.Flags().StringVar(&configFile, "config", "c", "configuration file")
	rootCmd.Flags().StringVar(&caRoot, "ca-root", "", "ca certificate")
	rootCmd.Flags().StringVar(&certFile, "cert", "", "client certificate")
	rootCmd.Flags().StringVar(&privateKey, "key", "", "private key")
	rootCmd.Flags().StringVar(&server, "server", "", "server address")
	rootCmd.Flags().StringVar(&namespace, "namespace", "default", "target namespace")
	rootCmd.Flags().StringVar(&logLevel, "log-level", "info", "log level")
	rootCmd.Flags().BoolVar(&profileManagerEnabled, "enable-profile-manager", true, "enable profile manager")
}

func setupLogger() *zap.Logger {
	loggerCfg := &zap.Config{
		Level:    zap.NewAtomicLevelAt(zapcore.InfoLevel),
		Encoding: "json",
		EncoderConfig: zapcore.EncoderConfig{
			TimeKey:        "time",
			LevelKey:       "severity",
			NameKey:        "logger",
			CallerKey:      "caller",
			MessageKey:     "message",
			StacktraceKey:  "stacktrace",
			LineEnding:     zapcore.DefaultLineEnding,
			EncodeTime:     zapcore.RFC3339TimeEncoder,
			EncodeLevel:    zapcore.LowercaseLevelEncoder,
			EncodeDuration: zapcore.MillisDurationEncoder, EncodeCaller: zapcore.ShortCallerEncoder},
		OutputPaths:      []string{"stdout"},
		ErrorOutputPaths: []string{"stderr"},
	}

	atomicLogLevel, err := zap.ParseAtomicLevel(logLevel)
	if err == nil {
		loggerCfg.Level = atomicLogLevel
	}

	plain, err := loggerCfg.Build(zap.AddStacktrace(zap.DPanicLevel))
	if err != nil {
		panic(err)
	}

	return plain
}

func initCertificateManager(caroot, certFile, keyFile string) (*certificate.Manager, error) {
	// read certificates
	caRoot, err := os.ReadFile(caroot)
	if err != nil {
		return nil, err
	}

	cert, err := os.ReadFile(certFile)
	if err != nil {
		return nil, err
	}

	privateKey, err := os.ReadFile(keyFile)
	if err != nil {
		return nil, err
	}

	certManager, err := certificate.New([][]byte{caRoot}, cert, privateKey)
	if err != nil {
		return nil, err
	}

	return certManager, nil
}
