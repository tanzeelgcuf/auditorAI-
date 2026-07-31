import React, { useEffect, useState } from "react";
import { SafeAreaView, StatusBar } from "react-native";
import { QueryClient, QueryClientProvider } from "react-query";
import * as Notifications from "expo-notifications";
import { LoginScreen } from "./src/screens/LoginScreen";
import { BooksScreen } from "./src/screens/BooksScreen";
import { BookTabs } from "./src/screens/BookTabs";
import { getAccessToken, clearTokens } from "./src/api";
import type { ClientBook } from "./src/types";

const queryClient = new QueryClient();

Notifications.setNotificationHandler({
  handleNotification: async () => ({
    shouldShowAlert: true,
    shouldShowBanner: true,
    shouldShowList: true,
    shouldPlaySound: false,
    shouldSetBadge: false,
  }),
});

export default function App() {
  const [authed, setAuthed] = useState(false);
  const [booted, setBooted] = useState(false);
  const [selectedBook, setSelectedBook] = useState<ClientBook | null>(null);

  useEffect(() => {
    getAccessToken()
      .then((t) => setAuthed(!!t))
      .finally(() => setBooted(true));
  }, []);

  useEffect(() => {
    // Request push permission so high-severity findings can notify
    Notifications.requestPermissionsAsync().catch(() => {});
  }, []);

  if (!booted) return null;

  return (
    <QueryClientProvider client={queryClient}>
      <StatusBar barStyle="dark-content" />
      <SafeAreaView style={{ flex: 1, backgroundColor: "#f8fafc" }}>
        {!authed ? (
          <LoginScreen onLoggedIn={() => setAuthed(true)} />
        ) : selectedBook ? (
          <BookTabs
            bookId={selectedBook.id}
            onBack={() => setSelectedBook(null)}
            onLogout={() => {
              clearTokens().then(() => setAuthed(false));
            }}
          />
        ) : (
          <BooksScreen onSelectBook={setSelectedBook} />
        )}
      </SafeAreaView>
    </QueryClientProvider>
  );
}
