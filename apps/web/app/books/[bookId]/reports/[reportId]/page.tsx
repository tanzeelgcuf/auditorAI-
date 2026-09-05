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
import { FileText, Eye, EyeOff, AlertTriangle } from "lucide-react";
import { MotionDiv, StaggerContainer } from "../../../../../components/ui/motion";
import { SkeletonCard } from "../../../../../components/ui/skeleton";

export default function ReportViewerPage() {
  const params = useParams();
  const reportId = String(params.reportId);
  const bookId = String(params.bookId);

  const { data: report, isLoading: reportLoading } = useReport(reportId);
  const { data: findings, isLoading: findingsLoading } = useFindings(bookId);
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
  const isLoading = reportLoading || findingsLoading;

  return (
    <Shell>
      <MotionDiv variant="slideUp" className="mb-6">
        <h1 className="text-2xl font-semibold text-foreground">Audit Report</h1>
        <p className="text-sm text-muted-foreground">
          {report ? (
            <>
              {formatDate(report.period_start)} → {formatDate(report.period_end)} · generated{" "}
              {formatDate(report.generated_at)}
            </>
          ) : (
            "Loading report…"
          )}
        </p>
      </MotionDiv>

      {isLoading ? (
        <div className="grid gap-6 lg:grid-cols-2">
          <StaggerContainer staggerChildren={0.04} staggerDelay={0.1}>
            <SkeletonCard showTitle showDescription contentLines={2} />
            <SkeletonCard showTitle showDescription contentLines={2} />
            <SkeletonCard showTitle showDescription contentLines={2} />
          </StaggerContainer>
          <SkeletonCard showTitle showDescription contentLines={8} />
        </div>
      ) : (
        <div className="grid gap-6 lg:grid-cols-2">
          {/* Findings list — left pane */}
          <MotionDiv variant="slideUp">
            <div className="space-y-3">
              <h2 className="font-medium text-foreground">Findings ({findingList.length})</h2>
              {findingList.length === 0 ? (
                <Card>
                  <CardContent className="py-8 text-center text-muted-foreground">
                    <FileText className="h-12 w-12 mx-auto mb-2 text-muted-foreground/50" aria-hidden="true" />
                    No findings for this report.
                  </CardContent>
                </Card>
              ) : (
                <StaggerContainer staggerChildren={0.03} staggerDelay={0.05}>
                  {findingList.map((f) => (
                    <Card
                      key={f.id}
                      className={`transition-all ${activeFindingId === f.id ? "ring-2 ring-primary" : ""}`}
                    >
                      <CardHeader className="pb-2">
                        <div className="flex items-center justify-between">
                          <CardTitle className="text-sm flex items-center gap-2">
                            <span
                              className={`inline-flex items-center rounded-full border px-2 py-0.5 text-xs font-medium ${severityStyles[f.severity]}`}
                            >
                              {f.severity === "high" && <AlertTriangle className="h-3 w-3 mr-1" aria-hidden="true" />}
                              {f.severity}
                            </span>
                            <span className="ml-1 font-mono text-xs text-muted-foreground">{f.rule_id}</span>
                          </CardTitle>
                          <Badge variant={f.exceeds_tolerance ? "destructive" : "success"}>
                            {f.exceeds_tolerance ? "Mismatch" : "OK"}
                          </Badge>
                        </div>
                      </CardHeader>
                      <CardContent className="text-sm">
                        <p className="font-mono text-xs text-muted-foreground">{f.calculation_formula}</p>
                        <p className="mt-2 text-muted-foreground">
                          Variance: {formatCents(f.calculated_variance_cents)} · Tolerance:{" "}
                          {formatCents(f.tolerance_cents)}
                        </p>
                        <button
                          className="mt-2 flex items-center gap-1 text-xs font-medium text-primary hover:text-primary/80"
                          onClick={() => setActiveFindingId(activeFindingId === f.id ? null : f.id)}
                        >
                          {activeFindingId === f.id ? (
                            <>
                              <EyeOff className="h-3 w-3" aria-hidden="true" />
                              Hide citation
                            </>
                          ) : (
                            <>
                              <Eye className="h-3 w-3" aria-hidden="true" />
                              View source citation
                            </>
                          )}
                        </button>
                      </CardContent>
                    </Card>
                  ))}
                </StaggerContainer>
              )}
            </div>
          </MotionDiv>

          {/* PDF viewer — right pane */}
          <MotionDiv variant="slideUp" style={{ transitionDelay: "100ms" }}>
            <div className="sticky top-6">
              <h2 className="mb-3 font-medium text-foreground flex items-center gap-2">
                <FileText className="h-5 w-5" aria-hidden="true" />
                Source Document
              </h2>
              <PdfViewer
                url={`${process.env.NEXT_PUBLIC_API_URL || "http://localhost:8080"}/v1/documents/${citationData?.source_document_id ?? "none"}/view`}
                citation={citation}
                onSelectCitation={(t) => setActiveFindingId(t.findingId)}
              />
              {!citation && (
                <p className="mt-2 text-xs text-muted-foreground">
                  Select a finding to highlight its source region in the PDF.
                </p>
              )}
            </div>
          </MotionDiv>
        </div>
      )}
    </Shell>
  );
}