// Copyright 2025 The PipeCD Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package sdk

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/pprof"
	"time"

	"github.com/pipe-cd/piped-plugin-sdk-go/logpersister"
	"github.com/pipe-cd/piped-plugin-sdk-go/toolregistry"
	"github.com/spf13/cobra"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"

	"github.com/pipe-cd/pipecd/pkg/admin"
	"github.com/pipe-cd/pipecd/pkg/cli"
	config "github.com/pipe-cd/pipecd/pkg/configv1"
	"github.com/pipe-cd/pipecd/pkg/rpc"
)

// DeployTargetsNone is a type alias for a slice of pointers to DeployTarget
// with an empty struct as the generic type parameter. It represents a case
// where there are no deployment targets.
// This utility is defined for plugins which has no deploy targets handling in ExecuteStage.
type DeployTargetsNone = []*DeployTarget[struct{}]

// ConfigNone is a type alias for a pointer to a struct with an empty struct as the generic type parameter.
// This utility is defined for plugins which has no config handling in ExecuteStage.
type ConfigNone = *struct{}

// DeployTarget defines the deploy target configuration for the piped.
type DeployTarget[Config any] struct {
	// The name of the deploy target.
	Name string `json:"name"`
	// The labes of the deploy target.
	Labels map[string]string `json:"labels,omitempty"`
	// The configuration of the deploy target.
	Config Config `json:"config"`
}

// InitializeInput is the input for the Initializer interface.
type InitializeInput[Config, DeployTargetConfig any] struct {
	// Config is the configuration of the plugin.
	Config *Config
	// DeployTargets is the deploy targets of the plugin.
	DeployTargets map[string]*DeployTarget[DeployTargetConfig]
	// Logger is the logger for the plugin.
	Logger *zap.Logger
}

// Initializer is an interface that defines the Initialize method.
type Initializer[Config, DeployTargetConfig any] interface {
	// Initialize initializes the plugin with the given context and input.
	// It is called multiple times when the plugin is registered multiple times, such as deployment, livestate, and plan-preview plugins.
	// It is recommended to use sync.Once to ensure that the plugin is initialized only once.
	Initialize(context.Context, *InitializeInput[Config, DeployTargetConfig]) error
}

type commonFields[Config, DeployTargetConfig any] struct {
	name          string
	version       string
	config        *config.PipedPlugin
	logger        *zap.Logger
	logPersister  logPersister
	client        *pluginServiceClient
	toolRegistry  *toolregistry.ToolRegistry
	pluginConfig  *Config
	deployTargets map[string]*DeployTarget[DeployTargetConfig]
}

type logPersister interface {
	StageLogPersister(deploymentID, stageID string) logpersister.StageLogPersister
}

// withLogger copies the commonFields and sets the logger to the given one.
func (c commonFields[Config, DeployTargetConfig]) withLogger(logger *zap.Logger) commonFields[Config, DeployTargetConfig] {
	c.logger = logger
	return c
}

// PluginOption is a function that configures the plugin.
type PluginOption[Config, DeployTargetConfig, ApplicationConfigSpec any] func(*Plugin[Config, DeployTargetConfig, ApplicationConfigSpec])

// WithStagePlugin is a function that sets the stage plugin.
// This is mutually exclusive with WithDeploymentPlugin.
func WithStagePlugin[Config, DeployTargetConfig, ApplicationConfigSpec any](stagePlugin StagePlugin[Config, DeployTargetConfig, ApplicationConfigSpec]) PluginOption[Config, DeployTargetConfig, ApplicationConfigSpec] {
	return func(plugin *Plugin[Config, DeployTargetConfig, ApplicationConfigSpec]) {
		plugin.stagePlugin = stagePlugin
	}
}

// WithDeploymentPlugin is a function that sets the deployment plugin.
// This is mutually exclusive with WithStagePlugin.
func WithDeploymentPlugin[Config, DeployTargetConfig, ApplicationConfigSpec any](deploymentPlugin DeploymentPlugin[Config, DeployTargetConfig, ApplicationConfigSpec]) PluginOption[Config, DeployTargetConfig, ApplicationConfigSpec] {
	return func(plugin *Plugin[Config, DeployTargetConfig, ApplicationConfigSpec]) {
		plugin.deploymentPlugin = deploymentPlugin
	}
}

// WithLivestatePlugin is a function that sets the livestate plugin.
func WithLivestatePlugin[Config, DeployTargetConfig, ApplicationConfigSpec any](livestatePlugin LivestatePlugin[Config, DeployTargetConfig, ApplicationConfigSpec]) PluginOption[Config, DeployTargetConfig, ApplicationConfigSpec] {
	return func(plugin *Plugin[Config, DeployTargetConfig, ApplicationConfigSpec]) {
		plugin.livestatePlugin = livestatePlugin
	}
}

// WithPlanPreviewPlugin is a function that sets the plan preview plugin.
func WithPlanPreviewPlugin[Config, DeployTargetConfig, ApplicationConfigSpec any](planPreviewPlugin PlanPreviewPlugin[Config, DeployTargetConfig, ApplicationConfigSpec]) PluginOption[Config, DeployTargetConfig, ApplicationConfigSpec] {
	return func(plugin *Plugin[Config, DeployTargetConfig, ApplicationConfigSpec]) {
		plugin.planPreviewPlugin = planPreviewPlugin
	}
}

// Plugin is a wrapper for the plugin.
// It provides a way to run the plugin with the given config and deploy target config.
type Plugin[Config, DeployTargetConfig, ApplicationConfigSpec any] struct {

	// plugin info
	version string
	// name is the name of the plugin defined in the piped plugin config.
	name string

	// plugin implementations
	stagePlugin       StagePlugin[Config, DeployTargetConfig, ApplicationConfigSpec]
	deploymentPlugin  DeploymentPlugin[Config, DeployTargetConfig, ApplicationConfigSpec]
	livestatePlugin   LivestatePlugin[Config, DeployTargetConfig, ApplicationConfigSpec]
	planPreviewPlugin PlanPreviewPlugin[Config, DeployTargetConfig, ApplicationConfigSpec]

	// command line options
	pipedPluginService   string
	gracePeriod          time.Duration
	tls                  bool
	certFile             string
	keyFile              string
	config               string
	enableGRPCReflection bool
}

// NewPlugin creates a new plugin.
func NewPlugin[Config, DeployTargetConfig, ApplicationConfigSpec any](version string, options ...PluginOption[Config, DeployTargetConfig, ApplicationConfigSpec]) (*Plugin[Config, DeployTargetConfig, ApplicationConfigSpec], error) {
	plugin := &Plugin[Config, DeployTargetConfig, ApplicationConfigSpec]{
		version: version,

		// Default values of command line options
		gracePeriod: 30 * time.Second,
	}

	for _, option := range options {
		option(plugin)
	}

	if plugin.stagePlugin == nil && plugin.deploymentPlugin == nil && plugin.livestatePlugin == nil {
		return nil, fmt.Errorf("at least one plugin must be registered")
	}

	if _, ok := plugin.stagePlugin.(DeploymentPlugin[Config, DeployTargetConfig, ApplicationConfigSpec]); ok {
		return nil, fmt.Errorf("stage plugin cannot be a deployment plugin, you must use WithDeploymentPlugin instead")
	}

	if plugin.stagePlugin != nil && plugin.deploymentPlugin != nil {
		return nil, fmt.Errorf("stage plugin and deployment plugin cannot be registered at the same time")
	}

	return plugin, nil
}

// Run runs the plugin.
func (p *Plugin[Config, DeployTargetConfig, ApplicationConfigSpec]) Run() error {
	app := cli.NewApp(
		"pipecd-plugin",
		"Plugin component for Piped.",
	)

	app.AddCommands(
		p.command(),
	)

	if err := app.Run(); err != nil {
		return err
	}

	return nil
}

// command returns the cobra command for the plugin.
func (p *Plugin[Config, DeployTargetConfig, ApplicationConfigSpec]) command() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "start",
		Short: "Start running a plugin.",
		RunE:  cli.WithContext(p.run),
	}

	cmd.Flags().StringVar(&p.pipedPluginService, "piped-plugin-service", p.pipedPluginService, "The address used to connect to the piped plugin service.")
	cmd.Flags().StringVar(&p.config, "config", p.config, "The configuration for the plugin.")
	cmd.Flags().DurationVar(&p.gracePeriod, "grace-period", p.gracePeriod, "How long to wait for graceful shutdown.")

	cmd.Flags().BoolVar(&p.tls, "tls", p.tls, "Whether running the gRPC server with TLS or not.")
	cmd.Flags().StringVar(&p.certFile, "cert-file", p.certFile, "The path to the TLS certificate file.")
	cmd.Flags().StringVar(&p.keyFile, "key-file", p.keyFile, "The path to the TLS key file.")

	// For debugging early in development
	cmd.Flags().BoolVar(&p.enableGRPCReflection, "enable-grpc-reflection", p.enableGRPCReflection, "Whether to enable the reflection service or not.")

	cmd.MarkFlagRequired("piped-plugin-service")
	cmd.MarkFlagRequired("config")

	return cmd
}

// run is the entrypoint of the plugin.
func (p *Plugin[Config, DeployTargetConfig, ApplicationConfigSpec]) run(ctx context.Context, input cli.Input) error {
	if p.stagePlugin != nil && p.deploymentPlugin != nil {
		// This is promised in the NewPlugin function.
		// When this happens, it means that there is a bug in the SDK, because these are private fields.
		input.Logger.Error(
			"something went wrong in the SDK, please report this issue to the developers",
			zap.String("version", p.version),
			zap.String("reason", "stage plugin and deployment plugin cannot be registered at the same time"),
			zap.String("report-url", "https://github.com/pipe-cd/pipecd/issues"),
		)
		return fmt.Errorf("something went wrong in the SDK, please report this issue to the developers")
	}

	// Make a cancellable context.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	group, ctx := errgroup.WithContext(ctx)

	pipedPluginServiceClient, err := newPluginServiceClient(ctx, p.pipedPluginService)
	if err != nil {
		input.Logger.Error("failed to create piped plugin service client", zap.Error(err))
		return err
	}

	// Load the configuration.
	cfg, err := config.ParsePluginConfig(p.config)
	if err != nil {
		input.Logger.Error("failed to parse the configuration", zap.Error(err))
		return err
	}

	logger := input.Logger.With(
		zap.String("plugin-name", cfg.Name),
		zap.String("plugin-version", p.version),
	)

	// Start running admin server.
	{
		var (
			ver   = []byte(p.version)
			admin = admin.NewAdmin(0, p.gracePeriod, logger) // TODO: add config for admin port
		)

		admin.HandleFunc("/version", func(w http.ResponseWriter, r *http.Request) {
			w.Write(ver)
		})
		admin.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte("ok"))
		})
		admin.HandleFunc("/debug/pprof/", pprof.Index)
		admin.HandleFunc("/debug/pprof/profile", pprof.Profile)
		admin.HandleFunc("/debug/pprof/trace", pprof.Trace)

		group.Go(func() error {
			return admin.Run(ctx)
		})
	}

	// Start log persister
	persister := logpersister.NewPersister(pipedPluginServiceClient, logger)
	group.Go(func() error {
		return persister.Run(ctx)
	})

	// Start a gRPC server for handling external API requests.
	{
		commonFields := commonFields[Config, DeployTargetConfig]{
			name:         cfg.Name,
			version:      p.version,
			config:       cfg,
			logPersister: persister,
			client:       pipedPluginServiceClient,
			toolRegistry: toolregistry.NewToolRegistry(pipedPluginServiceClient),
		}

		if cfg.Config != nil {
			if err := json.Unmarshal(cfg.Config, &commonFields.pluginConfig); err != nil {
				logger.Fatal("failed to unmarshal the plugin config", zap.Error(err))
				return err
			}
		}

		commonFields.deployTargets = make(map[string]*DeployTarget[DeployTargetConfig], len(cfg.DeployTargets))
		for _, dt := range cfg.DeployTargets {
			var sdkDt DeployTargetConfig
			if err := json.Unmarshal(dt.Config, &sdkDt); err != nil {
				logger.Fatal("failed to unmarshal deploy target config", zap.Error(err))
				return err
			}
			commonFields.deployTargets[dt.Name] = &DeployTarget[DeployTargetConfig]{
				Name:   dt.Name,
				Labels: dt.Labels,
				Config: sdkDt,
			}
		}

		initializeInput := &InitializeInput[Config, DeployTargetConfig]{
			Config:        commonFields.pluginConfig,
			DeployTargets: commonFields.deployTargets,
			Logger:        logger.Named("plugin-initializer"),
		}

		var services []rpc.Service

		if p.stagePlugin != nil {
			if initializer, ok := p.stagePlugin.(Initializer[Config, DeployTargetConfig]); ok {
				if err := initializer.Initialize(ctx, initializeInput); err != nil {
					logger.Error("failed to initialize stage plugin", zap.Error(err))
					return err
				}
			}
			stagePluginServiceServer := &StagePluginServiceServer[Config, DeployTargetConfig, ApplicationConfigSpec]{
				base:         p.stagePlugin,
				commonFields: commonFields.withLogger(logger.Named("stage-service")),
			}
			services = append(services, stagePluginServiceServer)
		}

		if p.deploymentPlugin != nil {
			if initializer, ok := p.deploymentPlugin.(Initializer[Config, DeployTargetConfig]); ok {
				if err := initializer.Initialize(ctx, initializeInput); err != nil {
					logger.Error("failed to initialize deployment plugin", zap.Error(err))
					return err
				}
			}
			deploymentPluginServiceServer := &DeploymentPluginServiceServer[Config, DeployTargetConfig, ApplicationConfigSpec]{
				base:         p.deploymentPlugin,
				commonFields: commonFields.withLogger(logger.Named("deployment-service")),
			}
			services = append(services, deploymentPluginServiceServer)
		}

		if p.livestatePlugin != nil {
			if initializer, ok := p.livestatePlugin.(Initializer[Config, DeployTargetConfig]); ok {
				if err := initializer.Initialize(ctx, initializeInput); err != nil {
					logger.Error("failed to initialize livestate plugin", zap.Error(err))
					return err
				}
			}
			livestatePluginServiceServer := &LivestatePluginServer[Config, DeployTargetConfig, ApplicationConfigSpec]{
				base:         p.livestatePlugin,
				commonFields: commonFields.withLogger(logger.Named("livestate-service")),
			}
			services = append(services, livestatePluginServiceServer)
		}

		if p.planPreviewPlugin != nil {
			if initializer, ok := p.planPreviewPlugin.(Initializer[Config, DeployTargetConfig]); ok {
				if err := initializer.Initialize(ctx, initializeInput); err != nil {
					logger.Error("failed to initialize plan-preview plugin", zap.Error(err))
					return err
				}
			}
			planPreviewPluginServiceServer := &PlanPreviewPluginServer[Config, DeployTargetConfig, ApplicationConfigSpec]{
				base:         p.planPreviewPlugin,
				commonFields: commonFields.withLogger(logger.Named("plan-preview-service")),
			}
			services = append(services, planPreviewPluginServiceServer)
		}

		if len(services) == 0 {
			// This is promised in the NewPlugin function.
			// When this happens, it means that *Plugin was initialized without using NewPlugin.
			logger.Error(
				"no plugin is registered, plugin implementation must use NewPlugin to initialize the plugin",
				zap.String("name", p.name),
				zap.String("version", p.version),
			)
			return fmt.Errorf("no plugin is registered, plugin implementation must use NewPlugin to initialize the plugin")
		}

		var (
			opts = []rpc.Option{
				rpc.WithPort(cfg.Port),
				rpc.WithGracePeriod(p.gracePeriod),
				rpc.WithLogger(logger),
				rpc.WithLogUnaryInterceptor(logger),
				rpc.WithRequestValidationUnaryInterceptor(),
				rpc.WithSignalHandlingUnaryInterceptor(),
			}
		)
		if p.tls {
			opts = append(opts, rpc.WithTLS(p.certFile, p.keyFile))
		}
		if p.enableGRPCReflection {
			opts = append(opts, rpc.WithGRPCReflection())
		}
		if input.Flags.Metrics {
			opts = append(opts, rpc.WithPrometheusUnaryInterceptor())
		}
		if len(services) > 1 {
			for _, service := range services[1:] {
				opts = append(opts, rpc.WithService(service))
			}
		}

		server := rpc.NewServer(services[0], opts...)

		group.Go(func() error {
			return server.Run(ctx)
		})
	}

	if err := group.Wait(); err != nil {
		logger.Error("failed while running", zap.Error(err))
		return err
	}
	return nil
}
