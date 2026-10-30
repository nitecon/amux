//! Enrolled CLI transport in front of deterministic HTTP contract fixtures.
//! The proxy requires a client certificate and forwards requests unchanged.
use amux::gateway_preferences::{EnrolledConnection, GatewayMode, GatewayPreferences};
use amux_gateway_backend::security::{SecurityStore, local_profile_path, read_private};
use std::{
    io::{Read, Write},
    net::{TcpListener, TcpStream},
    path::Path,
    sync::{
        Arc,
        atomic::{AtomicBool, Ordering},
    },
    thread,
    time::Duration,
};

pub struct NativeCli {
    #[allow(dead_code)] // Some callers use only the saved enrollment.
    pub url: String,
    _profile: tempfile::TempDir,
    stop: Arc<AtomicBool>,
    worker: Option<thread::JoinHandle<()>>,
}

impl NativeCli {
    pub fn new(state: &Path, upstream: &str) -> Self {
        std::fs::create_dir_all(state).unwrap();
        let profile = tempfile::tempdir().unwrap();
        let security = SecurityStore::open(profile.path(), &["127.0.0.1".into()]).unwrap();
        let listener = TcpListener::bind("127.0.0.1:0").unwrap();
        let url = format!("https://{}", listener.local_addr().unwrap());
        let local = security
            .provision_local(&url, &uuid::Uuid::new_v4().to_string())
            .unwrap();
        let (certificate, key) = security.server_paths().unwrap();
        let certificates =
            rustls_pemfile::certs(&mut read_private(&certificate).unwrap().as_slice())
                .collect::<std::io::Result<Vec<_>>>()
                .unwrap();
        let key = rustls_pemfile::private_key(&mut read_private(&key).unwrap().as_slice())
            .unwrap()
            .unwrap();
        let mut roots = rustls::RootCertStore::empty();
        for root in rustls_pemfile::certs(&mut security.root_pem.as_bytes()) {
            roots.add(root.unwrap()).unwrap();
        }
        let provider = Arc::new(rustls::crypto::ring::default_provider());
        let verifier = rustls::server::WebPkiClientVerifier::builder_with_provider(
            Arc::new(roots),
            provider.clone(),
        )
        .build()
        .unwrap();
        let config = rustls::ServerConfig::builder_with_provider(provider)
            .with_safe_default_protocol_versions()
            .unwrap()
            .with_client_cert_verifier(verifier)
            .with_single_cert(certificates, key)
            .unwrap();
        GatewayPreferences {
            mode: GatewayMode::Client,
            enrolled: Some(EnrolledConnection {
                url: url.clone(),
                cluster_id: local.cluster_id,
                node_id: local.node_id,
                profile_path: local_profile_path(profile.path()),
            }),
            ..Default::default()
        }
        .save(&state.join("gateway-preferences.json"))
        .unwrap();
        listener.set_nonblocking(true).unwrap();
        let upstream = reqwest::Url::parse(upstream).unwrap();
        let upstream = (
            upstream.host_str().unwrap().to_owned(),
            upstream.port_or_known_default().unwrap(),
        );
        let stop = Arc::new(AtomicBool::new(false));
        let stopped = stop.clone();
        let config = Arc::new(config);
        let worker = thread::spawn(move || {
            // Retain the authority lock and credentials for the fixture lifetime.
            let _security = security;
            while !stopped.load(Ordering::Relaxed) {
                let stream = match listener.accept() {
                    Ok((stream, _)) => stream,
                    Err(error) if error.kind() == std::io::ErrorKind::WouldBlock => {
                        thread::sleep(Duration::from_millis(5));
                        continue;
                    }
                    Err(error) => panic!("native fixture accept: {error}"),
                };
                stream.set_nonblocking(false).unwrap();
                stream
                    .set_read_timeout(Some(Duration::from_secs(5)))
                    .unwrap();
                stream
                    .set_write_timeout(Some(Duration::from_secs(5)))
                    .unwrap();
                let session = rustls::ServerConnection::new(config.clone()).unwrap();
                let mut stream = rustls::StreamOwned::new(session, stream);
                let request = match read_request(&mut stream) {
                    Ok(request) => request,
                    Err(_) => continue,
                };
                let Ok(mut backend) = TcpStream::connect((&*upstream.0, upstream.1)) else {
                    continue;
                };
                backend
                    .set_read_timeout(Some(Duration::from_secs(5)))
                    .unwrap();
                backend.write_all(&request).unwrap();
                let mut response = Vec::new();
                backend.read_to_end(&mut response).unwrap();
                let _ = stream.write_all(&response);
                let _ = stream.flush();
            }
        });
        Self {
            url,
            _profile: profile,
            stop,
            worker: Some(worker),
        }
    }
}

fn read_request(stream: &mut impl Read) -> std::io::Result<Vec<u8>> {
    let mut bytes = Vec::new();
    let (end, length) = loop {
        let mut buffer = [0; 4096];
        let count = stream.read(&mut buffer)?;
        if count == 0 {
            return Err(std::io::ErrorKind::UnexpectedEof.into());
        }
        bytes.extend_from_slice(&buffer[..count]);
        if let Some(end) = bytes.windows(4).position(|part| part == b"\r\n\r\n") {
            let header = String::from_utf8_lossy(&bytes[..end]).to_lowercase();
            let length = header
                .lines()
                .find_map(|line| line.strip_prefix("content-length: ")?.parse::<usize>().ok())
                .unwrap_or(0);
            break (end + 4, length);
        }
    };
    while bytes.len() < end + length {
        let mut buffer = [0; 4096];
        let count = stream.read(&mut buffer)?;
        if count == 0 {
            return Err(std::io::ErrorKind::UnexpectedEof.into());
        }
        bytes.extend_from_slice(&buffer[..count]);
    }
    Ok(bytes)
}

impl Drop for NativeCli {
    fn drop(&mut self) {
        self.stop.store(true, Ordering::Relaxed);
        self.worker.take().unwrap().join().unwrap();
    }
}
