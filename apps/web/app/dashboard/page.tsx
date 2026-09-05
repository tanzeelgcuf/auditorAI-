"use client";

import { useRouter } from "next/navigation";
import { useState } from "react";
import { Shell } from "../../components/layout/shell";
import { Card, CardContent, CardHeader, CardTitle } from "../../components/ui/card";
import { Input } from "../../components/ui/input";
import { useBooks, useCreateBook } from "../../lib/hooks";
import { isOnboardingComplete } from "../../lib/onboarding";
import { Plus, Loader2 } from "lucide-react";
import { MotionDiv, StaggerContainer, MotionButton, MotionLink } from "../../components/ui/motion";

export default function DashboardPage() {
  const router = useRouter();
  const { data, isLoading } = useBooks();
  const createBook = useCreateBook();
  const [showNewBook, setShowNewBook] = useState(false);
  const [clientName, setClientName] = useState("");
  const [error, setError] = useState<string | null>(null);

  const books = data?.items ?? [];

  if (!isLoading && books.length === 0 && !isOnboardingComplete()) {
    router.replace("/onboarding");
    return null;
  }

  async function handleCreate(e: React.FormEvent) {
    e.preventDefault();
    setError(null);
    try {
      await createBook.mutateAsync({ client_name: clientName });
      setClientName("");
      setShowNewBook(false);
    } catch (err) {
      setError(err instanceof Error ? err.message : "Failed to create book");
    }
  }

  return (
    <Shell>
      <div className="mb-6 flex items-center justify-between">
        <div>
          <h1 className="text-2xl font-semibold text-foreground">Client Books</h1>
          <p className="text-sm text-muted-foreground">
            Select a book to upload documents, review links, and generate reports.
          </p>
        </div>
        <MotionButton
          motionVariant="scaleIn"
          onClick={() => setShowNewBook((v) => !v)}
          className="gap-2"
        >
          <Plus className="h-4 w-4" aria-hidden="true" />
          {showNewBook ? "Cancel" : "New client book"}
        </MotionButton>
      </div>

      {showNewBook && (
        <MotionDiv variant="slideDown" className="mb-6 max-w-md space-y-3">
          <form onSubmit={handleCreate} className="space-y-3">
            <Input
              value={clientName}
              onChange={(e) => setClientName(e.target.value)}
              placeholder="Client company name"
              required
              autoFocus
            />
            {error && <p className="text-sm text-destructive">{error}</p>}
            <MotionButton type="submit" disabled={createBook.isPending} className="w-full gap-2">
              {createBook.isPending ? (
                <>
                  <Loader2 className="h-4 w-4 animate-spin" aria-hidden="true" />
                  Creating…
                </>
              ) : (
                <>
                  <Plus className="h-4 w-4" aria-hidden="true" />
                  Create book
                </>
              )}
            </MotionButton>
          </form>
        </MotionDiv>
      )}

      {isLoading ? (
        <div className="flex items-center justify-center py-12">
          <Loader2 className="h-8 w-8 animate-spin text-primary" aria-hidden="true" />
          <span className="sr-only">Loading client books…</span>
        </div>
      ) : books.length === 0 ? (
        <Card>
          <CardContent className="py-12 text-center">
            <p className="text-muted-foreground">
              No client books yet. Create your first one to begin.
            </p>
          </CardContent>
        </Card>
      ) : (
        <StaggerContainer staggerChildren={0.06} staggerDelay={0.1}>
          <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-3">
            {books.map((book) => (
              <MotionLink key={book.id} href={`/books/${book.id}/documents`} variant="scaleIn">
                <Card className="transition-shadow hover:shadow-lg">
                  <CardHeader>
                    <CardTitle className="text-base">{book.client_name}</CardTitle>
                  </CardHeader>
                  <CardContent>
                    <div className="space-y-1 text-sm text-muted-foreground">
                      <p>Currency: {book.base_currency}</p>
                      <p>Tolerance: {book.reconciliation_tolerance_cents}¢</p>
                      <p>Mode: {book.tolerance_mode}</p>
                    </div>
                  </CardContent>
                </Card>
              </MotionLink>
            ))}
          </div>
        </StaggerContainer>
      )}
    </Shell>
  );
}