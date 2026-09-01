"use client";

import { useState } from "react";
import Link from "next/link";
import { useRouter } from "next/navigation";
import { Card, CardContent, CardHeader, CardTitle, CardDescription } from "../../components/ui/card";
import { Button } from "../../components/ui/button";
import { Input } from "../../components/ui/input";
import { useCreateBook } from "../../lib/hooks";
import { completeOnboarding } from "../../lib/onboarding";
import { cn } from "../../lib/utils";
import { Check, Loader2, Upload, FileText, ArrowRight } from "lucide-react";
import { MotionDiv, MotionButton } from "../../components/ui/motion";

const STEPS = ["Create a client book", "Upload documents", "Review findings"];

export default function OnboardingPage() {
  const router = useRouter();
  const createBook = useCreateBook();
  const [step, setStep] = useState(0);
  const [clientName, setClientName] = useState("");
  const [bookId, setBookId] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);

  async function handleCreateBook(e: React.FormEvent) {
    e.preventDefault();
    setError(null);
    try {
      const book = await createBook.mutateAsync({ client_name: clientName });
      setBookId(book.id);
      setStep(1);
    } catch (err) {
      setError(err instanceof Error ? err.message : "Failed to create book");
    }
  }

  function finish() {
    completeOnboarding();
    router.push("/dashboard");
  }

  return (
    <div className="min-h-screen bg-background flex items-center justify-center p-6">
      <MotionDiv variant="slideUp" className="w-full max-w-md">
        <div className="text-center mb-8">
          <h1 className="text-3xl font-bold text-foreground">Welcome to AI Auditor</h1>
          <p className="text-muted-foreground mt-2">Set up your first reconciliation in three steps</p>
        </div>

        {/* Step indicator */}
        <MotionDiv variant="slideUp" className="mb-8">
          <div className="flex items-center justify-center gap-2">
            {STEPS.map((label, i) => (
              <MotionDiv key={label} variant="scaleIn" style={{ transitionDelay: `${i * 50}ms` }}>
                <div className="flex items-center gap-2">
                  <div
                    className={cn(
                      "flex h-7 w-7 items-center justify-center rounded-full text-xs font-semibold",
                      i < step
                        ? "bg-success text-success-foreground"
                        : i === step
                          ? "bg-primary text-primary-foreground"
                          : "bg-muted text-muted-foreground",
                    )}
                  >
                    {i < step ? <Check className="h-4 w-4" aria-hidden="true" /> : i + 1}
                  </div>
                  <span className={cn("text-sm", i === step ? "font-medium text-foreground" : "text-muted-foreground")}>
                    {label}
                  </span>
                </div>
              </MotionDiv>
            ))}
          </div>
        </MotionDiv>

        {step === 0 && (
          <MotionDiv variant="slideUp">
            <Card>
              <CardHeader>
                <CardTitle>Create your first client book</CardTitle>
                <CardDescription>
                  A client book is where invoices, bank statements, and GL exports are reconciled together.
                </CardDescription>
              </CardHeader>
              <CardContent>
                <form onSubmit={handleCreateBook} className="space-y-4">
                  <Input
                    value={clientName}
                    onChange={(e) => setClientName(e.target.value)}
                    placeholder="Client company name"
                    required
                    autoFocus
                  />
                  {error && <p className="text-sm text-destructive">{error}</p>}
                  <MotionButton type="submit" className="w-full gap-2" disabled={createBook.isPending}>
                    {createBook.isPending ? (
                      <>
                        <Loader2 className="h-4 w-4 animate-spin" aria-hidden="true" />
                        Creating…
                      </>
                    ) : (
                      <>
                        Create book
                        <ArrowRight className="h-4 w-4" aria-hidden="true" />
                      </>
                    )}
                  </MotionButton>
                </form>
              </CardContent>
            </Card>
          </MotionDiv>
        )}

        {step === 1 && bookId && (
          <MotionDiv variant="slideUp">
            <Card>
              <CardHeader>
                <CardTitle>Upload documents</CardTitle>
                <CardDescription>
                  Upload invoices, bank statements (PDF/CSV/OFX), and GL exports to start reconciliation.
                </CardDescription>
              </CardHeader>
              <CardContent className="space-y-3">
                <Link href={`/books/${bookId}/upload`} className="block">
                  <MotionButton className="w-full gap-2">
                    <Upload className="h-4 w-4" aria-hidden="true" />
                    Upload documents
                  </MotionButton>
                </Link>
                <Link href={`/books/${bookId}/upload`} className="block">
                  <Button variant="secondary" className="w-full gap-2">
                    <FileText className="h-4 w-4" aria-hidden="true" />
                    Try it with sample data
                  </Button>
                </Link>
                <p className="text-xs text-muted-foreground">
                  Tip: upload at least one invoice + one bank statement to see cross-linking in action.
                </p>
                <MotionButton variant="ghost" className="w-full" onClick={() => setStep(2)}>
                  Skip for now
                  <ArrowRight className="h-4 w-4" aria-hidden="true" />
                </MotionButton>
              </CardContent>
            </Card>
          </MotionDiv>
        )}

        {step === 2 && (
          <MotionDiv variant="slideUp">
            <Card>
              <CardHeader>
                <CardTitle>Review findings</CardTitle>
                <CardDescription>
                  Once documents are processed, low-confidence links land in the review queue for your
                  confirmation. High-severity mismatches surface as findings with full citations back to
                  the source documents.
                </CardDescription>
              </CardHeader>
              <CardContent className="space-y-3">
                {bookId && (
                  <Link href={`/books/${bookId}/review-queue`} className="block">
                    <Button variant="secondary" className="w-full gap-2">
                      <FileText className="h-4 w-4" aria-hidden="true" />
                      See the review queue
                    </Button>
                  </Link>
                )}
                <MotionButton className="w-full gap-2" onClick={finish}>
                  Get started
                  <ArrowRight className="h-4 w-4" aria-hidden="true" />
                </MotionButton>
              </CardContent>
            </Card>
          </MotionDiv>
        )}
      </MotionDiv>
    </div>
  );
}