import React from "react";
import { View, Text, TouchableOpacity, StyleSheet } from "react-native";
import { FindingsScreen } from "./FindingsScreen";
import { ReviewScreen } from "./ReviewScreen";

interface Props {
  bookId: string;
  onBack: () => void;
  onLogout: () => void;
}

type Tab = "findings" | "review";

export function BookTabs({ bookId, onBack, onLogout }: Props) {
  const [tab, setTab] = React.useState<Tab>("findings");

  return (
    <View style={styles.container}>
      <View style={styles.topBar}>
        <TouchableOpacity onPress={onBack}>
          <Text style={styles.back}>← Books</Text>
        </TouchableOpacity>
        <TouchableOpacity onPress={onLogout}>
          <Text style={styles.logout}>Sign out</Text>
        </TouchableOpacity>
      </View>

      <View style={styles.tabs}>
        {(["findings", "review"] as Tab[]).map((t) => (
          <TouchableOpacity
            key={t}
            style={[styles.tab, tab === t && styles.tabActive]}
            onPress={() => setTab(t)}
          >
            <Text style={[styles.tabText, tab === t && styles.tabTextActive]}>
              {t === "findings" ? "Findings" : "Review Queue"}
            </Text>
          </TouchableOpacity>
        ))}
      </View>

      {tab === "findings" ? <FindingsScreen bookId={bookId} /> : <ReviewScreen bookId={bookId} />}
    </View>
  );
}

const styles = StyleSheet.create({
  container: { flex: 1, backgroundColor: "#f8fafc" },
  topBar: {
    flexDirection: "row",
    justifyContent: "space-between",
    alignItems: "center",
    paddingHorizontal: 16,
    paddingTop: 12,
    paddingBottom: 4,
  },
  back: { fontSize: 14, fontWeight: "600", color: "#0f172a" },
  logout: { fontSize: 13, color: "#64748b" },
  tabs: { flexDirection: "row", paddingHorizontal: 12, marginTop: 8, gap: 8 },
  tab: {
    flex: 1,
    paddingVertical: 10,
    borderRadius: 8,
    alignItems: "center",
    backgroundColor: "#e2e8f0",
  },
  tabActive: { backgroundColor: "#0f172a" },
  tabText: { fontSize: 14, fontWeight: "600", color: "#334155" },
  tabTextActive: { color: "#fff" },
});
