"use client";

import Link from "next/link";
import { useParams } from "next/navigation";
import { Shell } from "../../../../components/layout/shell";
import { Card, CardContent } from "../../../../components/ui/card";
import { Badge } from "../../../../components/ui/badge";
import { Button } from "../../../../components/ui/button";
import { useDocuments } from "../../../../lib/hooks";
import { formatDate } from "../../../../lib/utils";

export default function DocumentsPage() {
  const params = useParams();
  const bookId = String(params.bookId);
  const { data, isLoading } = useDocuments(bookId);
  const docs = data?.items ?? [];

  return (
    <Shell>
      <div className="mb-6 flex items-center justify-between">
        <div>
          <h1 className="text-2xl font-semibold text-slate-900">Documents</h1>
          <p className="text-sm text-slate-500">
            Invoices, bank statements, and GL exports for this client book.
          </p>
        </div>
        <Link href={`/books/${bookId}/upload`}>
          <Button>Upload</Button>
        </Link>
      </div>

      {isLoading ? (
        <p className="text-slate-500">Loading…</p>
      ) : docs.length === 0 ? (
        <Card>
          <CardContent className="py-12 text-center text-slate-500">
            No documents yet. Upload the first batch.
          </CardContent>
        </Card>
      ) : (
        <Card>
          <CardContent className="p-0">
            <table className="w-full text-sm">
              <thead>
                <tr className="border-b text-left text-slate-500">
                  <th className="py-3 pl-4">Filename</th>
                  <th className="py-3">Type</th>
                  <th className="py-3">Status</th>
                  <th className="py-3">Pages</th>
                  <th className="py-3">Uploaded</th>
                  <th className="py-3 pr-4">View</th>
                </tr>
              </thead>
              <tbody>
                {docs.map((doc) => (
                  <tr key={doc.id} className="border-b last:border-0">
                    <td className="py-3 pl-4">{doc.filename}</td>
                    <td className="py-3">
                      <Badge variant="info">{doc.doc_type}</Badge>
                    </td>
                    <td className="py-3">
                      <Badge
                        variant={
                          doc.ocr_status === "done"
                            ? "success"
                            : doc.ocr_status === "failed"
                              ? "danger"
                              : "warning"
                        }
                      >
                        {doc.ocr_status}
                      </Badge>
                    </td>
                    <td className="py-3">{doc.page_count ?? "—"}</td>
                    <td className="py-3 text-slate-500">{formatDate(doc.uploaded_at)}</td>
                    <td className="py-3 pr-4">
                      <a
                        href={`${process.env.NEXT_PUBLIC_API_URL || "http://localhost:8080"}/v1/documents/${doc.id}/view`}
                        target="_blank"
                        rel="noreferrer"
                        className="text-slate-900 underline hover:text-slate-600"
                      >
                        View
                      </a>
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
