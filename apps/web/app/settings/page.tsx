"use client";

import { useState } from "react";
import { Shell } from "../../components/layout/shell";
import { Card, CardContent, CardHeader, CardTitle, CardDescription } from "../../components/ui/card";
import { Input } from "../../components/ui/input";
import { useTOTPStatus, useEnableTOTP, useVerifyTOTP, useGenerateRecoveryCodes } from "../../lib/hooks";
import { ShieldCheck, ShieldAlert, Loader2, Check, Copy, KeyRound } from "lucide-react";
import { MotionDiv, MotionButton } from "../../components/ui/motion";
import { QRCodeSVG } from "qrcode.react";

export default function SettingsPage() {
  const { data: status, isLoading: statusLoading } = useTOTPStatus();
  const enable = useEnableTOTP();
  const verify = useVerifyTOTP();
  const generateRecovery = useGenerateRecoveryCodes();

  const [code, setCode] = useState("");
  const [recoveryCodes, setRecoveryCodes] = useState<string[] | null>(null);
  const [copied, setCopied] = useState(false);

  async function handleVerify(e: React.FormEvent) {
    e.preventDefault();
    if (!code.trim()) return;
    await verify.mutateAsync({ code: code.trim() });
    // The factor is live. Generate the recovery codes NOW, in the same
    // success state, and show them ONCE — the plaintext is never retrievable
    // afterward, so this render is the user's only chance to save them.
    const res = await generateRecovery.mutateAsync();
    setRecoveryCodes(res.codes);
    setCode("");
  }

  async function handleRegenerate() {
    const res = await generateRecovery.mutateAsync();
    setRecoveryCodes(res.codes);
  }

  async function copyCodes() {
    if (!recoveryCodes) return;
    await navigator.clipboard.writeText(recoveryCodes.join("\n"));
    setCopied(true);
    setTimeout(() => setCopied(false), 2000);
  }

  return (
    <Shell>
      <MotionDiv variant="slideUp" className="mb-6">
        <h1 className="text-2xl font-semibold text-foreground flex items-center gap-2">
          <KeyRound className="h-6 w-6" aria-hidden="true" />
          Account Settings
        </h1>
      </MotionDiv>

      <div className="max-w-lg space-y-6">
        <MotionDiv variant="slideUp">
          <Card>
            <CardHeader>
              <CardTitle className="text-sm flex items-center gap-2">
                {status?.enabled ? (
                  <>
                    <ShieldCheck className="h-4 w-4 text-green-600" aria-hidden="true" />
                    Two-factor authentication is enabled
                  </>
                ) : (
                  <>
                    <ShieldAlert className="h-4 w-4 text-amber-600" aria-hidden="true" />
                    Two-factor authentication is not enabled
                  </>
                )}
              </CardTitle>
              <CardDescription>
                {status?.enabled
                  ? "Your account requires a code from your authenticator app, or a recovery code, at login."
                  : "Set up an authenticator app to require a second factor at login."}
              </CardDescription>
            </CardHeader>
            <CardContent>
              {statusLoading ? (
                <div className="flex items-center gap-2 text-sm text-muted-foreground">
                  <Loader2 className="h-4 w-4 animate-spin" aria-hidden="true" />
                  Checking two-factor status…
                </div>
              ) : !status?.enabled ? (
                !enable.data ? (
                  <div className="space-y-3">
                    <MotionButton
                      variant="default"
                      onClick={() => enable.mutate()}
                      disabled={enable.isPending}
                    >
                      {enable.isPending ? <Loader2 className="h-4 w-4 animate-spin" aria-hidden="true" /> : null}
                      Set up 2FA
                    </MotionButton>
                    {enable.isError && (
                      <p className="text-sm text-destructive" role="alert">{(enable.error as Error).message}</p>
                    )}
                  </div>
                ) : (
                  <form onSubmit={handleVerify} className="space-y-4">
                    <div className="flex flex-col items-start gap-3">
                      <div className="rounded-md border border-input bg-white p-2">
                        <QRCodeSVG value={enable.data.qr_code} size={140} aria-label="Authenticator QR code" role="img" />
                      </div>
                      <p className="text-xs text-muted-foreground">
                        Scan with your authenticator app, or enter this secret manually:
                      </p>
                      <code className="text-xs font-mono bg-muted px-2 py-1 rounded break-all select-all">
                        {enable.data.secret}
                      </code>
                    </div>
                    <div className="space-y-1">
                      <label htmlFor="totp-code" className="text-sm font-medium">
                        Enter the 6-digit code
                      </label>
                      <Input
                        id="totp-code"
                        value={code}
                        onChange={(e) => setCode(e.target.value)}
                        inputMode="numeric"
                        autoComplete="one-time-code"
                        placeholder="123456"
                      />
                      {verify.isError && (
                        <p className="text-sm text-destructive" role="alert">{(verify.error as Error).message}</p>
                      )}
                    </div>
                    <MotionButton
                      variant="default"
                      type="submit"
                      disabled={verify.isPending || code.trim().length === 0}
                    >
                      {verify.isPending ? <Loader2 className="h-4 w-4 animate-spin" aria-hidden="true" /> : null}
                      Verify and enable
                    </MotionButton>
                  </form>
                )
              ) : recoveryCodes ? (
                <div className="space-y-3">
                  <p
                    className="text-sm font-medium text-amber-900 bg-amber-100 border border-amber-300 rounded px-3 py-2"
                    role="alert"
                  >
                    Save these recovery codes now. They are shown once and cannot
                    be retrieved again. Each works once, in place of your
                    authenticator code.
                  </p>
                  <ul className="grid grid-cols-2 gap-1 font-mono text-sm">
                    {recoveryCodes.map((c) => (
                      <li key={c}>{c}</li>
                    ))}
                  </ul>
                  <MotionButton variant="secondary" onClick={copyCodes}>
                    {copied ? <Check className="h-4 w-4" aria-hidden="true" /> : <Copy className="h-4 w-4" aria-hidden="true" />}
                    {copied ? "Copied" : "Copy codes"}
                  </MotionButton>
                </div>
              ) : (
                <div className="space-y-3">
                  <MotionButton
                    variant="secondary"
                    onClick={handleRegenerate}
                    disabled={generateRecovery.isPending}
                  >
                    {generateRecovery.isPending ? <Loader2 className="h-4 w-4 animate-spin" aria-hidden="true" /> : null}
                    Regenerate recovery codes
                  </MotionButton>
                  <p className="text-xs text-muted-foreground">
                    Regenerating invalidates all existing recovery codes
                    immediately. The new batch is shown once.
                  </p>
                  {generateRecovery.isError && (
                    <p className="text-sm text-destructive" role="alert">{(generateRecovery.error as Error).message}</p>
                  )}
                </div>
              )}
            </CardContent>
          </Card>
        </MotionDiv>
      </div>
    </Shell>
  );
}
