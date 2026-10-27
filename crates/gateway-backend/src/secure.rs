//! Native TLS hosting. The legacy API listener remains a separate compatibility mode.
use crate::{
    runtime,
    security::{self, EnrollmentRequest, NodeStatus, RenewalRequest, SecurityStore},
};
use anyhow::{Context, Result};
use axum::{
    extract::{ConnectInfo, Request, State},
    http::StatusCode,
    middleware::{self, Next},
    response::{IntoResponse, Response},
    routing::{get, post},
    Json, Router,
};
use rustls::{pki_types::CertificateDer, RootCertStore};
use std::{
    net::SocketAddr,
    path::PathBuf,
    sync::{Arc, Mutex, RwLock},
    time::Duration,
};
use tokio::{
    net::{TcpListener, TcpStream},
    sync::watch,
    task::JoinSet,
};
use tokio_rustls::{server::TlsStream, TlsAcceptor};

struct OwnedTask<T>(tokio::task::JoinHandle<T>);
impl<T> Drop for OwnedTask<T> {
    fn drop(&mut self) {
        self.0.abort();
    }
}
#[derive(Clone)]
pub struct Principal {
    pub store: Arc<SecurityStore>,
    pub node: NodeStatus,
    pub serial: String,
    connection: PeerInfo,
}
impl Principal {
    pub fn valid(&self) -> bool {
        self.store.authorize(&self.serial).is_ok()
            && self
                .connection
                ._lease
                .as_ref()
                .is_none_or(|lease| self.store.server_serial_valid(&lease.server_serial))
    }
}
struct Lease {
    store: Arc<SecurityStore>,
    serial: Option<String>,
    server_serial: String,
}
impl Drop for Lease {
    fn drop(&mut self) {
        if let Some(serial) = &self.serial {
            self.store.connection_closed(serial);
        }
        self.store
            .connection_closed(&format!("server:{}", self.server_serial));
    }
}
#[derive(Clone)]
pub struct PeerInfo {
    pub remote: SocketAddr,
    pub serial: Option<String>,
    _lease: Option<Arc<Lease>>,
}
impl std::fmt::Debug for PeerInfo {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("PeerInfo")
            .field("remote", &self.remote)
            .field("serial", &self.serial)
            .finish()
    }
}
impl axum::extract::connect_info::Connected<axum::serve::IncomingStream<'_, TlsListener>>
    for PeerInfo
{
    fn connect_info(stream: axum::serve::IncomingStream<'_, TlsListener>) -> Self {
        stream.remote_addr().clone()
    }
}
struct TlsListener {
    tcp: TcpListener,
    config: Arc<RwLock<TlsConfig>>,
    store: Arc<SecurityStore>,
    pending: JoinSet<Result<(TlsStream<TcpStream>, PeerInfo)>>,
}
impl Drop for TlsListener {
    fn drop(&mut self) {
        self.pending.abort_all();
    }
}
impl axum::serve::Listener for TlsListener {
    type Io = TlsStream<TcpStream>;
    type Addr = PeerInfo;
    async fn accept(&mut self) -> (Self::Io, Self::Addr) {
        loop {
            tokio::select! {
             accepted=self.tcp.accept(),if self.pending.len()<64=>{match accepted{Ok((socket,remote))=>{let snapshot={let config=self.config.read().unwrap();let snapshot=config.clone();self.store.connection_open(&format!("server:{}",snapshot.serial));snapshot};let acceptor=TlsAcceptor::from(snapshot.config);let store=self.store.clone();let mut lease=Lease{store:store.clone(),serial:None,server_serial:snapshot.serial};self.pending.spawn(async move{let tls=tokio::time::timeout(Duration::from_secs(10),acceptor.accept(socket)).await??;store.observe_server(&lease.server_serial);let serial=tls.get_ref().1.peer_certificates().and_then(|c|c.first()).map(|c|security::certificate_serial(c.as_ref())).transpose()?;if let Some(serial)=&serial{store.connection_open(serial);}lease.serial=serial.clone();let lease=Some(Arc::new(lease));Ok((tls,PeerInfo{remote,serial,_lease:lease}))});},Err(_)=>tokio::time::sleep(Duration::from_millis(100)).await}},
             result=self.pending.join_next(),if !self.pending.is_empty()=>{if let Some(Ok(Ok(connection)))=result{return connection;}}
            }
        }
    }
    fn local_addr(&self) -> std::io::Result<Self::Addr> {
        Ok(PeerInfo {
            remote: self.tcp.local_addr()?,
            serial: None,
            _lease: None,
        })
    }
}
#[derive(Clone)]
struct TlsConfig {
    config: Arc<rustls::ServerConfig>,
    serial: String,
}
fn tls_config(store: &SecurityStore) -> Result<TlsConfig> {
    let provider = Arc::new(rustls::crypto::ring::default_provider());
    let mut roots = RootCertStore::empty();
    for cert in rustls_pemfile::certs(&mut store.root_pem.as_bytes()) {
        roots.add(cert?)?;
    }
    let verifier = rustls::server::WebPkiClientVerifier::builder_with_provider(
        Arc::new(roots),
        provider.clone(),
    )
    .allow_unauthenticated()
    .build()?;
    let (cert_path, key_path) = store.server_paths()?;
    let cert = std::fs::read(cert_path)?;
    let certs = rustls_pemfile::certs(&mut cert.as_slice())
        .collect::<std::result::Result<Vec<CertificateDer<'static>>, _>>()?;
    let key = security::read_private(&key_path)?;
    let key = rustls_pemfile::private_key(&mut key.as_slice())?.context("server key absent")?;
    let mut config = rustls::ServerConfig::builder_with_provider(provider)
        .with_safe_default_protocol_versions()?
        .with_client_cert_verifier(verifier)
        .with_single_cert(certs, key)?;
    config.alpn_protocols = vec![b"http/1.1".to_vec()];
    Ok(TlsConfig {
        config: Arc::new(config),
        serial: store.server_identity()?.serial,
    })
}
#[derive(Clone)]
struct SecureState {
    store: Arc<SecurityStore>,
    local_node: String,
    shutdown: watch::Sender<bool>,
    secure_bind: SocketAddr,
    discovery_bind: SocketAddr,
    server_names: Vec<String>,
    process_nonce: String,
    attempts: Arc<Mutex<std::collections::HashMap<std::net::IpAddr, (std::time::Instant, u32)>>>,
}
fn api_error(operation: &'static str, error: anyhow::Error) -> Response {
    let rejection = error.downcast_ref::<security::EnrollmentRejection>();
    let code = rejection.map_or("authorization_failed", |reason| reason.code());
    tracing::warn!(operation, reason = code, "Secure request rejected");
    (StatusCode::FORBIDDEN, Json(serde_json::json!({
        "error": code,
        "message": rejection.map_or_else(|| "Request authorization failed".into(), |reason| reason.to_string()),
    }))).into_response()
}
async fn authorize(State(state): State<SecureState>, mut request: Request, next: Next) -> Response {
    let Some(ConnectInfo(peer)) = request.extensions().get::<ConnectInfo<PeerInfo>>().cloned()
    else {
        return StatusCode::UNAUTHORIZED.into_response();
    };
    let bootstrap = request.uri().path() == "/v1/enrollment/requests"
        && request.method() == axum::http::Method::POST
        || request.method() == axum::http::Method::GET
            && request
                .uri()
                .path()
                .strip_prefix("/v1/enrollment/requests/")
                .is_some_and(|id| uuid::Uuid::parse_str(id).is_ok());
    if bootstrap {
        let permitted = {
            let mut attempts = state.attempts.lock().unwrap();
            attempts.retain(|_, (time, _)| time.elapsed() < Duration::from_secs(60));
            if attempts.len() >= 1024 && !attempts.contains_key(&peer.remote.ip()) {
                false
            } else {
                let entry = attempts
                    .entry(peer.remote.ip())
                    .or_insert((std::time::Instant::now(), 0));
                entry.1 += 1;
                entry.1 <= 10
            }
        };
        if !permitted {
            return StatusCode::TOO_MANY_REQUESTS.into_response();
        }
        return next.run(request).await;
    }
    if peer
        ._lease
        .as_ref()
        .is_some_and(|lease| !state.store.server_serial_valid(&lease.server_serial))
    {
        return StatusCode::UNAUTHORIZED.into_response();
    }
    let Some(serial) = peer.serial.clone() else {
        return StatusCode::UNAUTHORIZED.into_response();
    };
    let node = match state.store.authorize(&serial) {
        Ok(node) => node,
        Err(_) => return StatusCode::UNAUTHORIZED.into_response(),
    };
    if let Some(instance) = request.headers().get("x-agent-instance-id") {
        if instance
            .to_str()
            .ok()
            .is_none_or(|instance| state.store.bind_instance(&node.node_id, instance).is_err())
        {
            tracing::warn!(reason = "instance_binding", "Native request denied");
            return StatusCode::FORBIDDEN.into_response();
        }
    }
    if request.uri().path().starts_with("/v1/server/")
        && (!peer.remote.ip().is_loopback()
            || node.node_id != state.local_node
            || node.role != "administrator")
    {
        tracing::warn!(
            reason = "local_administration_required",
            "Native request denied"
        );
        return StatusCode::FORBIDDEN.into_response();
    }
    request.extensions_mut().insert(Principal {
        store: state.store,
        node,
        serial,
        connection: peer,
    });
    next.run(request).await
}
async fn enroll(
    State(state): State<SecureState>,
    Json(request): Json<EnrollmentRequest>,
) -> Response {
    match state.store.enroll(&request) {
        Ok(response) => Json(response).into_response(),
        Err(error) => api_error("enrollment", error),
    }
}
async fn enrollment_result(
    State(state): State<SecureState>,
    axum::extract::Path(id): axum::extract::Path<String>,
) -> Response {
    match state.store.issuance_response(&id) {
        Ok(Some(response)) => Json(response).into_response(),
        Ok(None) => StatusCode::NOT_FOUND.into_response(),
        Err(error) => api_error("enrollment_receipt", error),
    }
}
async fn renew(
    State(state): State<SecureState>,
    axum::Extension(peer): axum::Extension<Principal>,
    Json(request): Json<RenewalRequest>,
) -> Response {
    match state.store.renew(&peer.serial, &request) {
        Ok(response) => Json(response).into_response(),
        Err(error) => api_error("renewal", error),
    }
}
async fn activate(
    State(state): State<SecureState>,
    axum::Extension(peer): axum::Extension<Principal>,
) -> Response {
    match state.store.activate(&peer.serial) {
        Ok(response) => Json(response).into_response(),
        Err(error) => api_error("activation", error),
    }
}
async fn status(State(state): State<SecureState>) -> Response {
    match (state.store.nodes(),state.store.server_identity()){(Ok(nodes),Ok(server))=>Json(serde_json::json!({"cluster_id":state.store.cluster_id,"running":true,"process_id":std::process::id(),"process_nonce":state.process_nonce,"secure_bind":state.secure_bind.to_string(),"discovery_bind":state.discovery_bind.to_string(),"server_names":state.server_names,"nodes":nodes,"server_serial":server.serial,"server_expires_at":server.expires_at,"previous_server_serial":server.previous.as_ref().map(|p|&p.serial),"previous_server_until":server.previous.as_ref().map(|p|p.until)})).into_response(),_=>StatusCode::INTERNAL_SERVER_ERROR.into_response()}
}
async fn revoke(
    State(state): State<SecureState>,
    axum::extract::Path(node): axum::extract::Path<String>,
) -> Response {
    if node == state.local_node {
        return StatusCode::FORBIDDEN.into_response();
    }
    match state.store.revoke(&node) {
        Ok(()) => StatusCode::NO_CONTENT.into_response(),
        Err(error) => api_error("revocation", error),
    }
}

async fn invitation(State(state): State<SecureState>) -> Response {
    match state.store.copy_invitation() {
        Ok(response) => Json(response).into_response(),
        Err(error) => {
            let mut response = api_error("invitation_copy", error);
            *response.status_mut() = StatusCode::NOT_FOUND;
            response
        }
    }
}
async fn generate(State(state): State<SecureState>) -> Response {
    match state.store.generate_invitation() {
        Ok(response) => Json(response).into_response(),
        Err(error) => api_error("invitation_generation", error),
    }
}
async fn invalidate(State(state): State<SecureState>) -> Response {
    match state.store.invalidate_invitation() {
        Ok(()) => StatusCode::NO_CONTENT.into_response(),
        Err(error) => api_error("invitation_invalidation", error),
    }
}
#[derive(serde::Deserialize)]
struct ShutdownRequest {
    expected_process_nonce: String,
}
async fn shutdown(
    State(state): State<SecureState>,
    Json(request): Json<ShutdownRequest>,
) -> Response {
    if request.expected_process_nonce != state.process_nonce {
        return StatusCode::CONFLICT.into_response();
    }
    if let Err(error) = state.store.begin_shutdown() {
        return api_error("shutdown", error);
    }
    let _ = state.shutdown.send(true);
    StatusCode::ACCEPTED.into_response()
}
#[derive(Clone)]
pub struct SecureOptions {
    pub profile: PathBuf,
    pub bind: SocketAddr,
    pub discovery_bind: SocketAddr,
    pub names: Vec<String>,
    pub instance_id: String,
    pub webhook_bind: Option<SocketAddr>,
    pub parent_lease: Option<PathBuf>,
}
pub async fn serve(mut config: runtime::RuntimeConfig, options: SecureOptions) -> Result<()> {
    let mut names = options.names.clone();
    for name in ["localhost", "127.0.0.1", "::1"] {
        if !names.iter().any(|n| n == name) {
            names.push(name.into());
        }
    }
    let store = SecurityStore::open(&options.profile, &names)?;
    let (shutdown_tx, mut shutdown_rx) = watch::channel(false);
    let parent_watch = if let Some(path) = &options.parent_lease {
        let file = std::fs::OpenOptions::new()
            .read(true)
            .write(true)
            .open(path)
            .context("open parent lifetime lease")?;
        let store = store.clone();
        let shutdown = shutdown_tx.clone();
        Some(OwnedTask(tokio::spawn(async move {
            let mut tick = tokio::time::interval(Duration::from_secs(1));
            loop {
                tick.tick().await;
                match file.try_lock() {
                    Err(std::fs::TryLockError::WouldBlock) => continue,
                    Ok(()) => {}
                    Err(error) => tracing::error!("parent lifetime lease failed: {error}"),
                };
                let _ = store.begin_shutdown();
                let _ = shutdown.send(true);
                break;
            }
        })))
    } else {
        None
    };
    let listener = TcpListener::bind(options.bind).await?;
    let local = listener.local_addr()?;
    let discovery_listener = TcpListener::bind(options.discovery_bind).await?;
    let discovery_bind = discovery_listener.local_addr()?;
    store.set_discovery_port(discovery_bind.port());
    let local_url = format!(
        "https://{}:{}",
        if local.ip().is_ipv6() {
            "[::1]"
        } else {
            "127.0.0.1"
        },
        local.port()
    );
    let profile = store.provision_local(&local_url, &options.instance_id)?;
    // A separate non-empty compatibility secret protects legacy handler internals;
    // operational admission uses Principal and never forwards this as bearer auth.
    if config.api_key.is_empty() {
        config.api_key = uuid::Uuid::now_v7().to_string();
    }
    #[cfg(feature = "whatsapp")]
    let webhook_hmac_configured = config.whatsapp.as_ref().is_some_and(|config| {
        config
            .app_secret
            .as_ref()
            .is_some_and(|secret| !secret.is_empty())
    });
    let prepared = tokio::select! {prepared=runtime::prepare(config)=>prepared?,_=shutdown_rx.changed()=>anyhow::bail!("server owner exited during startup")};
    let state = SecureState {
        store: store.clone(),
        local_node: profile.node_id,
        shutdown: shutdown_tx,
        secure_bind: local,
        discovery_bind,
        server_names: names.clone(),
        process_nonce: uuid::Uuid::now_v7().to_string(),
        attempts: Arc::new(Mutex::new(std::collections::HashMap::new())),
    };
    let app = Router::new()
        .route("/v1/enrollment/requests", post(enroll))
        .route("/v1/enrollment/requests/{id}", get(enrollment_result))
        .route("/v1/enrollment/renew", post(renew))
        .route("/v1/enrollment/activate", post(activate))
        .route(
            "/v1/server/invitation",
            get(invitation).post(generate).delete(invalidate),
        )
        .route("/v1/server/status", get(status))
        .route("/v1/server/nodes/{id}/revoke", post(revoke))
        .route("/v1/server/shutdown", post(shutdown))
        .layer(axum::extract::DefaultBodyLimit::max(32 * 1024))
        .with_state(state.clone())
        .merge(prepared.app.clone())
        .layer(middleware::from_fn_with_state(state, authorize));
    let public = store.discovery(local.port());
    let discovery = Router::new().route(
        "/v1/discovery",
        get(move || {
            let public = public.clone();
            async move { Json(public) }
        }),
    );

    tracing::info!(
        "Native APIs listening on https://{}; public discovery http://{}",
        local,
        discovery_listener.local_addr()?
    );
    if !local.ip().is_loopback() && !local.ip().is_unspecified() {
        tracing::warn!("Local administration requires a loopback-reachable secure bind; use an unspecified or loopback address for native controls");
    }
    let discovery_task = OwnedTask(tokio::spawn(async move {
        axum::serve(discovery_listener, discovery).await
    }));
    // Provider ingress is opt-in and narrowly routed; it is never mounted on discovery.
    #[allow(unused_mut)]
    let mut webhook_task: Option<OwnedTask<std::io::Result<()>>> = None;
    #[cfg(feature = "whatsapp")]
    if let Some(bind) = options.webhook_bind {
        anyhow::ensure!(
            prepared.state.whatsapp.is_some(),
            "webhook ingress requires configured WhatsApp"
        );
        anyhow::ensure!(
            webhook_hmac_configured,
            "webhook ingress requires HMAC app secret"
        );
        let router = Router::new()
            .route(
                "/webhooks/whatsapp",
                get(crate::channels::whatsapp::verify_webhook)
                    .post(crate::channels::whatsapp::receive_webhook),
            )
            .with_state(prepared.state.clone());
        let tcp = TcpListener::bind(bind).await?;
        webhook_task = Some(OwnedTask(tokio::spawn(async move {
            axum::serve(tcp, router).await
        })));
    }
    let configs = Arc::new(RwLock::new(tls_config(&store)?));
    let tls = TlsListener {
        tcp: listener,
        config: configs.clone(),
        store: store.clone(),
        pending: JoinSet::new(),
    };
    let maintenance_store = store.clone();
    let local_instance = options.instance_id.clone();
    let maintenance = OwnedTask(tokio::spawn(async move {
        let mut tick = tokio::time::interval(Duration::from_secs(60));
        loop {
            tick.tick().await;
            if let Err(error) = (|| -> Result<()> {
                maintenance_store.provision_local(&local_url, &local_instance)?;
                maintenance_store.ensure_server_identity(&names)?;
                maintenance_store.retire_drained()?;
                *configs.write().unwrap() = tls_config(&maintenance_store)?;
                maintenance_store.retire_server_previous()?;
                maintenance_store.retire_local_previous()?;
                Ok(())
            })() {
                tracing::error!("credential maintenance failed: {error}");
            }
        }
    }));
    let mut drain_deadline = shutdown_rx.clone();
    let result = tokio::select! {
        result=async move {axum::serve(tls,app.into_make_service_with_connect_info::<PeerInfo>()).with_graceful_shutdown(async move{let _=shutdown_rx.changed().await;}).await}=>result,
        _=async move {let _=drain_deadline.changed().await;tokio::time::sleep(Duration::from_secs(5)).await;}=>{
            // The owned CLI runtime drops after serve returns, cancelling any
            // stalled connection tasks along with the retained plugin workers.
            tracing::warn!("Secure server shutdown drain deadline reached");
            Ok(())
        }
    };
    drop(parent_watch);
    drop(maintenance);
    drop(discovery_task);
    drop(webhook_task);
    store.invalidate_invitation()?;
    drop(prepared);
    result?;
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    use tokio::io::{AsyncReadExt, AsyncWriteExt};
    fn port() -> u16 {
        let tcp = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
        tcp.local_addr().unwrap().port()
    }
    async fn connection(
        port: u16,
        root: &str,
        identity: Option<(&str, &str)>,
    ) -> tokio_rustls::client::TlsStream<TcpStream> {
        let provider = Arc::new(rustls::crypto::ring::default_provider());
        let mut roots = RootCertStore::empty();
        for cert in rustls_pemfile::certs(&mut root.as_bytes()) {
            roots.add(cert.unwrap()).unwrap();
        }
        let builder = rustls::ClientConfig::builder_with_provider(provider)
            .with_safe_default_protocol_versions()
            .unwrap()
            .with_root_certificates(roots);
        let config = if let Some((cert, key)) = identity {
            let certs = rustls_pemfile::certs(&mut cert.as_bytes())
                .collect::<std::result::Result<Vec<_>, _>>()
                .unwrap();
            let key = rustls_pemfile::private_key(&mut key.as_bytes())
                .unwrap()
                .unwrap();
            builder.with_client_auth_cert(certs, key).unwrap()
        } else {
            builder.with_no_client_auth()
        };
        let tcp = TcpStream::connect(("127.0.0.1", port)).await.unwrap();
        let tls = tokio_rustls::TlsConnector::from(Arc::new(config))
            .connect("localhost".try_into().unwrap(), tcp)
            .await
            .unwrap();
        tls
    }
    async fn request(
        port: u16,
        root: &str,
        identity: Option<(&str, &str)>,
        method: &str,
        path: &str,
        body: &str,
        extra: &str,
    ) -> (u16, String) {
        let mut tls = connection(port, root, identity).await;
        let wire=format!("{method} {path} HTTP/1.1\r\nHost: localhost\r\nConnection: close\r\nContent-Type: application/json\r\nContent-Length: {}\r\n{extra}\r\n{body}",body.len());
        tls.write_all(wire.as_bytes()).await.unwrap();
        let mut output = Vec::new();
        tls.read_to_end(&mut output).await.unwrap();
        let text = String::from_utf8(output).unwrap();
        let (status, body) = text.split_once("\r\n\r\n").unwrap();
        (
            status.split_whitespace().nth(1).unwrap().parse().unwrap(),
            body.to_owned(),
        )
    }
    async fn websocket(
        port: u16,
        root: &str,
        cert: &str,
        key: &str,
        consumer: &str,
    ) -> (tokio_rustls::client::TlsStream<TcpStream>, String) {
        let mut tls = connection(port, root, Some((cert, key))).await;
        tls.write_all(b"GET /v1/tasks/stream HTTP/1.1\r\nHost: localhost\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n\r\n").await.unwrap();
        let mut header = Vec::new();
        while !header.ends_with(b"\r\n\r\n") {
            let mut byte = [0];
            tls.read_exact(&mut byte).await.unwrap();
            header.push(byte[0]);
        }
        assert!(String::from_utf8(header)
            .unwrap()
            .starts_with("HTTP/1.1 101"));
        let body=serde_json::json!({"type":"subscribe","protocol_version":1,"consumer_id":consumer,"after_event_id":0}).to_string();
        let body = body.as_bytes();
        let mut frame = vec![0x81, 0x80 | body.len() as u8];
        assert!(body.len() < 126);
        frame.extend_from_slice(&[0, 0, 0, 0]);
        frame.extend_from_slice(body);
        tls.write_all(&frame).await.unwrap();
        let message = websocket_message(&mut tls).await;
        (tls, message)
    }
    async fn websocket_message(tls: &mut tokio_rustls::client::TlsStream<TcpStream>) -> String {
        let mut prefix = [0; 2];
        tokio::time::timeout(Duration::from_secs(5), tls.read_exact(&mut prefix))
            .await
            .unwrap()
            .unwrap();
        if prefix[0] & 15 == 8 {
            return "closed".into();
        }
        let mut size = (prefix[1] & 127) as usize;
        if size == 126 {
            let mut b = [0; 2];
            tls.read_exact(&mut b).await.unwrap();
            size = u16::from_be_bytes(b) as usize;
        }
        let mut body = vec![0; size];
        tls.read_exact(&mut body).await.unwrap();
        String::from_utf8(body).unwrap()
    }
    #[tokio::test]
    async fn real_tls_bootstrap_local_control_and_node_authorization() {
        let base =
            std::path::Path::new(env!("CARGO_MANIFEST_DIR")).join("../../target/test-fixtures");
        std::fs::create_dir_all(&base).unwrap();
        let dir = tempfile::tempdir_in(base).unwrap();
        let secure_port = port();
        let discovery_port = port();
        let instance = uuid::Uuid::now_v7().to_string();
        let config = runtime::RuntimeConfig {
            listen_addr: "127.0.0.1:0".parse().unwrap(),
            database: crate::db::DatabaseConfig::sqlite(":memory:"),
            api_key: "legacy-secret".into(),
            default_channel: "none".into(),
            retention_days: 30,
            bot_retention_days: 7,
            artifact_operations: Default::default(),
            artifact_body_schema_enabled: true,
            artifact_auth_enforced: false,
            ui_auth_enabled: true,
            #[cfg(feature = "discord")]
            discord: None,
            #[cfg(feature = "whatsapp")]
            whatsapp: None,
        };
        let options = SecureOptions {
            profile: dir.path().to_owned(),
            bind: format!("127.0.0.1:{secure_port}").parse().unwrap(),
            discovery_bind: format!("127.0.0.1:{discovery_port}").parse().unwrap(),
            names: vec!["localhost".into()],
            instance_id: instance,
            webhook_bind: None,
            parent_lease: None,
        };
        let task = tokio::spawn(serve(config, options));
        let manifest = security::local_profile_path(dir.path());
        let deadline = tokio::time::Instant::now() + Duration::from_secs(10);
        while !manifest.exists() {
            assert!(tokio::time::Instant::now() < deadline);
            tokio::time::sleep(Duration::from_millis(20)).await;
        }
        tokio::time::sleep(Duration::from_millis(100)).await;
        let local = security::LocalProfile::load(&manifest).unwrap();
        let root = std::fs::read_to_string(&local.root_pem_path).unwrap();
        let cert = std::fs::read_to_string(&local.certificate_pem_path).unwrap();
        let key = String::from_utf8(security::read_private(&local.private_key_pem_path).unwrap())
            .unwrap();
        assert_eq!(
            request(
                secure_port,
                &root,
                None,
                "GET",
                "/v1/projects",
                "",
                "Authorization: Bearer legacy-secret\r\n"
            )
            .await
            .0,
            401
        );
        let invitation = request(
            secure_port,
            &root,
            Some((&cert, &key)),
            "POST",
            "/v1/server/invitation",
            "",
            "",
        )
        .await;
        assert_eq!(invitation.0, 200);
        let invitation: security::Invitation = serde_json::from_str(&invitation.1).unwrap();
        let identity = security::ClientIdentity::generate().unwrap();
        let instance = uuid::Uuid::now_v7().to_string();
        let enroll = EnrollmentRequest {
            request_id: uuid::Uuid::now_v7().to_string(),
            invitation: invitation.invitation,
            csr_pem: identity.csr_pem,
            instance_id: instance.clone(),
        };
        let enrolled = request(
            secure_port,
            &root,
            None,
            "POST",
            "/v1/enrollment/requests",
            &serde_json::to_string(&enroll).unwrap(),
            "",
        )
        .await;
        assert_eq!(enrolled.0, 200);
        let enrolled: security::EnrollmentResponse = serde_json::from_str(&enrolled.1).unwrap();
        let recovered = request(
            secure_port,
            &root,
            None,
            "GET",
            &format!("/v1/enrollment/requests/{}", enroll.request_id),
            "",
            "",
        )
        .await;
        assert_eq!(recovered.0, 200);
        let recovered: security::EnrollmentResponse = serde_json::from_str(&recovered.1).unwrap();
        assert_eq!(recovered.serial, enrolled.serial);
        let id = Some((
            enrolled.certificate_pem.as_str(),
            identity.private_key_pem.as_str(),
        ));
        assert_eq!(
            request(secure_port, &root, id, "GET", "/v1/projects", "", "")
                .await
                .0,
            200
        );
        assert_eq!(
            request(secure_port, &root, id, "GET", "/v1/users", "", "")
                .await
                .0,
            403
        );
        assert_eq!(
            request(
                secure_port,
                &root,
                id,
                "POST",
                "/v1/server/invitation",
                "",
                ""
            )
            .await
            .0,
            403
        );
        assert_eq!(
            request(
                secure_port,
                &root,
                id,
                "GET",
                "/v1/projects",
                "",
                "X-Agent-Instance-Id: 00000000-0000-4000-8000-000000000001\r\n"
            )
            .await
            .0,
            200
        );
        assert_eq!(
            request(
                secure_port,
                &root,
                id,
                "POST",
                "/v1/enrollment/activate",
                "",
                ""
            )
            .await
            .0,
            200
        );
        assert_eq!(
            request(
                secure_port,
                &root,
                id,
                "POST",
                "/v1/enrollment/activate",
                "",
                ""
            )
            .await
            .0,
            200
        );
        let server_status = request(
            secure_port,
            &root,
            Some((&cert, &key)),
            "GET",
            "/v1/server/status",
            "",
            "",
        )
        .await;
        assert_eq!(server_status.0, 200);
        let server_status: serde_json::Value = serde_json::from_str(&server_status.1).unwrap();
        assert_eq!(server_status["process_id"], std::process::id());
        assert_eq!(
            server_status["secure_bind"],
            format!("127.0.0.1:{secure_port}")
        );
        assert_eq!(
            server_status["discovery_bind"],
            format!("127.0.0.1:{discovery_port}")
        );
        assert_eq!(
            request(
                secure_port,
                &root,
                Some((&cert, &key)),
                "POST",
                "/v1/server/shutdown",
                r#"{"expected_process_nonce":"wrong-generation"}"#,
                ""
            )
            .await
            .0,
            409
        );
        let shutdown_body =
            serde_json::json!({"expected_process_nonce":server_status["process_nonce"]})
                .to_string();
        let (mut socket, message) = websocket(
            secure_port,
            &root,
            &enrolled.certificate_pem,
            &identity.private_key_pem,
            "fixture-consumer",
        )
        .await;
        assert!(message.contains("subscribed"));
        let second_identity = security::ClientIdentity::generate().unwrap();
        let second_req = EnrollmentRequest {
            request_id: uuid::Uuid::now_v7().to_string(),
            invitation: enroll.invitation.clone(),
            csr_pem: second_identity.csr_pem,
            instance_id: uuid::Uuid::now_v7().to_string(),
        };
        let second = request(
            secure_port,
            &root,
            None,
            "POST",
            "/v1/enrollment/requests",
            &serde_json::to_string(&second_req).unwrap(),
            "",
        )
        .await;
        assert_eq!(second.0, 200);
        let second: security::EnrollmentResponse = serde_json::from_str(&second.1).unwrap();
        let second_id = Some((
            second.certificate_pem.as_str(),
            second_identity.private_key_pem.as_str(),
        ));
        assert_eq!(
            request(
                secure_port,
                &root,
                second_id,
                "GET",
                "/v1/projects",
                "",
                "X-Agent-Instance-Id: 00000000-0000-4000-8000-000000000001\r\n"
            )
            .await
            .0,
            403
        );
        let (mut peer_socket, message) = websocket(
            secure_port,
            &root,
            &second.certificate_pem,
            &second_identity.private_key_pem,
            "fixture-consumer",
        )
        .await;
        assert!(message.contains("consumer belongs to another node"));
        let _ = peer_socket.shutdown().await;
        assert_eq!(
            request(
                secure_port,
                &root,
                Some((&cert, &key)),
                "POST",
                &format!("/v1/server/nodes/{}/revoke", enrolled.node_id),
                "",
                ""
            )
            .await
            .0,
            204
        );
        assert_eq!(websocket_message(&mut socket).await, "closed");
        let _ = socket.shutdown().await;
        assert_eq!(
            request(secure_port, &root, id, "GET", "/v1/projects", "", "")
                .await
                .0,
            401
        );
        assert_eq!(
            request(
                secure_port,
                &root,
                Some((&cert, &key)),
                "POST",
                "/v1/server/shutdown",
                &shutdown_body,
                ""
            )
            .await
            .0,
            202
        );
        tokio::time::timeout(Duration::from_secs(10), task)
            .await
            .unwrap()
            .unwrap()
            .unwrap();
    }
    #[tokio::test]
    async fn parent_lease_loss_bounds_shutdown_with_stalled_authorized_body() {
        let base =
            std::path::Path::new(env!("CARGO_MANIFEST_DIR")).join("../../target/test-fixtures");
        std::fs::create_dir_all(&base).unwrap();
        let dir = tempfile::tempdir_in(base).unwrap();
        let lease_path = dir.path().join("parent.lock");
        let parent = std::fs::OpenOptions::new()
            .create_new(true)
            .read(true)
            .write(true)
            .open(&lease_path)
            .unwrap();
        parent.try_lock().unwrap();
        let secure_port = port();
        let discovery_port = port();
        let config = runtime::RuntimeConfig {
            listen_addr: "127.0.0.1:0".parse().unwrap(),
            database: crate::db::DatabaseConfig::sqlite(":memory:"),
            api_key: "fixture-key".into(),
            default_channel: "none".into(),
            retention_days: 30,
            bot_retention_days: 7,
            artifact_operations: Default::default(),
            artifact_body_schema_enabled: true,
            artifact_auth_enforced: false,
            ui_auth_enabled: true,
            #[cfg(feature = "discord")]
            discord: None,
            #[cfg(feature = "whatsapp")]
            whatsapp: None,
        };
        let options = SecureOptions {
            profile: dir.path().to_owned(),
            bind: format!("127.0.0.1:{secure_port}").parse().unwrap(),
            discovery_bind: format!("127.0.0.1:{discovery_port}").parse().unwrap(),
            names: vec!["localhost".into()],
            instance_id: uuid::Uuid::now_v7().to_string(),
            webhook_bind: None,
            parent_lease: Some(lease_path),
        };
        let task = tokio::spawn(serve(config, options));
        let manifest = security::local_profile_path(dir.path());
        let deadline = tokio::time::Instant::now() + Duration::from_secs(10);
        while !manifest.exists() {
            assert!(tokio::time::Instant::now() < deadline);
            tokio::time::sleep(Duration::from_millis(20)).await;
        }
        let local = security::LocalProfile::load(&manifest).unwrap();
        let root = std::fs::read_to_string(local.root_pem_path).unwrap();
        let cert = std::fs::read_to_string(local.certificate_pem_path).unwrap();
        let key = String::from_utf8(security::read_private(&local.private_key_pem_path).unwrap())
            .unwrap();
        // A successful authenticated request proves readiness before opening an
        // unfinished JSON body that graceful HTTP shutdown cannot complete.
        assert_eq!(
            request(
                secure_port,
                &root,
                Some((&cert, &key)),
                "GET",
                "/v1/projects",
                "",
                ""
            )
            .await
            .0,
            200
        );
        let mut stalled = connection(secure_port, &root, Some((&cert, &key))).await;
        stalled.write_all(b"POST /v1/projects HTTP/1.1\r\nHost: localhost\r\nContent-Type: application/json\r\nContent-Length: 1000\r\n\r\n{").await.unwrap();
        tokio::time::sleep(Duration::from_millis(100)).await;
        let started = tokio::time::Instant::now();
        drop(parent);
        tokio::time::timeout(Duration::from_secs(9), task)
            .await
            .expect("parent lease shutdown must be bounded")
            .unwrap()
            .unwrap();
        assert!(started.elapsed() < Duration::from_secs(9));
        drop(stalled);
    }
}
