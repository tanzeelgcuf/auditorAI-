"use client";

import Link from "next/link";
import { useParams } from "next/navigation";
import { Shell } from "../../../../components/layout/shell";
import { Card, CardContent } from "../../../../components/ui/card";
import { Badge } from "../../../../components/ui/badge";
import { Button } from "../../../../components/ui/button";
import { useDocuments } from "../../../../lib/hooks";
import { formatDate } from "../../../../lib/utils";
import { Upload, Eye } from "lucide-react";
import { MotionDiv, StaggerContainer } from "../../../../components/ui/motion";
import { SkeletonTable } from "../../../../components/ui/skeleton";

export default function DocumentsPage() {
  const params = useParams();
  const bookId = String(params.bookId);
  const { data, isLoading } = useDocuments(bookId);
  const docs = data?.items ?? [];

  return (
    <Shell>
      <div className="mb-6 flex items-center justify-between">
        <div>
          <h1 className="text-2xl font-semibold text-foreground">Documents</h1>
          <p className="text-sm text-muted-foreground">
            Invoices, bank statements, and GL exports for this client book.
          </p>
        </div>
        <Link href={`/books/${bookId}/upload`}>
          <Button className="gap-2">
            <Upload className="h-4 w-4" aria-hidden="true" />
            Upload
          </Button>
        </Link>
      </div>

      {isLoading ? (
        <Card>
          <CardContent className="p-4">
            <SkeletonTable columns={6} rows={5} />
          </CardContent>
        </Card>
      ) : docs.length === 0 ? (
        <Card>
          <CardContent className="py-12 text-center text-muted-foreground">
            No documents yet. Upload the first batch.
          </CardContent>
        </Card>
      ) : (
        <MotionDiv variant="slideUp">
          <Card>
            <CardContent className="p-0">
              <div className="overflow-x-auto">
                <table className="w-full text-sm" role="table">
                  <thead>
                    <tr className="border-b text-left text-muted-foreground">
                      <th className="py-3 pl-4">Filename</th>
                      <th className="py-3">Type</th>
                      <th className="py-3">Status</th>
                      <th className="py-3">Pages</th>
                      <th className="py-3">Uploaded</th>
                      <th className="py-3 pr-4">View</th>
                    </tr>
                  </thead>
                  <tbody>
                    <StaggerContainer staggerChildren={0.04} staggerDelay={0.05}>
                      {docs.map((doc) => (
                        <tr
                          key={doc.id}
                          className="border-b last:border-0 transition-colors hover:bg-muted/50"
                        >
                          <td className="py-3 pl-4 font-medium">{doc.filename}</td>
                          <td className="py-3">
                            <Badge variant="info">{doc.doc_type}</Badge>
                          </td>
                          <td className="py-3">
                            <Badge
                              variant={
                                doc.ocr_status === "done"
                                  ? "success"
                                  : doc.ocr_status === "failed"
                                    ? "destructive"
                                    : "warning"
                              }
                            >
                              {doc.ocr_status}
                            </Badge>
                          </td>
                          <td className="py-3 text-muted-foreground">{doc.page_count ?? "—"}</td>
                          <td className="py-3 text-muted-foreground">{formatDate(doc.uploaded_at)}</td>
                          <td className="py-3 pr-4">
                            <a
                              href={`${process.env.NEXT_PUBLIC_API_URL || "http://localhost:8080"}/v1/documents/${doc.id}/view`}
                              target="_blank"
                              rel="noreferrer"
                              className="flex items-center gap-1 text-primary hover:text-primary/80 font-medium"
                            >
                              <Eye className="h-4 w-4" aria-hidden="true" />
                              View
                            </a>
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