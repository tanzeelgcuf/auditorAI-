# KNOWN_GAPS.md — deferred by decision, not forgotten

Every entry here is something deliberately NOT built, NOT promoted, or NOT
verified yet, WITH the trigger that would change the decision. The failure
mode this file exists to prevent is the silent middle: things that are
neither done nor acknowledged. If it is not here, it is either done or
unknown to us — nothing is parked in between.

1. **Sign-conflict detection is Python-only, not a Rust finding.** The
   detector (`link.py` pass 2) downgrades a group whose magnitudes
   reconcile but whose sign pattern contradicts the book's convention. It
   stays out of `services/verification` because it is new, unproven against
   real books, and has no measured incidence rate — while verification is
   the tier this product markets as authoritative. Every sign-conflict is
   already caught in Python and routed to review, so promoting it buys
   completeness, not safety. **Re-evaluate for v1.1** once a second book
   (different sign convention) and pilot data give a real false-positive
   rate. Decided 2026-09-17; codified as CLAUDE.md rule 18 and in the
   `link.py` set-site comment.

2. **source_ip retention is "forever".** The three audit tables
   (`access_log`, `config_change_log`, `period_reopen_log`) carry a
   nullable client IP beside `user_id` with no retention policy. An IP is
   GDPR personal data. **Trigger:** the pre-pilot compliance pass (SOC2
   roadmap item 10 also notes no endpoint reads `access_log` at all).

3. **TOTP has no recovery codes.** A user who loses their authenticator is
   locked out until an operator clears `totp_secret` by hand. **Trigger:**
   v1.1, or the first pilot customer who hits it — whichever comes first.

4. **2FA is not mandatory for firm_admin.** Enrollment is a policy gate that
   is not enforced anywhere yet. **Trigger:** the SOC2 CC6.x pass or pilot
   onboarding, whichever forces the decision.

5. **mypy --strict is advisory in CI.** 338 errors (measured 2026-09-16,
   output reviewed): annotation debt, not behavioral — the 88-test suite is
   green. Ratcheted with `continue-on-error` in ci.yml and a comment stating
   the exit condition. **Trigger:** pay down file-by-file as code is touched;
   remove the ratchet at zero.

6. **Pass-1 auto-link makes a money comparison outside Rust.**
   `link.py::_amounts_match` at confidence 1.0 is a comparison, not a
   conversion, but it is still a money decision in Python — the documented
   live tension with CLAUDE.md rule 1. **Trigger:** the gl_sign_convention
   work should supersede it; do not leave both conventions standing.

7. **seed-demo writes rows but no object bytes.** Demo findings' source
   documents cannot resolve in the UI (`GET /v1/documents/{id}/view` →
   object not found) — a documented, deliberate choice over writing stub
   files whose contents would contradict the findings. **Trigger:** the
   real end-to-end path (real uploads through the API) makes this moot for
   anything except throwaway demos.
