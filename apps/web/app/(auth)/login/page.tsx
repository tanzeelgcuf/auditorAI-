"use client";

import { useState } from "react";
import { useRouter } from "next/navigation";
import Link from "next/link";
import { useLogin } from "../../../lib/hooks";
import { setTokens } from "../../../lib/api";
import { Button } from "../../../components/ui/button";
import { Input } from "../../../components/ui/input";
import { Card, CardHeader, CardTitle, CardDescription, CardContent } from "../../../components/ui/card";

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
    <div className="flex min-h-screen items-center justify-center bg-slate-50 p-4">
      <Card className="w-full max-w-sm">
        <CardHeader>
          <CardTitle>Sign in</CardTitle>
          <CardDescription>AI Auditor — reconciliation platform</CardDescription>
        </CardHeader>
        <CardContent>
          <form onSubmit={handleSubmit} className="space-y-4">
            <div className="space-y-1">
              <label className="text-sm font-medium">Email</label>
              <Input
                type="email"
                required
                value={email}
                onChange={(e) => setEmail(e.target.value)}
                placeholder="you@firm.com"
              />
            </div>
            <div className="space-y-1">
              <label className="text-sm font-medium">Password</label>
              <Input
                type="password"
                required
                value={password}
                onChange={(e) => setPassword(e.target.value)}
                placeholder="••••••••"
              />
            </div>
            <div className="space-y-1">
              <label className="text-sm font-medium">2FA code (optional)</label>
              <Input
                value={totpCode}
                onChange={(e) => setTotpCode(e.target.value)}
                placeholder="000000"
                inputMode="numeric"
              />
            </div>

            {error && (
              <p className="rounded-md border border-red-200 bg-red-50 p-2 text-sm text-red-700">
                {error}
              </p>
            )}

            <Button type="submit" className="w-full" disabled={login.isPending}>
              {login.isPending ? "Signing in…" : "Sign in"}
            </Button>
          </form>

          <p className="mt-4 text-center text-sm text-slate-500">
            No account?{" "}
            <Link href="/signup" className="font-medium text-slate-900 hover:underline">
              Create your firm
            </Link>
          </p>
        </CardContent>
      </Card>
    </div>
  );
}
