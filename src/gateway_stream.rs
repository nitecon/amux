//! Bounded, authenticated gateway task-stream transport (CMUX wire parity).
//! Errors are static text: never the API key or raw network errors.
use futures_util::{SinkExt, StreamExt};
use reqwest::Url;
use serde_json::{Value, json};
use std::time::Duration;
use tokio_tungstenite::{MaybeTlsStream, WebSocketStream, tungstenite};
use tungstenite::{Message, client::IntoClientRequest, protocol::WebSocketConfig};
use uuid::Uuid;

pub type Socket = WebSocketStream<MaybeTlsStream<tokio::net::TcpStream>>;

/// Frame and message cap shared with the CMUX stream protocol.
pub const MAX_MESSAGE: usize = 64 * 1024;

/// Upgrade `/v1/tasks/stream` with a Bearer key, then subscribe as `consumer_id` after the cursor.
/// The connection is refused on redirects or after a ten-second handshake deadline.
pub async fn connect(
    url: &str,
    api_key: &str,
    consumer_id: Uuid,
    after_event_id: Option<i64>,
) -> Result<Socket, String> {
    if api_key.is_empty() {
        return Err("Enter a gateway API key".into());
    }
    if consumer_id.is_nil() {
        return Err("Invalid stream consumer ID".into());
    }
    if after_event_id.is_some_and(|id| id < 0) {
        return Err("Invalid stream cursor".into());
    }
    let mut url = stream_url(url)?;
    url.set_path("/v1/tasks/stream");
    let mut request = url
        .as_str()
        .into_client_request()
        .map_err(|_| "Invalid gateway upgrade")?;
    request.headers_mut().insert(
        "Authorization",
        format!("Bearer {api_key}")
            .parse()
            .map_err(|_| "Invalid gateway API key")?,
    );
    let limits = WebSocketConfig {
        max_message_size: Some(MAX_MESSAGE),
        max_frame_size: Some(MAX_MESSAGE),
        ..Default::default()
    };
    // tungstenite never follows redirects: a 3xx response is a rejected upgrade.
    let (mut socket, _) = tokio::time::timeout(
        Duration::from_secs(10),
        tokio_tungstenite::connect_async_with_config(request, Some(limits), false),
    )
    .await
    .map_err(|_| "Gateway upgrade timed out")?
    .map_err(|_| "Gateway upgrade rejected or unavailable")?;
    send_json(
        &mut socket,
        json!({"type":"subscribe", "protocol_version":1,
            "consumer_id":consumer_id, "after_event_id":after_event_id}),
    )
    .await?;
    Ok(socket)
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
