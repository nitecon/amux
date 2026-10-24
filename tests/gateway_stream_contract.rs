//! Gateway task-stream transport contract: authenticated upgrade, subscribe,
//! ping/pong, receipts, bounds, redirect refusal and redaction. Local fixtures only.
use amux::gateway_stream::{connect, next_json, send_json};
use futures_util::{SinkExt, StreamExt};
use serde_json::{Value, json};
use std::sync::{Arc, Mutex};
use tokio::{
    io::{AsyncReadExt, AsyncWriteExt},
    net::TcpListener,
};
use tokio_tungstenite::tungstenite::{
    Message,
    handshake::server::{Request, Response},
};
use uuid::Uuid;

const KEY: &str = "secret-test-key-7f3a";

type Server = tokio_tungstenite::WebSocketStream<tokio::net::TcpStream>;

/// Accept one upgrade, recording the request path and Authorization header.
// The handshake callback's error type is fixed by tungstenite.
#[allow(clippy::result_large_err)]
async fn fixture() -> (String, tokio::task::JoinHandle<(String, String, Server)>) {
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let url = format!("http://{}", listener.local_addr().unwrap());
    let handle = tokio::spawn(async move {
        let (tcp, _) = listener.accept().await.unwrap();
        let seen = Arc::new(Mutex::new((String::new(), String::new())));
        let record = seen.clone();
        let ws = tokio_tungstenite::accept_hdr_async(tcp, move |req: &Request, res: Response| {
            let auth = req
                .headers()
                .get("authorization")
                .map(|v| v.to_str().unwrap().to_owned());
            *record.lock().unwrap() = (req.uri().path().to_owned(), auth.unwrap_or_default());
            Ok(res)
        })
        .await
        .unwrap();
        let (path, auth) = seen.lock().unwrap().clone();
        (path, auth, ws)
    });
    (url, handle)
}

async fn read_text(ws: &mut Server) -> Value {
    loop {
        match ws.next().await.unwrap().unwrap() {
            Message::Text(text) => return serde_json::from_str(&text).unwrap(),
            Message::Pong(_) | Message::Ping(_) => {}
            other => panic!("unexpected client frame {other:?}"),
        }
    }
}

#[tokio::test]
async fn authenticated_subscribe_ping_event_and_receipt() {
    let (url, server) = fixture().await;
    let consumer = Uuid::new_v4();
    let mut socket = connect(&format!("{url}/"), KEY, consumer, Some(41))
        .await
        .unwrap();
    let (path, auth, mut ws) = server.await.unwrap();
    assert_eq!(path, "/v1/tasks/stream");
    assert_eq!(auth, format!("Bearer {KEY}"));
    assert_eq!(
        read_text(&mut ws).await,
        json!({"type":"subscribe","protocol_version":1,"consumer_id":consumer,"after_event_id":41})
    );

    ws.send(Message::Ping(b"hb".to_vec())).await.unwrap();
    let event = json!({"type":"event","event":{"id":42,"kind":"task_created"}});
    ws.send(Message::Text(event.to_string())).await.unwrap();
    assert_eq!(next_json(&mut socket).await.unwrap(), event);
    match ws.next().await.unwrap().unwrap() {
        Message::Pong(data) => assert_eq!(data, b"hb"),
        other => panic!("expected pong, got {other:?}"),
    }

    let receipt = json!({"type":"ack","event_id":42,"status":"received"});
    send_json(&mut socket, receipt.clone()).await.unwrap();
    assert_eq!(read_text(&mut ws).await, receipt);

    ws.close(None).await.unwrap();
    assert_eq!(
        next_json(&mut socket).await.unwrap_err(),
        "Gateway stream closed"
    );
}

#[tokio::test]
async fn absent_cursor_subscribes_with_null() {
    let (url, server) = fixture().await;
    let _socket = connect(&url, KEY, Uuid::new_v4(), None).await.unwrap();
    let (_, _, mut ws) = server.await.unwrap();
    assert_eq!(read_text(&mut ws).await["after_event_id"], Value::Null);
}

#[tokio::test]
async fn bad_config_is_refused_without_leaking_the_key() {
    let id = Uuid::new_v4();
    for url in [
        "ftp://127.0.0.1:1",
        "not a url",
        "http://gateway.example",
        "ws://gateway.example",
        "http://user:pw@127.0.0.1:1",
        "http://127.0.0.1:1/api",
        "http://127.0.0.1:1/?x=1",
        "http://127.0.0.1:1/#f",
    ] {
        let err = connect(url, KEY, id, None).await.unwrap_err();
        assert!(!err.contains(KEY) && !err.contains("pw"), "{err}");
    }
    assert!(connect("http://127.0.0.1:1", "", id, None).await.is_err());
    assert!(
        connect("http://127.0.0.1:1", KEY, Uuid::nil(), None)
            .await
            .is_err()
    );
    assert!(
        connect("http://127.0.0.1:1", KEY, id, Some(-1))
            .await
            .is_err()
    );
    assert!(
        connect("http://127.0.0.1:1", "bad\nkey", id, None)
            .await
            .is_err()
    );

    // Unreachable gateway: static message, no key and no raw OS error text.
    let port = TcpListener::bind("127.0.0.1:0")
        .await
        .unwrap()
        .local_addr()
        .unwrap()
        .port();
    let err = connect(&format!("ws://127.0.0.1:{port}"), KEY, id, None)
        .await
        .unwrap_err();
    assert_eq!(err, "Gateway upgrade rejected or unavailable");
}

#[tokio::test]
async fn oversized_binary_and_invalid_frames_fail_closed() {
    for (frame, expected) in [
        (
            Message::Text("x".repeat(64 * 1024 + 1)),
            "Gateway stream message exceeds 64 KiB",
        ),
        (
            Message::Binary(vec![1, 2, 3]),
            "Unexpected binary gateway frame",
        ),
        (
            Message::Text(format!("not json {KEY}")),
            "Invalid gateway stream JSON",
        ),
    ] {
        let (url, server) = fixture().await;
        let mut socket = connect(&url, KEY, Uuid::new_v4(), None).await.unwrap();
        let (_, _, mut ws) = server.await.unwrap();
        read_text(&mut ws).await;
        ws.send(frame).await.unwrap();
        assert_eq!(next_json(&mut socket).await.unwrap_err(), expected);
    }
}

/// A 3xx upgrade response is refused; the credential never reaches the redirect target.
#[tokio::test]
async fn redirects_and_rejections_are_not_followed() {
    let target = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let location = format!("ws://{}/v1/tasks/stream", target.local_addr().unwrap());
    for status in ["302 Found", "401 Unauthorized"] {
        let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let url = format!("http://{}", listener.local_addr().unwrap());
        let reply = format!(
            "HTTP/1.1 {status}\r\nLocation: {location}\r\nContent-Length: 0\r\nConnection: close\r\n\r\n"
        );
        let server = tokio::spawn(async move {
            let (mut tcp, _) = listener.accept().await.unwrap();
            let mut buf = vec![0; 4096];
            let _ = tcp.read(&mut buf).await.unwrap();
            tcp.write_all(reply.as_bytes()).await.unwrap();
        });
        let err = connect(&url, KEY, Uuid::new_v4(), None).await.unwrap_err();
        assert_eq!(err, "Gateway upgrade rejected or unavailable");
        server.await.unwrap();
    }
    let probe = tokio::time::timeout(std::time::Duration::from_millis(200), target.accept()).await;
    assert!(probe.is_err(), "redirect target was contacted");
}

#[tokio::test]
async fn oversized_outbound_frame_is_not_sent() {
    let (url, server) = fixture().await;
    let mut socket = connect(&url, KEY, Uuid::new_v4(), None).await.unwrap();
    let (_, _, mut ws) = server.await.unwrap();
    read_text(&mut ws).await;
    assert_eq!(
        send_json(&mut socket, json!({"oversized": "x".repeat(65536)}))
            .await
            .unwrap_err(),
        "Gateway stream message exceeds 64 KiB"
    );
    assert!(
        tokio::time::timeout(std::time::Duration::from_millis(30), ws.next())
            .await
            .is_err()
    );
}
