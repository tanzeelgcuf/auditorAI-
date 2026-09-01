"use client";

import { usePathname, useRouter } from "next/navigation";
import { useEffect } from "react";
import { getAccessToken, clearTokens } from "../../lib/api";
import { cn } from "../../lib/utils";
import {
  LayoutDashboard,
  FileText,
  Upload,
  Settings,
  Users,
  ClipboardList,
  BarChart3,
  LogOut,
} from "lucide-react";
import { MotionLink, MotionDiv, StaggerContainer } from "../../components/ui/motion";

const NAV = [
  { href: "/dashboard", label: "Dashboard", icon: LayoutDashboard },
];

const BOOK_NAV = [
  { href: "/books/:bookId/documents", label: "Documents", icon: FileText },
  { href: "/books/:bookId/upload", label: "Upload", icon: Upload },
  { href: "/books/:bookId/csv-mapping", label: "CSV Mapping", icon: Settings },
  { href: "/books/:bookId/review-queue", label: "Review Queue", icon: ClipboardList },
  { href: "/books/:bookId/reports/latest", label: "Reports", icon: BarChart3 },
];

const ADMIN_NAV = [
  { href: "/admin/team", label: "Team", icon: Users },
  { href: "/admin/settings", label: "Settings", icon: Settings },
];

export function Shell({ children }: { children: React.ReactNode }) {
  const pathname = usePathname();
  const router = useRouter();

  useEffect(() => {
    if (!getAccessToken()) {
      router.replace("/login");
    }
  }, [router]);

  const bookMatch = pathname?.match(/^\/books\/([^/]+)/);
  const bookId = bookMatch?.[1];

  const bookLinks = bookId
    ? BOOK_NAV.map((item) => ({
        ...item,
        href: item.href.replace(":bookId", bookId),
      }))
    : [];

  return (
    <div className="min-h-screen bg-background">
      <div className="flex">
        <aside className="w-64 shrink-0 border-r border-border bg-card min-h-screen">
          <div className="px-4 py-4 border-b border-border">
            <div className="font-semibold text-lg text-foreground">AI Auditor</div>
            <div className="text-xs text-muted-foreground">Reconciliation</div>
          </div>
          <nav className="p-2 space-y-1" role="navigation" aria-label="Main navigation">
            <StaggerContainer staggerChildren={0.04} staggerDelay={0.05}>
              {NAV.map((item) => (
                <MotionLink
                  key={item.href}
                  href={item.href}
                  variant="slideUp"
                  className={cn(
                    "flex items-center gap-3 rounded-md px-3 py-2.5 text-sm font-medium transition-colors",
                    pathname?.startsWith(item.href)
                      ? "bg-primary/10 text-primary"
                      : "text-muted-foreground hover:bg-accent/10 hover:text-foreground",
                  )}
                  whileTap={{ scale: 0.98 }}
                >
                  <item.icon className="h-5 w-5 flex-shrink-0" aria-hidden="true" />
                  <span>{item.label}</span>
                </MotionLink>
              ))}
            </StaggerContainer>

            {bookLinks.length > 0 && (
              <>
                <div className="px-3 pt-4 pb-1 text-xs font-medium uppercase tracking-wider text-muted-foreground">
                  Client Book
                </div>
                <StaggerContainer staggerChildren={0.04} staggerDelay={0.05}>
                  {bookLinks.map((item) => {
                    const isActive =
                      pathname === item.href ||
                      (item.label !== "Reports" && pathname?.startsWith(item.href.replace("/latest", "")));
                    return (
                      <MotionLink
                        key={item.label}
                        href={item.href}
                        variant="slideUp"
                        className={cn(
                          "flex items-center gap-3 rounded-md px-3 py-2 text-sm font-medium transition-colors",
                          isActive
                            ? "bg-primary/10 text-primary"
                            : "text-muted-foreground hover:bg-accent/10 hover:text-foreground",
                        )}
                        whileTap={{ scale: 0.98 }}
                      >
                        <item.icon className="h-4 w-4 flex-shrink-0" aria-hidden="true" />
                        <span>{item.label}</span>
                      </MotionLink>
                    );
                  })}
                </StaggerContainer>
              </>
            )}

            <div className="px-3 pt-4 pb-1 text-xs font-medium uppercase tracking-wider text-muted-foreground">
              Admin
            </div>
            <StaggerContainer staggerChildren={0.04} staggerDelay={0.05}>
              {ADMIN_NAV.map((item) => (
                <MotionLink
                  key={item.label}
                  href={item.href}
                  variant="slideUp"
                  className={cn(
                    "flex items-center gap-3 rounded-md px-3 py-2 text-sm font-medium transition-colors",
                    pathname?.startsWith(item.href)
                      ? "bg-primary/10 text-primary"
                      : "text-muted-foreground hover:bg-accent/10 hover:text-foreground",
                  )}
                  whileTap={{ scale: 0.98 }}
                >
                  <item.icon className="h-4 w-4 flex-shrink-0" aria-hidden="true" />
                  <span>{item.label}</span>
                </MotionLink>
              ))}
            </StaggerContainer>
          </nav>

          <div className="absolute bottom-0 left-0 right-0 p-4 border-t border-border">
            <button
              onClick={() => {
                clearTokens();
                router.replace("/login");
              }}
              className={cn(
                "flex w-full items-center gap-3 rounded-md px-3 py-2 text-sm font-medium text-muted-foreground transition-colors hover:bg-accent/10 hover:text-foreground",
              )}
            >
              <LogOut className="h-5 w-5 flex-shrink-0" aria-hidden="true" />
              <span>Sign out</span>
            </button>
          </div>
        </aside>

        <main className="flex-1 p-6 md:p-8 lg:p-10" role="main">
          <StaggerContainer staggerChildren={0.05} staggerDelay={0.1}>
            <MotionDiv variant="slideUp">
              {children}
            </MotionDiv>
          </StaggerContainer>
        </main>
      </div>
    </div>
  );
}