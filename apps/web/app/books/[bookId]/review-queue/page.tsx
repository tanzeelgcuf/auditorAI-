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

  if (isLoading) return <Shell><p>Loading…</p></Shell>;

  return (
    <Shell>
      <div className="mb-6 flex items-center justify-between">
        <div>
          <h1 className="text-2xl font-semibold text-slate-900">Review Queue</h1>
          <p className="text-sm text-slate-500">
            Confirm or reject low-confidence reconciliation links.
          </p>
        </div>
        <div className="flex gap-2">
          <Button variant="secondary" onClick={selectAllAbove95}>
            Select all &gt;95%
          </Button>
          <Button onClick={handleBulkConfirm} disabled={selected.size === 0}>
            Confirm {selected.size > 0 ? `${selected.size} selected` : ""}
          </Button>
        </div>
      </div>

      {items.length === 0 ? (
        <Card>
          <CardContent className="py-12 text-center text-slate-500">
            Nothing needs review. 🎉
          </CardContent>
        </Card>
      ) : (
        <Card>
          <CardContent className="p-0">
            <table className="w-full text-sm">
              <thead>
                <tr className="border-b text-left text-slate-500">
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
                {items.map((item: ReviewQueueItem) => (
                  <tr key={item.id} className="border-b last:border-0">
                    <td className="pl-4">
                      <input
                        type="checkbox"
                        checked={selected.has(item.id)}
                        onChange={() => toggle(item.id)}
                        aria-label="Select for bulk confirm"
                      />
                    </td>
                    <td className="py-3 font-mono text-xs">{shortId(item.invoice_entity_id)}</td>
                    <td className="py-3 font-mono text-xs">{shortId(item.bank_entity_id)}</td>
                    <td className="py-3 font-mono text-xs">{shortId(item.gl_entity_id)}</td>
                    <td className="py-3">{(item.link_confidence * 100).toFixed(1)}%</td>
                    <td className="py-3">{statusBadge(item.status)}</td>
                    <td className="py-3 text-slate-500">{formatDate(item.created_at)}</td>
                    <td className="py-3 pr-4">
                      <div className="flex gap-2">
                        <Button
                          size="sm"
                          disabled={confirmLink.isPending}
                          onClick={() => confirmLink.mutate({ linkId: item.id })}
                        >
                          Confirm
                        </Button>
                        <Button
                          size="sm"
                          variant="outline"
                          disabled={rejectLink.isPending}
                          onClick={() => rejectLink.mutate({ linkId: item.id })}
                        >
                          Reject
                        </Button>
                      </div>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </CardContent>
        </Card>
      )}
    </Shell>
  );
}

function shortId(id: string | null): string {
  if (!id) return "—";
  return id.slice(0, 8);
}
