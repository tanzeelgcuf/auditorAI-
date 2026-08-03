"use client";

import { useState } from "react";
import { useParams } from "next/navigation";
import { Shell } from "../../../../components/layout/shell";
import { Card, CardContent, CardHeader, CardTitle } from "../../../../components/ui/card";
import { Badge } from "../../../../components/ui/badge";
import { Dropzone } from "../../../../components/upload/dropzone";
import { ColumnMapping } from "../../../../components/upload/column-mapping";
import { useUploadDocument, useDocuments, useCsvMappings, useCreateCsvMapping } from "../../../../lib/hooks";
import { formatDate } from "../../../../lib/utils";

// Pending column-mapping gate for one file awaiting user confirmation.
// `rest` holds the other dropped files, uploaded after the mapping is saved.
interface PendingMapping {
  file: File;
  headers: string[];
  rest: File[];
}

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

const CSV_RE = /\.csv$/i;

// ponytail: header-row detection is CSV-only; reading XLSX headers client-side
// needs a spreadsheet lib (SheetJS). Add when/if XLSX export formats appear.
/** Read the first non-empty line of a CSV file client-side (doc 08 §1). */
async function readCsvHeaders(file: File): Promise<string[]> {
  const text = await file.text();
  const first = text.split(/\r?\n/).find((l) => l.trim().length > 0) ?? "";
  return first.split(",").map((h) => h.replace(/^"|"$/g, "").trim()).filter(Boolean);
}

export default function UploadPage() {
  const params = useParams();
  const bookId = String(params.bookId);
  const upload = useUploadDocument(bookId);
  const { data: docs } = useDocuments(bookId);
  const { data: mappings } = useCsvMappings(bookId);
  const createMapping = useCreateCsvMapping(bookId);
  const [uploadingFiles, setUploadingFiles] = useState<{ name: string; status: string }[]>([]);
  const [pending, setPending] = useState<PendingMapping | null>(null);

  const hasMapping = (mappings ?? []).length > 0;

  async function uploadOne(file: File) {
    await upload.mutateAsync({
      file,
      idempotencyKey: `upload-${file.name}-${Date.now()}`,
    });
  }

  async function handleFiles(files: File[]) {
    // CSV with no stored mapping for this book: parse the header row and ask
    // the user to confirm the column map before anything uploads.
    const unmapped = files.filter((f) => CSV_RE.test(f.name) && !hasMapping);
    if (unmapped.length > 0) {
      // Prompt for the first unmapped CSV; upload the rest once mapping persists.
      const first = unmapped[0];
      const headers = await readCsvHeaders(first);
      setPending({
        file: first,
        headers,
        rest: files.filter((f) => f !== first),
      });
      return;
    }
    await runUploads(files);
  }

  async function runUploads(files: File[]) {
    setUploadingFiles(files.map((f) => ({ name: f.name, status: "queued" })));
    for (const file of files) {
      setUploadingFiles((prev) =>
        prev.map((f) => (f.name === file.name ? { ...f, status: "uploading" } : f)),
      );
      try {
        await uploadOne(file);
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

  async function handleMappingConfirm(columnMap: Record<string, string>) {
    if (!pending) return;
    await createMapping.mutateAsync({ source_system: "manual_upload", column_map: columnMap });
    const file = pending.file;
    const rest = pending.rest;
    setPending(null);
    await runUploads([file, ...rest]);
  }

  return (
    <Shell>
      <h1 className="mb-6 text-2xl font-semibold text-slate-900">Upload Documents</h1>

      <Dropzone onFiles={handleFiles} uploading={uploadingFiles.some((f) => f.status === "uploading")} />

      {pending && (
        <ColumnMapping
          fileName={pending.file.name}
          headers={pending.headers}
          onConfirm={handleMappingConfirm}
          onCancel={() => setPending(null)}
          submitting={createMapping.isPending}
        />
      )}

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
