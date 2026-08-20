// services/verification/src/telemetry.rs
// GlitchTip (Sentry-compatible) error reporting for the verification service.
//
// Mirrors the API (Go sentry-go), agent-runtime (Python sentry-sdk), and
// ingestion (Rust sentry) pattern: GLITCHTIP_DSN unset => strict no-op.
// The service needs no Sentry server to run or test.
//
// NOTE on the panic path: sentry::init is called WITHOUT the default PanicIntegration
// (default_integrations(false)); instead install_panic_hook() chains our own
// capture on top of the default stderr hook, so local crash output is unchanged
// and we control exactly when capture + flush happen before the process dies.

use std::panic::{self, PanicHookInfo};
use std::sync::Once;

/// Initialize the Sentry/GlitchTip client from `GLITCHTIP_DSN`.
/// Strict no-op when the env var is empty/unset.
pub fn init_glitchtip() {
    static INIT: Once = Once::new();
    INIT.call_once(|| {
        let dsn = match std::env::var("GLITCHTIP_DSN") {
            Ok(dsn) if !dsn.trim().is_empty() => dsn,
            _ => return, // no DSN => no-op
        };
        let release = std::env::var("GIT_SHA").unwrap_or_else(|_| "dev".to_string());
        let options = sentry::ClientOptions::new()
            .dsn(&dsn)
            .release(release)
            .traces_sample_rate(0.0) // errors only; tracing stays in OTel
            .default_integrations(false); // panic handling wired manually below
        // Guard flushes queued events on drop. Forgetting keeps the client alive
        // for the process lifetime (flush then happens via install_panic_hook's
        // explicit client.flush before the process dies on panic).
        std::mem::forget(sentry::init(options));
    });
}

/// Install a panic hook that captures panics to GlitchTip BEFORE the process
/// dies, then still runs the default hook so local logs are unchanged.
/// Capturing is itself a no-op when the client was never initialized.
pub fn install_panic_hook() {
    static HOOK: Once = Once::new();
    HOOK.call_once(|| {
        let default_hook = panic::take_hook(); // chain: ours runs first, then default
        panic::set_hook(Box::new(move |info| {
            capture_panic(info);
            default_hook(info);
            // Flush so the error event reaches GlitchTip before the process dies.
            if let Some(client) = sentry::Hub::current().client() {
                client.flush(None);
            }
        }));
    });
}

/// Send a panic to GlitchTip, extracting payload + location for a useful event.
fn capture_panic(info: &PanicHookInfo) {
    let msg = if let Some(s) = info.payload().downcast_ref::<&str>() {
        (*s).to_string()
    } else if let Some(s) = info.payload().downcast_ref::<String>() {
        s.clone()
    } else {
        "panic (non-string payload)".to_string()
    };
    let location = info
        .location()
        .map(|l| format!("{}:{}", l.file(), l.line()))
        .unwrap_or_else(|| "unknown".to_string());
    sentry::capture_message(&format!("{} — at {}", msg, location), sentry::Level::Fatal);
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::atomic::{AtomicBool, Ordering};
    use std::sync::{Arc, Mutex};

    #[test]
    fn init_glitchtip_noops_when_dsn_unset() {
        // Ensure the relevant env var is absent in the test process.
        std::env::remove_var("GLITCHTIP_DSN");
        // Must not panic and must not require a Sentry server.
        init_glitchtip();
    }

    /// A recording transport that proves a panic event reaches the sentry
    /// client (i.e. the GlitchTip-bound path) without network access.
    #[derive(Default)]
    struct TestTransport {
        envelopes: Mutex<Vec<sentry::Envelope>>,
    }

    impl sentry::Transport for TestTransport {
        fn send_envelope(&self, envelope: sentry::Envelope) {
            if let Ok(mut guard) = self.envelopes.lock() {
                guard.push(envelope);
            }
        }
    }

    #[test]
    fn panic_hook_captures_panic_and_does_not_swallow() {
        std::env::remove_var("GLITCHTIP_DSN");
        // Client must NOT be initialized (DSN unset) so this test binds its own
        // transport; the panic path must still be safe when the client is live.
        let options = sentry::ClientOptions::new()
            .dsn("https://public@glitchtip.invalid/1")
            .traces_sample_rate(0.0)
            .default_integrations(false); // must not install its own panic hook
        let transport = Arc::new(TestTransport::default());
        let collect = transport.clone();
        sentry::Hub::current().bind_client(Some(Arc::new(
            options.transport(transport).into(),
        )));

        let hook_fired = Arc::new(AtomicBool::new(false));
        let hook_count = hook_fired.clone();

        // Preserve the current hook (default, or the one main() wired if this
        // test ran after it) and chain through it so stderr output is untouched.
        let prev: Box<dyn Fn(&PanicHookInfo) + Send + Sync> = panic::take_hook();
        let prev = Arc::new(prev);
        let chained = prev.clone();
        panic::set_hook(Box::new(move |info| {
            hook_count.store(true, Ordering::SeqCst);
            capture_panic(info); // GlitchTip capture path
            if let Some(client) = sentry::Hub::current().client() {
                client.flush(None);
            }
            chained(info); // default-style stderr print
        }));

        // Panic in a spawned thread: the hook must run there and must NOT
        // swallow the panic — join returns Err only if the thread panicked.
        let joined = std::thread::spawn(|| {
            panic!("deliberate test panic for telemetry hook");
        })
        .join();
        assert!(joined.is_err(), "hook must not swallow the panic");
        assert!(
            hook_fired.load(Ordering::SeqCst),
            "installed hook must have been invoked"
        );

        // The panic must have reached the sentry client bound to our transport.
        let events = collect
            .envelopes
            .lock()
            .expect("test transport lock poisoned");
        let captured = events.iter().filter(|env| env.event().is_some()).count();
        assert_eq!(
            captured, 1,
            "panic event must be captured by the sentry client"
        );
        drop(events);

        // Restore the previous hook so later tests keep a sane default.
        let restored = prev; // prev no longer borrowed; reinstall it
        panic::set_hook(Box::new(move |info| restored(info)));
    }
}