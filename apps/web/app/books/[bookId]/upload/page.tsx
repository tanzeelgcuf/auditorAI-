"use client";

import { useState } from "react";
import { useParams } from "next/navigation";
import { Shell } from "../../../../components/layout/shell";
import { Card, CardContent, CardHeader, CardTitle } from "../../../../components/ui/card";
import { Badge } from "../../../../components/ui/badge";
import { Dropzone } from "../../../../components/upload/dropzone";
import { useUploadDocument, useDocuments } from "../../../../lib/hooks";
import { formatDate } from "../../../../lib/utils";

function docTypeBadge(docType: string) {
  const map: Record<string, { label: string; variant: "info" | "default" }> = {
    invoice: { label: "Invoice", variant: "info" },
    bank_statement: { label: "Bank", variant: "default" },
    gl_export: { label: "GL Export", variant: "default" },
  };
  const m = map[docType] ?? { label: docType, variant: "default" as const };
  return <Badge variant={m.variant}>{m.label}</Badge>;
}

function ocrStatusBadge(status: string) {
  const map: Record<string, { label: string; variant: "default" | "success" | "warning" | "danger" }> = {
    pending: { label: "Pending", variant: "warning" },
    processing: { label: "Processing", variant: "warning" },
    done: { label: "Done", variant: "success" },
    failed: { label: "Failed", variant: "danger" },
  };
  const m = map[status] ?? { label: status, variant: "default" as const };
  return <Badge variant={m.variant}>{m.label}</Badge>;
}

export default function UploadPage() {
  const params = useParams();
  const bookId = String(params.bookId);
  const upload = useUploadDocument(bookId);
  const { data: docs } = useDocuments(bookId);
  const [uploadingFiles, setUploadingFiles] = useState<{ name: string; status: string }[]>([]);

  async function handleFiles(files: File[]) {
    setUploadingFiles(files.map((f) => ({ name: f.name, status: "queued" })));
    for (const file of files) {
      setUploadingFiles((prev) =>
        prev.map((f) => (f.name === file.name ? { ...f, status: "uploading" } : f)),
      );
      try {
        await upload.mutateAsync({
          file,
          idempotencyKey: `upload-${file.name}-${Date.now()}`,
        });
        setUploadingFiles((prev) =>
          prev.map((f) => (f.name === file.name ? { ...f, status: "done" } : f)),
        );
      } catch {
        setUploadingFiles((prev) =>
          prev.map((f) => (f.name === file.name ? { ...f, status: "failed" } : f)),
        );
      }
    }
  }

  return (
    <Shell>
      <h1 className="mb-6 text-2xl font-semibold text-slate-900">Upload Documents</h1>

      <Dropzone onFiles={handleFiles} uploading={uploadingFiles.some((f) => f.status === "uploading")} />

      {uploadingFiles.length > 0 && (
        <Card className="mt-6">
          <CardHeader>
            <CardTitle>Upload Progress</CardTitle>
          </CardHeader>
          <CardContent>
            <ul className="space-y-2">
              {uploadingFiles.map((f) => (
                <li key={f.name} className="flex items-center justify-between text-sm">
                  <span>{f.name}</span>
                  <span className="text-slate-500">{f.status}</span>
                </li>
              ))}
            </ul>
          </CardContent>
        </Card>
      )}

      <Card className="mt-6">
        <CardHeader>
          <CardTitle>Recent Documents</CardTitle>
        </CardHeader>
        <CardContent>
          <table className="w-full text-sm">
            <thead>
              <tr className="border-b text-left text-slate-500">
                <th className="py-2">Name</th>
                <th className="py-2">Type</th>
                <th className="py-2">Status</th>
                <th className="py-2">Uploaded</th>
              </tr>
            </thead>
            <tbody>
              {(docs?.items ?? []).map((doc) => (
                <tr key={doc.id} className="border-b last:border-0">
                  <td className="py-2">{doc.filename}</td>
                  <td className="py-2">{docTypeBadge(doc.doc_type)}</td>
                  <td className="py-2">{ocrStatusBadge(doc.ocr_status)}</td>
                  <td className="py-2 text-slate-500">{formatDate(doc.uploaded_at)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </CardContent>
      </Card>
    </Shell>
  );
}
