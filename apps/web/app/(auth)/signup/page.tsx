"use client";

import { useState } from "react";
import Link from "next/link";
import { useSignup } from "../../../lib/hooks";
import { Input } from "../../../components/ui/input";
import { Card, CardHeader, CardTitle, CardDescription, CardContent } from "../../../components/ui/card";
import { Building, Mail, Lock, Loader2, Check, AlertCircle } from "lucide-react";
import { MotionDiv, StaggerContainer, MotionButton } from "../../../components/ui/motion";

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
      <div className="flex min-h-screen items-center justify-center bg-background p-4">
        <MotionDiv variant="slideUp" className="w-full max-w-sm">
          <Card>
            <CardContent className="py-8 text-center">
              <div className="mx-auto mb-4 flex h-12 w-12 items-center justify-center rounded-full bg-success-bg">
                <Check className="h-6 w-6 text-success" aria-hidden="true" />
              </div>
              <p className="font-medium text-foreground">Check your email</p>
              <p className="mt-1 text-sm text-muted-foreground">
                We sent a verification link to <span className="font-medium text-foreground">{email}</span>. Click it
                to activate your account, then sign in.
              </p>
              <Link href="/login" className="mt-4 inline-block text-sm font-medium text-primary hover:underline">
                Back to sign in
              </Link>
            </CardContent>
          </Card>
        </MotionDiv>
      </div>
    );
  }

  return (
    <div className="flex min-h-screen items-center justify-center bg-background p-4">
      <MotionDiv variant="slideUp" className="w-full max-w-sm">
        <Card>
          <CardHeader className="text-center">
            <div className="mx-auto mb-4 flex h-12 w-12 items-center justify-center rounded-full bg-primary/10">
              <Building className="h-6 w-6 text-primary" aria-hidden="true" />
            </div>
            <CardTitle className="text-2xl">Create your firm</CardTitle>
            <CardDescription>Set up AI Auditor for your accounting firm</CardDescription>
          </CardHeader>
          <CardContent>
            <StaggerContainer staggerChildren={0.04} staggerDelay={0.1}>
              <form onSubmit={handleSubmit} className="space-y-4">
                <MotionDiv variant="slideUp">
                  <div className="space-y-1">
                    <label className="text-sm font-medium text-foreground">Firm name</label>
                    <div className="relative">
                      <Building className="absolute left-3 top-1/2 -translate-y-1/2 h-4 w-4 text-muted-foreground" aria-hidden="true" />
                      <Input
                        value={firmName}
                        onChange={(e) => setFirmName(e.target.value)}
                        placeholder="Smith & Associates CPA"
                        required
                        className="pl-10"
                      />
                    </div>
                  </div>
                </MotionDiv>
                <MotionDiv variant="slideUp">
                  <div className="space-y-1">
                    <label className="text-sm font-medium text-foreground">Work email</label>
                    <div className="relative">
                      <Mail className="absolute left-3 top-1/2 -translate-y-1/2 h-4 w-4 text-muted-foreground" aria-hidden="true" />
                      <Input
                        type="email"
                        value={email}
                        onChange={(e) => setEmail(e.target.value)}
                        placeholder="you@firm.com"
                        required
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
                        value={password}
                        onChange={(e) => setPassword(e.target.value)}
                        placeholder="••••••••"
                        minLength={8}
                        required
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
                  <MotionButton type="submit" className="w-full gap-2" disabled={signup.isPending}>
                    {signup.isPending ? (
                      <>
                        <Loader2 className="h-4 w-4 animate-spin" aria-hidden="true" />
                        Creating…
                      </>
                    ) : (
                      "Create firm"
                    )}
                  </MotionButton>
                </MotionDiv>
              </form>

              <MotionDiv variant="slideUp" className="mt-6 text-center text-sm text-muted-foreground">
                Already have an account?{" "}
                <Link href="/login" className="font-medium text-primary hover:underline">
                  Sign in
                </Link>
              </MotionDiv>
            </StaggerContainer>
          </CardContent>
        </Card>
      </MotionDiv>
    </div>
  );
}