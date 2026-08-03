"use client";

import { useState } from "react";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "../../components/ui/card";
import { Button } from "../../components/ui/button";
import { Badge } from "../../components/ui/badge";
import { TARGET_FIELDS, suggestColumnMap } from "../../lib/hooks";

// Detected column names (from the file header row), target field set, and an
// initially auto-suggested column_map. Live on the upload page + remap flow.
type ColumnMap = Record<string, string>;

interface ColumnMappingProps {
  fileName: string;
  headers: string[];
  initialMap?: ColumnMap;
  onConfirm: (columnMap: ColumnMap) => void;
  onCancel?: () => void;
  submitting?: boolean;
}

const FIELD_LABELS: Record<string, string> = {
  date: "Date",
  amount: "Amount",
  debit: "Debit",
  credit: "Credit",
  counterparty: "Counterparty",
  account: "Account",
  transaction_ref: "Transaction Ref",
};

export function ColumnMapping({ fileName, headers, initialMap, onConfirm, onCancel, submitting }: ColumnMappingProps) {
  const [map, setMap] = useState<ColumnMap>(initialMap ?? suggestColumnMap(headers));

  return (
    <Card className="mt-6">
      <CardHeader>
        <CardTitle>Map CSV Columns</CardTitle>
        <CardDescription>
          Match source columns in <span className="font-medium text-slate-700">{fileName}</span> to target fields.
          Suggestions are auto-detected — confirm or adjust before uploading.
        </CardDescription>
      </CardHeader>
      <CardContent>
        <div className="space-y-3">
          {TARGET_FIELDS.map((field) => (
            <div key={field} className="flex items-center gap-3">
              <span className="w-40 text-sm font-medium text-slate-700">{FIELD_LABELS[field]}</span>
              <select
                value={map[field] ?? ""}
                onChange={(e) => setMap((m) => ({ ...m, [field]: e.target.value }))}
                className="h-9 flex-1 rounded-md border border-slate-300 bg-white px-3 text-sm shadow-sm focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-slate-400"
              >
                <option value="">— not mapped —</option>
                {headers.map((h) => (
                  <option key={h} value={h}>
                    {h}
                  </option>
                ))}
              </select>
              {map[field] && <Badge variant="info">mapped</Badge>}
            </div>
          ))}
        </div>
        <div className="mt-6 flex gap-3">
          <Button
            onClick={() => onConfirm(map)}
            disabled={submitting}
          >
            Confirm mapping
          </Button>
          {onCancel && (
            <Button variant="secondary" onClick={onCancel} disabled={submitting}>
              Cancel
            </Button>
          )}
        </div>
      </CardContent>
    </Card>
  );
}