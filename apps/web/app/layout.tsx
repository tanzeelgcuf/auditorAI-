import type { Metadata } from "next";
import "./globals.css";
import { QueryProvider } from "../components/providers/query-provider";
import { ThemeProvider } from "../components/providers/theme-provider";

export const metadata: Metadata = {
  title: "AI Auditor",
  description: "3-way reconciliation for accounting firms",
};

export default function RootLayout({
  children,
}: Readonly<{
  children: React.ReactNode;
}>) {
  return (
    <html lang="en" suppressHydrationWarning>
      <body className="antialiased">
        <ThemeProvider attribute="data-theme" defaultTheme="system" enableSystem>
          <QueryProvider>{children}</QueryProvider>
        </ThemeProvider>
      </body>
    </html>
  );
}
