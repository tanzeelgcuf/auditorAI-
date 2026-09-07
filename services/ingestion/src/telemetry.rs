// services/ingestion/src/telemetry.rs
// GlitchTip (Sentry-compatible) error reporting for the ingestion pipeline.
//
// Mirrors the API (Go sentry-go) and agent-runtime (Python sentry-sdk) pattern:
// GLITCHTIP_DSN unset => strict no-op. The service needs no Sentry server to run.

use std::panic::{self, PanicHookInfo};
use std::sync::Once;

/// Initialize the Sentry/GlitchTip client from `GLITCHTIP_DSN`.
/// Strict no-op when the env var is empty/unset.
pub fn init_glitchtip() {
    // Read the DSN BEFORE the Once so a later call with the DSN set still
    // initializes (a no-op on the first call must not wedge the guard forever).
    let dsn = match std::env::var("GLITCHTIP_DSN") {
        Ok(dsn) if !dsn.trim().is_empty() => dsn,
        _ => return, // no DSN => no-op
    };
    static INIT: Once = Once::new();
    INIT.call_once(move || {
        let release = std::env::var("GIT_SHA").unwrap_or_else(|_| "dev".to_string());
        let mut options = sentry::ClientOptions::default();
        options.release = Some(release.into());
        // Disable sentry's own panic integration: our manual panic hook does the
        // capture, so default_integrations(false) avoids a double-capture per panic.
        options.default_integrations = false;
        let guard = sentry::init((dsn, options));
        // Keep the client alive for the process lifetime; flush-on-panic happens
        // explicitly in the panic hook (buffered events must not race process death).
        std::mem::forget(guard);
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
    // Flush the client so the buffered panic event reaches GlitchTip before the
    // process dies. The mem::forget'd guard never drops, so no other flush runs.
    if let Some(client) = sentry::Hub::current().client() {
        client.flush(None);
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::atomic::{AtomicBool, Ordering};
    use std::sync::Arc;

    #[test]
    fn init_glitchtip_noops_when_dsn_unset() {
        // Ensure the relevant env var is absent in the test process.
        std::env::remove_var("GLITCHTIP_DSN");
        // Must not panic and must not require a Sentry server.
        init_glitchtip();
    }

    #[test]
    fn panic_hook_fires_in_spawned_thread_without_swallowing() {
        std::env::remove_var("GLITCHTIP_DSN");
        init_glitchtip(); // no-op client; capture path must still be safe

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

        // Restore the previous hook so later tests keep a sane default.
        let restored = prev; // prev no longer borrowed; reinstall it
        panic::set_hook(Box::new(move |info| restored(info)));
    }
}