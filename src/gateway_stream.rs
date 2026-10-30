//! Bounded, authenticated gateway task-stream transport (CMUX wire parity).
//! Errors are static text: never the API key or raw network errors.
use futures_util::{SinkExt, StreamExt};
use reqwest::Url;
use serde_json::{Value, json};
use std::time::{Duration, Instant, UNIX_EPOCH};
use tokio_tungstenite::{MaybeTlsStream, WebSocketStream, tungstenite};
use tungstenite::{Message, client::IntoClientRequest, protocol::WebSocketConfig};
use uuid::Uuid;

pub type Socket = WebSocketStream<MaybeTlsStream<tokio::net::TcpStream>>;

/// Frame and message cap shared with the CMUX stream protocol.
pub const MAX_MESSAGE: usize = 64 * 1024;

/// Gateway time anchored at the upgrade response; advances by monotonic elapsed time only,
/// so local wall-clock skew never affects event freshness.
#[derive(Debug, Clone)]
pub struct GatewayClock {
    epoch_ms: i64,
    anchor: Instant,
}

impl GatewayClock {
    /// Anchor a positive HTTP `Date` (RFC 7231) at the current instant.
    pub fn from_http_date(value: &str) -> Option<Self> {
        let time = httpdate::parse_http_date(value.trim()).ok()?;
        let epoch_ms = i64::try_from(time.duration_since(UNIX_EPOCH).ok()?.as_millis()).ok()?;
        (epoch_ms > 0).then(|| Self {
            epoch_ms,
            anchor: Instant::now(),
        })
    }

    /// Current gateway time in Unix milliseconds.
    pub fn now_ms(&self) -> i64 {
        let elapsed = i64::try_from(self.anchor.elapsed().as_millis()).unwrap_or(i64::MAX);
        self.epoch_ms.saturating_add(elapsed)
    }
}

/// Upgrade `/v1/tasks/stream` with a Bearer key, then subscribe as `consumer_id` after the cursor.
/// The connection is refused on redirects or after a ten-second handshake deadline.
pub async fn connect(
    url: &str,
    api_key: &str,
    consumer_id: Uuid,
    after_event_id: Option<i64>,
) -> Result<Socket, String> {
    connect_with_clock(url, api_key, consumer_id, after_event_id)
        .await
        .map(|(socket, _)| socket)
}

/// [`connect`], also returning the clock from the upgrade response `Date`, when valid.
pub async fn connect_with_clock(
    url: &str,
    api_key: &str,
    consumer_id: Uuid,
    after_event_id: Option<i64>,
) -> Result<(Socket, Option<GatewayClock>), String> {
    if api_key.is_empty() {
        return Err("Enter a gateway API key".into());
    }
    connect_credentials(url, Some(api_key), None, consumer_id, after_event_id).await
}

pub async fn connect_profile_with_clock(
    profile: &crate::gateway_transport::TransportProfile,
    consumer_id: Uuid,
    after_event_id: Option<i64>,
) -> Result<(Socket, Option<GatewayClock>), String> {
    connect_profile_generation(profile, consumer_id, after_event_id)
        .await
        .map(|(socket, clock, _)| (socket, clock))
}

pub(crate) async fn connect_profile_generation(
    profile: &crate::gateway_transport::TransportProfile,
    consumer_id: Uuid,
    after_event_id: Option<i64>,
) -> Result<(Socket, Option<GatewayClock>, String), String> {
    let snapshot = profile
        .current()
        .await
        .map_err(|_| "Gateway credentials unavailable")?;
    let (socket, clock) = connect_credentials(
        snapshot.base.as_str(),
        snapshot.websocket_bearer(),
        snapshot.tls.clone(),
        consumer_id,
        after_event_id,
    )
    .await?;
    Ok((socket, clock, snapshot.generation))
}

async fn connect_credentials(
    url: &str,
    api_key: Option<&str>,
    tls: Option<std::sync::Arc<rustls::ClientConfig>>,
    consumer_id: Uuid,
    after_event_id: Option<i64>,
) -> Result<(Socket, Option<GatewayClock>), String> {
    if consumer_id.is_nil() {
        return Err("Invalid stream consumer ID".into());
    }
    if after_event_id.is_some_and(|id| id < 0) {
        return Err("Invalid stream cursor".into());
    }
    let (mut socket, clock) = upgrade(url, api_key, tls, "/v1/tasks/stream").await?;
    send_json(
        &mut socket,
        json!({"type":"subscribe", "protocol_version":1,
            "consumer_id":consumer_id, "after_event_id":after_event_id}),
    )
    .await?;
    Ok((socket, clock))
}

/// Upgrade `/v1/events/stream` with the profile's current credentials. The
/// caller sends its own subscribe frame. Failed upgrades reconnect normally.
pub async fn connect_events(
    profile: &crate::gateway_transport::TransportProfile,
) -> Result<(Socket, Option<GatewayClock>), String> {
    let snapshot = profile
        .current()
        .await
        .map_err(|_| "Gateway credentials unavailable")?;
    upgrade(
        snapshot.base.as_str(),
        snapshot.websocket_bearer(),
        snapshot.tls.clone(),
        "/v1/events/stream",
    )
    .await
}

async fn upgrade(
    url: &str,
    api_key: Option<&str>,
    tls: Option<std::sync::Arc<rustls::ClientConfig>>,
    path: &str,
) -> Result<(Socket, Option<GatewayClock>), String> {
    let mut url = stream_url(url)?;
    url.set_path(path);
    let mut request = url
        .as_str()
        .into_client_request()
        .map_err(|_| "Invalid gateway upgrade")?;
    if let Some(api_key) = api_key {
        request.headers_mut().insert(
            "Authorization",
            format!("Bearer {api_key}")
                .parse()
                .map_err(|_| "Invalid gateway API key")?,
        );
    }
    let limits = WebSocketConfig {
        max_message_size: Some(MAX_MESSAGE),
        max_frame_size: Some(MAX_MESSAGE),
        ..Default::default()
    };
    // tungstenite never follows redirects: a 3xx response is a rejected upgrade.
    let (socket, response) = tokio::time::timeout(Duration::from_secs(10), async move {
        match tls {
            Some(tls) => {
                tokio_tungstenite::connect_async_tls_with_config(
                    request,
                    Some(limits),
                    false,
                    Some(tokio_tungstenite::Connector::Rustls(tls)),
                )
                .await
            }
            None => {
                tokio_tungstenite::connect_async_with_config(request, Some(limits), false).await
            }
        }
    })
    .await
    .map_err(|_| "Gateway upgrade timed out")?
    .map_err(|_| "Gateway upgrade rejected or unavailable")?;
    let clock = response
        .headers()
        .get("date")
        .and_then(|value| value.to_str().ok())
        .and_then(GatewayClock::from_http_date);
    Ok((socket, clock))
}

/// Send one JSON text message with a five-second deadline.
pub async fn send_json(socket: &mut Socket, value: Value) -> Result<(), String> {
    let text = value.to_string();
    if text.len() > MAX_MESSAGE {
        return Err("Gateway stream message exceeds 64 KiB".into());
    }
    tokio::time::timeout(Duration::from_secs(5), socket.send(Message::Text(text)))
        .await
        .map_err(|_| "Gateway write timed out")?
        .map_err(|_| "Gateway stream write failed".to_owned())
}

/// Read the next JSON text message, answering pings; binary, close and errors end the stream.
pub async fn next_json(socket: &mut Socket) -> Result<Value, String> {
    loop {
        match socket.next().await {
            Some(Ok(Message::Text(text))) => {
                return serde_json::from_str(&text)
                    .map_err(|_| "Invalid gateway stream JSON".into());
            }
            Some(Ok(Message::Ping(data))) => {
                tokio::time::timeout(Duration::from_secs(5), socket.send(Message::Pong(data)))
                    .await
                    .map_err(|_| "Gateway write timed out")?
                    .map_err(|_| "Gateway stream write failed")?;
            }
            Some(Ok(Message::Pong(_) | Message::Frame(_))) => {}
            Some(Ok(Message::Binary(_))) => return Err("Unexpected binary gateway frame".into()),
            Some(Ok(Message::Close(_))) | None => return Err("Gateway stream closed".into()),
            Some(Err(tungstenite::Error::Capacity(_))) => {
                return Err("Gateway stream message exceeds 64 KiB".into());
            }
            Some(Err(_)) => return Err("Gateway stream read failed".into()),
        }
    }
}

/// Match CMUX: HTTPS/WSS origins, with plain transport allowed only on loopback.
fn stream_url(value: &str) -> Result<Url, String> {
    const INVALID: &str = "Use a gateway base URL without credentials, path, query or fragment";
    let mut url = Url::parse(value).map_err(|_| INVALID)?;
    if url.host_str().is_none()
        || !url.username().is_empty()
        || url.password().is_some()
        || url.query().is_some()
        || url.fragment().is_some()
        || !matches!(url.path(), "/" | "")
    {
        return Err(INVALID.into());
    }
    let loopback = matches!(
        url.host_str(),
        Some("localhost" | "127.0.0.1" | "[::1]" | "::1")
    );
    let scheme = match url.scheme() {
        "https" | "wss" => "wss",
        "http" | "ws" if loopback => "ws",
        _ => return Err(INVALID.into()),
    };
    url.set_scheme(scheme).map_err(|_| INVALID)?;
    Ok(url)
}
