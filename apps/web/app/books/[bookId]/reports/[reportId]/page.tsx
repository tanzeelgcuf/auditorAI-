"use client";

import { useState } from "react";
import { useParams } from "next/navigation";
import { Shell } from "../../../../../components/layout/shell";
import { Card, CardContent, CardHeader, CardTitle } from "../../../../../components/ui/card";
import { Badge } from "../../../../../components/ui/badge";
import { PdfViewer } from "../../../../../components/pdf-viewer/pdf-viewer";
import type { CitationTarget } from "../../../../../components/pdf-viewer/citation-overlay";
import { useReport, useFindings, useCitation } from "../../../../../lib/hooks";
import { formatDate, formatCents, severityStyles } from "../../../../../lib/utils";

export default function ReportViewerPage() {
  const params = useParams();
  const reportId = String(params.reportId);
  const bookId = String(params.bookId);

  const { data: report } = useReport(reportId);
  const { data: findings } = useFindings(bookId);
  const [activeFindingId, setActiveFindingId] = useState<string | null>(null);

  const { data: citationData } = useCitation(reportId, activeFindingId);

  const citation: CitationTarget | null = citationData
    ? {
        findingId: activeFindingId ?? "",
        pageNumber: citationData.page_number,
        bbox: citationData.bbox,
      }
    : null;

  const findingList = findings?.items ?? [];

  return (
    <Shell>
      <div className="mb-6">
        <h1 className="text-2xl font-semibold text-slate-900">Audit Report</h1>
        <p className="text-sm text-slate-500">
          {report ? (
            <>
              {formatDate(report.period_start)} → {formatDate(report.period_end)} · generated{" "}
              {formatDate(report.generated_at)}
            </>
          ) : (
            "Loading report…"
          )}
        </p>
      </div>

      <div className="grid gap-6 lg:grid-cols-2">
        {/* Findings list — left pane */}
        <div className="space-y-3">
          <h2 className="font-medium text-slate-700">Findings ({findingList.length})</h2>
          {findingList.length === 0 ? (
            <Card>
              <CardContent className="py-8 text-center text-slate-500">
                No findings for this report.
              </CardContent>
            </Card>
          ) : (
            findingList.map((f) => (
              <Card
                key={f.id}
                className={activeFindingId === f.id ? "ring-2 ring-amber-400" : ""}
              >
                <CardHeader className="pb-2">
                  <div className="flex items-center justify-between">
                    <CardTitle className="text-sm">
                      <span
                        className={`inline-block rounded-full border px-2 py-0.5 text-xs font-medium ${severityStyles[f.severity]}`}
                      >
                        {f.severity}
                      </span>{" "}
                      <span className="ml-1 font-mono text-xs text-slate-400">{f.rule_id}</span>
                    </CardTitle>
                    <Badge variant={f.exceeds_tolerance ? "danger" : "success"}>
                      {f.exceeds_tolerance ? "Mismatch" : "OK"}
                    </Badge>
                  </div>
                </CardHeader>
                <CardContent className="text-sm">
                  <p className="font-mono text-xs text-slate-600">{f.calculation_formula}</p>
                  <p className="mt-2 text-slate-500">
                    Variance: {formatCents(f.calculated_variance_cents)} · Tolerance:{" "}
                    {formatCents(f.tolerance_cents)}
                  </p>
                  <button
                    className="mt-2 text-xs font-medium text-slate-900 underline hover:text-slate-600"
                    onClick={() => setActiveFindingId(f.id)}
                  >
                    {activeFindingId === f.id ? "Hide citation" : "View source citation"}
                  </button>
                </CardContent>
              </Card>
            ))
          )}
        </div>

        {/* PDF viewer — right pane */}
        <div>
          <h2 className="mb-3 font-medium text-slate-700">Source Document</h2>
          <PdfViewer
            url={`${process.env.NEXT_PUBLIC_API_URL || "http://localhost:8080"}/v1/documents/${citationData?.source_document_id ?? "none"}/view`}
            citation={citation}
            onSelectCitation={(t) => setActiveFindingId(t.findingId)}
          />
          {!citation && (
            <p className="mt-2 text-xs text-slate-400">
              Select a finding to highlight its source region in the PDF.
            </p>
          )}
        </div>
      </div>
    </Shell>
  );
}
