"use client";

import { useState } from "react";
import { useParams } from "next/navigation";
import { Shell } from "../../../../components/layout/shell";
import { Card, CardContent } from "../../../../components/ui/card";
import { Button } from "../../../../components/ui/button";
import { Badge } from "../../../../components/ui/badge";
import {
  useReviewQueue,
  useConfirmLink,
  useRejectLink,
  useBulkConfirm,
  ReviewQueueItem,
} from "../../../../lib/hooks";
import { formatDate } from "../../../../lib/utils";
import { Check, X, CheckCircle, Loader2 } from "lucide-react";
import { MotionDiv, StaggerContainer } from "../../../../components/ui/motion";
import { SkeletonTable } from "../../../../components/ui/skeleton";

function statusBadge(status: string) {
  const map: Record<string, { label: string; variant: "warning" | "info" | "default" }> = {
    needs_review: { label: "Needs Review", variant: "warning" },
    auto_linked: { label: "Auto-linked", variant: "info" },
    confirmed: { label: "Confirmed", variant: "default" },
    rejected: { label: "Rejected", variant: "default" },
  };
  const m = map[status] ?? { label: status, variant: "default" as const };
  return <Badge variant={m.variant}>{m.label}</Badge>;
}

function shortId(id: string | null): string {
  if (!id) return "—";
  return id.slice(0, 8);
}

export default function ReviewQueuePage() {
  const params = useParams();
  const bookId = String(params.bookId);
  const { data, isLoading } = useReviewQueue(bookId);
  const confirmLink = useConfirmLink();
  const rejectLink = useRejectLink();
  const bulkConfirm = useBulkConfirm(bookId);

  const [selected, setSelected] = useState<Set<string>>(new Set());
  const items = data?.items ?? [];

  function toggle(id: string) {
    setSelected((prev) => {
      const next = new Set(prev);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });
  }

  function selectAllAbove95() {
    setSelected(new Set(items.filter((i) => i.link_confidence > 0.95).map((i) => i.id)));
  }

  function handleBulkConfirm() {
    if (selected.size === 0) return;
    bulkConfirm.mutate({ linkIds: Array.from(selected) });
    setSelected(new Set());
  }

  const selectAllLabel = "Select all >95%";

  return (
    <Shell>
      <MotionDiv variant="slideUp" className="mb-6 flex items-center justify-between">
        <div>
          <h1 className="text-2xl font-semibold text-foreground">Review Queue</h1>
          <p className="text-sm text-muted-foreground">
            Confirm or reject low-confidence reconciliation links.
          </p>
        </div>
        <div className="flex gap-2">
          <Button variant="secondary" onClick={selectAllAbove95} className="gap-2">
            <CheckCircle className="h-4 w-4" aria-hidden="true" />
            {selectAllLabel}
          </Button>
          <Button onClick={handleBulkConfirm} disabled={selected.size === 0} className="gap-2">
            <Check className="h-4 w-4" aria-hidden="true" />
            Confirm {selected.size > 0 ? `${selected.size} selected` : ""}
          </Button>
        </div>
      </MotionDiv>

      {isLoading ? (
        <Card>
          <CardContent className="p-4">
            <SkeletonTable columns={8} rows={5} />
          </CardContent>
        </Card>
      ) : items.length === 0 ? (
        <MotionDiv variant="scaleIn">
          <Card>
            <CardContent className="py-12 text-center text-muted-foreground">
              Nothing needs review.
              <CheckCircle className="h-6 w-6 mx-auto mt-2 text-success" aria-hidden="true" />
            </CardContent>
          </Card>
        </MotionDiv>
      ) : (
        <MotionDiv variant="slideUp">
          <Card>
            <CardContent className="p-0">
              <div className="overflow-x-auto">
                <table className="w-full text-sm" role="table">
                  <thead>
                    <tr className="border-b text-left text-muted-foreground">
                      <th className="w-10 py-3 pl-4"></th>
                      <th className="py-3">Invoice</th>
                      <th className="py-3">Bank</th>
                      <th className="py-3">GL</th>
                      <th className="py-3">Confidence</th>
                      <th className="py-3">Status</th>
                      <th className="py-3">Created</th>
                      <th className="py-3 pr-4">Actions</th>
                    </tr>
                  </thead>
                  <tbody>
                    <StaggerContainer staggerChildren={0.03} staggerDelay={0.05}>
                      {items.map((item: ReviewQueueItem) => (
                        <tr
                          key={item.id}
                          className="border-b last:border-0 transition-colors hover:bg-muted/50"
                        >
                          <td className="pl-4">
                            <input
                              type="checkbox"
                              checked={selected.has(item.id)}
                              onChange={() => toggle(item.id)}
                              aria-label="Select for bulk confirm"
                              className="w-4 h-4 rounded border-input text-primary focus:ring-primary/20"
                            />
                          </td>
                          <td className="py-3 font-mono text-xs text-muted-foreground">{shortId(item.invoice_entity_id)}</td>
                          <td className="py-3 font-mono text-xs text-muted-foreground">{shortId(item.bank_entity_id)}</td>
                          <td className="py-3 font-mono text-xs text-muted-foreground">{shortId(item.gl_entity_id)}</td>
                          <td className="py-3 font-medium">
                            <span className={item.link_confidence > 0.95 ? "text-success" : item.link_confidence > 0.7 ? "text-warning" : "text-destructive"}>
                              {(item.link_confidence * 100).toFixed(1)}%
                            </span>
                          </td>
                          <td className="py-3">{statusBadge(item.status)}</td>
                          <td className="py-3 text-muted-foreground">{formatDate(item.created_at)}</td>
                          <td className="py-3 pr-4">
                            <div className="flex gap-2">
                              <Button
                                size="sm"
                                disabled={confirmLink.isPending}
                                onClick={() => confirmLink.mutate({ linkId: item.id })}
                                className="gap-1"
                              >
                                {confirmLink.isPending ? (
                                  <Loader2 className="h-4 w-4 animate-spin" aria-hidden="true" />
                                ) : (
                                  <Check className="h-4 w-4" aria-hidden="true" />
                                )}
                                Confirm
                              </Button>
                              <Button
                                size="sm"
                                variant="outline"
                                disabled={rejectLink.isPending}
                                onClick={() => rejectLink.mutate({ linkId: item.id })}
                                className="gap-1"
                              >
                                {rejectLink.isPending ? (
                                  <Loader2 className="h-4 w-4 animate-spin" aria-hidden="true" />
                                ) : (
                                  <X className="h-4 w-4" aria-hidden="true" />
                                )}
                                Reject
                              </Button>
                            </div>
                          </td>
                        </tr>
                      ))}
                    </StaggerContainer>
                  </tbody>
                </table>
              </div>
            </CardContent>
          </Card>
        </MotionDiv>
      )}
    </Shell>
  );
}