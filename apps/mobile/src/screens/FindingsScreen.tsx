import React from "react";
import {
  View,
  Text,
  FlatList,
  TouchableOpacity,
  StyleSheet,
  RefreshControl,
} from "react-native";
import { useQuery, useQueryClient } from "react-query";
import { api } from "../api";
import type { Finding, Page } from "../types";

const SEVERITY_COLORS: Record<string, string> = {
  info: "#64748b",
  low: "#3b82f6",
  medium: "#f59e0b",
  high: "#ef4444",
};

interface Props {
  bookId: string;
}

export function FindingsScreen({ bookId }: Props) {
  const qc = useQueryClient();
  const { data, isLoading, refetch, isFetching } = useQuery(
    ["findings", bookId],
    () => api.get<Page<Finding>>(`/v1/books/${bookId}/findings`),
  );

  const findings = data?.items ?? [];

  function statusColor(status: string) {
    return status === "open" ? "#dc2626" : status === "acknowledged" ? "#d97706" : "#16a34a";
  }

  return (
    <View style={styles.container}>
      <FlatList
        data={findings}
        keyExtractor={(f) => f.id}
        refreshControl={<RefreshControl refreshing={isFetching} onRefresh={() => refetch()} />}
        ListEmptyComponent={
          <Text style={styles.empty}>{isLoading ? "Loading…" : "No findings"}</Text>
        }
        renderItem={({ item }) => (
          <View style={styles.card}>
            <View style={styles.cardHeader}>
              <View
                style={[styles.severityDot, { backgroundColor: SEVERITY_COLORS[item.severity] }]}
              />
              <Text style={styles.severity}>{item.severity.toUpperCase()}</Text>
              <Text style={styles.rule}>{item.rule_id}</Text>
              <Text style={[styles.status, { color: statusColor(item.status) }]}>
                {item.status}
              </Text>
            </View>

            <Text style={styles.formula} numberOfLines={2}>
              {item.calculation_formula}
            </Text>

            <Text style={styles.meta}>
              Variance: ${(item.calculated_variance_cents / 100).toFixed(2)} · Tolerance: $
              {(item.tolerance_cents / 100).toFixed(2)}
            </Text>

            {item.status === "open" && (
              <TouchableOpacity
                style={styles.ackButton}
                onPress={() =>
                  api.patch(`/v1/findings/${item.id}/status`, { status: "acknowledged" }).then(() =>
                    qc.invalidateQueries(["findings", bookId]),
                  )
                }
              >
                <Text style={styles.ackText}>Acknowledge</Text>
              </TouchableOpacity>
            )}
          </View>
        )}
      />
    </View>
  );
}

const styles = StyleSheet.create({
  container: { flex: 1, backgroundColor: "#f8fafc" },
  empty: { textAlign: "center", marginTop: 48, color: "#64748b" },
  card: {
    backgroundColor: "#fff",
    borderRadius: 10,
    padding: 14,
    marginHorizontal: 12,
    marginTop: 12,
    borderWidth: 1,
    borderColor: "#e2e8f0",
  },
  cardHeader: { flexDirection: "row", alignItems: "center", gap: 6 },
  severityDot: { width: 10, height: 10, borderRadius: 5 },
  severity: { fontSize: 11, fontWeight: "700", color: "#334155" },
  rule: { fontSize: 11, color: "#94a3b8", flex: 1 },
  status: { fontSize: 11, fontWeight: "600" },
  formula: { fontFamily: "monospace", fontSize: 12, color: "#334155", marginTop: 10 },
  meta: { fontSize: 12, color: "#64748b", marginTop: 6 },
  ackButton: {
    marginTop: 10,
    borderWidth: 1,
    borderColor: "#cbd5e1",
    borderRadius: 6,
    paddingVertical: 6,
    alignItems: "center",
  },
  ackText: { fontSize: 13, fontWeight: "600", color: "#0f172a" },
});
