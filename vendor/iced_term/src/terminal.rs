use crate::actions::Action;
use crate::backend;
use crate::bindings::{Binding, BindingAction, BindingsLayout, InputKind};
use crate::font::TermFont;
use crate::settings::{FontSettings, Settings, ThemeSettings};
use crate::theme::{ColorPalette, Theme};
use crate::AlacrittyEvent;
use alacritty_terminal::grid::Dimensions;
use alacritty_terminal::index::{Column, Line};
use alacritty_terminal::vte::ansi::{Color as AnsiColor, NamedColor, Rgb};
use iced::futures::stream::BoxStream;
use iced::futures::{SinkExt, StreamExt};
use iced::widget::canvas::Cache;
use iced::Subscription;
use std::hash::{Hash, Hasher};
use std::io::Result;
use std::sync::Arc;
use tokio::sync::mpsc::{self, Receiver};
use tokio::sync::Mutex;

#[derive(Debug, Clone)]
pub enum Event {
    BackendCall(u64, backend::Command),
}

#[derive(Debug, Clone)]
pub enum Command {
    ChangeTheme(Box<ColorPalette>),
    ChangeFont(FontSettings),
    AddBindings(Vec<(Binding<InputKind>, BindingAction)>),
    ProxyToBackend(backend::Command),
}

pub struct Terminal {
    pub id: u64,
    widget_id: iced::widget::Id,
    pub(crate) font: TermFont,
    pub(crate) theme: Theme,
    pub(crate) cache: Cache,
    pub(crate) bindings: BindingsLayout,
    pub(crate) backend: backend::Backend,
    backend_event_rx: Arc<Mutex<Receiver<AlacrittyEvent>>>,
}

impl Terminal {
    pub fn new(id: u64, settings: Settings) -> Result<Self> {
        let (backend_event_tx, backend_event_rx) = mpsc::channel(100);
        let theme = Theme::new(settings.theme);
        let font = TermFont::new(settings.font);
        let font_measure = font.measure;

        Ok(Self {
            id,
            widget_id: iced::widget::Id::unique(),
            font,
            theme,
            bindings: BindingsLayout::default(),
            cache: Cache::default(),
            backend: backend::Backend::new(
                id,
                backend_event_tx,
                settings.backend,
                font_measure,
            )?,
            backend_event_rx: Arc::new(Mutex::new(backend_event_rx)),
        })
    }

    pub fn widget_id(&self) -> &iced::widget::Id {
        &self.widget_id
    }

    /// Native initial PTY child; callers must also verify its process generation.
    /// This is not necessarily the foreground agent or an authority identity.
    pub fn child_pid(&self) -> Option<u32> {
        self.backend.child_pid()
    }

    /// Borrowed native PTY master for foreground input verification. Never a PID lookup.
    #[cfg(target_os = "linux")]
    pub fn native_terminal_fd(&self) -> std::os::fd::BorrowedFd<'_> {
        self.backend.native_terminal_fd()
    }

    /// A successful enqueue is not proof of a PTY write or agent acknowledgment.
    /// An error may follow channel acceptance; callers must retain uncertainty.
    /// Time since native PTY output or terminal input; no screen interpretation.
    pub fn activity_elapsed(&self) -> Option<std::time::Duration> {
        self.backend.activity_elapsed()
    }

    pub fn try_enqueue_input(&self, input: Vec<u8>) -> std::io::Result<()> {
        self.backend.try_enqueue_input(input)
    }

    /// Negotiated terminal input protocol flags; does not inspect rendered text.
    pub fn input_mode(&self) -> alacritty_terminal::term::TermMode {
        self.backend.input_mode()
    }

    /// A bounded live screen observation, never an acknowledgment or readiness proof.
    pub fn observation(
        &self,
        max_cells: usize,
    ) -> Option<crate::TerminalObservation> {
        self.backend.observation(max_cells)
    }

    /// The same bounded observation with styling for every visible cell,
    /// captured atomically. Never an acknowledgment or readiness proof.
    pub fn grid_observation(
        &self,
        max_cells: usize,
    ) -> Option<crate::TerminalGridObservation> {
        self.backend.grid_observation(max_cells)
    }

    /// Plain text from the current rendered viewport, capped at 32,768 characters.
    /// Includes combining marks; excludes scrollback and any readiness semantics.
    pub fn screen_text(&self) -> String {
        let grid = &self.backend.renderable_content().grid;
        let mut text = String::new();
        let mut remaining = 32_768;
        for row in 0..grid.screen_lines() {
            for column in 0..grid.columns() {
                let cell = &grid[Line(row as i32)][Column(column)];
                for character in std::iter::once(cell.c)
                    .chain(cell.zerowidth().into_iter().flatten().copied())
                    .take(remaining)
                {
                    text.push(character);
                    remaining -= 1;
                }
                if remaining == 0 {
                    return text;
                }
            }
            text.push('\n');
            remaining -= 1;
            if remaining == 0 {
                break;
            }
        }
        text.trim_end().to_owned()
    }

    pub fn subscription(&self) -> Subscription<Event> {
        let data = TerminalSubscriptionData {
            id: self.id,
            event_receiver: self.backend_event_rx.clone(),
        };

        Subscription::run_with(data, terminal_subscription_stream)
    }

    pub fn handle(&mut self, cmd: Command) -> Action {
        let mut action = Action::default();

        match cmd {
            Command::ChangeTheme(color_pallete) => {
                self.theme = Theme::new(ThemeSettings::new(color_pallete));
            },
            Command::ChangeFont(font_settings) => {
                self.font = TermFont::new(font_settings);
            },
            Command::AddBindings(bindings) => {
                self.bindings.add_bindings(bindings);
            },
            Command::ProxyToBackend(
                backend::Command::ProcessAlacrittyEvent(
                    AlacrittyEvent::ColorRequest(index, formatter),
                ),
            ) => {
                let color = match index {
                    0..=255 => Some(AnsiColor::Indexed(index as u8)),
                    256 | 258 => Some(AnsiColor::Named(NamedColor::Foreground)),
                    257 => Some(AnsiColor::Named(NamedColor::Background)),
                    _ => None,
                };
                if let Some(color) = color {
                    let [r, g, b, _] = self.theme.get_color(color).into_rgba8();
                    self.backend.handle(
                        backend::Command::ProcessAlacrittyEvent(
                            AlacrittyEvent::PtyWrite(formatter(Rgb {
                                r,
                                g,
                                b,
                            })),
                        ),
                    );
                }
            },
            Command::ProxyToBackend(cmd) => {
                action = self.backend.handle(cmd);
            },
        };

        self.sync_and_redraw();
        action
    }

    fn sync_and_redraw(&mut self) {
        self.sync_font();
        self.backend.sync();
        self.redraw();
    }

    fn sync_font(&mut self) {
        self.font.sync();
        self.backend
            .handle(backend::Command::Resize(None, Some(self.font.measure)));
    }

    fn redraw(&mut self) {
        self.cache.clear();
    }
}

#[derive(Clone)]
struct TerminalSubscriptionData {
    id: u64,
    event_receiver: Arc<Mutex<Receiver<AlacrittyEvent>>>,
}

impl Hash for TerminalSubscriptionData {
    fn hash<H: Hasher>(&self, state: &mut H) {
        self.id.hash(state);
    }
}

fn terminal_subscription_stream(
    data: &TerminalSubscriptionData,
) -> BoxStream<'static, Event> {
    let id = data.id;
    let event_receiver = data.event_receiver.clone();
    iced::stream::channel(1000, async move |mut output| {
        let mut shutdown = false;
        loop {
            let mut event_receiver = event_receiver.lock().await;
            match event_receiver.recv().await {
                Some(event) => {
                    if let AlacrittyEvent::Exit = event {
                        shutdown = true
                    };

                    output
                        .send(Event::BackendCall(id, backend::Command::ProcessAlacrittyEvent(event)))
                        .await
                        .unwrap_or_else(|_| {
                            panic!("iced_term stream {}: sending BackendEventReceived event is failed", id)
                        });
                },
                None => {
                    if !shutdown {
                        panic!("iced_term stream {}: terminal event channel closed unexpected", id);
                    }
                    break;
                },
            }
        }
    })
    .boxed()
}

#[cfg(all(test, target_os = "linux"))]
mod amux_tests {
    use super::*;
    use crate::settings::BackendSettings;
    use alacritty_terminal::grid::Dimensions;
    use alacritty_terminal::index::{Column, Line};
    use std::time::Duration;

    #[test]
    #[ignore = "internal bounded subscription fixture"]
    fn closed_subscription_fixture() {
        if std::env::var_os("AMUX_CLOSED_SUBSCRIPTION_FIXTURE").is_none() {
            return;
        }
        let (sender, receiver) = mpsc::channel(4);
        sender
            .blocking_send(AlacrittyEvent::Title("final title".into()))
            .unwrap();
        sender.blocking_send(AlacrittyEvent::Exit).unwrap();
        drop(sender);
        let data = TerminalSubscriptionData {
            id: 930_019,
            event_receiver: Arc::new(Mutex::new(receiver)),
        };
        let stream = terminal_subscription_stream(&data);
        let events = tokio::runtime::Runtime::new()
            .unwrap()
            .block_on(stream.collect::<Vec<_>>());
        assert_eq!(events.len(), 2);
        assert!(
            matches!(&events[0], Event::BackendCall(930_019, backend::Command::ProcessAlacrittyEvent(AlacrittyEvent::Title(title))) if title == "final title")
        );
        assert!(matches!(
            &events[1],
            Event::BackendCall(
                930_019,
                backend::Command::ProcessAlacrittyEvent(AlacrittyEvent::Exit)
            )
        ));
    }

    #[test]
    fn closed_after_exit_subscription_finishes_without_spinning() {
        let mut child =
            std::process::Command::new(std::env::current_exe().unwrap())
                .args([
                    "--exact",
                    "terminal::amux_tests::closed_subscription_fixture",
                    "--ignored",
                ])
                .env("AMUX_CLOSED_SUBSCRIPTION_FIXTURE", "1")
                .stdout(std::process::Stdio::null())
                .spawn()
                .unwrap();
        let deadline = std::time::Instant::now() + Duration::from_secs(5);
        loop {
            if let Some(status) = child.try_wait().unwrap() {
                assert!(status.success(), "closed subscription fixture failed");
                break;
            }
            if std::time::Instant::now() >= deadline {
                child.kill().unwrap();
                child.wait().unwrap();
                panic!("closed terminal event subscription spun instead of finishing");
            }
            std::thread::sleep(Duration::from_millis(10));
        }
    }

    fn rendered(terminal: &Terminal) -> String {
        let grid = &terminal.backend.renderable_content().grid;
        let mut text = String::new();
        for row in 0..grid.screen_lines() {
            for column in 0..grid.columns() {
                text.push(grid[Line(row as i32)][Column(column)].c);
            }
        }
        text
    }

    fn events_for(terminal: &Terminal) -> BoxStream<'static, Event> {
        iced::advanced::subscription::into_recipes(terminal.subscription())
            .pop()
            .unwrap()
            .stream(iced::futures::stream::empty().boxed())
    }

    fn drive_until_rendered(
        terminal: &mut Terminal,
        events: &mut BoxStream<'static, Event>,
        runtime: &tokio::runtime::Runtime,
        marker: &str,
    ) {
        let deadline = std::time::Instant::now() + Duration::from_secs(8);
        while !rendered(terminal).contains(marker) {
            let timeout =
                deadline.saturating_duration_since(std::time::Instant::now());
            // Desktop handles commands on the GUI thread, outside Tokio.
            let event = runtime
                .block_on(async {
                    tokio::time::timeout(timeout, events.next()).await
                })
                .unwrap_or_else(|_| {
                    panic!(
                        "missing render event for {marker}; live={:?}",
                        terminal.observation(32_768)
                    )
                })
                .expect("terminal event stream closed");
            let Event::BackendCall(id, command) = event;
            assert_eq!(id, terminal.id);
            terminal.handle(Command::ProxyToBackend(command));
        }
    }

    #[test]
    fn native_checked_input_reports_a_closed_transport() {
        let mut terminal = Terminal::new(
            930_003,
            Settings {
                backend: BackendSettings {
                    program: "/bin/sh".into(),
                    args: vec!["-c".into(), "printf 'CHECKED_READY\\r\\n'; read line; printf 'CHECKED_REPLY=%s\\r\\n' \"$line\"".into()],
                    ..Default::default()
                },
                ..Default::default()
            },
        ).unwrap();
        let mut events = events_for(&terminal);
        let runtime = tokio::runtime::Runtime::new().unwrap();
        drive_until_rendered(
            &mut terminal,
            &mut events,
            &runtime,
            "CHECKED_READY",
        );
        terminal.try_enqueue_input(b"checked\r".to_vec()).unwrap();
        drive_until_rendered(
            &mut terminal,
            &mut events,
            &runtime,
            "CHECKED_REPLY=checked",
        );
        let deadline = std::time::Instant::now() + Duration::from_secs(5);
        while terminal.try_enqueue_input(Vec::new()).is_ok() {
            assert!(
                std::time::Instant::now() < deadline,
                "exited PTY input channel stayed open"
            );
            std::thread::sleep(Duration::from_millis(10));
        }
        assert!(terminal
            .try_enqueue_input(b"must not succeed".to_vec())
            .is_err());
    }

    #[test]
    fn native_terminal_queries_receive_configured_color_and_real_pixel_size() {
        let script = r#"
import fcntl, os, select, struct, termios, time, tty
tty.setraw(0)
rows, cols, width, height = struct.unpack('HHHH', fcntl.ioctl(0, termios.TIOCGWINSZ, bytes(8)))
color = b'\x1b]11;rgb:1212/3434/5656\x07'
size = f'\x1b[4;{height};{width}t'.encode()
os.write(1, b'\x1b]11;?\x07\x1b[14t')
reply = b''
deadline = time.monotonic() + 5
while time.monotonic() < deadline and (color not in reply or size not in reply):
    if select.select([0], [], [], 0.05)[0]:
        reply += os.read(0, 4096)
os.write(1, b'\r\nPROTOCOL_OK\r\n' if color in reply and size in reply else b'\r\nPROTOCOL_FAIL ' + repr(reply).encode())
time.sleep(30)
"#;
        let mut terminal = Terminal::new(
            930_002,
            Settings {
                backend: BackendSettings {
                    program: "/usr/bin/python3".into(),
                    args: vec!["-c".into(), script.into()],
                    ..Default::default()
                },
                theme: ThemeSettings::new(Box::new(ColorPalette {
                    background: "#123456".into(),
                    ..Default::default()
                })),
                ..Default::default()
            },
        )
        .unwrap();
        let mut events = events_for(&terminal);
        let runtime = tokio::runtime::Runtime::new().unwrap();
        drive_until_rendered(
            &mut terminal,
            &mut events,
            &runtime,
            "PROTOCOL_OK",
        );
    }

    #[test]
    fn native_synchronized_output_reaches_the_render_snapshot() {
        let mut terminal = Terminal::new(
            930_001,
            Settings {
                backend: BackendSettings {
                    program: "/bin/sh".into(),
                    args: vec![
                        "-c".into(),
                        "printf 'RENDER_READY\\r\\n'; read line; printf '\\033[?2026hRENDER_REPLY=%s\\033[?2026l' \"$line\"; exec sleep 30".into(),
                    ],
                    ..Default::default()
                },
                ..Default::default()
            },
        )
        .unwrap();
        let mut events = events_for(&terminal);
        let runtime = tokio::runtime::Runtime::new().unwrap();
        for marker in ["RENDER_READY", "RENDER_REPLY=marker"] {
            drive_until_rendered(&mut terminal, &mut events, &runtime, marker);
            if marker == "RENDER_READY" {
                terminal.handle(Command::ProxyToBackend(
                    backend::Command::Write(b"marker\r".to_vec()),
                ));
            }
        }
    }

    /// Many separate PTY reads, each of which would otherwise queue a Wakeup.
    /// The child writes `done` after its last output; waiting on that file never
    /// takes the parser lock a blocked event loop may hold.
    fn burst(id: u64, marker: &str, done: &std::path::Path) -> Terminal {
        Terminal::new(
            id,
            Settings {
                backend: BackendSettings {
                    program: "/bin/sh".into(),
                    args: vec![
                        "-c".into(),
                        format!(
                            "i=0; while [ $i -lt 150 ]; do printf 'line %s\\r\\n' $i; sleep 0.005; i=$((i+1)); done; printf '{marker}\\r\\n'; : > \"$1\"; sleep 30"
                        ),
                        // The path is an argument, so quotes or spaces in it are safe.
                        "sh".into(),
                        done.display().to_string(),
                    ],
                    ..Default::default()
                },
                ..Default::default()
            },
        )
        .unwrap()
    }

    #[test]
    fn stalled_redraws_coalesce_per_terminal_and_show_latest_output() {
        let fixtures = std::path::Path::new(env!("CARGO_MANIFEST_DIR"))
            .join("../../target/test-fixtures");
        std::fs::create_dir_all(&fixtures).unwrap();
        let markers = ["BURST_A_DONE", "BURST_B_DONE"];
        let done: Vec<_> = markers
            .iter()
            .map(|m| fixtures.join(format!("{m}-{}", std::process::id())))
            .collect();
        for path in &done {
            let _ = std::fs::remove_file(path);
        }
        let runtime = tokio::runtime::Runtime::new().unwrap();
        let mut terminals = [
            burst(930_101, markers[0], &done[0]),
            burst(930_102, markers[1], &done[1]),
        ];
        let mut streams: Vec<_> = terminals.iter().map(events_for).collect();
        // Nobody consumes events while both bursts run, like a slow renderer.
        let deadline = std::time::Instant::now() + Duration::from_secs(10);
        while !done.iter().all(|path| path.exists()) {
            if std::time::Instant::now() > deadline {
                // A blocked event loop could also block Terminal's drop.
                std::mem::forget(terminals);
                std::mem::forget(streams);
                panic!(
                    "bursts did not finish while redraw events were unconsumed"
                );
            }
            std::thread::sleep(Duration::from_millis(20));
        }
        for path in &done {
            let _ = std::fs::remove_file(path);
        }
        for ((terminal, events), marker) in
            terminals.iter_mut().zip(streams.iter_mut()).zip(markers)
        {
            let mut redraws = 0;
            while let Ok(Some(Event::BackendCall(id, command))) = runtime
                .block_on(async {
                    tokio::time::timeout(
                        Duration::from_millis(300),
                        events.next(),
                    )
                    .await
                })
            {
                assert_eq!(id, terminal.id, "events stay with their terminal");
                if matches!(
                    command,
                    backend::Command::ProcessAlacrittyEvent(
                        AlacrittyEvent::Wakeup
                            | AlacrittyEvent::MouseCursorDirty
                    )
                ) {
                    redraws += 1;
                }
                terminal.handle(Command::ProxyToBackend(command));
            }
            assert!(redraws <= 2, "{marker}: {redraws} queued redraws");
            assert!(rendered(terminal).contains(marker), "{marker} not drawn");
        }
    }
}
