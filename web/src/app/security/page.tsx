"use client";

import React, { useCallback, useEffect, useState } from "react";
import { useAuth } from "@/components/AuthContext";
import { apiFetch } from "@/lib/api";
import { Shield } from "lucide-react";

type Line = { label: string; value: string; emphasis?: boolean };
type SessionRow = {
  id: string;
  current: boolean;
  created_at: string;
  last_seen_at: string;
  expires_at: string;
  stepped_up: boolean;
};
type EventRow = {
  id: string;
  action: string;
  outcome: string;
  reason: string;
  created_at: string;
};
type Console = {
  title: string;
  subtitle: string;
  copy: string;
  lines: Line[];
  sessions: SessionRow[];
  events: EventRow[];
};

export default function SecurityPage() {
  const { token } = useAuth();
  const [screen, setScreen] = useState<Console | null>(null);
  const [banner, setBanner] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  const load = useCallback(async () => {
    try {
      const c = await apiFetch<Console>("/v1/security/console", { token });
      setScreen(c);
    } catch (e) {
      setBanner(e instanceof Error ? e.message : "Security console unavailable");
    }
  }, [token]);

  useEffect(() => {
    load();
  }, [load]);

  const revokeOthers = async () => {
    setBusy(true);
    setBanner(null);
    try {
      await apiFetch("/v1/security/sessions/revoke-others", { method: "POST", token, body: {} });
      setBanner("Other sessions signed out.");
      await load();
    } catch (e) {
      setBanner(e instanceof Error ? e.message : "Could not revoke sessions");
    } finally {
      setBusy(false);
    }
  };

  const revokeOne = async (id: string) => {
    setBusy(true);
    try {
      await apiFetch(`/v1/security/sessions/${id}/revoke`, { method: "POST", token, body: {} });
      await load();
    } catch (e) {
      setBanner(e instanceof Error ? e.message : "Could not revoke session");
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="space-y-6">
      <section className="ml-panel overflow-hidden p-6 sm:p-8">
        <span className="ml-chip">
          <Shield className="h-3 w-3" /> Security
        </span>
        <h1 className="font-display mt-4 max-w-xl text-4xl font-semibold leading-[1.05] text-paper sm:text-5xl">
          {screen?.title || "This device, this account."}
        </h1>
        <p className="mt-3 max-w-lg text-sm leading-relaxed text-white/55">
          {screen?.copy ||
            "Passkeys sign you in. Step-up unlocks tokens and saved cards. Events never include tokens or meter numbers."}
        </p>
      </section>

      {banner && (
        <div className="rounded-2xl border border-white/10 bg-white/5 px-4 py-3 text-sm text-paper">
          {banner}
        </div>
      )}

      <div className="grid gap-4 sm:grid-cols-3">
        {(screen?.lines || []).map((l) => (
          <div key={l.label} className="ml-card p-4">
            <p className="text-[11px] font-semibold uppercase tracking-[0.16em] text-stone-500">
              {l.label}
            </p>
            <p className={`mt-2 font-display text-2xl font-semibold ${l.emphasis ? "text-ink" : "text-ink"}`}>
              {l.value}
            </p>
          </div>
        ))}
      </div>

      <section className="ml-card p-5">
        <div className="mb-3 flex items-center justify-between gap-3">
          <h2 className="font-display text-lg font-semibold">Sessions</h2>
          <button
            type="button"
            disabled={busy}
            onClick={revokeOthers}
            className="rounded-full bg-ink px-4 py-2 text-xs font-semibold text-volt disabled:opacity-40"
          >
            Sign out other devices
          </button>
        </div>
        {(screen?.sessions || []).length === 0 ? (
          <p className="text-sm text-stone-500">
            Dev tokens have no passkey session. Sign in with a passkey to see devices here.
          </p>
        ) : (
          <ul className="space-y-2">
            {(screen?.sessions || []).map((s) => (
              <li
                key={s.id}
                className="flex flex-wrap items-center justify-between gap-2 rounded-xl border border-stone-200 px-3 py-2 text-sm"
              >
                <div>
                  <p className="font-medium">
                    {s.current ? "This device" : "Another device"}
                    {s.stepped_up ? " · stepped up" : ""}
                  </p>
                  <p className="font-mono text-[11px] text-stone-500">
                    since {new Date(s.created_at).toLocaleString("en-NG")}
                  </p>
                </div>
                {!s.current && (
                  <button
                    type="button"
                    disabled={busy}
                    onClick={() => revokeOne(s.id)}
                    className="text-xs font-medium text-rose-700"
                  >
                    Revoke
                  </button>
                )}
              </li>
            ))}
          </ul>
        )}
      </section>

      <section className="ml-card p-5">
        <h2 className="font-display text-lg font-semibold">Recent events</h2>
        {(screen?.events || []).length === 0 ? (
          <p className="mt-2 text-sm text-stone-500">Nothing recorded yet.</p>
        ) : (
          <ul className="mt-3 space-y-2">
            {(screen?.events || []).map((e) => (
              <li key={e.id} className="flex flex-wrap items-baseline justify-between gap-2 text-sm">
                <span>
                  <span className="font-mono text-xs">{e.action}</span>
                  <span className="ml-2 text-stone-500">{e.reason}</span>
                </span>
                <span className="font-mono text-[11px] text-stone-400">
                  {e.outcome} · {new Date(e.created_at).toLocaleString("en-NG")}
                </span>
              </li>
            ))}
          </ul>
        )}
      </section>
    </div>
  );
}
