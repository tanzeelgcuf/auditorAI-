"use client";

import { useState } from "react";
import { useRouter } from "next/navigation";
import Link from "next/link";
import { useLogin } from "../../../lib/hooks";
import { setTokens } from "../../../lib/api";
import { Input } from "../../../components/ui/input";
import { Card, CardHeader, CardTitle, CardDescription, CardContent } from "../../../components/ui/card";
import { Lock, Loader2, Mail, User, AlertCircle } from "lucide-react";
import { MotionDiv, StaggerContainer, MotionButton } from "../../../components/ui/motion";

export default function LoginPage() {
  const router = useRouter();
  const login = useLogin();
  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const [totpCode, setTotpCode] = useState("");
  const [error, setError] = useState<string | null>(null);

  async function handleSubmit(e: React.FormEvent) {
    e.preventDefault();
    setError(null);
    try {
      const result = await login.mutateAsync({
        email,
        password,
        totp_code: totpCode || undefined,
      });
      setTokens(result.access_token, result.refresh_token);
      router.replace("/dashboard");
    } catch (err) {
      setError(err instanceof Error ? err.message : "Login failed");
    }
  }

  return (
    <div className="flex min-h-screen items-center justify-center bg-background p-4">
      <MotionDiv variant="slideUp" className="w-full max-w-sm">
        <Card>
          <CardHeader className="text-center">
            <CardTitle className="text-2xl">Sign in</CardTitle>
            <CardDescription>AI Auditor — reconciliation platform</CardDescription>
          </CardHeader>
          <CardContent>
            <StaggerContainer staggerChildren={0.04} staggerDelay={0.1}>
              <form onSubmit={handleSubmit} className="space-y-4">
                <MotionDiv variant="slideUp">
                  <div className="space-y-1">
                    <label className="text-sm font-medium text-foreground">Email</label>
                    <div className="relative">
                      <Mail className="absolute left-3 top-1/2 -translate-y-1/2 h-4 w-4 text-muted-foreground" aria-hidden="true" />
                      <Input
                        type="email"
                        required
                        value={email}
                        onChange={(e) => setEmail(e.target.value)}
                        placeholder="you@firm.com"
                        className="pl-10"
                      />
                    </div>
                  </div>
                </MotionDiv>
                <MotionDiv variant="slideUp">
                  <div className="space-y-1">
                    <label className="text-sm font-medium text-foreground">Password</label>
                    <div className="relative">
                      <Lock className="absolute left-3 top-1/2 -translate-y-1/2 h-4 w-4 text-muted-foreground" aria-hidden="true" />
                      <Input
                        type="password"
                        required
                        value={password}
                        onChange={(e) => setPassword(e.target.value)}
                        placeholder="••••••••"
                        className="pl-10"
                      />
                    </div>
                  </div>
                </MotionDiv>
                <MotionDiv variant="slideUp">
                  <div className="space-y-1">
                    <label className="text-sm font-medium text-foreground">2FA code (optional)</label>
                    <div className="relative">
                      <User className="absolute left-3 top-1/2 -translate-y-1/2 h-4 w-4 text-muted-foreground" aria-hidden="true" />
                      <Input
                        value={totpCode}
                        onChange={(e) => setTotpCode(e.target.value)}
                        placeholder="000000"
                        inputMode="numeric"
                        className="pl-10"
                      />
                    </div>
                  </div>
                </MotionDiv>

                {error && (
                  <MotionDiv variant="slideDown" className="flex items-center gap-2 rounded-md border border-destructive/20 bg-destructive-bg p-3 text-sm text-destructive">
                    <AlertCircle className="h-4 w-4 flex-shrink-0" aria-hidden="true" />
                    {error}
                  </MotionDiv>
                )}

                <MotionDiv variant="slideUp">
                  <MotionButton type="submit" className="w-full gap-2" disabled={login.isPending}>
                    {login.isPending ? (
                      <>
                        <Loader2 className="h-4 w-4 animate-spin" aria-hidden="true" />
                        Signing in…
                      </>
                    ) : (
                      "Sign in"
                    )}
                  </MotionButton>
                </MotionDiv>
              </form>

              <MotionDiv variant="slideUp" className="mt-6 text-center text-sm text-muted-foreground">
                No account?{" "}
                <Link href="/signup" className="font-medium text-primary hover:underline">
                  Create your firm
                </Link>
              </MotionDiv>
            </StaggerContainer>
          </CardContent>
        </Card>
      </MotionDiv>
    </div>
  );
}