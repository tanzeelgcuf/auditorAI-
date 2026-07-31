"use client";

import { useEffect, useState } from "react";
import { Shell } from "../../../components/layout/shell";
import { Card, CardContent, CardHeader, CardTitle, CardDescription } from "../../../components/ui/card";
import { Button } from "../../../components/ui/button";
import { Input } from "../../../components/ui/input";
import { useFirmSettings, useUpdateFirmSettings } from "../../../lib/hooks";

export default function SettingsPage() {
  const { data } = useFirmSettings();
  const update = useUpdateFirmSettings();

  const [color, setColor] = useState("#0F172A");
  const [footer, setFooter] = useState("");

  useEffect(() => {
    if (data) {
      setColor(data.brand_primary_color || "#0F172A");
      setFooter(data.report_footer_text || "");
    }
  }, [data]);

  const [saved, setSaved] = useState(false);

  async function handleSave(e: React.FormEvent) {
    e.preventDefault();
    await update.mutateAsync({ brand_primary_color: color, report_footer_text: footer });
    setSaved(true);
    setTimeout(() => setSaved(false), 2000);
  }

  return (
    <Shell>
      <h1 className="mb-6 text-2xl font-semibold text-slate-900">Firm Settings</h1>

      <div className="max-w-lg space-y-6">
        <Card>
          <CardHeader>
            <CardTitle>Branding</CardTitle>
            <CardDescription>
              Applied to generated audit report PDFs. The methodology disclosure always stays
              regardless of branding.
            </CardDescription>
          </CardHeader>
          <CardContent>
            <form onSubmit={handleSave} className="space-y-4">
              <div className="flex items-center gap-3">
                <label className="w-32 text-sm font-medium">Primary color</label>
                <input
                  type="color"
                  value={color}
                  onChange={(e) => setColor(e.target.value)}
                  className="h-9 w-14 cursor-pointer rounded border border-slate-300"
                />
                <span className="font-mono text-xs text-slate-500">{color}</span>
              </div>
              <div className="space-y-1">
                <label className="text-sm font-medium">Report footer text</label>
                <Input
                  value={footer}
                  onChange={(e) => setFooter(e.target.value)}
                  placeholder="Your firm's disclaimer / contact line"
                />
              </div>
              <Button type="submit" disabled={update.isPending}>
                {update.isPending ? "Saving…" : "Save"}
              </Button>
              {saved && <span className="ml-3 text-sm text-green-600">Saved ✓</span>}
            </form>
          </CardContent>
        </Card>

        <Card>
          <CardHeader>
            <CardTitle>Billing</CardTitle>
            <CardDescription>Managed via Stripe Checkout.</CardDescription>
          </CardHeader>
          <CardContent>
            <a href={`${process.env.NEXT_PUBLIC_API_URL || "http://localhost:8080"}/v1/billing/checkout`}>
              <Button variant="secondary">Manage subscription</Button>
            </a>
          </CardContent>
        </Card>
      </div>
    </Shell>
  );
}
