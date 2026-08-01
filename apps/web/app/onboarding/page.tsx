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
    <div className="space-y-6">
      {/* Step indicator */}
      <div className="flex items-center justify-center gap-2">
        {STEPS.map((label, i) => (
          <div key={label} className="flex items-center gap-2">
            <div
              className={cn(
                "flex h-7 w-7 items-center justify-center rounded-full text-xs font-semibold",
                i < step
                  ? "bg-green-600 text-white"
                  : i === step
                    ? "bg-slate-900 text-white"
                    : "bg-slate-200 text-slate-500",
              )}
            >
              {i < step ? "✓" : i + 1}
            </div>
            <span className={cn("text-sm", i === step ? "font-medium text-slate-900" : "text-slate-500")}>
              {label}
            </span>
          </div>
        ))}
      </div>

      {step === 0 && (
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
              />
              {error && <p className="text-sm text-red-600">{error}</p>}
              <Button type="submit" className="w-full" disabled={createBook.isPending}>
                {createBook.isPending ? "Creating…" : "Create book"}
              </Button>
            </form>
          </CardContent>
        </Card>
      )}

      {step === 1 && bookId && (
        <Card>
          <CardHeader>
            <CardTitle>Upload documents</CardTitle>
            <CardDescription>
              Upload invoices, bank statements (PDF/CSV/OFX), and GL exports to start reconciliation.
            </CardDescription>
          </CardHeader>
          <CardContent className="space-y-3">
            <Link href={`/books/${bookId}/upload`} className="block">
              <Button className="w-full">Upload documents</Button>
            </Link>
            {/* ponytail: real sample-data endpoint deferred — for now the seed-demo
                CLI (services/api/cmd/seed-demo) produces a demo book to explore. */}
            <Link href={`/books/${bookId}/upload`} className="block">
              <Button variant="secondary" className="w-full">
                Try it with sample data
              </Button>
            </Link>
            <p className="text-xs text-slate-400">
              Tip: upload at least one invoice + one bank statement to see cross-linking in action.
            </p>
            <Button variant="ghost" className="w-full" onClick={() => setStep(2)}>
              Skip for now
            </Button>
          </CardContent>
        </Card>
      )}

      {step === 2 && (
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
                <Button variant="secondary" className="w-full">
                  See the review queue
                </Button>
              </Link>
            )}
            <Button className="w-full" onClick={finish}>
              Get started
            </Button>
          </CardContent>
        </Card>
      )}
    </div>
  );
}
