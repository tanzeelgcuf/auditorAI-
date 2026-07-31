"use client";

import Link from "next/link";
import { usePathname, useRouter } from "next/navigation";
import { useEffect } from "react";
import { getAccessToken, clearTokens } from "../../lib/api";
import { cn } from "../../lib/utils";

const NAV = [
  { href: "/dashboard", label: "Dashboard", icon: "▤" },
];

export function Shell({ children }: { children: React.ReactNode }) {
  const pathname = usePathname();
  const router = useRouter();

  useEffect(() => {
    if (!getAccessToken()) {
      router.replace("/login");
    }
  }, [router]);

  // Parse bookId from path for nested nav
  const bookMatch = pathname?.match(/^\/books\/([^/]+)/);
  const bookId = bookMatch?.[1];

  const bookLinks = bookId
    ? [
        { href: `/books/${bookId}/documents`, label: "Documents" },
        { href: `/books/${bookId}/upload`, label: "Upload" },
        { href: `/books/${bookId}/review-queue`, label: "Review Queue" },
        { href: `/books/${bookId}/reports/${"latest"}`, label: "Reports" },
      ]
    : [];

  const adminLinks = [
    { href: "/admin/team", label: "Team" },
    { href: "/admin/settings", label: "Settings" },
  ];

  return (
    <div className="min-h-screen bg-slate-50">
      <div className="flex">
        <aside className="w-56 shrink-0 border-r border-slate-200 bg-white min-h-screen">
          <div className="px-4 py-4 border-b border-slate-200">
            <div className="font-semibold text-slate-900">AI Auditor</div>
            <div className="text-xs text-slate-500">Reconciliation</div>
          </div>
          <nav className="p-2 space-y-1">
            {NAV.map((item) => (
              <Link
                key={item.href}
                href={item.href}
                className={cn(
                  "flex items-center gap-2 rounded-md px-3 py-2 text-sm hover:bg-slate-100",
                  pathname?.startsWith(item.href) && "bg-slate-100 font-medium",
                )}
              >
                <span>{item.icon}</span> {item.label}
              </Link>
            ))}

            {bookLinks.length > 0 && (
              <>
                <div className="px-3 pt-4 pb-1 text-xs font-medium uppercase tracking-wider text-slate-400">
                  Client Book
                </div>
                {bookLinks.map((item) => (
                  <Link
                    key={item.label}
                    href={item.href}
                    className={cn(
                      "block rounded-md px-3 py-2 text-sm hover:bg-slate-100",
                      pathname?.includes(item.label.toLowerCase().replace(" ", "-")) &&
                        "bg-slate-100",
                    )}
                  >
                    {item.label}
                  </Link>
                ))}
              </>
            )}

            <div className="px-3 pt-4 pb-1 text-xs font-medium uppercase tracking-wider text-slate-400">
              Admin
            </div>
            {adminLinks.map((item) => (
              <Link
                key={item.label}
                href={item.href}
                className={cn(
                  "block rounded-md px-3 py-2 text-sm hover:bg-slate-100",
                  pathname?.includes(item.label.toLowerCase()) && "bg-slate-100",
                )}
              >
                {item.label}
              </Link>
            ))}
          </nav>
        </aside>

        <main className="flex-1 p-8">{children}</main>
      </div>

      <button
        onClick={() => {
          clearTokens();
          router.replace("/login");
        }}
        className="fixed bottom-4 right-4 rounded-md border border-slate-300 bg-white px-3 py-1.5 text-xs text-slate-600 shadow hover:bg-slate-50"
      >
        Sign out
      </button>
    </div>
  );
}
