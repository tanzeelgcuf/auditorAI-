"use client";

import { useState } from "react";
import Link from "next/link";
import { useSignup } from "../../../lib/hooks";
import { Button } from "../../../components/ui/button";
import { Input } from "../../../components/ui/input";
import { Card, CardHeader, CardTitle, CardDescription, CardContent } from "../../../components/ui/card";

export default function SignupPage() {
  const signup = useSignup();
  const [firmName, setFirmName] = useState("");
  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const [done, setDone] = useState(false);
  const [error, setError] = useState<string | null>(null);

  async function handleSubmit(e: React.FormEvent) {
    e.preventDefault();
    setError(null);
    try {
      await signup.mutateAsync({ firm_name: firmName, email, password });
      setDone(true);
    } catch (err) {
      setError(err instanceof Error ? err.message : "Signup failed");
    }
  }

  if (done) {
    return (
      <div className="flex min-h-screen items-center justify-center bg-slate-50 p-4">
        <Card className="w-full max-w-sm">
          <CardContent className="py-8 text-center">
            <p className="font-medium">Check your email</p>
            <p className="mt-1 text-sm text-slate-500">
              We sent a verification link to <span className="font-medium">{email}</span>. Click it
              to activate your account, then sign in.
            </p>
            <Link href="/login" className="mt-4 inline-block text-sm font-medium text-slate-900 underline">
              Back to sign in
            </Link>
          </CardContent>
        </Card>
      </div>
    );
  }

  return (
    <div className="flex min-h-screen items-center justify-center bg-slate-50 p-4">
      <Card className="w-full max-w-sm">
        <CardHeader>
          <CardTitle>Create your firm</CardTitle>
          <CardDescription>Set up AI Auditor for your accounting firm</CardDescription>
        </CardHeader>
        <CardContent>
          <form onSubmit={handleSubmit} className="space-y-4">
            <div className="space-y-1">
              <label className="text-sm font-medium">Firm name</label>
              <Input
                value={firmName}
                onChange={(e) => setFirmName(e.target.value)}
                placeholder="Smith & Associates CPA"
                required
              />
            </div>
            <div className="space-y-1">
              <label className="text-sm font-medium">Work email</label>
              <Input
                type="email"
                value={email}
                onChange={(e) => setEmail(e.target.value)}
                placeholder="you@firm.com"
                required
              />
            </div>
            <div className="space-y-1">
              <label className="text-sm font-medium">Password</label>
              <Input
                type="password"
                value={password}
                onChange={(e) => setPassword(e.target.value)}
                placeholder="••••••••"
                minLength={8}
                required
              />
            </div>
            {error && (
              <p className="rounded-md border border-red-200 bg-red-50 p-2 text-sm text-red-700">
                {error}
              </p>
            )}
            <Button type="submit" className="w-full" disabled={signup.isPending}>
              {signup.isPending ? "Creating…" : "Create firm"}
            </Button>
          </form>

          <p className="mt-4 text-center text-sm text-slate-500">
            Already have an account?{" "}
            <Link href="/login" className="font-medium text-slate-900 hover:underline">
              Sign in
            </Link>
          </p>
        </CardContent>
      </Card>
    </div>
  );
}
