"use client";

import { Shell } from "../../../components/layout/shell";
import { Card, CardContent, CardHeader, CardTitle, CardDescription } from "../../../components/ui/card";
import { useBooks } from "../../../lib/hooks";

export default function TeamPage() {
  const { data } = useBooks();
  const books = data?.items ?? [];

  return (
    <Shell>
      <h1 className="mb-6 text-2xl font-semibold text-slate-900">Team & Assignments</h1>
      <p className="mb-6 text-sm text-slate-500">
        Invite staff and assign them to client books. Staff only see assigned books (RLS enforced
        at the book level).
      </p>

      <Card>
        <CardHeader>
          <CardTitle>Client books</CardTitle>
          <CardDescription>
            Invites are created per-book. Staff invitations are sent via email.
          </CardDescription>
        </CardHeader>
        <CardContent>
          <div className="space-y-3">
            {books.length === 0 && <p className="text-sm text-slate-500">No books yet.</p>}
            {books.map((book) => (
              <div
                key={book.id}
                className="flex items-center justify-between rounded-md border border-slate-200 p-3"
              >
                <div>
                  <p className="font-medium text-sm">{book.client_name}</p>
                  <p className="text-xs text-slate-500">Tolerance: {book.reconciliation_tolerance_cents}¢</p>
                </div>
                <button
                  className="rounded-md border border-slate-300 px-3 py-1.5 text-xs font-medium text-slate-700 hover:bg-slate-50"
                  onClick={() => alert("Invite flow: enter staff email → sends signed invite link")}
                >
                  + Invite staff
                </button>
              </div>
            ))}
          </div>
        </CardContent>
      </Card>
    </Shell>
  );
}
