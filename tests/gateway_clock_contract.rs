//! Gateway clock contract: HTTP `Date` parsing, monotonic advance, and the connection
//! emitting `Clock` after `Subscribed` and before event frames. Local fixtures only.
use amux::delegation_connection::{ConnectionEvent, run};
use amux::gateway_stream::GatewayClock;
use futures_util::{SinkExt, StreamExt};
use serde_json::json;
use std::time::Duration;
use tokio::{net::TcpListener, sync::mpsc};
use tokio_tungstenite::tungstenite::{
    Message,
    handshake::server::{Request, Response},
};
use uuid::Uuid;

/// Sun, 06 Nov 1994 08:49:37 GMT (RFC 7231 example).
const DATE: &str = "Sun, 06 Nov 1994 08:49:37 GMT";
const DATE_MS: i64 = 784_111_777_000;

#[test]
fn parses_valid_http_dates_and_rejects_others() {
    let clock = GatewayClock::from_http_date(DATE).expect("valid date");
    let now = clock.now_ms();
    assert!((DATE_MS..DATE_MS + 1_000).contains(&now), "{now}");
    for bad in [
        "",
        "not a date",
        "1994-11-06T08:49:37Z",
        "Thu, 01 Jan 1970 00:00:00 GMT",
    ] {
        assert!(GatewayClock::from_http_date(bad).is_none(), "{bad}");
    }
}

#[test]
fn advances_by_monotonic_elapsed_time() {
    let clock = GatewayClock::from_http_date(DATE).unwrap();
    let before = clock.now_ms();
    std::thread::sleep(Duration::from_millis(30));
    let after = clock.clone().now_ms();
    assert!(
        after >= before + 30 && after < before + 1_000,
        "{before} {after}"
    );
}

/// Serve one upgrade (optionally with `Date`), subscribe, then send one rejected event frame.
// The handshake callback's error type is fixed by tungstenite.
#[allow(clippy::result_large_err)]
async fn events_for(date: Option<&'static str>) -> Vec<String> {
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let url = format!("http://{}", listener.local_addr().unwrap());
    let consumer = Uuid::new_v4();
    let (_commands, command_rx) = mpsc::channel(8);
    let (event_tx, mut events) = mpsc::channel(8);
    let worker = tokio::spawn(run(url, "key".into(), consumer, None, command_rx, event_tx));
    let (tcp, _) = listener.accept().await.unwrap();
    let mut ws = tokio_tungstenite::accept_hdr_async(tcp, move |_: &Request, mut res: Response| {
        if let Some(date) = date {
            res.headers_mut().insert("date", date.parse().unwrap());
        }
        Ok(res)
    })
    .await
    .unwrap();
    ws.next().await.unwrap().unwrap(); // subscribe
    let subscribed = json!({"type":"subscribed","protocol_version":1,
        "consumer_id":consumer,"cursor":0,"latest_event_id":0});
    let event = json!({"type":"event","event":{"id":1,"kind":"task_deleted"}});
    for frame in [subscribed, event] {
        ws.send(Message::Text(frame.to_string())).await.unwrap();
    }
    let mut seen = Vec::new();
    loop {
        let event = tokio::time::timeout(Duration::from_secs(5), events.recv())
            .await
            .unwrap()
            .unwrap();
        let rejected = matches!(event, ConnectionEvent::Rejected { .. });
        seen.push(match event {
            ConnectionEvent::State(state) => state,
            ConnectionEvent::Subscribed { .. } => "Subscribed".into(),
            ConnectionEvent::Clock(clock) => {
                let now = clock.now_ms();
                assert!((DATE_MS..DATE_MS + 5_000).contains(&now), "{now}");
                "Clock".into()
            }
            ConnectionEvent::Rejected { .. } => "Rejected".into(),
            other => panic!("unexpected {other:?}"),
        });
        if rejected {
            break;
        }
    }
    worker.abort();
    seen
}

#[tokio::test]
async fn connection_emits_clock_after_subscribed_before_events() {
    assert_eq!(
        events_for(Some(DATE)).await,
        ["Connecting", "Connected", "Subscribed", "Clock", "Rejected"]
    );
}

#[tokio::test]
async fn missing_or_invalid_date_emits_no_clock() {
    let expected = ["Connecting", "Connected", "Subscribed", "Rejected"];
    assert_eq!(events_for(None).await, expected);
    assert_eq!(events_for(Some("garbage")).await, expected);
}
