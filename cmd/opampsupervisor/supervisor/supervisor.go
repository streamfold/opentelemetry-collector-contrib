// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package supervisor

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	_ "embed"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"text/template"
	"time"

	"github.com/google/uuid"
	"github.com/knadh/koanf/maps"
	"github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/rawbytes"
	"github.com/knadh/koanf/v2"
	"github.com/open-telemetry/opamp-go/client"
	"github.com/open-telemetry/opamp-go/client/types"
	"github.com/open-telemetry/opamp-go/protobufs"
	"github.com/open-telemetry/opamp-go/server"
	serverTypes "github.com/open-telemetry/opamp-go/server/types"
	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/config/configopaque"
	"go.opentelemetry.io/collector/config/configtelemetry"
	"go.opentelemetry.io/collector/config/configtls"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/contrib/bridges/otelzap"
	telemetryconfig "go.opentelemetry.io/contrib/otelconf/v0.3.0"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/log"
	semconv "go.opentelemetry.io/otel/semconv/v1.21.0"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/multierr"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"google.golang.org/protobuf/proto"

	"github.com/open-telemetry/opentelemetry-collector-contrib/cmd/opampsupervisor/supervisor/commander"
	"github.com/open-telemetry/opentelemetry-collector-contrib/cmd/opampsupervisor/supervisor/config"
)

var (
	//go:embed templates/nooppipeline.yaml
	noopPipelineTpl string

	//go:embed templates/extratelemetryconfig.yaml
	extraConfigTpl string

	//go:embed templates/opampextension.yaml
	opampextensionTpl string

	//go:embed templates/owntelemetry.yaml
	ownTelemetryTpl string

	lastRecvRemoteConfigFile       = "last_recv_remote_config.dat"
	lastRecvOwnTelemetryConfigFile = "last_recv_own_telemetry_config.dat"

	errNonMatchingInstanceUID = errors.New("received collector instance UID does not match expected UID set by the supervisor")
)

const (
	persistentStateFileName     = "persistent_state.yaml"
	agentConfigFileName         = "effective.yaml"
	AllowNoPipelinesFeatureGate = "service.AllowNoPipelines"
)

const maxBufferedCustomMessages = 10

type configState struct {
	// Supervisor-assembled config to be given to the Collector.
	mergedConfig string
	// true if the server provided configmap was empty
	configMapIsEmpty bool
}

func (c *configState) equal(other *configState) bool {
	return other.mergedConfig == c.mergedConfig && other.configMapIsEmpty == c.configMapIsEmpty
}

type agentStartStatus string

var (
	agentStarting    agentStartStatus = "starting"
	agentNotStarting agentStartStatus = "notStarting"
)

type telemetrySettings struct {
	component.TelemetrySettings
	loggerProvider log.LoggerProvider
}

// Supervisor implements supervising of OpenTelemetry Collector and uses OpAMPClient
// to work with an OpAMP Server.
type Supervisor struct {
	pidProvider pidProvider

	// Commander that starts/stops the Agent process.
	commander *commander.Commander

	// Supervisor's own config.
	config config.Supervisor

	agentDescription    *atomic.Value
	availableComponents *atomic.Value

	// Supervisor's persistent state
	persistentState *persistentState

	noopPipelineTemplate         *template.Template
	opampextensionTemplate       *template.Template
	extraTelemetryConfigTemplate *template.Template
	ownTelemetryTemplate         *template.Template

	agentConn *atomic.Value

	// A config section to be added to the Collector's config to fetch its own telemetry.
	// TODO: store this persistently so that when starting we can compose the effective
	// config correctly.
	// https://github.com/open-telemetry/opentelemetry-collector-contrib/issues/21078
	agentConfigOwnTelemetrySection *atomic.Value

	// Internal config state for agent use. See the [configState] struct for more details.
	cfgState *atomic.Value

	// Final effective config of the Collector.
	effectiveConfig *atomic.Value

	// Last received remote config.
	remoteConfig *protobufs.AgentRemoteConfig

	// A channel to indicate there is a new config to apply.
	hasNewConfig chan struct{}
	// configApplyTimeout is the maximum time to wait for the agent to apply a new config.
	// After this time passes without the agent reporting health as OK, the agent is considered unhealthy.
	configApplyTimeout time.Duration
	// lastHealthFromClient is the last health status of the agent received from the client.
	lastHealthFromClient atomic.Pointer[protobufs.ComponentHealth]

	// The OpAMP client to connect to the OpAMP Server.
	opampClient client.OpAMPClient

	doneChan chan struct{}
	agentWG  sync.WaitGroup

	customMessageToServer chan *protobufs.CustomMessage
	customMessageWG       sync.WaitGroup

	// agentReady is true if the agent has started and is fully ready.
	agentReady atomic.Bool
	// agentReadyChan is a channel that can be used to wait for the agent to
	// start in case [agentReady] is false.
	agentReadyChan chan struct{}

	// agentRestarting is true if the agent is restarting.
	agentRestarting atomic.Bool

	// The OpAMP server to communicate with the Collector's OpAMP extension
	opampServer     server.OpAMPServer
	opampServerPort int

	telemetrySettings telemetrySettings

	featureGates map[string]struct{}
}

func NewSupervisor(logger *zap.Logger, cfg config.Supervisor) (*Supervisor, error) {
	s := &Supervisor{
		pidProvider:                    defaultPIDProvider{},
		hasNewConfig:                   make(chan struct{}, 1),
		agentConfigOwnTelemetrySection: &atomic.Value{},
		cfgState:                       &atomic.Value{},
		effectiveConfig:                &atomic.Value{},
		agentDescription:               &atomic.Value{},
		availableComponents:            &atomic.Value{},
		doneChan:                       make(chan struct{}),
		customMessageToServer:          make(chan *protobufs.CustomMessage, maxBufferedCustomMessages),
		agentConn:                      &atomic.Value{},
		featureGates:                   map[string]struct{}{},
		agentReady:                     atomic.Bool{},
		agentReadyChan:                 make(chan struct{}, 1),
	}
	if err := s.createTemplates(); err != nil {
		return nil, err
	}

	telSettings, err := initTelemetrySettings(logger, cfg.Telemetry)
	if err != nil {
		return nil, err
	}
	s.telemetrySettings = telSettings

	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("error validating config: %w", err)
	}
	s.config = cfg

	if err := os.MkdirAll(s.config.Storage.Directory, 0o700); err != nil {
		return nil, fmt.Errorf("error creating storage dir: %w", err)
	}

	s.configApplyTimeout = s.config.Agent.ConfigApplyTimeout

	return s, nil
}

func initTelemetrySettings(logger *zap.Logger, cfg config.Telemetry) (telemetrySettings, error) {
	readers := cfg.Metrics.Readers
	if cfg.Metrics.Level == configtelemetry.LevelNone {
		readers = []telemetryconfig.MetricReader{}
	}

	pcommonRes := pcommon.NewResource()
	for k, v := range cfg.Resource {
		pcommonRes.Attributes().PutStr(k, *v)
	}

	if _, ok := cfg.Resource[string(semconv.ServiceNameKey)]; !ok {
		pcommonRes.Attributes().PutStr(string(semconv.ServiceNameKey), "opamp-supervisor")
	}

	if _, ok := cfg.Resource[string(semconv.ServiceInstanceIDKey)]; !ok {
		instanceUUID, _ := uuid.NewRandom()
		instanceID := instanceUUID.String()
		pcommonRes.Attributes().PutStr(string(semconv.ServiceInstanceIDKey), instanceID)
	}

	// TODO currently we do not have the build info containing the version available to set semconv.ServiceVersionKey

	var attrs []telemetryconfig.AttributeNameValue
	for k, v := range pcommonRes.Attributes().All() {
		attrs = append(attrs, telemetryconfig.AttributeNameValue{Name: k, Value: v.Str()})
	}

	sch := semconv.SchemaURL

	ctx := context.Background()

	sdk, err := telemetryconfig.NewSDK(
		telemetryconfig.WithContext(ctx),
		telemetryconfig.WithOpenTelemetryConfiguration(
			telemetryconfig.OpenTelemetryConfiguration{
				MeterProvider: &telemetryconfig.MeterProvider{
					Readers: readers,
				},
				TracerProvider: &telemetryconfig.TracerProvider{
					Processors: cfg.Traces.Processors,
				},
				LoggerProvider: &telemetryconfig.LoggerProvider{
					Processors: cfg.Logs.Processors,
				},
				Resource: &telemetryconfig.Resource{
					SchemaUrl:  &sch,
					Attributes: attrs,
				},
			},
		),
	)
	if err != nil {
		return telemetrySettings{}, err
	}

	var lp log.LoggerProvider
	if len(cfg.Logs.Processors) > 0 {
		lp = sdk.LoggerProvider()
		logger = logger.WithOptions(zap.WrapCore(func(c zapcore.Core) zapcore.Core {
			core, err := zapcore.NewIncreaseLevelCore(zapcore.NewTee(
				c,
				otelzap.NewCore("github.com/open-telemetry/opentelemetry-collector-contrib/cmd/opampsupervisor",
					otelzap.WithLoggerProvider(lp),
				),
			), zap.NewAtomicLevelAt(cfg.Logs.Level))
			if err != nil {
				panic(err)
			}
			return core
		}))
	}

	return telemetrySettings{
		component.TelemetrySettings{
			Logger:         logger,
			TracerProvider: sdk.TracerProvider(),
			MeterProvider:  sdk.MeterProvider(),
			Resource:       pcommonRes,
		},
		lp,
	}, nil
}

func (s *Supervisor) Start() error {
	var err error
	s.persistentState, err = loadOrCreatePersistentState(s.persistentStateFilePath(), s.telemetrySettings.Logger)
	if err != nil {
		return err
	}

	if err = s.getFeatureGates(); err != nil {
		return fmt.Errorf("could not get feature gates from the Collector: %w", err)
	}

	if err = s.getBootstrapInfo(); err != nil {
		return fmt.Errorf("could not get bootstrap info from the Collector: %w", err)
	}

	s.telemetrySettings.Logger.Info("Supervisor starting",
		zap.String("id", s.persistentState.InstanceID.String()))

	if err = s.startOpAMP(); err != nil {
		return fmt.Errorf("cannot start OpAMP client: %w", err)
	}

	err = s.loadAndWriteInitialMergedConfig()
	if err != nil {
		return fmt.Errorf("failed loading initial config: %w", err)
	}

	flags := []string{
		"--config", s.agentConfigFilePath(),
	}
	featureGateFlag := s.getFeatureGateFlag()
	if len(featureGateFlag) > 0 {
		flags = append(flags, featureGateFlag...)
	}
	s.commander, err = commander.NewCommander(
		s.telemetrySettings.Logger,
		s.config.Storage.Directory,
		s.config.Agent,
		flags...,
	)
	if err != nil {
		return err
	}

	s.agentWG.Add(1)
	go func() {
		defer s.agentWG.Done()
		s.runAgentProcess()
	}()

	s.customMessageWG.Add(1)
	go func() {
		defer s.customMessageWG.Done()
		s.forwardCustomMessagesToServerLoop()
	}()

	return nil
}

func (s *Supervisor) getFeatureGates() error {
	cmd, err := commander.NewCommander(
		s.telemetrySettings.Logger,
		s.config.Storage.Directory,
		s.config.Agent,
		"featuregate",
	)
	if err != nil {
		return err
	}

	stdout, _, err := cmd.StartOneShot()
	if err != nil {
		return err
	}
	scanner := bufio.NewScanner(bytes.NewBuffer(stdout))

	// First line only contains headers, discard it.
	_ = scanner.Scan()
	for scanner.Scan() {
		line := scanner.Text()
		i := strings.Index(line, " ")
		flag := line[0:i]

		if flag == AllowNoPipelinesFeatureGate {
			s.featureGates[AllowNoPipelinesFeatureGate] = struct{}{}
		}
	}
	if err := scanner.Err(); err != nil {
		fmt.Fprintln(os.Stderr, "reading standard input:", err)
	}

	return nil
}

func (s *Supervisor) createTemplates() error {
	var err error

	if s.noopPipelineTemplate, err = template.New("nooppipeline").Parse(noopPipelineTpl); err != nil {
		return err
	}
	if s.extraTelemetryConfigTemplate, err = template.New("extratelemetryconfig").Parse(extraConfigTpl); err != nil {
		return err
	}
	if s.opampextensionTemplate, err = template.New("opampextension").Parse(opampextensionTpl); err != nil {
		return err
	}
	if s.ownTelemetryTemplate, err = template.New("owntelemetry").Parse(ownTelemetryTpl); err != nil {
		return err
	}

	return nil
}

// getBootstrapInfo obtains the Collector's agent description by
// starting a Collector with a specific config that only starts
// an OpAMP extension, obtains the agent description, then
// shuts down the Collector. This only needs to happen
// once per Collector binary.
func (s *Supervisor) getBootstrapInfo() (err error) {
	_, span := s.getTracer().Start(context.Background(), "GetBootstrapInfo")
	defer span.End()
	s.opampServerPort, err = s.getSupervisorOpAMPServerPort()
	if err != nil {
		span.SetStatus(codes.Error, fmt.Sprintf("Could not get supervisor opamp service port: %v", err))
		return err
	}

	bootstrapConfig, err := s.composeNoopConfig()
	if err != nil {
		span.SetStatus(codes.Error, fmt.Sprintf("Could not compose noop config config: %v", err))
		return err
	}

	err = os.WriteFile(s.agentConfigFilePath(), bootstrapConfig, 0o600)
	if err != nil {
		span.SetStatus(codes.Error, fmt.Sprintf("Failed to write agent config: %v", err))
		return fmt.Errorf("failed to write agent config: %w", err)
	}

	srv := server.New(newLoggerFromZap(s.telemetrySettings.Logger, "opamp-server"))

	done := make(chan error, 1)
	var connected atomic.Bool
	var doneReported atomic.Bool

	// Start a one-shot server to get the Collector's agent description
	// and available components using the Collector's OpAMP extension.
	err = srv.Start(flattenedSettings{
		endpoint: fmt.Sprintf("localhost:%d", s.opampServerPort),
		onConnecting: func(*http.Request) (bool, int) {
			connected.Store(true)
			return true, http.StatusOK
		},
		onMessage: func(_ serverTypes.Connection, message *protobufs.AgentToServer) *protobufs.ServerToAgent {
			response := &protobufs.ServerToAgent{}
			if message.GetAvailableComponents() != nil {
				s.setAvailableComponents(message.AvailableComponents)
			}

			if message.AgentDescription != nil {
				instanceIDSeen := false
				s.setAgentDescription(message.AgentDescription)
				identAttr := message.AgentDescription.IdentifyingAttributes

				for _, attr := range identAttr {
					if attr.Key == string(semconv.ServiceInstanceIDKey) {
						if attr.Value.GetStringValue() != s.persistentState.InstanceID.String() {
							done <- fmt.Errorf(
								"the Collector's instance ID (%s) does not match with the instance ID set by the Supervisor (%s): %w",
								attr.Value.GetStringValue(),
								s.persistentState.InstanceID.String(),
								errNonMatchingInstanceUID,
							)
							return response
						}
						instanceIDSeen = true
					}
				}

				if !instanceIDSeen {
					done <- errors.New("the Collector did not specify an instance ID in its AgentDescription message")
					return response
				}
			}

			// agent description must be defined
			_, ok := s.agentDescription.Load().(*protobufs.AgentDescription)
			if !ok {
				return response
			}

			// if available components have not been reported, agent description is sufficient to continue
			availableComponents, availableComponentsOk := s.availableComponents.Load().(*protobufs.AvailableComponents)
			if availableComponentsOk {
				// must have a full list of components if available components have been reported
				if availableComponents.GetComponents() != nil {
					if !doneReported.Load() {
						done <- nil
						doneReported.Store(true)
					}
				} else {
					// if we don't have a full component list, ask for it
					response.Flags = uint64(protobufs.ServerToAgentFlags_ServerToAgentFlags_ReportAvailableComponents)
				}
				return response
			}

			// need to only report done once, not on each message - otherwise, we get a hung thread
			if !doneReported.Load() {
				done <- nil
				doneReported.Store(true)
			}

			return response
		},
	}.toServerSettings())
	if err != nil {
		span.SetStatus(codes.Error, fmt.Sprintf("Could not start OpAMP server: %v", err))
		return err
	}

	defer func() {
		if stopErr := srv.Stop(context.Background()); stopErr != nil {
			err = errors.Join(err, fmt.Errorf("error when stopping the opamp server: %w", stopErr))
		}
	}()

	flags := []string{
		"--config", s.agentConfigFilePath(),
	}
	featuregateFlag := s.getFeatureGateFlag()
	if len(featuregateFlag) > 0 {
		flags = append(flags, featuregateFlag...)
	}
	cmd, err := commander.NewCommander(
		s.telemetrySettings.Logger,
		s.config.Storage.Directory,
		s.config.Agent,
		flags...,
	)
	if err != nil {
		span.SetStatus(codes.Error, fmt.Sprintf("Could not start Agent: %v", err))
		return err
	}

	if err = cmd.Start(context.Background()); err != nil {
		span.SetStatus(codes.Error, fmt.Sprintf("Could not start Agent: %v", err))
		return err
	}

	defer func() {
		if stopErr := cmd.Stop(context.Background()); stopErr != nil {
			err = errors.Join(err, fmt.Errorf("error when stopping the collector: %w", stopErr))
		}
	}()

	select {
	case <-time.After(s.config.Agent.BootstrapTimeout):
		if connected.Load() {
			msg := "collector connected but never responded with an AgentDescription message"
			span.SetStatus(codes.Error, msg)
			return errors.New(msg)
		} else {
			msg := "collector's OpAMP client never connected to the Supervisor"
			span.SetStatus(codes.Error, msg)
			return errors.New(msg)
		}
	case err = <-done:
		if errors.Is(err, errNonMatchingInstanceUID) {
			// try to report the issue to the OpAMP server
			if startOpAMPErr := s.startOpAMPClient(); startOpAMPErr == nil {
				defer func(s *Supervisor) {
					if stopErr := s.stopOpAMPClient(); stopErr != nil {
						s.telemetrySettings.Logger.Error("Could not stop OpAmp client", zap.Error(stopErr))
					}
				}(s)
				if healthErr := s.opampClient.SetHealth(&protobufs.ComponentHealth{
					Healthy:   false,
					LastError: err.Error(),
				}); healthErr != nil {
					s.telemetrySettings.Logger.Error("Could not report health to OpAMP server", zap.Error(healthErr))
				}
			} else {
				s.telemetrySettings.Logger.Error("Could not start OpAMP client to report health to server", zap.Error(startOpAMPErr))
			}
		}
		if err != nil {
			s.telemetrySettings.Logger.Error("Could not complete bootstrap", zap.Error(err))
			span.SetStatus(codes.Error, err.Error())
		} else {
			span.SetStatus(codes.Ok, "")
		}
		return err
	}
}

func (s *Supervisor) startOpAMP() error {
	if err := s.startOpAMPClient(); err != nil {
		return err
	}

	if err := s.startOpAMPServer(); err != nil {
		return err
	}

	return nil
}

func (s *Supervisor) startOpAMPClient() error {
	// determine if we need to load a TLS config or not
	var tlsConfig *tls.Config
	parsedURL, err := url.Parse(s.config.Server.Endpoint)
	if err != nil {
		return fmt.Errorf("parse server endpoint: %w", err)
	}
	if parsedURL.Scheme == "wss" || parsedURL.Scheme == "https" {
		tlsConfig, err = s.config.Server.TLS.LoadTLSConfig(context.Background())
		if err != nil {
			return err
		}
	}
	logger := newLoggerFromZap(s.telemetrySettings.Logger, "opamp-client")
	switch parsedURL.Scheme {
	case "ws", "wss":
		s.opampClient = client.NewWebSocket(logger)
	case "http", "https":
		s.opampClient = client.NewHTTP(logger)
	default:
		return fmt.Errorf("unsupported scheme in server endpoint: %q", parsedURL.Scheme)
	}

	s.telemetrySettings.Logger.Debug("Connecting to OpAMP server...", zap.String("endpoint", s.config.Server.Endpoint), zap.Any("headers", s.config.Server.Headers))
	settings := types.StartSettings{
		OpAMPServerURL:     s.config.Server.Endpoint,
		Header:             s.config.Server.Headers,
		TLSConfig:          tlsConfig,
		InstanceUid:        types.InstanceUid(s.persistentState.InstanceID),
		RemoteConfigStatus: s.persistentState.GetLastRemoteConfigStatus(),
		Callbacks: types.Callbacks{
			OnConnect: func(context.Context) {
				s.telemetrySettings.Logger.Info("Connected to the server.")
			},
			OnConnectFailed: func(_ context.Context, err error) {
				s.telemetrySettings.Logger.Error("Failed to connect to the server", zap.Error(err))
			},
			OnError: func(_ context.Context, err *protobufs.ServerErrorResponse) {
				s.telemetrySettings.Logger.Error("Server returned an error response", zap.String("message", err.ErrorMessage))
			},
			OnMessage: s.onMessage,
			OnOpampConnectionSettings: func(ctx context.Context, settings *protobufs.OpAMPConnectionSettings) error {
				//nolint:errcheck
				go s.onOpampConnectionSettings(ctx, settings)
				return nil
			},
			OnCommand: func(_ context.Context, command *protobufs.ServerToAgentCommand) error {
				cmdType := command.GetType()
				if *cmdType.Enum() == protobufs.CommandType_CommandType_Restart {
					return s.handleRestartCommand()
				}
				return nil
			},
			SaveRemoteConfigStatus: func(context.Context, *protobufs.RemoteConfigStatus) {
				// TODO: https://github.com/open-telemetry/opentelemetry-collector-contrib/issues/21079
			},
			GetEffectiveConfig: func(context.Context) (*protobufs.EffectiveConfig, error) {
				return s.createEffectiveConfigMsg(), nil
			},
		},
		Capabilities: s.config.Capabilities.SupportedCapabilities(),
	}
	ad := s.agentDescription.Load().(*protobufs.AgentDescription)
	if err := s.opampClient.SetAgentDescription(ad); err != nil {
		return err
	}

	if err := s.opampClient.SetHealth(&protobufs.ComponentHealth{Healthy: false}); err != nil {
		return err
	}

	if ac, ok := s.availableComponents.Load().(*protobufs.AvailableComponents); ok && ac != nil {
		if err := s.opampClient.SetAvailableComponents(ac); err != nil {
			return err
		}
	}

	s.telemetrySettings.Logger.Debug("Starting OpAMP client...")
	if err := s.opampClient.Start(context.Background(), settings); err != nil {
		return err
	}
	s.telemetrySettings.Logger.Debug("OpAMP client started.")

	return nil
}

// startOpAMPServer starts an OpAMP server that will communicate
// with an OpAMP extension running inside a Collector to receive
// data from inside the Collector. The internal server's lifetime is not
// matched to the Collector's process, but may be restarted
// depending on information received by the Supervisor from the remote
// OpAMP server.
func (s *Supervisor) startOpAMPServer() error {
	s.opampServer = server.New(newLoggerFromZap(s.telemetrySettings.Logger, "opamp-server"))

	var err error
	s.opampServerPort, err = s.getSupervisorOpAMPServerPort()
	if err != nil {
		return err
	}

	s.telemetrySettings.Logger.Debug("Starting OpAMP server...")

	connected := &atomic.Bool{}

	err = s.opampServer.Start(flattenedSettings{
		endpoint: fmt.Sprintf("localhost:%d", s.opampServerPort),
		onConnecting: func(*http.Request) (bool, int) {
			// Only allow one agent to be connected the this server at a time.
			alreadyConnected := connected.Swap(true)
			return !alreadyConnected, http.StatusConflict
		},
		onMessage: s.handleAgentOpAMPMessage,
		onConnectionClose: func(serverTypes.Connection) {
			connected.Store(false)
		},
	}.toServerSettings())
	if err != nil {
		return err
	}

	s.telemetrySettings.Logger.Debug("OpAMP server started.")

	return nil
}

func (s *Supervisor) handleAgentOpAMPMessage(conn serverTypes.Connection, message *protobufs.AgentToServer) *protobufs.ServerToAgent {
	s.agentConn.Store(conn)

	s.telemetrySettings.Logger.Debug("Received OpAMP message from the agent")
	if message.AgentDescription != nil {
		s.setAgentDescription(message.AgentDescription)
	}

	if message.EffectiveConfig != nil {
		if cfg, ok := message.EffectiveConfig.GetConfigMap().GetConfigMap()[""]; ok {
			s.telemetrySettings.Logger.Debug("Received effective config from agent")
			s.effectiveConfig.Store(string(cfg.Body))
			err := s.opampClient.UpdateEffectiveConfig(context.Background())
			if err != nil {
				s.telemetrySettings.Logger.Error("The OpAMP client failed to update the effective config", zap.Error(err))
			}
		} else {
			s.telemetrySettings.Logger.Error("Got effective config message, but the instance config was not present. Ignoring effective config.")
		}
	}

	// Proxy client capabilities to server
	if message.CustomCapabilities != nil {
		err := s.opampClient.SetCustomCapabilities(message.CustomCapabilities)
		if err != nil {
			s.telemetrySettings.Logger.Error("Failed to send custom capabilities to OpAMP server")
		}
	}

	// Proxy agent custom messages to server
	if message.CustomMessage != nil {
		select {
		case s.customMessageToServer <- message.CustomMessage:
		default:
			s.telemetrySettings.Logger.Warn(
				"Buffer full, skipping forwarding custom message to server",
				zap.String("capability", message.CustomMessage.Capability),
				zap.String("type", message.CustomMessage.Type),
			)
		}
	}

	if message.Health != nil {
		s.telemetrySettings.Logger.Debug("Received health status from agent", zap.Bool("healthy", message.Health.Healthy))
		s.lastHealthFromClient.Store(message.Health)
		err := s.opampClient.SetHealth(message.Health)
		if err != nil {
			s.telemetrySettings.Logger.Error("Could not report health to OpAMP server", zap.Error(err))
		}
		if !s.agentReady.Load() && message.Health.Healthy {
			s.markAgentReady()
		}
	}

	return &protobufs.ServerToAgent{}
}

func (s *Supervisor) forwardCustomMessagesToServerLoop() {
	for {
		select {
		case cm := <-s.customMessageToServer:
			for {
				sendingChan, err := s.opampClient.SendCustomMessage(cm)
				switch {
				case errors.Is(err, types.ErrCustomMessagePending):
					s.telemetrySettings.Logger.Debug("Custom message pending, waiting to send...")
					<-sendingChan
					continue
				case err == nil: // OK
					s.telemetrySettings.Logger.Debug("Custom message forwarded to server.")
				default:
					s.telemetrySettings.Logger.Error("Failed to send custom message to OpAMP server")
				}
				break
			}
		case <-s.doneChan:
			return
		}
	}
}

// setAgentDescription sets the agent description, merging in any user-specified attributes from the supervisor configuration.
func (s *Supervisor) setAgentDescription(ad *protobufs.AgentDescription) {
	ad.IdentifyingAttributes = applyKeyValueOverrides(s.config.Agent.Description.IdentifyingAttributes, ad.IdentifyingAttributes)
	ad.NonIdentifyingAttributes = applyKeyValueOverrides(s.config.Agent.Description.NonIdentifyingAttributes, ad.NonIdentifyingAttributes)
	s.agentDescription.Store(ad)
}

// setAvailableComponents sets the available components of the OpAMP agent
func (s *Supervisor) setAvailableComponents(ac *protobufs.AvailableComponents) {
	s.availableComponents.Store(ac)
}

// applyKeyValueOverrides merges the overrides map into the array of key value pairs.
// If a key from overrides already exists in the array of key value pairs, it is overwritten by the value from the overrides map.
// An array of KeyValue pair is returned, with each key value pair having a distinct key.
func applyKeyValueOverrides(overrides map[string]string, orig []*protobufs.KeyValue) []*protobufs.KeyValue {
	kvMap := make(map[string]*protobufs.KeyValue, len(orig)+len(overrides))

	for _, kv := range orig {
		kvMap[kv.Key] = kv
	}

	for k, v := range overrides {
		kvMap[k] = &protobufs.KeyValue{
			Key: k,
			Value: &protobufs.AnyValue{
				Value: &protobufs.AnyValue_StringValue{
					StringValue: v,
				},
			},
		}
	}

	// Sort keys for stable output, makes it easier to test.
	keys := make([]string, 0, len(kvMap))
	for k := range kvMap {
		keys = append(keys, k)
	}

	sort.Strings(keys)

	kvOut := make([]*protobufs.KeyValue, 0, len(kvMap))
	for _, k := range keys {
		v := kvMap[k]
		kvOut = append(kvOut, v)
	}

	return kvOut
}

func (s *Supervisor) stopOpAMPClient() error {
	s.telemetrySettings.Logger.Debug("Stopping OpAMP client...")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := s.opampClient.Stop(ctx)
	// TODO(srikanthccv): remove context.DeadlineExceeded after https://github.com/open-telemetry/opamp-go/pull/213
	if err != nil && !errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	s.telemetrySettings.Logger.Debug("OpAMP client stopped.")

	return nil
}

func (*Supervisor) getHeadersFromSettings(protoHeaders *protobufs.Headers) http.Header {
	headers := make(http.Header)
	for _, header := range protoHeaders.Headers {
		headers.Add(header.Key, header.Value)
	}
	return headers
}

func (s *Supervisor) onOpampConnectionSettings(_ context.Context, settings *protobufs.OpAMPConnectionSettings) error {
	if settings == nil {
		s.telemetrySettings.Logger.Debug("Received ConnectionSettings request with nil settings")
		return nil
	}

	newServerConfig := config.OpAMPServer{}

	if settings.DestinationEndpoint != "" {
		newServerConfig.Endpoint = settings.DestinationEndpoint
	}
	if settings.Headers != nil {
		newServerConfig.Headers = s.getHeadersFromSettings(settings.Headers)
	}
	if settings.Certificate != nil {
		if len(settings.Certificate.CaCert) != 0 {
			newServerConfig.TLS.CAPem = configopaque.String(settings.Certificate.CaCert)
		}
		if len(settings.Certificate.Cert) != 0 {
			newServerConfig.TLS.CertPem = configopaque.String(settings.Certificate.Cert)
		}
		if len(settings.Certificate.PrivateKey) != 0 {
			newServerConfig.TLS.KeyPem = configopaque.String(settings.Certificate.PrivateKey)
		}
	} else {
		newServerConfig.TLS = configtls.NewDefaultClientConfig()
		newServerConfig.TLS.InsecureSkipVerify = true
	}

	if err := newServerConfig.Validate(); err != nil {
		s.telemetrySettings.Logger.Error("New OpAMP settings resulted in invalid configuration", zap.Error(err))
		return err
	}

	if err := s.stopOpAMPClient(); err != nil {
		s.telemetrySettings.Logger.Error("Cannot stop the OpAMP client", zap.Error(err))
		return err
	}

	// take a copy of the current OpAMP server config
	oldServerConfig := s.config.Server
	// update the OpAMP server config
	s.config.Server = newServerConfig

	if err := s.startOpAMPClient(); err != nil {
		s.telemetrySettings.Logger.Error("Cannot connect to the OpAMP server using the new settings", zap.Error(err))
		// revert the OpAMP server config
		s.config.Server = oldServerConfig
		// start the OpAMP client with the old settings
		if err := s.startOpAMPClient(); err != nil {
			s.telemetrySettings.Logger.Error("Cannot reconnect to the OpAMP server after restoring old settings", zap.Error(err))
			return err
		}
	}
	return nil
}

func (s *Supervisor) addSpecialConfigFiles() {
	missingSpecialConfigFiles := make(map[config.SpecialConfigFile]struct{}, len(config.SpecialConfigFiles))
	for _, file := range config.SpecialConfigFiles {
		missingSpecialConfigFiles[file] = struct{}{}
	}
	for _, file := range s.config.Agent.ConfigFiles {
		if strings.HasPrefix(file, "$") {
			delete(missingSpecialConfigFiles, config.SpecialConfigFile(file))
		}
	}

	// if missing builtin, add it to the beginning
	if _, ok := missingSpecialConfigFiles[config.SpecialConfigFileOwnTelemetry]; ok {
		s.config.Agent.ConfigFiles = slices.Insert(s.config.Agent.ConfigFiles, 0, string(config.SpecialConfigFileOwnTelemetry))
	}

	// if missing opamp extension, add it to the end
	if _, ok := missingSpecialConfigFiles[config.SpecialConfigFileOpAMPExtension]; ok {
		s.config.Agent.ConfigFiles = append(s.config.Agent.ConfigFiles, string(config.SpecialConfigFileOpAMPExtension))
	}

	// if missing remote config, add it to the end
	if _, ok := missingSpecialConfigFiles[config.SpecialConfigFileRemoteConfig]; ok {
		s.config.Agent.ConfigFiles = append(s.config.Agent.ConfigFiles, string(config.SpecialConfigFileRemoteConfig))
	}
}

func (s *Supervisor) composeNoopPipeline() ([]byte, error) {
	var cfg bytes.Buffer

	if !s.isFeatureGateSupported(AllowNoPipelinesFeatureGate) {
		err := s.noopPipelineTemplate.Execute(&cfg, map[string]any{})
		if err != nil {
			return nil, err
		}
	}

	return cfg.Bytes(), nil
}

func (s *Supervisor) createRemoteConfigComposers(incomingConfig *protobufs.AgentRemoteConfig) []configComposer {
	if !s.config.Capabilities.AcceptsRemoteConfig {
		return []configComposer{}
	}

	hasIncomingConfigMap := len(incomingConfig.GetConfig().GetConfigMap()) != 0
	remoteConfigComposers := []configComposer{}
	if hasIncomingConfigMap {
		c := incomingConfig.GetConfig()

		// Sort to make sure the order of merging is stable.
		var names []string
		for name := range c.ConfigMap {
			if name == "" {
				// skip instance config
				continue
			}
			names = append(names, name)
		}

		sort.Strings(names)

		// Append instance config as the last item.
		names = append(names, "")

		// Merge received configs.
		for _, name := range names {
			item := c.ConfigMap[name]
			if item == nil {
				continue
			}
			remoteConfigComposers = append(remoteConfigComposers, func() []byte {
				return item.Body
			})
		}
	}
	return remoteConfigComposers
}

func (s *Supervisor) composeNoopConfig() ([]byte, error) {
	k := koanf.New("::")

	cfg, err := s.composeNoopPipeline()
	if err != nil {
		return nil, err
	}
	if err := k.Load(rawbytes.Provider(cfg), yaml.Parser(), koanf.WithMergeFunc(configMergeFunc)); err != nil {
		return nil, err
	}
	if err := k.Load(rawbytes.Provider(s.composeOpAMPExtensionConfig()), yaml.Parser(), koanf.WithMergeFunc(configMergeFunc)); err != nil {
		return nil, err
	}

	return k.Marshal(yaml.Parser())
}

func (s *Supervisor) composeOwnTelemetryConfig() []byte {
	ownMetricsCfg, ok := s.agentConfigOwnTelemetrySection.Load().(string)
	if !ok {
		return nil
	}
	return []byte(ownMetricsCfg)
}

func (s *Supervisor) composeExtraTelemetryConfig() []byte {
	var cfg bytes.Buffer
	resourceAttrs := map[string]string{}
	ad := s.agentDescription.Load().(*protobufs.AgentDescription)
	for _, attr := range ad.IdentifyingAttributes {
		resourceAttrs[attr.Key] = attr.Value.GetStringValue()
	}
	for _, attr := range ad.NonIdentifyingAttributes {
		resourceAttrs[attr.Key] = attr.Value.GetStringValue()
	}
	tplVars := map[string]any{
		"ResourceAttributes": resourceAttrs,
		"SupervisorPort":     s.opampServerPort,
	}
	err := s.extraTelemetryConfigTemplate.Execute(
		&cfg,
		tplVars,
	)
	if err != nil {
		s.telemetrySettings.Logger.Error("Could not compose local config", zap.Error(err))
		return nil
	}

	return cfg.Bytes()
}

func (s *Supervisor) composeOpAMPExtensionConfig() []byte {
	orphanPollInterval := 5 * time.Second
	if s.config.Agent.OrphanDetectionInterval > 0 {
		orphanPollInterval = s.config.Agent.OrphanDetectionInterval
	}

	var cfg bytes.Buffer
	tplVars := map[string]any{
		"InstanceUid":                s.persistentState.InstanceID.String(),
		"SupervisorPort":             s.opampServerPort,
		"PID":                        s.pidProvider.PID(),
		"PPIDPollInterval":           orphanPollInterval,
		"ReportsAvailableComponents": s.config.Capabilities.ReportsAvailableComponents,
	}
	err := s.opampextensionTemplate.Execute(
		&cfg,
		tplVars,
	)
	if err != nil {
		s.telemetrySettings.Logger.Error("Could not compose local config", zap.Error(err))
		return nil
	}

	return cfg.Bytes()
}

type configComposer func() []byte

func (s *Supervisor) composeAgentConfigFiles(incomingConfig *protobufs.AgentRemoteConfig) ([]byte, error) {
	conf := koanf.New("::")

	specialConfigComposers := map[config.SpecialConfigFile][]configComposer{
		config.SpecialConfigFileOwnTelemetry:   {s.composeOwnTelemetryConfig, s.composeExtraTelemetryConfig},
		config.SpecialConfigFileOpAMPExtension: {s.composeOpAMPExtensionConfig},
		config.SpecialConfigFileRemoteConfig:   s.createRemoteConfigComposers(incomingConfig),
	}

	for _, file := range s.config.Agent.ConfigFiles {
		// The special config files should always be valid yaml and most of them
		// will be due to the typing in the Supervisor's config struct and its
		// validation function.
		// The special config file for the remote configuration is the exception
		// here: it could be invalid yaml. In case it is, the `koanf` loader
		// will return an error.
		// Normal config files with invalid yaml should be just ignored.
		if strings.HasPrefix(file, "$") {
			cfgProviders := specialConfigComposers[config.SpecialConfigFile(file)]
			for _, cfgProvider := range cfgProviders {
				cfgBytes := cfgProvider()
				err := conf.Load(rawbytes.Provider(cfgBytes), yaml.Parser(), koanf.WithMergeFunc(configMergeFunc))
				if err != nil {
					s.telemetrySettings.Logger.Error("Could not merge special config file", zap.String("specialConfig", file), zap.Error(err))
					return nil, err
				}
			}
			continue
		}

		cfgBytes, err := os.ReadFile(file)
		if err != nil {
			s.telemetrySettings.Logger.Error("Could not read local config file", zap.Error(err))
			continue
		}

		err = conf.Load(rawbytes.Provider(cfgBytes), yaml.Parser(), koanf.WithMergeFunc(configMergeFunc))
		if err != nil {
			s.telemetrySettings.Logger.Error("Could not merge local config file: "+file, zap.Error(err))
			continue
		}
	}

	b, err := conf.Marshal(yaml.Parser())
	if err != nil {
		s.telemetrySettings.Logger.Error("Could not marshal merged local config files", zap.Error(err))
		return []byte(""), err
	}
	return b, nil
}

// loadAndWriteInitialMergedConfig loads and writes the initial config by
// merging the last received remote config, last received own metrics config,
// and any local configs.
func (s *Supervisor) loadAndWriteInitialMergedConfig() error {
	// load the last received remote config
	s.loadRemoteConfig()

	// load the last received own telemetry config
	s.loadLastReceivedOwnTelemetryConfig()

	// compose the initial merged config
	_, err := s.composeMergedConfig(s.remoteConfig)
	if err != nil {
		return fmt.Errorf("could not compose initial merged config: %w", err)
	}

	// write the initial merged config to disk
	cfgState := s.cfgState.Load().(*configState)
	if err := os.WriteFile(s.agentConfigFilePath(), []byte(cfgState.mergedConfig), 0o600); err != nil {
		s.telemetrySettings.Logger.Error("Failed to write agent config.", zap.Error(err))
	}

	return nil
}

// loadRemoteConfig loads the last received remote config from file if the capability is supported.
func (s *Supervisor) loadRemoteConfig() {
	if !s.config.Capabilities.AcceptsRemoteConfig {
		s.telemetrySettings.Logger.Debug("Remote config is not supported, will not attempt to load config from file")
		return
	}

	// Try to load the last received remote config if it exists.
	var lastRecvRemoteConfig []byte
	var err error
	lastRecvRemoteConfig, err = os.ReadFile(filepath.Join(s.config.Storage.Directory, lastRecvRemoteConfigFile))
	switch {
	case err == nil:
		config := &protobufs.AgentRemoteConfig{}
		err = proto.Unmarshal(lastRecvRemoteConfig, config)
		if err != nil {
			s.telemetrySettings.Logger.Error("Cannot parse last received remote config", zap.Error(err))
		} else {
			s.remoteConfig = config
		}
	case errors.Is(err, os.ErrNotExist):
		s.telemetrySettings.Logger.Info("No last received remote config found")
	default:
		s.telemetrySettings.Logger.Error("error while reading last received config", zap.Error(err))
	}
}

// loadLastReceivedOwnTelemetryConfig loads the last received own telemetry config from file if the capability is supported.
func (s *Supervisor) loadLastReceivedOwnTelemetryConfig() {
	// If none of the own telemetry capabilities are supported, do nothing.
	if !s.config.Capabilities.ReportsOwnMetrics && !s.config.Capabilities.ReportsOwnTraces && !s.config.Capabilities.ReportsOwnLogs {
		s.telemetrySettings.Logger.Debug("Own telemetry is not supported, will not attempt to load config from file")
		return
	}

	// Try to load the last received own metrics config if it exists.
	var lastRecvOwnTelemetryConfig []byte
	var err error
	lastRecvOwnTelemetryConfig, err = os.ReadFile(filepath.Join(s.config.Storage.Directory, lastRecvOwnTelemetryConfigFile))
	if err == nil {
		set := &protobufs.ConnectionSettingsOffers{}
		err = proto.Unmarshal(lastRecvOwnTelemetryConfig, set)
		if err != nil {
			s.telemetrySettings.Logger.Error("Cannot parse last received own telemetry config", zap.Error(err))
		} else {
			s.setupOwnTelemetry(context.Background(), set)
		}
	}
}

// createEffectiveConfigMsg create an EffectiveConfig with the content of the
// current effective config.
func (s *Supervisor) createEffectiveConfigMsg() *protobufs.EffectiveConfig {
	cfgStr, ok := s.effectiveConfig.Load().(string)
	if !ok {
		cfgState, ok := s.cfgState.Load().(*configState)
		if !ok {
			cfgStr = ""
		} else {
			cfgStr = cfgState.mergedConfig
		}
	}

	cfg := &protobufs.EffectiveConfig{
		ConfigMap: &protobufs.AgentConfigMap{
			ConfigMap: map[string]*protobufs.AgentConfigFile{
				"": {Body: []byte(cfgStr)},
			},
		},
	}

	return cfg
}

func (*Supervisor) updateOwnTelemetryData(data map[string]any, signal string, settings *protobufs.TelemetryConnectionSettings) map[string]any {
	if settings == nil || settings.DestinationEndpoint == "" {
		return data
	}
	data[fmt.Sprintf("%sEndpoint", signal)] = settings.DestinationEndpoint
	data[fmt.Sprintf("%sHeaders", signal)] = []protobufs.Header{}

	if settings.Headers != nil {
		data[fmt.Sprintf("%sHeaders", signal)] = settings.Headers.Headers
	}
	return data
}

func (s *Supervisor) setupOwnTelemetry(_ context.Context, settings *protobufs.ConnectionSettingsOffers) (configChanged bool) {
	var cfg bytes.Buffer

	data := s.updateOwnTelemetryData(map[string]any{}, "Metrics", settings.GetOwnMetrics())
	data = s.updateOwnTelemetryData(data, "Logs", settings.GetOwnLogs())
	data = s.updateOwnTelemetryData(data, "Traces", settings.GetOwnTraces())

	if len(data) == 0 {
		s.telemetrySettings.Logger.Debug("Disabling own telemetry pipeline in the config")
	} else {
		err := s.ownTelemetryTemplate.Execute(&cfg, data)
		if err != nil {
			s.telemetrySettings.Logger.Error("Could not setup own telemetry", zap.Error(err))
			return
		}
	}
	s.agentConfigOwnTelemetrySection.Store(cfg.String())

	// Need to recalculate the Agent config so that the metric config is included in it.
	configChanged, err := s.composeMergedConfig(s.remoteConfig)
	if err != nil {
		s.telemetrySettings.Logger.Error("Error composing merged config for own metrics. Ignoring agent self metrics config", zap.Error(err))
		return
	}

	return configChanged
}

// composeMergedConfig composes the merged config from multiple sources:
// 1) the remote config from OpAMP Server
// 2) the own metrics config section
// 3) the local override config that is hard-coded in the Supervisor.
func (s *Supervisor) composeMergedConfig(incomingConfig *protobufs.AgentRemoteConfig) (configChanged bool, err error) {
	k := koanf.New("::")

	s.addSpecialConfigFiles()

	hasIncomingConfigMap := len(incomingConfig.GetConfig().GetConfigMap()) != 0
	if s.config.Capabilities.AcceptsRemoteConfig && !hasIncomingConfigMap {
		// Add noop pipeline
		var noopConfig []byte
		noopConfig, err = s.composeNoopPipeline()
		if err != nil {
			return false, fmt.Errorf("could not compose noop pipeline: %w", err)
		}

		if err = k.Load(rawbytes.Provider(noopConfig), yaml.Parser(), koanf.WithMergeFunc(configMergeFunc)); err != nil {
			return false, fmt.Errorf("could not merge noop pipeline: %w", err)
		}
	}

	agentConfigBytes, err := s.composeAgentConfigFiles(incomingConfig)
	if err != nil {
		return false, err
	}

	err = k.Load(rawbytes.Provider(agentConfigBytes), yaml.Parser(), koanf.WithMergeFunc(configMergeFunc))
	if err != nil {
		return false, err
	}

	// The merged final result is our new merged config.
	newMergedConfigBytes, err := k.Marshal(yaml.Parser())
	if err != nil {
		return false, err
	}

	// Check if supervisor's merged config is changed.

	newConfigState := &configState{
		mergedConfig:     string(newMergedConfigBytes),
		configMapIsEmpty: (incomingConfig != nil && !hasIncomingConfigMap),
	}

	configChanged = false

	oldConfigState := s.cfgState.Swap(newConfigState)
	if oldConfigState == nil || !oldConfigState.(*configState).equal(newConfigState) {
		s.telemetrySettings.Logger.Debug("Merged config changed.")
		configChanged = true
	}

	return configChanged, nil
}

func (s *Supervisor) handleRestartCommand() error {
	s.agentRestarting.Store(true)
	defer s.agentRestarting.Store(false)
	s.telemetrySettings.Logger.Debug("Received restart command")
	err := s.commander.Restart(context.Background())
	if err != nil {
		s.telemetrySettings.Logger.Error("Could not restart agent process", zap.Error(err))
	}
	s.resetAgentReady()
	return err
}

func (s *Supervisor) startAgent() (agentStartStatus, error) {
	if s.cfgState.Load().(*configState).configMapIsEmpty {
		// Don't start the agent if there is no config to run
		s.telemetrySettings.Logger.Info("No config present, not starting agent.")
		// need to manually trigger updating effective config
		err := s.opampClient.UpdateEffectiveConfig(context.Background())
		if err != nil {
			s.telemetrySettings.Logger.Error("The OpAMP client failed to update the effective config", zap.Error(err))
		}
		return agentNotStarting, nil
	}

	err := s.commander.Start(context.Background())
	if err != nil {
		s.telemetrySettings.Logger.Error("Cannot start the agent", zap.Error(err))
		startErr := fmt.Errorf("cannot start the agent: %w", err)
		err = s.opampClient.SetHealth(&protobufs.ComponentHealth{Healthy: false, LastError: startErr.Error()})
		if err != nil {
			s.telemetrySettings.Logger.Error("Failed to report OpAMP client health", zap.Error(err))
		}
		return "", startErr
	}

	return agentStarting, nil
}

func (s *Supervisor) runAgentProcess() {
	if _, err := os.Stat(s.agentConfigFilePath()); err == nil {
		// We have an effective config file saved previously. Use it to start the agent.
		s.telemetrySettings.Logger.Debug("Effective config found, starting agent initial time")
		_, err := s.startAgent()
		if err != nil {
			s.telemetrySettings.Logger.Error("starting agent failed", zap.Error(err))
			s.saveAndReportConfigStatus(protobufs.RemoteConfigStatuses_RemoteConfigStatuses_FAILED, err.Error())
		}
	}

	restartTimer := time.NewTimer(0)
	restartTimer.Stop()

	configApplyTimeoutTimer := time.NewTimer(0)
	configApplyTimeoutTimer.Stop()

	for {
		select {
		case <-s.hasNewConfig:
			s.lastHealthFromClient.Store(nil)
			s.telemetrySettings.Logger.Debug("agent has new config", zap.String("previous_health", s.lastHealthFromClient.Load().String()))
			if !configApplyTimeoutTimer.Stop() {
				select {
				case <-configApplyTimeoutTimer.C: // Try to drain the channel
				default:
				}
			}
			configApplyTimeoutTimer.Reset(s.config.Agent.ConfigApplyTimeout)
			restartTimer.Stop()

			if s.config.Agent.UseHUPConfigReload {
				if err := s.hupReloadAgent(); err != nil {
					s.telemetrySettings.Logger.Error("Failed to HUP restart agent", zap.Error(err))
					s.saveAndReportConfigStatus(protobufs.RemoteConfigStatuses_RemoteConfigStatuses_FAILED, err.Error())
					continue
				}
			} else {
				s.stopAgentApplyConfig()
			}

			s.telemetrySettings.Logger.Debug("Agent is not running, starting new instance")
			// This call to [startAgent] is useful for both the normal config reload
			// and the HUP one. It takes care of not starting the agent if the config
			// is empty and also of starting it when the config changes from empty
			// to non-empty.
			status, err := s.startAgent()
			if err != nil {
				s.telemetrySettings.Logger.Error("starting agent with new config failed", zap.Error(err))
				s.saveAndReportConfigStatus(protobufs.RemoteConfigStatuses_RemoteConfigStatuses_FAILED, err.Error())
			}
			if status == agentNotStarting {
				// not starting agent because of nop config: clear timer, report applied status, report healthy status
				configApplyTimeoutTimer.Stop()
				s.saveAndReportConfigStatus(protobufs.RemoteConfigStatuses_RemoteConfigStatuses_APPLIED, "")
				if err := s.opampClient.SetHealth(&protobufs.ComponentHealth{Healthy: true, LastError: ""}); err != nil {
					s.telemetrySettings.Logger.Error("Could not report healthy status to OpAMP server", zap.Error(err))
				}
				// need to clear exit channel to avoid triggering `s.commander.Exited()` case
				// because we stopped the agent and aren't restarting it this case will trigger
				// and report an unhealthy status (collector would be healthy, just choosing to not run)
				if len(s.commander.Exited()) > 0 {
					select {
					case <-s.commander.Exited():
					default:
					}
				}
			}
		case <-s.commander.Exited():
			// the agent process exit is expected for restart command and will not attempt to restart
			if s.agentRestarting.Load() {
				continue
			}

			s.telemetrySettings.Logger.Debug("Agent process exited unexpectedly. Will restart in a bit...", zap.Int("pid", s.commander.Pid()), zap.Int("exit_code", s.commander.ExitCode()))
			errMsg := fmt.Sprintf(
				"Agent process PID=%d exited unexpectedly, exit code=%d. Will restart in a bit...",
				s.commander.Pid(), s.commander.ExitCode(),
			)
			err := s.opampClient.SetHealth(&protobufs.ComponentHealth{Healthy: false, LastError: errMsg})
			if err != nil {
				s.telemetrySettings.Logger.Error("Could not report health to OpAMP server", zap.Error(err))
			}

			// TODO: decide why the agent stopped. If it was due to bad config, report it to server.
			// https://github.com/open-telemetry/opentelemetry-collector-contrib/issues/21079

			// Wait 5 seconds before starting again.
			if !restartTimer.Stop() {
				select {
				case <-restartTimer.C: // Try to drain the channel
				default:
				}
			}
			restartTimer.Reset(5 * time.Second)

		case <-restartTimer.C:
			s.telemetrySettings.Logger.Debug("Agent starting after start backoff")
			_, err := s.startAgent()
			if err != nil {
				s.telemetrySettings.Logger.Error("restarting agent failed", zap.Error(err))
				s.saveAndReportConfigStatus(protobufs.RemoteConfigStatuses_RemoteConfigStatuses_FAILED, err.Error())
			}

		case <-configApplyTimeoutTimer.C:
			lastHealth := s.lastHealthFromClient.Load()
			if lastHealth == nil || !lastHealth.Healthy {
				s.saveAndReportConfigStatus(protobufs.RemoteConfigStatuses_RemoteConfigStatuses_FAILED, "Config apply timeout exceeded")
			} else {
				s.saveAndReportConfigStatus(protobufs.RemoteConfigStatuses_RemoteConfigStatuses_APPLIED, "")
			}

		case <-s.doneChan:
			err := s.commander.Stop(context.Background())
			if err != nil {
				s.telemetrySettings.Logger.Error("Could not stop agent process", zap.Error(err))
			}
			return
		}
	}
}

// markAgentReady marks the agent as ready and sends a signal to
// [agentReadyChan].
func (s *Supervisor) markAgentReady() {
	s.agentReady.Store(true)
	select {
	case s.agentReadyChan <- struct{}{}:
	default:
	}
}

// resetAgentReady resets the agent as not ready and drains [agentReadyChan].
func (s *Supervisor) resetAgentReady() {
	s.agentReady.Store(false)
	select {
	case <-s.agentReadyChan:
	default:
	}
}

// waitForAgentReady waits for the agent to be ready. The agent is considered to
// be ready when its first health report is received by the Supervisor's opamp
// server.
// WARNING: this is not thread-safe! If there are two goroutines waiting for
// the agent to be ready, only one of them will be able to proceed.
func (s *Supervisor) waitForAgentReady() error {
	if s.agentReady.Load() {
		return nil
	}

	bootstrapTimer := time.NewTimer(s.config.Agent.BootstrapTimeout)
	defer bootstrapTimer.Stop()

	select {
	case <-s.agentReadyChan:
		return nil
	case <-bootstrapTimer.C:
		return fmt.Errorf("agent has not started after %s", s.config.Agent.BootstrapTimeout)
	}
}

// hupReloadAgent sends a HUP signal to the agent process  with the intent of
// triggering a configuration reload. There are 3 possible outcomes of this:
// 1. The agent is the "official" Otel Collector, which properly traps the HUP
// signal and reloads the config.
// 2. The agent is a custom process, which does not trap the HUP signal: in
// this case the default behavior of a process that receives a HUP signal is to
// exit. This ends up being the same behavior as the standard stop -> restart
// configuration reload method.
// 3. The agent traps the HUP signal, but does nothing. On the next health
// report the agent will be consiered healthy and ready, even though it might
// be running on old configuration.
func (s *Supervisor) hupReloadAgent() error {
	s.agentRestarting.Store(true)
	defer s.agentRestarting.Store(false)
	defer s.resetAgentReady()

	// If we have an empty config, the agent should be stopped.
	if s.cfgState.Load().(*configState).configMapIsEmpty {
		s.stopAgentApplyConfig()
		return nil
	}

	if err := s.writeAgentConfig(); err != nil {
		return fmt.Errorf("failed to write agent new config: %w", err)
	}

	// If the agent is not running, we can't send a HUP signal to it, so we
	// return and let it be started by the caller.
	if !s.commander.IsRunning() {
		return nil
	}

	if err := s.waitForAgentReady(); err != nil {
		return err
	}

	s.telemetrySettings.Logger.Debug("agent is running, reloading config")
	if err := s.reloadAgentConfig(); err != nil {
		return err
	}

	return nil
}

func (s *Supervisor) reloadAgentConfig() error {
	s.telemetrySettings.Logger.Debug("Reloading the agent config")
	err := s.commander.ReloadConfigFile()
	if err != nil {
		return err
	}
	return nil
}

func (s *Supervisor) writeAgentConfig() error {
	cfgState := s.cfgState.Load().(*configState)
	if err := os.WriteFile(s.agentConfigFilePath(), []byte(cfgState.mergedConfig), 0o600); err != nil {
		return err
	}
	return nil
}

func (s *Supervisor) stopAgentApplyConfig() {
	s.telemetrySettings.Logger.Debug("Stopping the agent to apply new config")
	err := s.commander.Stop(context.Background())
	if err != nil {
		s.telemetrySettings.Logger.Error("Could not stop agent process", zap.Error(err))
	}

	if err := s.writeAgentConfig(); err != nil {
		s.telemetrySettings.Logger.Error("Failed to write agent config", zap.Error(err))
	}
}

func (s *Supervisor) Shutdown() {
	s.telemetrySettings.Logger.Debug("Supervisor shutting down...")
	close(s.doneChan)

	// Shutdown in order from producer to consumer (agent -> customMessageForwarder -> local OpAMP server -> client to remote OpAMP server).
	s.agentWG.Wait()
	s.customMessageWG.Wait()

	if s.opampServer != nil {
		s.telemetrySettings.Logger.Debug("Stopping OpAMP server...")
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		err := s.opampServer.Stop(ctx)
		if err != nil {
			s.telemetrySettings.Logger.Error("Could not stop the OpAMP Server")
		} else {
			s.telemetrySettings.Logger.Debug("OpAMP server stopped.")
		}
	}

	if s.opampClient != nil {
		err := s.opampClient.SetHealth(
			&protobufs.ComponentHealth{
				Healthy: false, LastError: "Supervisor is shutdown",
			},
		)
		if err != nil {
			s.telemetrySettings.Logger.Error("Could not report health to OpAMP server", zap.Error(err))
		}

		err = s.stopOpAMPClient()
		if err != nil {
			s.telemetrySettings.Logger.Error("Could not stop the OpAMP client", zap.Error(err))
		}
	}

	if err := s.shutdownTelemetry(); err != nil {
		s.telemetrySettings.Logger.Error("Could not shut down self telemetry", zap.Error(err))
	}
}

func (s *Supervisor) shutdownTelemetry() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// The metric.MeterProvider and trace.TracerProvider interfaces do not have a Shutdown method.
	// To shutdown the providers we try to cast to this interface, which matches the type signature used in the SDK.
	type shutdownable interface {
		Shutdown(context.Context) error
	}

	var err error

	if prov, ok := s.telemetrySettings.MeterProvider.(shutdownable); ok {
		if shutdownErr := prov.Shutdown(ctx); shutdownErr != nil {
			err = multierr.Append(err, fmt.Errorf("failed to shutdown meter provider: %w", shutdownErr))
		}
	}

	if prov, ok := s.telemetrySettings.TracerProvider.(shutdownable); ok {
		if shutdownErr := prov.Shutdown(ctx); shutdownErr != nil {
			err = multierr.Append(err, fmt.Errorf("failed to shutdown tracer provider: %w", shutdownErr))
		}
	}

	if prov, ok := s.telemetrySettings.loggerProvider.(shutdownable); ok {
		if shutdownErr := prov.Shutdown(ctx); shutdownErr != nil {
			err = multierr.Append(err, fmt.Errorf("failed to shutdown logger provider: %w", shutdownErr))
		}
	}

	return err
}

func (s *Supervisor) saveLastReceivedConfig(config *protobufs.AgentRemoteConfig) error {
	cfg, err := proto.Marshal(config)
	if err != nil {
		return err
	}

	return os.WriteFile(filepath.Join(s.config.Storage.Directory, lastRecvRemoteConfigFile), cfg, 0o600)
}

func (s *Supervisor) saveLastReceivedOwnTelemetrySettings(set *protobufs.ConnectionSettingsOffers, filePath string) error {
	cfg, err := proto.Marshal(set)
	if err != nil {
		return err
	}

	return os.WriteFile(filepath.Join(s.config.Storage.Directory, filePath), cfg, 0o600)
}

// saveAndReportConfigStatus saves the config status to the persistent state and reports it to the server.
func (s *Supervisor) saveAndReportConfigStatus(status protobufs.RemoteConfigStatuses, errorMessage string) {
	if !s.config.Capabilities.ReportsRemoteConfig {
		s.telemetrySettings.Logger.Debug("supervisor is not configured to report remote config status")
	}
	rcs := &protobufs.RemoteConfigStatus{
		LastRemoteConfigHash: s.remoteConfig.GetConfigHash(),
		Status:               status,
		ErrorMessage:         errorMessage,
	}

	// save status to persistent state
	if err := s.persistentState.SetLastRemoteConfigStatus(rcs); err != nil {
		s.telemetrySettings.Logger.Error("Could not save last remote config status", zap.Error(err))
	}
	// report status to server
	if err := s.opampClient.SetRemoteConfigStatus(rcs); err != nil {
		s.telemetrySettings.Logger.Error("Could not report OpAMP remote config status", zap.Error(err))
	}
}

func (s *Supervisor) onMessage(ctx context.Context, msg *types.MessageData) {
	configChanged := false

	if msg.AgentIdentification != nil {
		configChanged = s.processAgentIdentificationMessage(msg.AgentIdentification) || configChanged
	}

	if msg.RemoteConfig != nil {
		configChanged = s.processRemoteConfigMessage(msg.RemoteConfig) || configChanged
	}

	if msg.OwnMetricsConnSettings != nil || msg.OwnTracesConnSettings != nil || msg.OwnLogsConnSettings != nil {
		configChanged = s.processOwnTelemetryConnSettingsMessage(ctx, &protobufs.ConnectionSettingsOffers{
			OwnMetrics: msg.OwnMetricsConnSettings,
			OwnTraces:  msg.OwnTracesConnSettings,
			OwnLogs:    msg.OwnLogsConnSettings,
		}) || configChanged
	}

	// Update the agent config if any messages have touched the config
	if configChanged {
		err := s.opampClient.UpdateEffectiveConfig(ctx)
		if err != nil {
			s.telemetrySettings.Logger.Error("The OpAMP client failed to update the effective config", zap.Error(err))
		}

		s.telemetrySettings.Logger.Debug("Config is changed. Signal to restart the agent")
		// Signal that there is a new config.
		select {
		case s.hasNewConfig <- struct{}{}:
		default:
		}
	}

	messageToAgent := &protobufs.ServerToAgent{
		InstanceUid: s.persistentState.InstanceID[:],
	}
	haveMessageForAgent := false
	// Proxy server capabilities to opamp extension
	if msg.CustomCapabilities != nil {
		messageToAgent.CustomCapabilities = msg.CustomCapabilities
		haveMessageForAgent = true
	}

	// Proxy server messages to opamp extension
	if msg.CustomMessage != nil {
		messageToAgent.CustomMessage = msg.CustomMessage
		haveMessageForAgent = true
	}

	// Send any messages that need proxying to the agent.
	if haveMessageForAgent {
		conn, ok := s.agentConn.Load().(serverTypes.Connection)
		if ok {
			err := conn.Send(ctx, messageToAgent)
			if err != nil {
				s.telemetrySettings.Logger.Error("Error forwarding message to agent from server", zap.Error(err))
			}
		}
	}
}

// processRemoteConfigMessage processes an AgentRemoteConfig message, returning true if the agent config has changed.
func (s *Supervisor) processRemoteConfigMessage(msg *protobufs.AgentRemoteConfig) bool {
	if !s.config.Capabilities.AcceptsRemoteConfig {
		s.telemetrySettings.Logger.Warn("Got remote config message, but the agent does not accept remote config. Ignoring remote config.")
		return false
	}

	if err := s.saveLastReceivedConfig(msg); err != nil {
		s.telemetrySettings.Logger.Error("Could not save last received remote config", zap.Error(err))
	}

	s.remoteConfig = msg
	s.telemetrySettings.Logger.Debug("Received remote config from server", zap.String("hash", fmt.Sprintf("%x", s.remoteConfig.ConfigHash)))

	var err error
	configChanged, err := s.composeMergedConfig(s.remoteConfig)
	if err != nil {
		s.telemetrySettings.Logger.Error("Error composing merged config. Reporting failed remote config status.", zap.Error(err))
		s.saveAndReportConfigStatus(protobufs.RemoteConfigStatuses_RemoteConfigStatuses_FAILED, err.Error())
	}
	if configChanged {
		// only report applying if the config has changed and will run agent with new config
		s.saveAndReportConfigStatus(protobufs.RemoteConfigStatuses_RemoteConfigStatuses_APPLYING, "")
	}

	return configChanged
}

// processOwnTelemetryConnSettingsMessage processes a TelemetryConnectionSettings message, returning true if the agent config has changed.
func (s *Supervisor) processOwnTelemetryConnSettingsMessage(ctx context.Context, msg *protobufs.ConnectionSettingsOffers) bool {
	if err := s.saveLastReceivedOwnTelemetrySettings(msg, lastRecvOwnTelemetryConfigFile); err != nil {
		s.telemetrySettings.Logger.Error("Could not save last received own telemetry settings", zap.Error(err))
	}
	return s.setupOwnTelemetry(ctx, msg)
}

// processAgentIdentificationMessage processes an AgentIdentification message, returning true if the agent config has changed.
func (s *Supervisor) processAgentIdentificationMessage(msg *protobufs.AgentIdentification) bool {
	newInstanceID, err := uuid.FromBytes(msg.NewInstanceUid)
	if err != nil {
		s.telemetrySettings.Logger.Error("Failed to parse instance UUID", zap.Error(err))
		return false
	}

	s.telemetrySettings.Logger.Debug("Agent identity is changing",
		zap.String("old_id", s.persistentState.InstanceID.String()),
		zap.String("new_id", newInstanceID.String()))

	err = s.persistentState.SetInstanceID(newInstanceID)
	if err != nil {
		s.telemetrySettings.Logger.Error("Failed to persist new instance ID, instance ID will revert on restart.", zap.String("new_id", newInstanceID.String()), zap.Error(err))
	}

	err = s.opampClient.SetAgentDescription(s.agentDescription.Load().(*protobufs.AgentDescription))
	if err != nil {
		s.telemetrySettings.Logger.Error("Failed to send agent description to OpAMP server")
	}

	// Need to recalculate the Agent config so that the new agent identification is included in it.
	configChanged, err := s.composeMergedConfig(s.remoteConfig)
	if err != nil {
		s.telemetrySettings.Logger.Error("Error composing merged config with new instance ID", zap.Error(err))
		return false
	}

	return configChanged
}

func (s *Supervisor) persistentStateFilePath() string {
	return filepath.Join(s.config.Storage.Directory, persistentStateFileName)
}

func (s *Supervisor) agentConfigFilePath() string {
	return filepath.Join(s.config.Storage.Directory, agentConfigFileName)
}

func (s *Supervisor) getSupervisorOpAMPServerPort() (int, error) {
	if s.config.Agent.OpAMPServerPort != 0 {
		return s.config.Agent.OpAMPServerPort, nil
	}
	return s.findRandomPort()
}

func (s *Supervisor) getFeatureGateFlag() []string {
	flags := []string{}
	for k := range s.featureGates {
		flags = append(flags, k)
	}

	if len(flags) == 0 {
		return []string{}
	}

	return []string{"--feature-gates", strings.Join(flags, ",")}
}

func (s *Supervisor) isFeatureGateSupported(gate string) bool {
	_, ok := s.featureGates[gate]
	return ok
}

func (*Supervisor) findRandomPort() (int, error) {
	l, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		return 0, err
	}

	port := l.Addr().(*net.TCPAddr).Port

	err = l.Close()
	if err != nil {
		return 0, err
	}

	return port, nil
}

func (s *Supervisor) getTracer() trace.Tracer {
	tracer := s.telemetrySettings.TracerProvider.Tracer("github.com/open-telemetry/opentelemetry-collector-contrib/cmd/opampsupervisor")
	return tracer
}

// The default koanf behavior is to override lists in the config.
// Instead, we provide this function, which merges the source and destination config's
// extension lists by concatenating the two.
// Will be resolved by https://github.com/open-telemetry/opentelemetry-collector/issues/8754
func configMergeFunc(src, dest map[string]any) error {
	srcExtensions := maps.Search(src, []string{"service", "extensions"})
	destExtensions := maps.Search(dest, []string{"service", "extensions"})

	maps.Merge(src, dest)

	if destExt, ok := destExtensions.([]any); ok {
		if srcExt, ok := srcExtensions.([]any); ok {
			if service, ok := dest["service"].(map[string]any); ok {
				allExt := slices.Concat(destExt, srcExt)
				// This is a small hack to ensure that the order is consitent and
				// follows this simple rule: extensions from [src], then from [dest],
				// in the order that they appear.
				// We cannot use other simpler methods, like [sort.Strings], because
				// we work with a `[]any` that cannot be cast to `[]string`.
				seenExt := make(map[any]struct{}, len(allExt))
				var uniqueExts []any
				for _, ext := range allExt {
					if _, ok := seenExt[ext]; !ok {
						seenExt[ext] = struct{}{}
						uniqueExts = append(uniqueExts, ext)
					}
				}
				service["extensions"] = uniqueExts
			}
		}
	}

	return nil
}
