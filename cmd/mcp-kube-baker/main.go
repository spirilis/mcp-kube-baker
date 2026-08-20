// mcp-kube-baker is an MCP server (protocol 2026-07-28) exposing one or more
// Kubernetes clusters — selected by kubeconfig context — to MCP clients.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/spirilis/generic-go-mcp/auth"
	"github.com/spirilis/generic-go-mcp/compat"
	"github.com/spirilis/generic-go-mcp/logging"
	"github.com/spirilis/generic-go-mcp/mcp"
	"github.com/spirilis/generic-go-mcp/transport"

	"github.com/spirilis/mcp-kube-baker/internal/config"
	"github.com/spirilis/mcp-kube-baker/internal/kube"
	"github.com/spirilis/mcp-kube-baker/internal/resources"
	"github.com/spirilis/mcp-kube-baker/internal/skills"
	"github.com/spirilis/mcp-kube-baker/internal/tools"
)

const (
	serverName    = "mcp-kube-baker"
	serverVersion = "0.2.0"

	serverInstructions = "Read-only access to one or more Kubernetes clusters, each named by a " +
		"kubeconfig context. Start with kubectl_get_contexts to learn the cluster names; every " +
		"other tool takes one as its context argument. Full object manifests are served as " +
		"resources under mcp+kubectl://{context}/... — tool results include resource_link entries " +
		"pointing at them. Hosts supporting the experimental io.modelcontextprotocol/skills " +
		"extension can call skills/list for troubleshooting procedures written against these " +
		"tools."
)

func main() {
	configPath := flag.String("config", "", "Path to the YAML configuration file (optional; without it: stdio transport, legacy compat on, kubeconfig from $KUBECONFIG or ~/.kube/config)")
	kubeconfigPath := flag.String("kubeconfig", "", "Path to a kubeconfig file (overrides the config file and the $KUBECONFIG/~/.kube/config default)")
	flag.Parse()

	var cfg *config.Config
	var err error
	if *configPath != "" {
		cfg, err = config.Load(*configPath)
	} else {
		cfg, err = config.Default()
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error loading config: %v\n", err)
		os.Exit(1)
	}

	// Kubeconfig precedence: --kubeconfig flag > config file > $KUBECONFIG >
	// ~/.kube/config. Validate the winner exactly once.
	if *kubeconfigPath != "" {
		cfg.Kubeconfig = *kubeconfigPath
	} else if cfg.Kubeconfig == "" {
		if cfg.Kubeconfig, err = config.DefaultKubeconfigPath(); err != nil {
			fmt.Fprintf(os.Stderr, "Error loading config: %v\n", err)
			os.Exit(1)
		}
	}
	if err := config.ValidateKubeconfig(cfg.Kubeconfig); err != nil {
		if *configPath == "" && *kubeconfigPath == "" {
			err = fmt.Errorf("%w (tried $KUBECONFIG, then ~/.kube/config; use --kubeconfig or --config to point elsewhere)", err)
		}
		fmt.Fprintf(os.Stderr, "Error loading config: %v\n", err)
		os.Exit(1)
	}

	logLevel, logFormat := "info", "text"
	if cfg.Logging != nil {
		if cfg.Logging.Level != "" {
			logLevel = cfg.Logging.Level
		}
		if cfg.Logging.Format != "" {
			logFormat = cfg.Logging.Format
		}
	}
	logging.Initialize(logLevel, logFormat, os.Stderr)

	clients, err := kube.NewClientManager(cfg.Kubeconfig)
	if err != nil {
		logging.Error("Error loading kubeconfig", "error", err)
		os.Exit(1)
	}
	contexts, current := clients.Contexts()
	logging.Info("Kubeconfig loaded", "path", cfg.Kubeconfig, "contexts", len(contexts), "current", current)

	registry := mcp.NewToolRegistry()
	tools.RegisterAll(registry, clients)

	resourceRegistry := mcp.NewResourceRegistry()
	if err := resources.RegisterAll(resourceRegistry, clients); err != nil {
		logging.Error("Error registering resource templates", "error", err)
		os.Exit(1)
	}

	// Skills are embedded and immutable per build, so a load failure can only
	// mean this binary shipped malformed content — never anything about this
	// cluster or environment. Fail loudly, exactly as the resource templates
	// above do. Disabled means never loading them at all, so their files stay
	// out of resources/list too, and the extension is absent rather than empty.
	var skillRegistry *mcp.SkillRegistry
	if cfg.SkillsEnabled() {
		if skillRegistry, err = skills.RegisterAll(resourceRegistry); err != nil {
			logging.Error("Error registering skills", "error", err)
			os.Exit(1)
		}
		logging.Info("Skills registered", "count", len(skillRegistry.List()), "prefix", skills.URIPrefix)
	}

	// Plugins add the two meta-tools now, and their own tools and skills only
	// when a client enables one. Registering here, before NewServer wires the
	// registries to the notification broker, means startup itself announces
	// nothing. A nil skillRegistry leaves a plugin's tools intact and only its
	// skills unpublished.
	if _, err = tools.RegisterPlugins(registry, resourceRegistry, skillRegistry, clients); err != nil {
		logging.Error("Error registering plugins", "error", err)
		os.Exit(1)
	}

	authEnabled := cfg.Auth != nil && cfg.Auth.Enabled
	legacyCompatEnabled := cfg.Server.LegacyCompat != nil && cfg.Server.LegacyCompat.Enabled

	// One version list feeds server/discover, the legacy initialize
	// diagnostic, and -32022, so clients always hear the same story.
	advertisedVersions := []string{mcp.ProtocolVersion}
	if legacyCompatEnabled {
		advertisedVersions = append(advertisedVersions, compat.LegacyVersions...)
	}

	serverCfg := &mcp.ServerConfig{
		Name:               serverName,
		Version:            serverVersion,
		Instructions:       serverInstructions,
		AdvertisedVersions: advertisedVersions,
		// A nil registry is the documented off switch: no capability key, and
		// skills/list, skills/get and resources/directory/read all answer
		// -32601. Unlike the AuthService field below, this is a concrete
		// pointer type rather than an interface, so assigning nil is safe.
		Skills: skillRegistry,
		// Lets a client list skill://<name> to find a skill's bundled
		// references instead of parsing them out of the skills/list entry.
		// Nothing else is exposed: DirectoryChildren walks only registered
		// concrete resources, and every cluster-data resource here is a
		// template.
		SkillsDirectoryRead: skillRegistry != nil,
	}
	if authEnabled {
		// Catalog and resource content reflect what the caller is authorized
		// to see — never let an intermediary reuse one user's responses for
		// another.
		serverCfg.DefaultCacheScope = "private"
		serverCfg.PrincipalFromContext = func(ctx context.Context) string {
			if u := auth.GetUserFromContext(ctx); u != nil {
				return u.ID
			}
			return ""
		}
	}
	server := mcp.NewServer(registry, resourceRegistry, serverCfg)

	// Legacy-compat overlay: handler is what the transport gets started with;
	// legacySessions (nil unless enabled) drives HTTP's legacy GET/DELETE.
	var handler transport.MessageHandler = server
	var legacySessions transport.LegacySessions
	if legacyCompatEnabled {
		sessionTTL := time.Duration(0) // compat.Overlay applies its own 30-minute default
		if cfg.Server.LegacyCompat.SessionTTL != "" {
			sessionTTL, err = time.ParseDuration(cfg.Server.LegacyCompat.SessionTTL)
			if err != nil {
				logging.Error("Invalid server.legacy_compat.session_ttl", "value", cfg.Server.LegacyCompat.SessionTTL, "error", err)
				os.Exit(1)
			}
		}
		overlay := compat.New(server, compat.Config{
			SessionTTL: sessionTTL,
			ServerInfo: mcp.Implementation{Name: serverName, Version: serverVersion},
		})
		handler = overlay
		legacySessions = overlay.LegacySessions()
	}

	logging.Info("MCP protocol support",
		"modern", mcp.ProtocolVersion,
		"legacy_compat", legacyCompatEnabled,
		"advertised_versions", advertisedVersions)

	var authService *auth.AuthService
	if authEnabled {
		authService, err = auth.NewAuthService(cfg.Auth)
		if err != nil {
			logging.Error("Error initializing auth", "error", err)
			os.Exit(1)
		}
		defer authService.Close()
	}

	var trans transport.Transport
	switch cfg.Server.Mode {
	case "", "stdio":
		trans = transport.NewStdioTransport()
		logging.Info("Starting MCP server in stdio mode")
	case "http":
		httpCfg := transport.HTTPTransportConfig{
			Host:           cfg.Server.HTTP.Host,
			Port:           cfg.Server.HTTP.Port,
			AllowedOrigins: cfg.Server.HTTP.AllowedOrigins,
			LegacySessions: legacySessions,
		}
		// Conditional on purpose: assigning a nil *auth.AuthService
		// unconditionally would store a typed nil in the AuthProvider
		// interface field and enable auth with a nil receiver.
		if authService != nil {
			httpCfg.AuthService = authService
		}
		trans = transport.NewHTTPTransport(httpCfg)
		logging.Info("Starting MCP server in HTTP mode", "host", cfg.Server.HTTP.Host, "port", cfg.Server.HTTP.Port)
	case "unix":
		// The UNIX transport's management convention: expose /name and /pid
		// as concrete resources under the same custom scheme.
		resourceRegistry.Register(mcp.Resource{
			URI:         "mcp+unix:///name",
			Name:        "Endpoint Name",
			Description: "The configured name of this MCP endpoint",
			MimeType:    "text/plain",
		}, func(ctx context.Context) (mcp.ResourceContentResult, error) {
			return mcp.ResourceContentResult{Text: cfg.Server.Unix.Name}, nil
		})
		resourceRegistry.Register(mcp.Resource{
			URI:         "mcp+unix:///pid",
			Name:        "Process ID",
			Description: "PID of the MCP server process (send SIGINT or SIGTERM to stop)",
			MimeType:    "text/plain",
		}, func(ctx context.Context) (mcp.ResourceContentResult, error) {
			return mcp.ResourceContentResult{Text: strconv.Itoa(os.Getpid())}, nil
		})
		trans = transport.NewUnixTransport(transport.UnixTransportConfig{
			SocketPath: cfg.Server.Unix.SocketPath,
			FileMode:   os.FileMode(cfg.Server.Unix.FileMode),
		})
		logging.Info("Starting MCP server in UNIX socket mode",
			"socket", cfg.Server.Unix.SocketPath, "name", cfg.Server.Unix.Name)
	default:
		logging.Error("Unknown transport mode", "mode", cfg.Server.Mode)
		os.Exit(1)
	}

	if err := trans.Start(handler); err != nil {
		logging.Error("Error starting transport", "error", err)
		os.Exit(1)
	}

	// Wait for a signal, or (stdio only) the transport's input reaching EOF —
	// the portable shutdown signal when the launching client is done. For
	// http/unix the assertion fails and transDone stays nil, so the select
	// waits on the signal alone.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)

	var transDone <-chan struct{}
	if d, ok := trans.(transport.DoneNotifier); ok {
		transDone = d.Done()
	}

	select {
	case <-sigCh:
		logging.Info("Shutting down gracefully", "reason", "signal")
	case <-transDone:
		logging.Info("Shutting down gracefully", "reason", "input closed")
	}

	if err := trans.Stop(); err != nil {
		logging.Error("Error stopping transport", "error", err)
		os.Exit(1)
	}

	// A stdio run that ended because reading input failed is not a clean exit.
	if stdio, ok := trans.(*transport.StdioTransport); ok {
		select {
		case <-stdio.Done():
			if err := stdio.Err(); err != nil {
				logging.Error("stdio input failed", "error", err)
				os.Exit(1)
			}
		default:
		}
	}

	logging.Info("Shutdown complete")
}
