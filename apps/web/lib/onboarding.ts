// First-login onboarding helpers (doc 07 §9).
// v1 uses localStorage; the backend users.has_completed_onboarding flag is
// deferred. ponytail: switch to the API flag once accounts have multi-device need.

const KEY = "ai_auditor_onboarding_complete";

export function isOnboardingComplete(): boolean {
  if (typeof window === "undefined") return true;
  return localStorage.getItem(KEY) === "true";
}

export function completeOnboarding(): void {
  if (typeof window === "undefined") return;
  localStorage.setItem(KEY, "true");
}

export function clearOnboarding(): void {
  if (typeof window === "undefined") return;
  localStorage.removeItem(KEY);
}
