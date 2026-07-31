export interface ClientBook {
  id: string;
  client_name: string;
  base_currency: string;
  reconciliation_tolerance_cents: number;
  tolerance_mode: string;
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

export interface ReviewQueueItem {
  id: string;
  invoice_entity_id: string | null;
  bank_entity_id: string | null;
  gl_entity_id: string | null;
  link_confidence: number;
  status: string;
  created_at: string;
}

export interface Report {
  id: string;
  period_start: string;
  period_end: string;
  generated_at: string;
  finding_ids: string[];
}

export interface Page<T> {
  items: T[];
  next_cursor: string | null;
}
