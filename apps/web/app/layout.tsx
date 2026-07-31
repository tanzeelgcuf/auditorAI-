import type { Metadata } from "next";
import "./globals.css";
import { QueryProvider } from "../components/providers/query-provider";

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
    <html lang="en">
      <body className="antialiased">
        <QueryProvider>{children}</QueryProvider>
      </body>
    </html>
  );
}
