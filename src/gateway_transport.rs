//! One connection profile for REST, dispatch and WebSocket credentials.
use crate::gateway_preferences::EnrolledConnection;
use amux_gateway_backend::security::{LocalProfile, read_private};
use anyhow::{Context, Result, bail, ensure};
use reqwest::{Client, Method, RequestBuilder, Url};
use sha2::{Digest, Sha256};
use std::{
    fmt,
    io::Cursor,
    sync::{Arc, Mutex},
    time::Duration,
};

#[derive(Clone)]
enum Kind {
    Bearer { url: String, token: String },
    Enrolled(EnrolledConnection),
}

#[derive(Clone)]
pub struct TransportProfile {
    kind: Kind,
    cached: Arc<Mutex<Option<TransportSnapshot>>>,
}

impl fmt::Debug for TransportProfile {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(if self.is_enrolled() {
            "TransportProfile(mTLS)"
        } else {
            "TransportProfile(bearer)"
        })
    }
}

#[derive(Clone)]
pub struct TransportSnapshot {
    pub client: Client,
    pub base: Url,
    pub tls: Option<Arc<rustls::ClientConfig>>,
    pub generation: String,
    bearer: Option<String>,
}

impl TransportSnapshot {
    pub fn request(&self, method: Method, url: Url) -> RequestBuilder {
        let request = self.client.request(method, url);
        match &self.bearer {
            Some(token) => request.bearer_auth(token),
            None => request,
        }
    }

    pub fn websocket_bearer(&self) -> Option<&str> {
        self.bearer.as_deref()
    }
}

pub fn origin(value: &str, secure: bool) -> Result<Url> {
    let url = Url::parse(value.trim()).context("invalid gateway URL")?;
    ensure!(
        matches!(url.scheme(), "http" | "https")
            && (!secure || url.scheme() == "https")
            && url.host_str().is_some()
            && url.username().is_empty()
            && url.password().is_none()
            && url.query().is_none()
            && url.fragment().is_none()
            && url.path() == "/",
        "gateway URL must be an HTTP(S) origin without credentials, path, query or fragment"
    );
    Ok(url)
}

impl TransportProfile {
    pub fn bearer(url: &str, token: String) -> Result<Self> {
        origin(url, false)?;
        Ok(Self {
            kind: Kind::Bearer {
                url: url.into(),
                token,
            },
            cached: Arc::new(Mutex::new(None)),
        })
    }

    pub fn enrolled(connection: &EnrolledConnection) -> Result<Self> {
        origin(&connection.url, true)?;
        for value in [&connection.cluster_id, &connection.node_id] {
            let id = uuid::Uuid::parse_str(value).context("invalid enrolled identity")?;
            ensure!(
                !id.is_nil() && id.to_string() == *value,
                "invalid enrolled identity"
            );
        }
        Ok(Self {
            kind: Kind::Enrolled(connection.clone()),
            cached: Arc::new(Mutex::new(None)),
        })
    }

    pub fn is_enrolled(&self) -> bool {
        matches!(self.kind, Kind::Enrolled(_))
    }

    pub fn url(&self) -> Result<Url> {
        match &self.kind {
            Kind::Bearer { url, .. } => origin(url, false),
            Kind::Enrolled(connection) => origin(&connection.url, true),
        }
    }

    pub fn namespace(&self) -> String {
        // Bearer profiles retain their existing journal identity.
        let value = match &self.kind {
            Kind::Bearer { url, token } => format!("{url}\0{token}"),
            Kind::Enrolled(connection) => format!(
                "amux-mtls-v1\0{}\0{}",
                connection.cluster_id, connection.node_id
            ),
        };
        format!("{:x}", Sha256::digest(value.as_bytes()))
    }

    /// Load one complete credential generation; callers keep its pooled client
    /// until a replacement generation is installed atomically.
    pub fn snapshot(&self) -> Result<TransportSnapshot> {
        let base = self.url()?;
        let mut cached = self
            .cached
            .lock()
            .map_err(|_| anyhow::anyhow!("connection state unavailable"))?;
        let (generation, local) = match &self.kind {
            Kind::Bearer { .. } => (self.namespace(), None),
            Kind::Enrolled(connection) => {
                let local = LocalProfile::load(&connection.profile_path)
                    .context("could not load enrolled credentials")?;
                ensure!(
                    local.version == 1
                        && local.cluster_id == connection.cluster_id
                        && local.node_id == connection.node_id,
                    "enrolled credentials belong to another server or node"
                );
                (local.serial.clone(), Some(local))
            }
        };
        if let Some(snapshot) = cached
            .as_ref()
            .filter(|snapshot| snapshot.generation == generation && snapshot.base == base)
        {
            return Ok(snapshot.clone());
        }
        let mut builder = Client::builder()
            .timeout(Duration::from_secs(15))
            .redirect(reqwest::redirect::Policy::none());
        let (tls, bearer) = match (&self.kind, local) {
            (Kind::Bearer { token, .. }, _) => (None, Some(token.clone())),
            (Kind::Enrolled(_), Some(local)) => {
                let root =
                    read_private(&local.root_pem_path).context("could not load server trust")?;
                let certificate = read_private(&local.certificate_pem_path)
                    .context("could not load node certificate")?;
                let key = read_private(&local.private_key_pem_path)
                    .context("could not load node private key")?;
                let roots = rustls_pemfile::certs(&mut Cursor::new(&root))
                    .collect::<std::io::Result<Vec<_>>>()?;
                ensure!(
                    roots.len() == 1,
                    "server trust must contain one installation root"
                );
                let certificates = rustls_pemfile::certs(&mut Cursor::new(&certificate))
                    .collect::<std::io::Result<Vec<_>>>()?;
                ensure!(!certificates.is_empty(), "node certificate chain is empty");
                let key_der = rustls_pemfile::private_key(&mut Cursor::new(&key))?
                    .context("node private key is missing")?;
                let mut store = rustls::RootCertStore::empty();
                store.add(roots[0].clone())?;
                let config = rustls::ClientConfig::builder_with_provider(Arc::new(
                    rustls::crypto::ring::default_provider(),
                ))
                .with_safe_default_protocol_versions()?
                .with_root_certificates(store)
                .with_client_auth_cert(certificates, key_der)?;
                let mut identity = key;
                identity.extend_from_slice(&certificate);
                // Native REST and discovery use the same direct route as native
                // WebSockets; system HTTP proxies must not intercept pairing.
                builder = builder
                    .no_proxy()
                    .tls_built_in_root_certs(false)
                    .add_root_certificate(reqwest::Certificate::from_pem(&root)?)
                    .identity(reqwest::Identity::from_pem(&identity)?);
                (Some(Arc::new(config)), None)
            }
            _ => bail!("enrolled credentials are unavailable"),
        };
        let snapshot = TransportSnapshot {
            client: builder.build()?,
            base,
            tls,
            generation,
            bearer,
        };
        *cached = Some(snapshot.clone());
        Ok(snapshot)
    }

    pub async fn current(&self) -> Result<TransportSnapshot> {
        if let Kind::Enrolled(connection) = &self.kind
            && let Err(error) = crate::server_controller::renew_if_due(connection.clone()).await
        {
            // Renewal starts before expiry. A temporarily unavailable issuer
            // must not prevent use of the still-valid enrolled identity.
            let active = LocalProfile::load(&connection.profile_path)?;
            if active.expires_at <= amux_gateway_backend::security::now_ms() {
                return Err(error).context("node certificate expired; renewal failed");
            }
            eprintln!("amux: certificate renewal pending; using current certificate: {error}");
        }
        let profile = self.clone();
        tokio::task::spawn_blocking(move || profile.snapshot())
            .await
            .context("credential loader stopped")?
    }
}
