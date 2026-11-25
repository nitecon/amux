package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"flag"
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
	"github.com/nitecon/amux/appupdate"
	"github.com/nitecon/amux/channels"
	"github.com/nitecon/amux/plugins"
	"github.com/nitecon/amux/server"
	"github.com/nitecon/amux/storage"
)

func run(ctx context.Context, args []string, stdout, stderr io.Writer, getenv func(string) string) error {
	if len(args) > 0 && args[0] == "--generate-client-config" {
		return generateClientConfig(args[1:], stdout, stderr)
	}
	if len(args) > 0 && args[0] == "--reload-config" {
		if len(args) == 3 && args[1] == "--config" {
			return requestClientReloadAt(ctx, args[2], stdout)
		}
		if len(args) != 1 {
			return fmt.Errorf("usage: amux --reload-config [--config PATH]")
		}
		return requestClientReload(ctx, stdout)
	}
	if len(args) > 0 && args[0] == "--update" {
		if len(args) != 1 {
			return fmt.Errorf("usage: amux --update")
		}
		archive, update, err := appupdate.Stage(ctx, os.Args[0])
		if err != nil {
			return err
		}
		if !update.Available {
			_, err = fmt.Fprintf(stdout, "amux %s is up to date.\n", update.Version)
			return err
		}
		_, err = fmt.Fprintf(stdout, "Verified amux %s (%s) and staged it at %s. Close amux-desktop, then run: amux --apply-update %q\n", update.Version, update.Revision[:12], archive, archive)
		return err
	}
	if len(args) > 0 && args[0] == "--apply-update" {
		if len(args) < 2 {
			return fmt.Errorf("usage: amux --apply-update ARCHIVE [--restart-desktop --wait-pid PID ... -- ARGS...]")
		}
		restartArgs := []string(nil)
		waitPIDs := []int(nil)
		index := 2
		if index < len(args) && args[index] == "--restart-desktop" {
			restartArgs = []string{}
			index++
		}
		for index < len(args) && args[index] == "--wait-pid" {
			if index+1 >= len(args) {
				return fmt.Errorf("--wait-pid requires a process id")
			}
			pid, err := strconv.Atoi(args[index+1])
			if err != nil || pid <= 0 {
				return fmt.Errorf("invalid --wait-pid")
			}
			waitPIDs = append(waitPIDs, pid)
			index += 2
		}
		if index < len(args) {
			if args[index] != "--" || restartArgs == nil {
				return fmt.Errorf("unexpected --apply-update arguments")
			}
			restartArgs = append(restartArgs, args[index+1:]...)
		}
		if err := appupdate.StartApply(os.Args[0], args[1], waitPIDs, restartArgs); err != nil {
			return err
		}
		_, err := fmt.Fprintln(stdout, "Update helper started. The previous desktop and CLI will close before the verified package is installed.")
		return err
	}
	if len(args) > 0 && args[0] == "--finish-update" {
		if err := appupdate.FinishHelper(args); err != nil {
			return err
		}
		return nil
	}
	if len(args) >= 2 && args[0] == "client" && args[1] == "enroll" {
		return runClientEnroll(ctx, args[2:], os.Stdin, stdout, stderr)
	}
	if len(args) >= 2 && args[0] == "client" && args[1] == "run" {
		return runHeadlessClient(ctx, args[2:], stdout, stderr)
	}
	if len(args) >= 2 && args[0] == "client" && args[1] == "renew" {
		return runClientRenew(ctx, args[2:], stderr)
	}
	if len(args) >= 2 && args[0] == "server" && args[1] == "peering-key" {
		return runServerPeeringKey(ctx, args[2:], stdout, stderr)
	}
	if len(args) >= 1 && args[0] == "messages" {
		return runMessages(ctx, args[1:], stdout, stderr, getenv)
	}
	if len(args) >= 1 && args[0] == "tasks" {
		return runTasks(ctx, args[1:], stdout, stderr, getenv)
	}
	if len(args) >= 1 && args[0] == "submit-work" {
		return runSubmitWorkAlias(ctx, args[1:], stdout, stderr, getenv)
	}
	if len(args) >= 1 && args[0] == "work-submission" {
		return runWorkSubmissionSettings(args[1:], stdout, stderr, getenv)
	}
	if len(args) >= 1 && args[0] == "mcp" {
		return runMCP(ctx, args[1:], os.Stdin, stdout, stderr, getenv)
	}
	if len(args) > 0 && (args[0] == "-add-project" || args[0] == "--add-project") {
		if len(args) < 2 || strings.HasPrefix(args[1], "-") {
			return fmt.Errorf("usage: amux -add-project PATH --state PATH --enrolled-profile PATH [--provider NAME] [--slots N]")
		}
		poolArgs := append([]string{"--directory", args[1]}, args[2:]...)
		return runPoolMode(ctx, poolArgs, stdout, stderr, getenv, true)
	}
	if len(args) >= 2 && args[0] == "worker" && args[1] == "pool" {
		return runPool(ctx, args[2:], stdout, stderr, getenv)
	}
	if len(args) >= 2 && args[0] == "worker" && args[1] == "run" {
		return runWorker(ctx, args[2:], stdout, stderr, getenv)
	}
	generateServerConfig := len(args) > 0 && args[0] == "--generate-server-config"
	if generateServerConfig && len(args) != 1 {
		return fmt.Errorf("usage: amux --generate-server-config")
	}
	if !generateServerConfig && (len(args) < 2 || args[0] != "server" || args[1] != "run") {
		return fmt.Errorf("usage: amux --generate-server-config | amux server run (--config PATH | --database PATH | --profile DIR) [--listen 127.0.0.1:7913] | amux server peering-key --profile DIR [--copy] | amux worker run --state PATH --server URL --project IDENT --task UUID --directory PATH | amux worker pool --state PATH --server URL --project IDENT --directory PATH | amux tasks --server URL --project IDENT [actor options] OPERATION | amux messages [--gateway-url URL] [--project IDENT] list|get|send | amux submit-work [common task options] --directory PATH TASK MESSAGE PATHS... | amux work-submission show|set")
	}
	flags := flag.NewFlagSet("amux server run", flag.ContinueOnError)
	flags.SetOutput(stderr)
	database := flags.String("database", "", "durable SQLite database path (required)")
	profile := flags.String("profile", "", "explicit Rust-compatible server profile directory")
	secureInstance := flags.String("secure-instance-id", "", "enable enrolled-certificate TLS using this stable local administrator instance UUID")
	discoveryBind := flags.String("discovery-bind", "", "public enrollment discovery address (defaults to secure listen host on port 24181)")
	var profileTLSNames []string
	flags.Func("profile-tls-name", "DNS or IP name for the profile-managed TLS certificate (repeatable)", func(value string) error {
		profileTLSNames = append(profileTLSNames, value)
		return nil
	})
	listen := flags.String("listen", "127.0.0.1:7913", "TCP address to listen on")
	keyEnv := flags.String("api-key-env", "GATEWAY_API_KEY", "environment variable containing the server API credential")
	certPath := flags.String("tls-cert", "", "TLS server certificate PEM file")
	keyPath := flags.String("tls-key", "", "TLS server private key PEM file")
	retentionDays := flags.Uint64("message-retention-days", 30, "message history retention in days")
	botRetentionDays := flags.Uint64("bot-message-retention-days", 7, "bot message history retention in days")
	discordTokenEnv := flags.String("discord-token-env", "", "enable Discord messaging and inbound room ingestion using the token in this environment variable")
	discordGuildID := flags.String("discord-guild-id", "", "Discord guild ID (required with --discord-token-env)")
	discordCategoryID := flags.String("discord-category-id", "", "optional Discord category ID for newly created rooms")
	if generateServerConfig {
		return writeServerConfig(flags, stdout)
	}
	configPath := flags.String("config", "", "YAML server configuration file")
	if err := flags.Parse(args[2:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected server arguments")
	}
	if *configPath != "" {
		configuredEnv, err := applyServerConfig(flags, *configPath)
		if err != nil {
			return err
		}
		originalEnv := getenv
		getenv = func(key string) string {
			if value, ok := configuredEnv[key]; ok {
				return value
			}
			return originalEnv(key)
		}
	}
	artifactAuthEnforced, err := readServerBool(getenv, "GATEWAY_ARTIFACT_AUTH_ENFORCED", defaultArtifactAuthEnforced)
	if err != nil {
		return err
	}
	artifactBodySchemaEnabled, err := readServerBool(getenv, "GATEWAY_ARTIFACT_BODY_SCHEMA_ENABLED", defaultArtifactBodySchemaEnabled)
	if err != nil {
		return err
	}
	if (*database == "") == (*profile == "") {
		return fmt.Errorf("supply exactly one of --database or --profile")
	}
	if *discoveryBind != "" && *secureInstance == "" {
		return fmt.Errorf("--discovery-bind requires --secure-instance-id")
	}
	if *secureInstance != "" {
		instance, err := uuid.Parse(*secureInstance)
		if err != nil || instance == uuid.Nil || instance.String() != *secureInstance {
			return fmt.Errorf("--secure-instance-id requires a canonical nonzero UUID")
		}
		if *profile == "" || *certPath != "" || *keyPath != "" {
			return fmt.Errorf("--secure-instance-id requires --profile and profile-managed TLS")
		}
	}
	if len(profileTLSNames) > 0 && *profile == "" {
		return fmt.Errorf("--profile-tls-name requires --profile")
	}
	if len(profileTLSNames) > 0 && (*certPath != "" || *keyPath != "") {
		return fmt.Errorf("--profile-tls-name cannot be combined with --tls-cert or --tls-key")
	}
	if *secureInstance != "" || len(profileTLSNames) > 0 {
		profileTLSNames = storage.ProfileServerTLSNames(ctx, profileTLSNames)
	}
	databasePath := *database
	if *profile != "" {
		databasePath = filepath.Join(*profile, "application.sqlite")
	}
	apiKey := getenv(*keyEnv)
	if apiKey == "" {
		if *secureInstance == "" {
			return fmt.Errorf("server API credential is not configured")
		}
		internalKey, err := uuid.NewRandom()
		if err != nil {
			return err
		}
		apiKey = internalKey.String()
	}
	if (*certPath == "") != (*keyPath == "") {
		return fmt.Errorf("--tls-cert and --tls-key must be supplied together")
	}
	var outbound map[string]channels.Outbound
	var discord *channels.Discord
	if *discordTokenEnv != "" || *discordGuildID != "" || *discordCategoryID != "" {
		if *discordTokenEnv == "" || *discordGuildID == "" {
			return fmt.Errorf("--discord-token-env and --discord-guild-id must be supplied together")
		}
		configuredDiscord, err := channels.NewDiscord(channels.DiscordConfig{
			Token: getenv(*discordTokenEnv), GuildID: *discordGuildID, CategoryID: *discordCategoryID,
		})
		if err != nil {
			return fmt.Errorf("configure Discord outbound messaging: %w", err)
		}
		discord = configuredDiscord
		outbound = map[string]channels.Outbound{"discord": discord}
	}
	host, _, err := net.SplitHostPort(*listen)
	if err != nil {
		return fmt.Errorf("invalid listen address")
	}
	ip := net.ParseIP(host)
	if *certPath == "" && len(profileTLSNames) == 0 && (ip == nil || !ip.IsLoopback()) {
		return fmt.Errorf("non-loopback server listeners require --tls-cert and --tls-key")
	}
	var tlsConfig *tls.Config
	if *certPath != "" {
		certificate, err := tls.LoadX509KeyPair(*certPath, *keyPath)
		if err != nil {
			return fmt.Errorf("load server TLS certificate: %w", err)
		}
		tlsConfig = &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{certificate}}
	}
	profileLock, err := server.LockProfile(databasePath)
	if err != nil {
		return err
	}
	defer profileLock.Close()
	var enrollment *storage.SecurityEnrollment
	var clusterID string
	if *profile != "" {
		securityLock, err := server.LockSecurityProfile(*profile)
		if err != nil {
			return err
		}
		defer securityLock.Close()
		clusterID, err = storage.EnsureSecurityInstallation(ctx, *profile)
		if err != nil {
			return fmt.Errorf("initialize security profile: %w", err)
		}
		if err := storage.EnsureSecurityRoot(*profile, clusterID); err != nil {
			return fmt.Errorf("initialize security profile root: %w", err)
		}
		if len(profileTLSNames) != 0 {
			certificate, err := storage.EnsureProfileServerTLS(*profile, clusterID, profileTLSNames)
			if err != nil {
				return fmt.Errorf("initialize profile TLS identity: %w", err)
			}
			tlsConfig = &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{certificate}}
		}
		if *secureInstance != "" {
			enrollment, err = storage.OpenSecurityEnrollment(ctx, *profile)
			if err != nil {
				return fmt.Errorf("open security enrollment: %w", err)
			}
			defer enrollment.Close()
			rootPEM, err := os.ReadFile(filepath.Join(*profile, "root.pem"))
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
	builtins, err := server.NewGatewayRouter(store, *retentionDays, *botRetentionDays)
	if err != nil {
		return err
	}
	store.UseRouterReviews()
	listener, err := net.Listen("tcp", *listen)
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
		local, err := enrollment.ProvisionLocal(ctx, localURL, *secureInstance)
		if err != nil {
			return fmt.Errorf("provision local administrator: %w", err)
		}
		localNodeID = local.NodeID
		address := *discoveryBind
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
	var secureConnections *server.SecureConnections
	if enrollment != nil {
		listener, secureConnections, err = server.TrackSecureConnections(listener, tlsConfig)
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
	remoteConfig := server.RemoteConfig{
		Context: runtimeCtx, Registry: builtins.Registry(), Forwarded: plugins.RemoteForwardedEvents(),
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
	handler, err := server.NewHandlerWithConfig(store, apiKey, server.HandlerConfig{
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
			maintainSecureCredentials(runtimeCtx, store.EventBus(), secureConnections, enrollment, localURL, *secureInstance, *profile, clusterID, profileTLSNames)
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
			discoveryDone <- server.Serve(runtimeCtx, discoveryListener, discoveryHandler, 10*time.Second)
			stopRuntime()
		}()
	}
	var ingress *discordIngress
	if discord != nil {
		ingress = startDiscordIngress(runtimeCtx, discord, store, builtins.Registry())
	}
	serveErr := server.Serve(runtimeCtx, listener, handler, 10*time.Second)
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

func readServerBool(getenv func(string) string, key string, defaultValue bool) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(getenv(key))) {
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
