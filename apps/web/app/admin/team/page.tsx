"use client";

import { Shell } from "../../../components/layout/shell";
import { Card, CardContent, CardHeader, CardTitle, CardDescription } from "../../../components/ui/card";
import { useBooks } from "../../../lib/hooks";
import { Users, Plus } from "lucide-react";
import { MotionDiv, StaggerContainer, MotionButton } from "../../../components/ui/motion";
import { SkeletonCard } from "../../../components/ui/skeleton";

export default function TeamPage() {
  const { data, isLoading } = useBooks();
  const books = data?.items ?? [];

  return (
    <Shell>
      <MotionDiv variant="slideUp" className="mb-6">
        <h1 className="text-2xl font-semibold text-foreground flex items-center gap-2">
          <Users className="h-6 w-6" aria-hidden="true" />
          Team & Assignments
        </h1>
        <p className="text-sm text-muted-foreground">
          Invite staff and assign them to client books. Staff only see assigned books (RLS enforced
          at the book level).
        </p>
      </MotionDiv>

      <MotionDiv variant="slideUp">
        <Card>
          <CardHeader>
            <CardTitle>Client books</CardTitle>
            <CardDescription>
              Invites are created per-book. Staff invitations are sent via email.
            </CardDescription>
          </CardHeader>
          <CardContent>
            {isLoading ? (
              <StaggerContainer staggerChildren={0.04}>
                <SkeletonCard title description contentLines={1} />
                <SkeletonCard title description contentLines={1} />
                <SkeletonCard title description contentLines={1} />
              </StaggerContainer>
            ) : books.length === 0 ? (
              <div className="py-8 text-center text-muted-foreground">
                <Users className="h-12 w-12 mx-auto mb-2 text-muted-foreground/50" aria-hidden="true" />
                No books yet. Create a client book from the dashboard.
              </div>
            ) : (
              <StaggerContainer staggerChildren={0.03} staggerDelay={0.05}>
                <div className="space-y-3">
                  {books.map((book) => (
                    <MotionDiv
                      key={book.id}
                      variant="slideUp"
                      className="flex items-center justify-between rounded-md border border-border p-3 hover:bg-muted/50 transition-colors"
                    >
                      <div>
                        <p className="font-medium text-sm text-foreground">{book.client_name}</p>
                        <p className="text-xs text-muted-foreground">Tolerance: {book.reconciliation_tolerance_cents}¢</p>
                      </div>
                      <MotionButton
                        variant="outline"
                        size="sm"
                        className="gap-2"
                        onClick={() => alert("Invite flow: enter staff email → sends signed invite link")}
                      >
                        <Plus className="h-4 w-4" aria-hidden="true" />
                        Invite staff
                      </MotionButton>
                    </MotionDiv>
                  ))}
                </div>
              </StaggerContainer>
            )}
          </CardContent>
        </Card>
      </MotionDiv>
    </Shell>
  );
}