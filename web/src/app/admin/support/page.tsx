"use client";

import React, { useCallback, useEffect, useRef, useState } from "react";
import { useAuth } from "@/components/AuthContext";
import { apiFetch } from "@/lib/api";
import {
  SupportIdentityKey,
  SupportMessage,
  SupportThread,
  SupportThreadDetail,
} from "@/lib/types";
import {
  decryptMessage,
  encryptMessage,
  ensureIdentity,
  loadConvKey,
  saveConvKey,
  unwrapConversationKey,
} from "@/lib/e2e";
import { Headphones, Lock, ShieldAlert } from "lucide-react";

export default function AdminSupportPage() {
  const { token, setPersonaById } = useAuth();
  const isStaff = token.startsWith("operator:") || token.startsWith("admin:");
  const [inbox, setInbox] = useState<SupportThread[]>([]);
  const [active, setActive] = useState<SupportThread | null>(null);
  const [detail, setDetail] = useState<SupportThreadDetail | null>(null);
  const [messages, setMessages] = useState<SupportMessage[]>([]);
  const [plain, setPlain] = useState<Record<string, string>>({});
  const [draft, setDraft] = useState("");
  const [reason, setReason] = useState("Customer requested a human operator");
  const [banner, setBanner] = useState<string | null>(null);
  const [e2eReady, setE2eReady] = useState(false);
  const bottom = useRef<HTMLDivElement>(null);

  const bootKeys = useCallback(async () => {
    const ident = await ensureIdentity(token);
    await apiFetch<SupportIdentityKey>("/v1/support/keys", {
      method: "PUT",
      token,
      body: { public_key: ident.publicKeyB64, algorithm: "P-256" },
    });
  }, [token]);

  const loadInbox = useCallback(async () => {
    if (!isStaff) return;
    const res = await apiFetch<{ data: SupportThread[] }>("/v1/admin/support/threads", { token });
    setInbox(res.data || []);
  }, [isStaff, token]);

  useEffect(() => {
    if (!isStaff) return;
    bootKeys().then(loadInbox).catch((e) => setBanner(e instanceof Error ? e.message : "Inbox failed"));
  }, [isStaff, bootKeys, loadInbox]);

  const openThread = async (th: SupportThread) => {
    setActive(th);
    setBanner(null);
    const d = await apiFetch<SupportThreadDetail>(`/v1/admin/support/threads/${th.id}`, { token });
    setDetail(d);
    let conv = loadConvKey(th.id);
    if (!conv && d.wrapped_keys?.[0] && d.customer_public_key) {
      try {
        const ident = await ensureIdentity(token);
        conv = await unwrapConversationKey({
          identity: ident,
          threadId: th.id,
          peerPublicB64: d.customer_public_key,
          wrappedKeyB64: d.wrapped_keys[0].wrapped_key,
          nonceB64: d.wrapped_keys[0].nonce,
        });
        saveConvKey(th.id, conv);
      } catch {
        conv = null;
      }
    }
    setE2eReady(!!conv);
    const res = await apiFetch<{ data: SupportMessage[] }>(
      `/v1/admin/support/threads/${th.id}/messages`,
      { token }
    );
    const rows = res.data || [];
    setMessages(rows);
    if (conv) {
      const next: Record<string, string> = {};
      for (const m of rows) {
        if (m.e2e && m.ciphertext && m.nonce) {
          try {
            next[m.id] = await decryptMessage(conv, th.id, m.ciphertext, m.nonce);
          } catch {
            next[m.id] = "(undecryptable on this device)";
          }
        }
      }
      setPlain(next);
    }
  };

  useEffect(() => {
    if (!active || !isStaff) return;
    const t = setInterval(() => openThread(active).catch(() => undefined), 3000);
    return () => clearInterval(t);
  }, [active, isStaff]); // eslint-disable-line react-hooks/exhaustive-deps

  useEffect(() => {
    bottom.current?.scrollIntoView({ behavior: "smooth" });
  }, [messages, plain]);

  const claim = async () => {
    if (!active) return;
    await apiFetch(`/v1/admin/support/threads/${active.id}/claim`, {
      method: "POST",
      token,
      body: { reason },
    });
    await loadInbox();
    await openThread(active);
  };

  const resolve = async () => {
    if (!active) return;
    await apiFetch(`/v1/admin/support/threads/${active.id}/resolve`, {
      method: "POST",
      token,
      body: { reason },
    });
    await loadInbox();
  };

  const send = async () => {
    if (!active || !draft.trim()) return;
    const conv = loadConvKey(active.id);
    if (!conv) {
      setBanner("Waiting for the customer's encryption handshake.");
      return;
    }
    const enc = await encryptMessage(conv, active.id, draft.trim());
    await apiFetch(`/v1/admin/support/threads/${active.id}/messages`, {
      method: "POST",
      token,
      body: enc,
    });
    setDraft("");
    await openThread(active);
  };

  if (!isStaff) {
    return (
      <div className="rounded-2xl border border-amber-200 bg-amber-50 p-6 text-sm text-amber-900">
        <p className="mb-3 font-semibold">Staff only</p>
        <p className="mb-4">Switch to an operator or admin persona to open the support inbox.</p>
        <button
          type="button"
          onClick={() => setPersonaById("operator:kemi")}
          className="rounded-lg bg-emerald-600 px-3 py-1.5 text-xs font-semibold text-white"
        >
          Use operator:kemi
        </button>
      </div>
    );
  }

  return (
    <div className="grid gap-4 lg:grid-cols-[280px_1fr]">
      <aside className="rounded-2xl border border-stone-200 bg-white p-4">
        <h1 className="mb-3 flex items-center gap-2 text-sm font-semibold">
          <Headphones className="h-4 w-4 text-emerald-600" />
          Support inbox
        </h1>
        <ul className="space-y-1">
          {inbox.map((th) => (
            <li key={th.id}>
              <button
                type="button"
                onClick={() => openThread(th)}
                className={`w-full rounded-xl px-3 py-2 text-left text-xs ${
                  active?.id === th.id ? "bg-emerald-50" : "hover:bg-stone-50"
                }`}
              >
                <div className="font-medium">{th.status.replaceAll("_", " ")}</div>
                <div className="font-mono text-[10px] text-stone-500">{th.customer_id.slice(0, 8)}</div>
              </button>
            </li>
          ))}
          {inbox.length === 0 && <p className="text-xs text-stone-500">No threads yet.</p>}
        </ul>
      </aside>

      <section className="flex min-h-[520px] flex-col rounded-2xl border border-stone-200 bg-white">
        <header className="flex flex-wrap items-center justify-between gap-2 border-b border-stone-100 px-5 py-3">
          <div>
            <div className="text-sm font-semibold">
              {active ? `Thread ${active.id.slice(0, 8)}` : "Select a thread"}
            </div>
            <p className="text-[11px] text-stone-500">
              Agent history is visible (processed by Meterly). Human messages stay end-to-end encrypted.
            </p>
          </div>
          {active && (
            <div className="flex gap-2">
              {active.status === "WAITING_HUMAN" && (
                <button
                  type="button"
                  onClick={claim}
                  className="rounded-lg bg-emerald-600 px-3 py-1.5 text-xs font-semibold text-white"
                >
                  Claim
                </button>
              )}
              <button
                type="button"
                onClick={resolve}
                className="rounded-lg border border-stone-200 px-3 py-1.5 text-xs font-medium"
              >
                Resolve
              </button>
            </div>
          )}
        </header>
        {banner && <div className="mx-5 mt-3 rounded-lg bg-amber-50 px-3 py-2 text-xs text-amber-900">{banner}</div>}
        <div className="flex-1 space-y-3 overflow-y-auto px-5 py-4">
          {messages.map((m) => (
            <div
              key={m.id}
              className={`max-w-[85%] rounded-2xl px-3.5 py-2 text-sm ${
                m.sender_kind === "OPERATOR" ? "ml-auto bg-emerald-600 text-white" : "bg-stone-100"
              }`}
            >
              <div className="mb-1 text-[10px] uppercase opacity-70">
                {m.sender_kind}
                {m.e2e ? " · e2e" : " · agent/system"}
              </div>
              <div className="whitespace-pre-wrap">
                {m.e2e ? plain[m.id] || (e2eReady ? "…" : "Encrypted — waiting for key") : m.body}
              </div>
            </div>
          ))}
          <div ref={bottom} />
        </div>
        <form
          className="flex flex-col gap-2 border-t border-stone-100 p-3"
          onSubmit={(e) => {
            e.preventDefault();
            send();
          }}
        >
          <input
            value={reason}
            onChange={(e) => setReason(e.target.value)}
            className="rounded-lg border border-stone-200 px-3 py-1.5 text-xs"
            placeholder="Audit reason for claim/resolve"
          />
          <div className="flex gap-2">
            <input
              value={draft}
              onChange={(e) => setDraft(e.target.value)}
              placeholder={e2eReady ? "Encrypted reply…" : "Claim the thread and wait for the handshake"}
              className="flex-1 rounded-xl border border-stone-200 px-3 py-2 text-sm"
            />
            <button
              type="submit"
              disabled={!e2eReady || !draft.trim()}
              className="inline-flex items-center gap-1 rounded-xl bg-emerald-600 px-4 py-2 text-sm font-semibold text-white disabled:opacity-40"
            >
              <Lock className="h-3.5 w-3.5" />
              Send
            </button>
          </div>
        </form>
        {active && !e2eReady && active.mode === "HUMAN" && (
          <p className="flex items-center gap-1 px-4 pb-3 text-[11px] text-stone-500">
            <ShieldAlert className="h-3.5 w-3.5" />
            No conversation key for this device yet. The customer wrap is created when they tap Talk to a person.
          </p>
        )}
        {detail?.me && (
          <p className="px-4 pb-3 font-mono text-[10px] text-stone-400">operator {detail.me.slice(0, 8)}</p>
        )}
      </section>
    </div>
  );
}
