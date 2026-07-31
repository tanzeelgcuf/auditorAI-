import React from "react";
import { View, Text, FlatList, TouchableOpacity, StyleSheet } from "react-native";
import { useQuery, useQueryClient } from "react-query";
import { api } from "../api";
import type { Page, ReviewQueueItem } from "../types";

interface Props {
  bookId: string;
}

export function ReviewScreen({ bookId }: Props) {
  const qc = useQueryClient();
  const { data, isLoading } = useQuery(
    ["review", bookId],
    () => api.get<Page<ReviewQueueItem>>(`/v1/books/${bookId}/review-queue`),
  );

  const items = data?.items ?? [];

  function shortId(id: string | null): string {
    return id ? id.slice(0, 8) : "—";
  }

  function act(linkId: string, action: "confirm" | "reject") {
    api
      .post(`/v1/entity-links/${linkId}/${action}`)
      .then(() => qc.invalidateQueries(["review", bookId]))
      .catch(() => {});
  }

  return (
    <View style={styles.container}>
      <FlatList
        data={items}
        keyExtractor={(i) => i.id}
        ListEmptyComponent={
          <Text style={styles.empty}>{isLoading ? "Loading…" : "Nothing to review 🎉"}</Text>
        }
        renderItem={({ item }) => (
          <View style={styles.card}>
            <Text style={styles.ids}>
              <Text style={styles.label}>INV </Text>
              {shortId(item.invoice_entity_id)}  ·  <Text style={styles.label}>BNK </Text>
              {shortId(item.bank_entity_id)}  ·  <Text style={styles.label}>GL </Text>
              {shortId(item.gl_entity_id)}
            </Text>
            <Text style={styles.confidence}>
              Confidence: {(item.link_confidence * 100).toFixed(1)}%
            </Text>
            <View style={styles.actions}>
              <TouchableOpacity
                style={[styles.button, styles.confirm]}
                onPress={() => act(item.id, "confirm")}
              >
                <Text style={styles.confirmText}>✓ Confirm</Text>
              </TouchableOpacity>
              <TouchableOpacity
                style={[styles.button, styles.reject]}
                onPress={() => act(item.id, "reject")}
              >
                <Text style={styles.rejectText}>✗ Reject</Text>
              </TouchableOpacity>
            </View>
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
  ids: { fontFamily: "monospace", fontSize: 13, color: "#334155" },
  label: { color: "#94a3b8" },
  confidence: { fontSize: 13, color: "#64748b", marginTop: 8 },
  actions: { flexDirection: "row", gap: 8, marginTop: 12 },
  button: { flex: 1, borderRadius: 6, paddingVertical: 8, alignItems: "center", borderWidth: 1 },
  confirm: { backgroundColor: "#0f172a", borderColor: "#0f172a" },
  reject: { borderColor: "#cbd5e1" },
  confirmText: { color: "#fff", fontWeight: "600", fontSize: 14 },
  rejectText: { color: "#0f172a", fontWeight: "600", fontSize: 14 },
});
