// lib/hooks.ts — React Query hooks for all API endpoints.

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api, cursorParams, Page } from "./api";

// ---- types ----
export interface ClientBook {
  id: string;
  firm_id: string;
  client_name: string;
  base_currency: string;
  reconciliation_tolerance_cents: number;
  tolerance_mode: string;
  fiscal_year_start_month: number;
  auto_link_confidence_threshold: number;
}

export interface SourceDocument {
  id: string;
  client_book_id: string;
  filename: string;
  doc_type: string;
  ocr_status: "pending" | "processing" | "done" | "failed";
  page_count: number | null;
  uploaded_at: string;
}

export interface ReviewQueueItem {
  id: string;
  invoice_entity_id: string | null;
  bank_entity_id: string | null;
  gl_entity_id: string | null;
  link_confidence: number;
  status: "auto_linked" | "needs_review" | "confirmed" | "rejected";
  created_at: string;
}

export interface Finding {
  id: string;
  rule_id: string;
  rule_version: string;
  calculated_variance_cents: number;
  tolerance_cents: number;
  exceeds_tolerance: boolean;
  calculation_formula: string;
  severity: "info" | "low" | "medium" | "high";
  status: "open" | "acknowledged" | "resolved";
  created_at: string;
}

export interface Report {
  id: string;
  client_book_id: string;
  period_start: string;
  period_end: string;
  generated_at: string;
  finding_ids: string[];
}

export interface Citation {
  source_document_id: string;
  page_number: number;
  bbox: { x: number; y: number; width: number; height: number };
}

// ---- auth ----
export function useLogin() {
  return useMutation({
    mutationFn: (credentials: { email: string; password: string; totp_code?: string }) =>
      api.post<{ access_token: string; refresh_token: string }>("/v1/auth/login", credentials),
  });
}

export function useSignup() {
  return useMutation({
    mutationFn: (body: { firm_name: string; email: string; password: string }) =>
      api.post<{ message: string }>("/v1/auth/signup", body),
  });
}

// ---- books ----
export function useBooks() {
  return useQuery({
    queryKey: ["books"],
    queryFn: () => api.get<Page<ClientBook>>(`/v1/books?${cursorParams()}`),
  });
}

export function useBook(bookId: string) {
  return useQuery({
    queryKey: ["book", bookId],
    queryFn: () => api.get<ClientBook>(`/v1/books/${bookId}`),
    enabled: !!bookId,
  });
}

export function useCreateBook() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (body: { client_name: string; base_currency?: string }) =>
      api.post<ClientBook>("/v1/books", body),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["books"] }),
  });
}

export function useUpdateBookSettings(bookId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (body: Partial<ClientBook>) =>
      api.patch<ClientBook>(`/v1/books/${bookId}/settings`, body),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["book", bookId] }),
  });
}

// ---- csv column mappings ----
export interface CsvColumnMapping {
  id: string;
  client_book_id: string;
  source_system: string;
  column_map: Record<string, string>; // target field -> source column
  created_at: string;
}

export const TARGET_FIELDS = ["date", "amount", "debit", "credit", "counterparty", "account", "transaction_ref"] as const;

/** Normalize header/target names for matching. */
function norm(s: string): string {
  return s.toLowerCase().replace(/[^a-z0-9]+/g, "");
}

/**
 * Auto-suggest a detected source column for a target field (doc 08 §1).
 * Normalized substring/equality match against a per-field alias list.
 */
function suggestField(field: string, headerNames: string[]): string {
  const f = norm(field);
  const aliases: Record<string, string[]> = {
    date: ["date", "courant"],
    amount: ["amount", "amt", "value", "total"],
    debit: ["debit", "dr", "db"],
    credit: ["credit", "cr", "cd"],
    counterparty: ["counterparty", "vendor", "supplier", "payee", "customer", "name", "description"],
    account: ["account", "gl", "code", "category"],
    transaction_ref: ["ref", "reference", "trans", "check", "invoice"],
  };
  const fieldAliases = aliases[field] ?? [];

  for (const h of headerNames) {
    const hn = norm(h);
    if (hn === f) return h;
    for (const a of fieldAliases) {
      const aa = norm(a);
      if (hn === aa || hn.includes(aa) || aa.includes(hn)) return h;
    }
  }
  // transaction_ref: invoice/check + no/num
  if (field === "transaction_ref") {
    for (const h of headerNames) {
      const hn = norm(h);
      if (/\d+/.test(hn) && /(invoice|check|ref|no|num)/.test(hn)) return h;
    }
  }
  return "";
}

/** Build an initial column_map with auto-suggested source headers. */
export function suggestColumnMap(headerNames: string[]): Record<string, string> {
  const map: Record<string, string> = {};
  for (const t of TARGET_FIELDS) map[t] = suggestField(t, headerNames);
  return map;
}

export function useCsvMappings(bookId: string) {
  return useQuery({
    queryKey: ["csv-mappings", bookId],
    queryFn: () => api.get<CsvColumnMapping[]>(`/v1/books/${bookId}/csv-mappings`),
    enabled: !!bookId,
  });
}

export function useCreateCsvMapping(bookId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (body: { source_system: string; column_map: Record<string, string> }) =>
      api.post<CsvColumnMapping>(`/v1/books/${bookId}/csv-mappings`, body),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["csv-mappings", bookId] }),
  });
}

export function useUpdateCsvMapping(bookId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ mappingId, body }: { mappingId: string; body: { column_map: Record<string, string> } }) =>
      api.put<CsvColumnMapping>(`/v1/books/${bookId}/csv-mappings/${mappingId}`, body),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["csv-mappings", bookId] }),
  });
}

// ---- documents ----
export function useDocuments(bookId: string, cursor?: string | null) {
  return useQuery({
    queryKey: ["documents", bookId, cursor],
    queryFn: () =>
      api.get<Page<SourceDocument>>(`/v1/books/${bookId}/documents?${cursorParams(cursor)}`),
    enabled: !!bookId,
    // Poll OCR status while docs are processing
    refetchInterval: (query) => {
      const docs = (query.state.data?.items ?? []) as SourceDocument[];
      return docs.some((d) => d.ocr_status === "pending" || d.ocr_status === "processing")
        ? 3000
        : false;
    },
  });
}

export function useUploadDocument(bookId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async ({ file, idempotencyKey }: { file: File; idempotencyKey: string }) => {
      const formData = new FormData();
      formData.append("file", file);
      const token = localStorage.getItem("ai_auditor_access_token");
      const res = await fetch(`${process.env.NEXT_PUBLIC_API_URL || "http://localhost:8080"}/v1/books/${bookId}/documents`, {
        method: "POST",
        headers: {
          Authorization: `Bearer ${token}`,
          "Idempotency-Key": idempotencyKey,
        },
        body: formData,
      });
      if (!res.ok) throw new Error(`Upload failed: ${res.status}`);
      return res.json();
    },
    onSuccess: () => qc.invalidateQueries({ queryKey: ["documents", bookId] }),
  });
}

// ---- review queue ----
export function useReviewQueue(bookId: string) {
  return useQuery({
    queryKey: ["review-queue", bookId],
    queryFn: () => api.get<Page<ReviewQueueItem>>(`/v1/books/${bookId}/review-queue`),
    enabled: !!bookId,
  });
}

export function useConfirmLink() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ linkId }: { linkId: string }) =>
      api.post(`/v1/entity-links/${linkId}/confirm`),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["review-queue"] });
      qc.invalidateQueries({ queryKey: ["findings"] });
    },
  });
}

export function useRejectLink() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ linkId }: { linkId: string }) =>
      api.post(`/v1/entity-links/${linkId}/reject`),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["review-queue"] }),
  });
}

export function useBulkConfirm(bookId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ linkIds }: { linkIds: string[] }) =>
      api.post(`/v1/books/${bookId}/review-queue/bulk-confirm`, { entity_link_ids: linkIds }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["review-queue", bookId] });
    },
  });
}

// ---- findings ----
export function useFindings(bookId: string) {
  return useQuery({
    queryKey: ["findings", bookId],
    queryFn: () => api.get<Page<Finding>>(`/v1/books/${bookId}/findings`),
    enabled: !!bookId,
  });
}

export function useUpdateFindingStatus(findingId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (status: "open" | "acknowledged" | "resolved") =>
      api.patch(`/v1/findings/${findingId}/status`, { status }),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["findings"] }),
  });
}

// ---- reports ----
export function useGenerateReport(bookId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (body: { period_start: string; period_end: string }) =>
      api.post<Report>(`/v1/books/${bookId}/reports`, body),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["reports", bookId] }),
  });
}

export function useReport(reportId: string) {
  return useQuery({
    queryKey: ["report", reportId],
    queryFn: () => api.get<Report>(`/v1/reports/${reportId}`),
    enabled: !!reportId,
  });
}

export function useCitation(reportId: string, findingId: string | null) {
  return useQuery({
    queryKey: ["citation", reportId, findingId],
    queryFn: () => api.get<Citation>(`/v1/reports/${reportId}/citation/${findingId}`),
    enabled: !!reportId && !!findingId,
  });
}

// ---- admin ----
export function useFirmSettings() {
  return useQuery({
    queryKey: ["firm-settings"],
    queryFn: () =>
      api.get<{
        id: string;
        name: string;
        logo_storage_key?: string;
        brand_primary_color?: string;
        report_footer_text?: string;
      }>("/v1/admin/settings"),
  });
}

export function useUpdateFirmSettings() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (body: { brand_primary_color?: string; report_footer_text?: string; logo_storage_key?: string }) =>
      api.patch("/v1/admin/settings", body),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["firm-settings"] }),
  });
}
