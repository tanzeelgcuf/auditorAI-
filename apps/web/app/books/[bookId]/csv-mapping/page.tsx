"use client";

import { useState } from "react";
import { useParams, useRouter } from "next/navigation";
import { Shell } from "../../../../components/layout/shell";
import { Card, CardContent, CardHeader, CardTitle, CardDescription } from "../../../../components/ui/card";
import { Dropzone } from "../../../../components/upload/dropzone";
import { ColumnMapping } from "../../../../components/upload/column-mapping";
import { useCsvMappings, useCreateCsvMapping, useUpdateCsvMapping } from "../../../../lib/hooks";
import { ArrowLeft, RefreshCw } from "lucide-react";
import { MotionDiv, MotionButton } from "../../../../components/ui/motion";

const CSV_RE = /\.csv$/i;

function readCsvHeaders(file: File): Promise<string[]> {
  return file.text().then((text) => {
    const first = text.split(/\r?\n/).find((l) => l.trim().length > 0) ?? "";
    return first.split(",").map((h) => h.replace(/^"|"$/g, "").trim()).filter(Boolean);
  });
}

export default function CsvMappingPage() {
  const params = useParams();
  const router = useRouter();
  const bookId = String(params.bookId);
  const { data: mappings } = useCsvMappings(bookId);
  const createMapping = useCreateCsvMapping(bookId);
  const updateMapping = useUpdateCsvMapping(bookId);

  const existing = mappings?.[0];
  const [remapping, setRemapping] = useState(false);
  const [headers, setHeaders] = useState<string[]>([]);
  const [fileName, setFileName] = useState("");

  const submitting = createMapping.isPending || updateMapping.isPending;

  async function saveMapping(columnMap: Record<string, string>) {
    if (existing) {
      await updateMapping.mutateAsync({ mappingId: existing.id, body: { column_map: columnMap } });
    } else {
      await createMapping.mutateAsync({ source_system: "manual_upload", column_map: columnMap });
    }
    router.replace(`/books/${bookId}/upload`);
  }

  return (
    <Shell>
      <MotionDiv variant="slideUp" className="mb-6">
        <div className="flex items-center gap-2">
          <MotionButton variant="ghost" size="icon" onClick={() => router.back()}>
            <ArrowLeft className="h-4 w-4" aria-hidden="true" />
          </MotionButton>
          <div>
            <h1 className="text-2xl font-semibold text-foreground">CSV Column Mapping</h1>
            <p className="text-sm text-muted-foreground">
              The stored column layout for this client book. Re-run it when the export format changes.
            </p>
          </div>
        </div>
      </MotionDiv>

      {!remapping && (
        <MotionDiv variant="slideUp">
          <Card>
            <CardHeader>
              <CardTitle>{existing ? "Stored Mapping" : "No Mapping Yet"}</CardTitle>
              {existing && (
                <CardDescription>
                  Source: {existing.source_system} · created {new Date(existing.created_at).toLocaleDateString()}
                </CardDescription>
              )}
            </CardHeader>
            <CardContent>
              {existing ? (
                <pre className="overflow-x-auto rounded-md bg-muted p-4 text-xs text-foreground">
                  {JSON.stringify(existing.column_map, null, 2)}
                </pre>
              ) : (
                <p className="text-sm text-muted-foreground">
                  No CSV mapping is stored yet. Create one below — the first CSV upload will prompt for it automatically.
                </p>
              )}
              <div className="mt-4 flex gap-2">
                <MotionButton onClick={() => setRemapping(true)} className="gap-2">
                  <RefreshCw className="h-4 w-4" aria-hidden="true" />
                  Remap CSV Columns
                </MotionButton>
              </div>
            </CardContent>
          </Card>
        </MotionDiv>
      )}

      {remapping && (
        <MotionDiv variant="slideDown">
          {headers.length === 0 ? (
            <Card>
              <CardContent className="pt-6">
                <Dropzone
                  onFiles={async (files) => {
                    const f = files.find((file) => CSV_RE.test(file.name));
                    if (f) {
                      setFileName(f.name);
                      setHeaders(await readCsvHeaders(f));
                    }
                  }}
                />
                <MotionButton
                  className="mt-4"
                  variant="secondary"
                  onClick={() => {
                    setRemapping(false);
                    setHeaders([]);
                  }}
                >
                  <ArrowLeft className="h-4 w-4" aria-hidden="true" />
                  Cancel
                </MotionButton>
              </CardContent>
            </Card>
          ) : (
            <ColumnMapping
              fileName={fileName}
              headers={headers}
              initialMap={existing?.column_map}
              onConfirm={saveMapping}
              onCancel={() => {
                setHeaders([]);
                setRemapping(false);
              }}
              submitting={submitting}
            />
          )}
        </MotionDiv>
      )}
    </Shell>
  );
}