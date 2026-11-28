// Package server hosts the native API without a desktop or provider worker.
package server

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/nitecon/amux/channels"
	"github.com/nitecon/amux/config"
	"github.com/nitecon/amux/controllers"
	"github.com/nitecon/amux/storage"
)

// HandlerConfig holds the immutable runtime integrations for a server.
type HandlerConfig struct {
	Remote                    *RemoteConfig
	Channels                  map[string]channels.Outbound
	ArtifactAuthEnforced      bool
	DisableArtifactBodySchema bool
	SecurityEnrollment        *storage.SecurityEnrollment
	LocalNodeID               string
	DiscoveryPort             uint16
	ProcessNonce              string
	SecureBind                string
	DiscoveryBind             string
	Shutdown                  func()
}

// NewHandler wires the ported project API. Unported endpoints remain absent;
// this handler is not yet the complete Rust gateway replacement.
func NewHandler(store *storage.Store, apiKey string) (http.Handler, error) {
	return NewHandlerWithConfig(store, apiKey, HandlerConfig{})
}

func NewHandlerWithRemote(store *storage.Store, apiKey string, config RemoteConfig) (http.Handler, error) {
	return NewHandlerWithConfig(store, apiKey, HandlerConfig{Remote: &config})
}

// NewHandlerWithConfig composes remote event streaming and outbound project
// channels without exposing mutable runtime registries to request handlers.
func NewHandlerWithConfig(store *storage.Store, apiKey string, config HandlerConfig) (http.Handler, error) {
	if store == nil {
		return nil, fmt.Errorf("server requires durable storage")
	}
	if apiKey == "" {
		return nil, fmt.Errorf("server requires an API credential")
	}
	if (config.SecurityEnrollment == nil) != (config.LocalNodeID == "") {
		return nil, fmt.Errorf("secure handler requires both enrollment store and local node ID")
	}
	if config.LocalNodeID != "" {
		if parsed, err := uuid.Parse(config.LocalNodeID); err != nil || parsed.String() != config.LocalNodeID || parsed == uuid.Nil {
			return nil, fmt.Errorf("secure handler local node ID must be a canonical UUID")
		}
		nonce, err := uuid.Parse(config.ProcessNonce)
		if err != nil || nonce == uuid.Nil || nonce.String() != config.ProcessNonce || config.Shutdown == nil || config.SecureBind == "" {
			return nil, fmt.Errorf("secure handler requires a process nonce, secure bind and shutdown callback")
		}
	}
	var remote *remoteStreamConfig
	if config.Remote != nil {
		validated, err := validateRemoteConfig(*config.Remote)
		if err != nil {
			return nil, err
		}
		remote = validated
	}
	configuredChannels := make(map[string]channels.Outbound, len(config.Channels))
	for name, provider := range config.Channels {
		if strings.TrimSpace(name) == "" || strings.TrimSpace(name) != name || name == "internal" {
			return nil, fmt.Errorf("invalid or reserved outbound channel name %q", name)
		}
		if provider == nil {
			return nil, fmt.Errorf("outbound channel %q is nil", name)
		}
		configuredChannels[name] = provider
	}
	mux := http.NewServeMux()
	api := newAPI(store, apiKey)
	api.remote = remote
	api.channels = configuredChannels
	api.artifactAuthEnforced = config.ArtifactAuthEnforced
	api.disableArtifactBodySchema = config.DisableArtifactBodySchema
	api.securityEnrollment = config.SecurityEnrollment
	api.localNodeID = config.LocalNodeID
	api.discoveryPort = config.DiscoveryPort
	api.processNonce = config.ProcessNonce
	api.secureBind = config.SecureBind
	api.discoveryBind = config.DiscoveryBind
	api.shutdown = config.Shutdown
	mux.HandleFunc("POST /v1/projects", api.registerProject)
	mux.HandleFunc("DELETE /v1/projects/{ident}", api.deleteProject)
	mux.HandleFunc("GET /v1/projects", api.projectInventory)
	mux.HandleFunc("GET /v1/navigation/stream", api.navigationStream)
	api.registerTaskRoutes(mux)
	api.registerExecutionHistoryRoutes(mux)
	api.registerTaskCancellationRoutes(mux)
	api.registerConversationRoutes(mux)
	api.registerFleetConversationRoutes(mux)
	api.registerConversationCommandRoutes(mux)
	api.registerPresenceRoutes(mux)
	api.registerCheckoutRoutes(mux)
	api.registerProjectMessageRoutes(mux)
	api.registerProjectLinkRoutes(mux)
	api.registerGatewayMemoryReadRoutes(mux)
	api.registerPatternRoutes(mux)
	api.registerSelfServiceUserRoutes(mux)
	api.registerUserAdminRoutes(mux)
	api.registerPluginRoutes(mux)
	api.registerProviderRoutes(mux)
	api.registerSkillRoutes(mux)
	api.registerArtifactReadRoutes(mux)
	api.registerArtifactWriteRoutes(mux)
	api.registerSpecManifestTaskRoutes(mux)
	api.registerArtifactLinkWriteRoutes(mux)
	api.registerArtifactDiscourseWriteRoutes(mux)
	api.registerArtifactAcceptanceRoutes(mux)
	api.registerArtifactWorkflowRunRoutes(mux)
	api.registerWorkerAssignmentRoutes(mux)
	mux.HandleFunc("POST /v1/projects/{ident}/conversations/{conversation}/tools", api.callTaskTool)
	mux.HandleFunc("GET /v1/tasks/stream", api.taskStream)
	mux.HandleFunc("GET /v1/events/stream", api.remoteEventStream)
	mux.HandleFunc("POST /v1/notifications/global", api.publishGlobalNotification)
	mux.HandleFunc("GET /v1/task-events", api.taskEvents)
	mux.HandleFunc("GET /v1/task-events/consumers", api.taskConsumerStatuses)
	if config.SecurityEnrollment != nil {
		mux.HandleFunc("POST /v1/enrollment/requests", api.enroll)
		mux.HandleFunc("GET /v1/enrollment/requests/{id}", api.enrollmentResult)
		mux.HandleFunc("POST /v1/enrollment/renew", api.renewSecurityCertificate)
		mux.HandleFunc("POST /v1/enrollment/activate", api.activateSecurityCertificate)
		mux.HandleFunc("POST /v1/server/nodes/{id}/revoke", api.revokeSecurityNode)
		if config.DiscoveryPort != 0 {
			mux.HandleFunc("POST /v1/server/invitation", api.generateInvitation)
			mux.HandleFunc("GET /v1/server/invitation", api.copyInvitation)
			mux.HandleFunc("DELETE /v1/server/invitation", api.invalidateInvitation)
		}
	}
	// Operator routes are registered separately from enrollment and renewal so
	// their lifecycle authorization remains clear and independently scoped.
	if config.SecurityEnrollment != nil {
		mux.HandleFunc("GET /v1/server/status", api.serverStatus)
		mux.HandleFunc("POST /v1/server/shutdown", api.serverShutdown)
	}
	mux.HandleFunc("PATCH /v1/projects/{ident}/repo", func(w http.ResponseWriter, r *http.Request) {
		var req *struct {
			Provider  string `json:"provider"`
			Namespace string `json:"namespace"`
			RepoName  string `json:"repo_name"`
		}
		r.Body = http.MaxBytesReader(w, r.Body, 65536)
		decoder := json.NewDecoder(r.Body)
		if err := decoder.Decode(&req); err != nil || req == nil {
			writeError(w, http.StatusBadRequest, "invalid repository mapping")
			return
		}
		var extra any
		if err := decoder.Decode(&extra); err != io.EOF {
			writeError(w, http.StatusBadRequest, "request must contain one JSON object")
			return
		}
		if (strings.TrimSpace(req.Namespace) == "") != (strings.TrimSpace(req.RepoName) == "") {
			writeError(w, http.StatusBadRequest, "namespace and repo_name must be supplied together")
			return
		}
		project, err := store.UpdateRepoMapping(r.Context(), r.PathValue("ident"), req.Provider, req.Namespace, req.RepoName)
		respondProject(w, project, err)
	})
	for _, operation := range []string{"archive", "restore"} {
		mux.HandleFunc("POST /v1/projects/{ident}/"+operation, func(w http.ResponseWriter, r *http.Request) {
			project, err := store.SetProjectArchived(r.Context(), r.PathValue("ident"), operation == "archive")
			respondProject(w, project, err)
		})
	}
	authenticatedMux := api.withGatewayAuthentication(mux)
	api.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if api.securityEnrollment != nil {
			api.serveSecure(w, r, authenticatedMux)
			return
		}
		authenticatedMux.ServeHTTP(w, r)
	})
	return api, nil
}

func respondProject(w http.ResponseWriter, p *storage.Project, err error) {
	if err != nil {
		writeError(w, http.StatusInternalServerError, "project operation failed")
		return
	}
	if p == nil {
		writeError(w, http.StatusNotFound, "project not found")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(p)
}
func writeError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": message})
}

// Serve owns the listener until failure or cancellation. It drains active HTTP
// requests on cancellation, then closes remaining connections at the deadline.
func Serve(ctx context.Context, listener net.Listener, handler http.Handler, shutdownTimeout time.Duration) error {
	if listener == nil || handler == nil || shutdownTimeout <= 0 {
		return fmt.Errorf("listener, handler and positive shutdown timeout are required")
	}
	srv := &http.Server{Handler: handler, ConnState: observeSecureHandshake, ConnContext: secureSocketContext, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, IdleTimeout: 90 * time.Second, MaxHeaderBytes: 1 << 20}
	defer srv.Close()
	defer func() {
		if streams, ok := handler.(interface{ ShutdownStreams(context.Context) error }); ok {
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
			defer cancel()
			streams.ShutdownStreams(cleanup)
		}
	}()
	done := make(chan error, 1)
	go func() { done <- srv.Serve(listener) }()
	select {
	case err := <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
		defer cancel()
		if streams, ok := handler.(interface{ ShutdownStreams(context.Context) error }); ok {
			if err := streams.ShutdownStreams(shutdownCtx); err != nil {
				srv.Close()
				<-done
				return err
			}
		}
		err := srv.Shutdown(shutdownCtx)
		if err != nil {
			srv.Close()
		}
		serveErr := <-done
		if err != nil {
			return fmt.Errorf("drain server: %w", err)
		}
		if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			return serveErr
		}
		return nil
	}
}

const (
	defaultArtifactAuthEnforced      = false
	defaultArtifactBodySchemaEnabled = true
)

// Start runs the configured server until shutdown.
func Start(ctx context.Context, stdout, _ io.Writer) error {
	options := config.GetServer()
	database := options.Database
	profile := options.Profile
	secureInstance := options.SecureInstance
	discoveryBind := options.DiscoveryBind
	profileTLSNames := options.ProfileTLSNames
	listen := options.Listen
	keyEnv := options.KeyEnv
	certPath := options.CertPath
	keyPath := options.KeyPath
	retentionDays := options.RetentionDays
	botRetentionDays := options.BotRetentionDays
	discordTokenEnv := options.DiscordTokenEnv
	discordGuildID := options.DiscordGuildID
	discordCategoryID := options.DiscordCategoryID
	artifactAuthEnforced, err := serverBool(options.ArtifactAuthEnforced, "GATEWAY_ARTIFACT_AUTH_ENFORCED", defaultArtifactAuthEnforced)
	if err != nil {
		return err
	}
	artifactBodySchemaEnabled, err := serverBool(options.ArtifactBodySchemaEnabled, "GATEWAY_ARTIFACT_BODY_SCHEMA_ENABLED", defaultArtifactBodySchemaEnabled)
	if err != nil {
		return err
	}
	if err := options.Validate(); err != nil {
		return err
	}
	if secureInstance != "" || len(profileTLSNames) > 0 {
		profileTLSNames = storage.ProfileServerTLSNames(ctx, profileTLSNames)
	}
	databasePath := database
	if profile != "" {
		databasePath = filepath.Join(profile, "application.sqlite")
	}
	apiKey := os.Getenv(keyEnv)
	if apiKey == "" {
		if secureInstance == "" {
			return fmt.Errorf("server API credential is not configured")
		}
		internalKey, err := uuid.NewRandom()
		if err != nil {
			return err
		}
		apiKey = internalKey.String()
	}
	var outbound map[string]channels.Outbound
	var discord *channels.Discord
	if discordTokenEnv != "" || discordGuildID != "" || discordCategoryID != "" {
		configuredDiscord, err := channels.NewDiscord(channels.DiscordConfig{
			Token: os.Getenv(discordTokenEnv), GuildID: discordGuildID, CategoryID: discordCategoryID,
		})
		if err != nil {
			return fmt.Errorf("configure Discord outbound messaging: %w", err)
		}
		discord = configuredDiscord
		outbound = map[string]channels.Outbound{"discord": discord}
	}
	host, _, _ := net.SplitHostPort(listen)
	ip := net.ParseIP(host)
	if certPath == "" && len(profileTLSNames) == 0 && (ip == nil || !ip.IsLoopback()) {
		return fmt.Errorf("non-loopback server listeners require tls-cert and tls-key")
	}
	var tlsConfig *tls.Config
	if certPath != "" {
		certificate, err := tls.LoadX509KeyPair(certPath, keyPath)
		if err != nil {
			return fmt.Errorf("load server TLS certificate: %w", err)
		}
		tlsConfig = &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{certificate}}
	}
	profileLock, err := LockProfile(databasePath)
	if err != nil {
		return err
	}
	defer profileLock.Close()
	var enrollment *storage.SecurityEnrollment
	var clusterID string
	if profile != "" {
		securityLock, err := LockSecurityProfile(profile)
		if err != nil {
			return err
		}
		defer securityLock.Close()
		clusterID, err = storage.EnsureSecurityInstallation(ctx, profile)
		if err != nil {
			return fmt.Errorf("initialize security profile: %w", err)
		}
		if err := storage.EnsureSecurityRoot(profile, clusterID); err != nil {
			return fmt.Errorf("initialize security profile root: %w", err)
		}
		if len(profileTLSNames) != 0 {
			certificate, err := storage.EnsureProfileServerTLS(profile, clusterID, profileTLSNames)
			if err != nil {
				return fmt.Errorf("initialize profile TLS identity: %w", err)
			}
			tlsConfig = &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{certificate}}
		}
		if secureInstance != "" {
			enrollment, err = storage.OpenSecurityEnrollment(ctx, profile)
			if err != nil {
				return fmt.Errorf("open security enrollment: %w", err)
			}
			defer enrollment.Close()
			rootPEM, err := os.ReadFile(filepath.Join(profile, "root.pem"))
			if err != nil {
				return fmt.Errorf("read profile trust root: %w", err)
			}
			tlsConfig.ClientCAs = x509.NewCertPool()
			if !tlsConfig.ClientCAs.AppendCertsFromPEM(rootPEM) {
				return fmt.Errorf("invalid profile trust root")
			}
			tlsConfig.ClientAuth = tls.VerifyClientCertIfGiven
		}
	}
	store, err := storage.Open(ctx, databasePath)
	if err != nil {
		return err
	}
	defer store.Close()
	if err := store.RecoverTaskConsumers(ctx); err != nil {
		return err
	}
	if discord != nil {
		if err := registerDiscordRooms(ctx, store, discord); err != nil {
			return err
		}
	}
	builtins, err := NewGatewayFISM(store, retentionDays, botRetentionDays)
	if err != nil {
		return err
	}
	store.UseRouterReviews()
	listener, err := net.Listen("tcp", listen)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	defer listener.Close()
	var localNodeID string
	var localURL string
	var discoveryListener net.Listener
	var discoveryHandler http.Handler
	var discoveryPort uint16
	if enrollment != nil {
		bound := listener.Addr().(*net.TCPAddr)
		loopback := "127.0.0.1"
		if bound.IP.To4() == nil {
			loopback = "::1"
		}
		localURL = "https://" + net.JoinHostPort(loopback, strconv.Itoa(bound.Port))
		local, err := enrollment.ProvisionLocal(ctx, localURL, secureInstance)
		if err != nil {
			return fmt.Errorf("provision local administrator: %w", err)
		}
		localNodeID = local.NodeID
		address := discoveryBind
		if address == "" {
			address = net.JoinHostPort(host, "24181")
		}
		discoveryListener, err = net.Listen("tcp", address)
		if err != nil {
			return fmt.Errorf("listen for enrollment discovery: %w", err)
		}
		defer discoveryListener.Close()
		discoveryPort = uint16(discoveryListener.Addr().(*net.TCPAddr).Port)
		public := enrollment.Discovery(uint16(bound.Port))
		mux := http.NewServeMux()
		mux.HandleFunc("GET /v1/discovery", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(public)
		})
		discoveryHandler = mux
	}
	var secureConnections *SecureConnections
	if enrollment != nil {
		listener, secureConnections, err = TrackSecureConnections(listener, tlsConfig)
		if err != nil {
			return fmt.Errorf("track secure TLS connections: %w", err)
		}
	} else if tlsConfig != nil {
		listener = tls.NewListener(listener, tlsConfig)
	}
	runtimeCtx, stopRuntime := context.WithCancel(ctx)
	defer stopRuntime()
	routerDone := make(chan error, 1)
	ready := make(chan struct{})
	go func() {
		routerDone <- builtins.Run(runtimeCtx, ready)
		stopRuntime()
	}()
	joined := false
	defer func() {
		stopRuntime()
		if !joined {
			<-routerDone
		}
	}()
	select {
	case <-ready:
	case err := <-routerDone:
		joined = true
		return err
	case <-runtimeCtx.Done():
		err := <-routerDone
		joined = true
		return err
	}
	remoteConfig := RemoteConfig{
		Context: runtimeCtx, Registry: builtins.Registry(), Forwarded: controllers.RemoteForwardedEvents(),
	}
	processNonce := ""
	secureBind, discoveryAddress := "", ""
	var shutdown func()
	if enrollment != nil {
		processNonce = uuid.NewString()
		secureBind = listener.Addr().String()
		discoveryAddress = discoveryListener.Addr().String()
		shutdown = stopRuntime
	}
	handler, err := NewHandlerWithConfig(store, apiKey, HandlerConfig{
		Remote: &remoteConfig, Channels: outbound, ArtifactAuthEnforced: artifactAuthEnforced,
		DisableArtifactBodySchema: !artifactBodySchemaEnabled,
		SecurityEnrollment:        enrollment, LocalNodeID: localNodeID,
		DiscoveryPort: discoveryPort, ProcessNonce: processNonce, SecureBind: secureBind,
		DiscoveryBind: discoveryAddress, Shutdown: shutdown,
	})
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintln(stdout, "listening", listener.Addr().String()); err != nil {
		return err
	}
	if secureConnections != nil {
		maintenanceDone := make(chan struct{})
		go func() {
			defer close(maintenanceDone)
			maintainSecureCredentials(runtimeCtx, store.EventBus(), secureConnections, enrollment, localURL, secureInstance, profile, clusterID, profileTLSNames)
		}()
		defer func() {
			stopRuntime()
			<-maintenanceDone
		}()
	}
	var discoveryDone chan error
	if discoveryListener != nil {
		discoveryDone = make(chan error, 1)
		go func() {
			discoveryDone <- Serve(runtimeCtx, discoveryListener, discoveryHandler, 10*time.Second)
			stopRuntime()
		}()
	}
	var ingress *discordIngress
	if discord != nil {
		ingress = startDiscordIngress(runtimeCtx, discord, store.AcceptObservation, builtins.Registry())
	}
	serveErr := Serve(runtimeCtx, listener, handler, 10*time.Second)
	stopRuntime()
	if discoveryDone != nil {
		if err := <-discoveryDone; err != nil {
			serveErr = errors.Join(serveErr, fmt.Errorf("serve enrollment discovery: %w", err))
		}
	}
	if ingress != nil {
		<-ingress.providerDone
		sourceErr := <-ingress.sourceDone
		if sourceErr != nil && !(runtimeCtx.Err() != nil && errors.Is(sourceErr, context.Canceled)) {
			serveErr = errors.Join(serveErr, fmt.Errorf("persist Discord inbound events: %w", sourceErr))
		}
	}
	routerErr := <-routerDone
	joined = true
	return errors.Join(serveErr, routerErr)
}

func readServerBool(key string, defaultValue bool) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
	case "1", "true", "yes", "on":
		return true, nil
	case "0", "false", "no", "off":
		return false, nil
	case "":
		return defaultValue, nil
	default:
		return false, fmt.Errorf("%s must be a boolean flag", key)
	}
}

func serverBool(configured *bool, name string, fallback bool) (bool, error) {
	if configured != nil {
		return *configured, nil
	}
	return readServerBool(name, fallback)
}
