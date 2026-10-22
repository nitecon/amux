//! Local desktop notifications. A delivery attempt is never task receipt, claim
//! or execution start. A worker owns the queue and a single attempt thread calls
//! the OS, so a missing, slow or failing notification service cannot block the
//! caller, and each attempt has a deadline.
use std::{
    sync::mpsc::{self, Receiver, RecvTimeoutError, SyncSender, TryRecvError, TrySendError},
    time::Duration,
};

/// Pending notifications beyond this are dropped, never queued without bound.
pub const QUEUE: usize = 16;
pub const MAX_TITLE: usize = 120;
pub const MAX_BODY: usize = 1000;
/// How long one OS notification call may take before its outcome is unknown.
pub const ATTEMPT_TIMEOUT: Duration = Duration::from_secs(5);

/// Shows one notification. Errors are reported, never retried.
pub trait Backend: Send + 'static {
    fn show(&mut self, title: &str, body: &str) -> Result<(), String>;
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Outcome {
    /// The OS notification service accepted the request. It does not prove the
    /// user saw it.
    Shown {
        id: u64,
    },
    Failed {
        id: u64,
        reason: String,
    },
    /// No answer within the deadline; the notification may or may not appear.
    TimedOut {
        id: u64,
    },
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Dropped {
    EmptyTitle,
    QueueFull,
    WorkerStopped,
}

struct Job {
    id: u64,
    title: String,
    body: String,
}

pub struct Notifier {
    jobs: SyncSender<Job>,
    outcomes: Receiver<Outcome>,
    next: u64,
}

impl Notifier {
    pub fn new(backend: impl Backend) -> Self {
        Self::with_timeout(backend, ATTEMPT_TIMEOUT)
    }

    /// If the worker thread cannot start, every `notify` reports `WorkerStopped`;
    /// notification setup never fails the caller.
    pub fn with_timeout(backend: impl Backend, timeout: Duration) -> Self {
        let (jobs, queue) = mpsc::sync_channel::<Job>(QUEUE);
        let (report, outcomes) = mpsc::sync_channel(QUEUE);
        let _ = std::thread::Builder::new()
            .name("amux-notifications".into())
            .spawn(move || deliver(backend, queue, report, timeout));
        Self {
            jobs,
            outcomes,
            next: 1,
        }
    }

    /// The platform's desktop notification service.
    #[cfg(feature = "desktop")]
    pub fn native() -> Self {
        #[cfg(not(windows))]
        let native = Native;
        #[cfg(windows)]
        let native = Native::default();
        Self::new(native)
    }

    /// Queues one attempt and returns its ID without waiting for delivery.
    pub fn notify(&mut self, title: &str, body: &str) -> Result<u64, Dropped> {
        let title = clean(title, MAX_TITLE, false);
        if title.is_empty() {
            return Err(Dropped::EmptyTitle);
        }
        let id = self.next;
        let job = Job {
            id,
            title,
            body: clean(body, MAX_BODY, true),
        };
        match self.jobs.try_send(job) {
            Ok(()) => {
                self.next += 1;
                Ok(id)
            }
            Err(TrySendError::Full(_)) => Err(Dropped::QueueFull),
            Err(TrySendError::Disconnected(_)) => Err(Dropped::WorkerStopped),
        }
    }

    /// Outcomes finished so far, without blocking.
    pub fn outcomes(&self) -> Vec<Outcome> {
        self.outcomes.try_iter().collect()
    }
}

/// Runs on the worker thread. Only one OS call is ever outstanding: while an
/// attempt is past its deadline, later attempts fail at once instead of piling
/// up threads.
fn deliver(
    backend: impl Backend,
    queue: Receiver<Job>,
    report: SyncSender<Outcome>,
    timeout: Duration,
) {
    let (attempts, inbox) = mpsc::channel::<Job>();
    let (done, results) = mpsc::channel::<Outcome>();
    let attempt = std::thread::Builder::new()
        .name("amux-notification-attempt".into())
        .spawn(move || {
            let mut backend = backend;
            for job in inbox {
                let shown = std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| {
                    backend.show(&job.title, &job.body)
                }));
                let outcome = match shown {
                    Ok(Ok(())) => Outcome::Shown { id: job.id },
                    Ok(Err(reason)) => Outcome::Failed { id: job.id, reason },
                    Err(_) => Outcome::Failed {
                        id: job.id,
                        reason: "notification backend panicked".into(),
                    },
                };
                if done.send(outcome).is_err() {
                    break;
                }
            }
        });
    let mut overdue = false;
    for job in queue {
        let id = job.id;
        let failed = |reason: &str| Outcome::Failed {
            id,
            reason: reason.into(),
        };
        // The late result of an overdue attempt was already reported as TimedOut.
        if overdue && !matches!(results.try_recv(), Err(TryRecvError::Empty)) {
            overdue = false;
        }
        let outcome = if attempt.is_err() {
            failed("could not start the notification thread")
        } else if overdue {
            failed(
                "an earlier notification has not finished; the notification service is unresponsive",
            )
        } else if attempts.send(job).is_err() {
            failed("the notification thread stopped")
        } else {
            match results.recv_timeout(timeout) {
                Ok(outcome) => outcome,
                Err(RecvTimeoutError::Timeout) => {
                    overdue = true;
                    Outcome::TimedOut { id }
                }
                Err(RecvTimeoutError::Disconnected) => failed("the notification thread stopped"),
            }
        };
        // Unread outcomes are dropped rather than stalling delivery.
        let _ = report.try_send(outcome);
    }
}

/// Drops control characters (keeping body line breaks) and truncates to `max`
/// characters, marking the cut.
fn clean(text: &str, max: usize, lines: bool) -> String {
    let text: String = text
        .chars()
        .map(|c| match c {
            '\n' if lines => '\n',
            '\n' | '\t' => ' ',
            c => c,
        })
        .filter(|c| *c == '\n' || !c.is_control())
        .collect();
    let text = text.trim();
    if text.chars().count() <= max {
        return text.to_owned();
    }
    let mut cut: String = text.chars().take(max - 1).collect();
    cut.push('…');
    cut
}

/// Copies whole characters into a NUL-terminated UTF-16 buffer that starts
/// zeroed, never splitting a surrogate pair.
#[cfg_attr(not(all(feature = "desktop", windows)), allow(dead_code))]
fn utf16_into(text: &str, into: &mut [u16]) {
    let room = into.len().saturating_sub(1);
    let mut used = 0;
    for c in text.chars() {
        let mut units = [0; 2];
        let units = c.encode_utf16(&mut units);
        if used + units.len() > room {
            break;
        }
        into[used..used + units.len()].copy_from_slice(units);
        used += units.len();
    }
}

/// Notification services that support body markup would otherwise interpret
/// these characters; line breaks pass through unchanged.
#[cfg_attr(not(all(feature = "desktop", not(windows))), allow(dead_code))]
fn escape_markup(text: &str) -> String {
    text.replace('&', "&amp;")
        .replace('<', "&lt;")
        .replace('>', "&gt;")
}

/// Linux: the freedesktop D-Bus notification service.
#[cfg(all(feature = "desktop", not(windows)))]
struct Native;

#[cfg(all(feature = "desktop", not(windows)))]
impl Backend for Native {
    fn show(&mut self, title: &str, body: &str) -> Result<(), String> {
        notify_rust::Notification::new()
            .appname("amux")
            .summary(title)
            .body(&escape_markup(body))
            .show()
            .map(|_| ())
            .map_err(|e| e.to_string())
    }
}

/// Windows: a notification-area balloon from a hidden message-only window,
/// shown as the amux executable. Needs no AppUserModelID, Start menu entry or
/// installer, but adds a tray icon while the notifier is alive. The window
/// lives on the attempt thread, which is the only thread that calls `show`.
#[cfg(all(feature = "desktop", windows))]
#[derive(Default)]
struct Native {
    window: isize,
}

#[cfg(all(feature = "desktop", windows))]
impl Backend for Native {
    fn show(&mut self, title: &str, body: &str) -> Result<(), String> {
        use windows_sys::Win32::UI::{
            Shell::{
                NIF_ICON, NIF_INFO, NIF_TIP, NIIF_INFO, NIM_ADD, NIM_MODIFY, NOTIFYICONDATAW,
                Shell_NotifyIconW,
            },
            WindowsAndMessaging::{CreateWindowExW, HWND_MESSAGE, IDI_APPLICATION, LoadIconW},
        };
        // SAFETY: plain Win32 calls with owned, zero-initialized buffers. The
        // window and icon belong to this thread and are released in Drop.
        unsafe {
            let adding = self.window == 0;
            if adding {
                let class: Vec<u16> = "STATIC\0".encode_utf16().collect();
                let window = CreateWindowExW(
                    0,
                    class.as_ptr(),
                    std::ptr::null(),
                    0,
                    0,
                    0,
                    0,
                    0,
                    HWND_MESSAGE,
                    std::ptr::null_mut(),
                    std::ptr::null_mut(),
                    std::ptr::null(),
                );
                if window.is_null() {
                    return Err(format!(
                        "could not create notification window: {}",
                        std::io::Error::last_os_error()
                    ));
                }
                self.window = window as isize;
            }
            let mut data: NOTIFYICONDATAW = std::mem::zeroed();
            data.cbSize = std::mem::size_of::<NOTIFYICONDATAW>() as u32;
            data.hWnd = self.window as _;
            data.uID = 1;
            data.uFlags = NIF_ICON | NIF_TIP | NIF_INFO;
            data.hIcon = LoadIconW(std::ptr::null_mut(), IDI_APPLICATION);
            data.dwInfoFlags = NIIF_INFO;
            utf16_into("amux", &mut data.szTip);
            utf16_into(title, &mut data.szInfoTitle);
            utf16_into(body, &mut data.szInfo);
            if Shell_NotifyIconW(if adding { NIM_ADD } else { NIM_MODIFY }, &data) == 0 {
                // Start the next attempt from a fresh window and icon.
                self.release();
                return Err("the Windows notification area refused the notification".into());
            }
        }
        Ok(())
    }
}

#[cfg(all(feature = "desktop", windows))]
impl Native {
    /// Removes the icon and window created by `show` on this thread, if any.
    fn release(&mut self) {
        use windows_sys::Win32::UI::{
            Shell::{NIM_DELETE, NOTIFYICONDATAW, Shell_NotifyIconW},
            WindowsAndMessaging::DestroyWindow,
        };
        if self.window == 0 {
            return;
        }
        // SAFETY: the window was created by this thread; deleting an icon that
        // was never added is a harmless failure.
        unsafe {
            let mut data: NOTIFYICONDATAW = std::mem::zeroed();
            data.cbSize = std::mem::size_of::<NOTIFYICONDATAW>() as u32;
            data.hWnd = self.window as _;
            data.uID = 1;
            Shell_NotifyIconW(NIM_DELETE, &data);
            DestroyWindow(self.window as _);
        }
        self.window = 0;
    }
}

#[cfg(all(feature = "desktop", windows))]
impl Drop for Native {
    fn drop(&mut self) {
        self.release();
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn utf16_truncation_keeps_whole_characters() {
        // "a😀" is 3 UTF-16 units; a 3-unit buffer holds 2 plus the NUL.
        let mut buffer = [0u16; 3];
        utf16_into("a😀", &mut buffer);
        assert_eq!(buffer, [u16::from(b'a'), 0, 0], "no half surrogate pair");
        let mut buffer = [0u16; 4];
        utf16_into("a😀b", &mut buffer);
        assert_eq!(String::from_utf16(&buffer[..3]).unwrap(), "a😀");
        assert_eq!(buffer[3], 0);
        let mut buffer = [7u16; 1];
        utf16_into("text", &mut buffer);
        assert_eq!(buffer, [7], "no room beyond the NUL");
    }

    #[test]
    fn markup_characters_are_escaped_and_line_breaks_kept() {
        assert_eq!(
            escape_markup("a <b> & c\n</b>"),
            "a &lt;b&gt; &amp; c\n&lt;/b&gt;"
        );
    }
}
