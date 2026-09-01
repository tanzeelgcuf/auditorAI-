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
import { Upload, Loader2, FileText, CheckCircle, XCircle } from "lucide-react";
import { MotionDiv, StaggerContainer } from "../../../../components/ui/motion";

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
  const map: Record<string, { label: string; variant: "default" | "success" | "warning" | "destructive" }> = {
    pending: { label: "Pending", variant: "warning" },
    processing: { label: "Processing", variant: "warning" },
    done: { label: "Done", variant: "success" },
    failed: { label: "Failed", variant: "destructive" },
  };
  const m = map[status] ?? { label: status, variant: "default" as const };
  return <Badge variant={m.variant}>{m.label}</Badge>;
}

const CSV_RE = /\.csv$/i;

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
    const unmapped = files.filter((f) => CSV_RE.test(f.name) && !hasMapping);
    if (unmapped.length > 0) {
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
      <MotionDiv variant="slideUp" className="mb-6">
        <h1 className="text-2xl font-semibold text-foreground">Upload Documents</h1>
        <p className="text-sm text-muted-foreground">Drop invoices, bank statements, or GL exports here</p>
      </MotionDiv>

      <MotionDiv variant="slideUp">
        <Dropzone onFiles={handleFiles} uploading={uploadingFiles.some((f) => f.status === "uploading")} />
      </MotionDiv>

      {pending && (
        <MotionDiv variant="slideDown">
          <ColumnMapping
            fileName={pending.file.name}
            headers={pending.headers}
            onConfirm={handleMappingConfirm}
            onCancel={() => setPending(null)}
            submitting={createMapping.isPending}
          />
        </MotionDiv>
      )}

      {uploadingFiles.length > 0 && (
        <MotionDiv variant="slideUp" className="mt-6">
          <Card>
            <CardHeader>
              <CardTitle className="flex items-center gap-2">
                <Upload className="h-5 w-5" aria-hidden="true" />
                Upload Progress
              </CardTitle>
            </CardHeader>
            <CardContent>
              <ul className="space-y-2" role="list" aria-live="polite">
                {uploadingFiles.map((f) => (
                  <li key={f.name} className="flex items-center justify-between text-sm">
                    <span>{f.name}</span>
                    <span className="flex items-center gap-2">
                      {f.status === "uploading" && <Loader2 className="h-4 w-4 animate-spin text-primary" aria-hidden="true" />}
                      {f.status === "done" && <CheckCircle className="h-4 w-4 text-success" aria-hidden="true" />}
                      {f.status === "failed" && <XCircle className="h-4 w-4 text-destructive" aria-hidden="true" />}
                      <span className="text-muted-foreground capitalize">{f.status}</span>
                    </span>
                  </li>
                ))}
              </ul>
            </CardContent>
          </Card>
        </MotionDiv>
      )}

      <MotionDiv variant="slideUp" className="mt-6">
        <Card>
          <CardHeader>
            <CardTitle className="flex items-center gap-2">
              <FileText className="h-5 w-5" aria-hidden="true" />
              Recent Documents
            </CardTitle>
          </CardHeader>
          <CardContent>
            {docs?.items?.length === 0 ? (
              <p className="text-center text-muted-foreground py-8">No documents uploaded yet</p>
            ) : (
              <div className="overflow-x-auto">
                <table className="w-full text-sm" role="table">
                  <thead>
                    <tr className="border-b text-left text-muted-foreground">
                      <th className="py-2">Name</th>
                      <th className="py-2">Type</th>
                      <th className="py-2">Status</th>
                      <th className="py-2">Uploaded</th>
                    </tr>
                  </thead>
                  <tbody>
                    <StaggerContainer staggerChildren={0.03} staggerDelay={0.05}>
                      {(docs?.items ?? []).map((doc) => (
                        <tr key={doc.id} className="border-b last:border-0">
                          <td className="py-2 font-medium">{doc.filename}</td>
                          <td className="py-2">{docTypeBadge(doc.doc_type)}</td>
                          <td className="py-2">{ocrStatusBadge(doc.ocr_status)}</td>
                          <td className="py-2 text-muted-foreground">{formatDate(doc.uploaded_at)}</td>
                        </tr>
                      ))}
                    </StaggerContainer>
                  </tbody>
                </table>
              </div>
            )}
          </CardContent>
        </Card>
      </MotionDiv>
    </Shell>
  );
}