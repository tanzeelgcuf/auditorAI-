"use client";

import { useState } from "react";
import { useParams, useRouter } from "next/navigation";
import { Shell } from "../../../../components/layout/shell";
import { Card, CardContent, CardHeader, CardTitle, CardDescription } from "../../../../components/ui/card";
import { Button } from "../../../../components/ui/button";
import { Dropzone } from "../../../../components/upload/dropzone";
import { ColumnMapping } from "../../../../components/upload/column-mapping";
import { useCsvMappings, useCreateCsvMapping, useUpdateCsvMapping } from "../../../../lib/hooks";

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
      <div className="mb-6">
        <h1 className="text-2xl font-semibold text-slate-900">CSV Column Mapping</h1>
        <p className="text-sm text-slate-500">
          The stored column layout for this client book. Re-run it when the export format changes.
        </p>
      </div>

      {!remapping && (
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
              <pre className="overflow-x-auto rounded-md bg-slate-50 p-4 text-xs text-slate-700">
                {JSON.stringify(existing.column_map, null, 2)}
              </pre>
            ) : (
              <p className="text-sm text-slate-500">
                No CSV mapping is stored yet. Create one below — the first CSV upload will prompt for it automatically.
              </p>
            )}
            <div className="mt-4">
              <Button onClick={() => setRemapping(true)}>Remap CSV Columns</Button>
            </div>
          </CardContent>
        </Card>
      )}

      {remapping && (
        <>
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
                <Button
                  className="mt-4"
                  variant="secondary"
                  onClick={() => {
                    setRemapping(false);
                    setHeaders([]);
                  }}
                >
                  Cancel
                </Button>
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
        </>
      )}
    </Shell>
  );
}