import React from "react";
import { View, Text, FlatList, TouchableOpacity, StyleSheet } from "react-native";
import { useQuery } from "react-query";
import { api } from "../api";
import type { ClientBook, Page } from "../types";

interface Props {
  onSelectBook: (book: ClientBook) => void;
}

export function BooksScreen({ onSelectBook }: Props) {
  const { data, isLoading } = useQuery(["books"], () => api.get<Page<ClientBook>>("/v1/books"));

  const books = data?.items ?? [];

  return (
    <View style={styles.container}>
      <Text style={styles.header}>Client Books</Text>
      <FlatList
        data={books}
        keyExtractor={(b) => b.id}
        ListEmptyComponent={
          <Text style={styles.empty}>{isLoading ? "Loading…" : "No client books"}</Text>
        }
        renderItem={({ item }) => (
          <TouchableOpacity style={styles.card} onPress={() => onSelectBook(item)}>
            <Text style={styles.name}>{item.client_name}</Text>
            <Text style={styles.meta}>
              {item.base_currency} · tolerance {item.reconciliation_tolerance_cents}¢ ·{" "}
              {item.tolerance_mode}
            </Text>
          </TouchableOpacity>
        )}
      />
    </View>
  );
}

const styles = StyleSheet.create({
  container: { flex: 1, backgroundColor: "#f8fafc", paddingTop: 60 },
  header: { fontSize: 24, fontWeight: "700", color: "#0f172a", paddingHorizontal: 16, marginBottom: 8 },
  empty: { textAlign: "center", marginTop: 48, color: "#64748b" },
  card: {
    backgroundColor: "#fff",
    borderRadius: 10,
    padding: 16,
    marginHorizontal: 12,
    marginTop: 10,
    borderWidth: 1,
    borderColor: "#e2e8f0",
  },
  name: { fontSize: 16, fontWeight: "600", color: "#0f172a" },
  meta: { fontSize: 13, color: "#64748b", marginTop: 4 },
});
