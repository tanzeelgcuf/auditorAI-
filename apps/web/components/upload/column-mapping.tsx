"use client";

import { useState } from "react";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "../../components/ui/card";
import { Badge } from "../../components/ui/badge";
import { TARGET_FIELDS, suggestColumnMap } from "../../lib/hooks";
import { Check, X, Loader2, ArrowRight } from "lucide-react";
import { MotionDiv, StaggerContainer, MotionButton } from "../../components/ui/motion";

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

const FIELD_DESCRIPTIONS: Record<string, string> = {
  date: "Transaction date (required)",
  amount: "Single amount column (debit - credit)",
  debit: "Debit amount (if separate from credit)",
  credit: "Credit amount (if separate from debit)",
  counterparty: "Vendor / customer name",
  account: "GL account code or name",
  transaction_ref: "Unique transaction identifier",
};

export function ColumnMapping({
  fileName,
  headers,
  initialMap,
  onConfirm,
  onCancel,
  submitting,
}: ColumnMappingProps) {
  const [map, setMap] = useState<ColumnMap>(initialMap ?? suggestColumnMap(headers));

  const mappedCount = Object.values(map).filter(Boolean).length;
  const hasRequiredMapping = map.date || (map.debit && map.credit);

  return (
    <MotionDiv variant="slideUp">
      <Card>
        <CardHeader>
          <CardTitle className="flex items-center gap-2">
            <span className="h-8 w-8 rounded-lg bg-primary/10 flex items-center justify-center">
              <span className="text-primary font-mono text-sm">📊</span>
            </span>
            Map CSV Columns
          </CardTitle>
          <CardDescription>
            Match source columns in <span className="font-medium text-foreground">{fileName}</span> to target fields.
            Suggestions are auto-detected — confirm or adjust before uploading.
          </CardDescription>
        </CardHeader>
        <CardContent>
          <div className="mb-4 flex items-center gap-4 text-sm">
            <div className="flex items-center gap-1.5">
              <span className="px-2 py-0.5 rounded bg-success-bg text-success text-xs font-medium">
                {mappedCount}/{TARGET_FIELDS.length} mapped
              </span>
            </div>
            {!hasRequiredMapping && (
              <div className="flex items-center gap-1.5 text-warning">
                <span className="px-2 py-0.5 rounded bg-warning-bg text-warning text-xs font-medium">
                  Required: Date + (Amount or Debit+Credit)
                </span>
              </div>
            )}
          </div>

          <StaggerContainer staggerChildren={0.03} staggerDelay={0.05}>
            <div className="space-y-3">
              {TARGET_FIELDS.map((field) => (
                <MotionDiv key={field} variant="slideRight">
                  <div className="flex items-center gap-3">
                    <div className="w-36 flex-shrink-0">
                      <label className="block text-sm font-medium text-foreground">{FIELD_LABELS[field]}</label>
                      <p className="text-xs text-muted-foreground">{FIELD_DESCRIPTIONS[field]}</p>
                    </div>
                    <div className="flex-1 relative">
                      <select
                        value={map[field] ?? ""}
                        onChange={(e) =>
                          setMap((m) => ({ ...m, [field]: e.target.value || undefined }))
                        }
                        disabled={submitting}
                        className="w-full h-10 rounded-md border border-input bg-background px-3 py-2 text-sm shadow-sm focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring disabled:opacity-50 disabled:pointer-events-none appearance-none bg-no-repeat bg-right pr-10"
                        style={{
                          backgroundImage: `url("data:image/svg+xml,%3csvg xmlns='http://www.w3.org/2000/svg' fill='none' viewBox='0 0 20 20'%3e%3cpath stroke='%236b7280' stroke-linecap='round' stroke-linejoin='round' stroke-width='1.5' d='M6 8l4 4 4-4'/%3e%3c/svg%3e")`,
                          backgroundPosition: "right 0.5rem center",
                          backgroundSize: "1.5em 1.5em",
                        }}
                      >
                        <option value="">— not mapped —</option>
                        {headers.map((h) => (
                          <option key={h} value={h}>
                            {h}
                          </option>
                        ))}
                      </select>
                    </div>
                    {map[field] && (
                      <Badge variant="success" className="gap-1">
                        <Check className="h-3 w-3" aria-hidden="true" />
                        mapped
                      </Badge>
                    )}
                  </div>
                </MotionDiv>
              ))}
            </div>
          </StaggerContainer>

          <MotionDiv variant="slideUp" className="mt-6 flex gap-3">
            <MotionButton
              onClick={() => onConfirm(map)}
              disabled={submitting || !hasRequiredMapping}
              className="gap-2"
            >
              {submitting ? (
                <>
                  <Loader2 className="h-4 w-4 animate-spin" aria-hidden="true" />
                  Confirming…
                </>
              ) : (
                <>
                  Confirm mapping
                  <ArrowRight className="h-4 w-4" aria-hidden="true" />
                </>
              )}
            </MotionButton>
            {onCancel && (
              <MotionButton variant="secondary" onClick={onCancel} disabled={submitting} className="gap-2">
                <X className="h-4 w-4" aria-hidden="true" />
                Cancel
              </MotionButton>
            )}
          </MotionDiv>
        </CardContent>
          </Card>
    </MotionDiv>
  );
}