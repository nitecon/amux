// Package server hosts the native API without a desktop or provider worker.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/nitecon/amux/channels"
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

type authenticatedPrincipalContextKey struct{}

const bearerAPIKeyPrincipal = "api-key"

func authenticatedPrincipal(ctx context.Context) string {
	principal, _ := ctx.Value(authenticatedPrincipalContextKey{}).(string)
	return principal
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
