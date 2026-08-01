"use client";

import Link from "next/link";
import { useRouter } from "next/navigation";
import { useState } from "react";
import { Shell } from "../../components/layout/shell";
import { Card, CardContent, CardHeader, CardTitle } from "../../components/ui/card";
import { Button } from "../../components/ui/button";
import { Input } from "../../components/ui/input";
import { useBooks, useCreateBook } from "../../lib/hooks";
import { isOnboardingComplete } from "../../lib/onboarding";

export default function DashboardPage() {
  const router = useRouter();
  const { data, isLoading } = useBooks();
  const createBook = useCreateBook();
  const [showNewBook, setShowNewBook] = useState(false);
  const [clientName, setClientName] = useState("");
  const [error, setError] = useState<string | null>(null);

  const books = data?.items ?? [];

  // First-login gate (doc 07 §9): route new firms with no client books through
  // the onboarding wizard before showing the empty dashboard.
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
          <h1 className="text-2xl font-semibold text-slate-900">Client Books</h1>
          <p className="text-sm text-slate-500">
            Select a book to upload documents, review links, and generate reports.
          </p>
        </div>
        <Button onClick={() => setShowNewBook((v) => !v)}>
          {showNewBook ? "Cancel" : "New client book"}
        </Button>
      </div>

      {showNewBook && (
        <form onSubmit={handleCreate} className="mb-6 max-w-md space-y-3">
          <Input
            value={clientName}
            onChange={(e) => setClientName(e.target.value)}
            placeholder="Client company name"
            required
          />
          {error && <p className="text-sm text-red-600">{error}</p>}
          <Button type="submit" disabled={createBook.isPending}>
            {createBook.isPending ? "Creating…" : "Create book"}
          </Button>
        </form>
      )}

      {isLoading ? (
        <p className="text-slate-500">Loading…</p>
      ) : books.length === 0 ? (
        <Card>
          <CardContent className="py-12 text-center">
            <p className="text-slate-500">
              No client books yet. Create your first one to begin.
            </p>
          </CardContent>
        </Card>
      ) : (
        <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-3">
          {books.map((book) => (
            <Link key={book.id} href={`/books/${book.id}/documents`}>
              <Card className="transition-shadow hover:shadow-md">
                <CardHeader>
                  <CardTitle className="text-base">{book.client_name}</CardTitle>
                </CardHeader>
                <CardContent>
                  <div className="space-y-1 text-sm text-slate-500">
                    <p>Currency: {book.base_currency}</p>
                    <p>Tolerance: {book.reconciliation_tolerance_cents}¢</p>
                    <p>Mode: {book.tolerance_mode}</p>
                  </div>
                </CardContent>
              </Card>
            </Link>
          ))}
        </div>
      )}
    </Shell>
  );
}
