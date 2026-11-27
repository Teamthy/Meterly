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
  newConvKey,
  saveConvKey,
  wrapConversationKey,
} from "@/lib/e2e";
import {
  Lock,
  MessageCircle,
  Shield,
  Sparkles,
  UserRound,
} from "lucide-react";

export default function SupportPage() {
  const { token } = useAuth();
  const [threads, setThreads] = useState<SupportThread[]>([]);
  const [active, setActive] = useState<SupportThread | null>(null);
  const [messages, setMessages] = useState<SupportMessage[]>([]);
  const [plain, setPlain] = useState<Record<string, string>>({});
  const [draft, setDraft] = useState("");
  const [me, setMe] = useState<SupportIdentityKey | null>(null);
  const [banner, setBanner] = useState<{ type: "ok" | "err"; text: string } | null>(null);
  const [busy, setBusy] = useState(false);
  const bottom = useRef<HTMLDivElement>(null);

  const boot = useCallback(async () => {
    const ident = await ensureIdentity(token);
    const key = await apiFetch<SupportIdentityKey>("/v1/support/keys", {
      method: "PUT",
      token,
      body: { public_key: ident.publicKeyB64, algorithm: "P-256" },
    });
    setMe(key);
    const list = await apiFetch<{ data: SupportThread[] }>("/v1/support/threads", { token });
    setThreads(list.data || []);
    if (!active && list.data?.length) setActive(list.data[0]);
  }, [token]); // eslint-disable-line react-hooks/exhaustive-deps

  useEffect(() => {
    boot().catch((e) =>
      setBanner({ type: "err", text: e instanceof Error ? e.message : "Support unavailable" })
    );
  }, [boot]);

  const loadMessages = useCallback(async (thread: SupportThread) => {
    const res = await apiFetch<{ data: SupportMessage[] }>(
      `/v1/support/threads/${thread.id}/messages`,
      { token }
    );
    const rows = res.data || [];
    setMessages(rows);
    const conv = loadConvKey(thread.id);
    if (!conv) return;
    const next: Record<string, string> = {};
    for (const m of rows) {
      if (m.e2e && m.ciphertext && m.nonce) {
        try {
          next[m.id] = await decryptMessage(conv, thread.id, m.ciphertext, m.nonce);
        } catch {
          next[m.id] = "(Could not decrypt on this device)";
        }
      }
    }
    setPlain(next);
  }, [token]);

  useEffect(() => {
    if (!active) return;
    loadMessages(active).catch(() => undefined);
    const t = setInterval(() => loadMessages(active).catch(() => undefined), 2500);
    return () => clearInterval(t);
  }, [active, loadMessages]);

  useEffect(() => {
    bottom.current?.scrollIntoView({ behavior: "smooth" });
  }, [messages, plain]);

  const startThread = async () => {
    setBusy(true);
    try {
      const created = await apiFetch<{ thread: SupportThread; messages: SupportMessage[] }>(
        "/v1/support/threads",
        { method: "POST", token, body: {} }
      );
      setThreads((prev) => [created.thread, ...prev]);
      setActive(created.thread);
      setMessages(created.messages || []);
      setPlain({});
    } catch (e) {
      setBanner({ type: "err", text: e instanceof Error ? e.message : "Could not start chat" });
    } finally {
      setBusy(false);
    }
  };

  const send = async () => {
    if (!active || !draft.trim()) return;
    setBusy(true);
    try {
      if (active.mode === "HUMAN") {
        const conv = loadConvKey(active.id);
        if (!conv) throw new Error("Encryption key missing on this device");
        const enc = await encryptMessage(conv, active.id, draft.trim());
        await apiFetch(`/v1/support/threads/${active.id}/messages`, {
          method: "POST",
          token,
          body: enc,
        });
      } else {
        const res = await apiFetch<{
          message: SupportMessage;
          agent_message?: SupportMessage;
          suggest_human?: boolean;
        }>(`/v1/support/threads/${active.id}/messages`, {
          method: "POST",
          token,
          body: { body: draft.trim() },
        });
        if (res.suggest_human) {
          setBanner({
            type: "ok",
            text: "I can connect you to a person — tap Talk to a person for an encrypted chat.",
          });
        }
      }
      setDraft("");
      await loadMessages(active);
      await boot();
    } catch (e) {
      setBanner({ type: "err", text: e instanceof Error ? e.message : "Send failed" });
    } finally {
      setBusy(false);
    }
  };

  const escalate = async () => {
    if (!active || !me) return;
    setBusy(true);
    try {
      const ident = await ensureIdentity(token);
      const conv = newConvKey();
      saveConvKey(active.id, conv);
      const roster = await apiFetch<{ data: SupportIdentityKey[] }>("/v1/support/roster", { token });
      const holders = [
        { user_id: me.user_id, public_key: ident.publicKeyB64 },
        ...(roster.data || []),
      ];
      const wrapped = [];
      for (const h of holders) {
        wrapped.push(
          await wrapConversationKey({
            identity: ident,
            threadId: active.id,
            convRaw: conv,
            holderUserId: h.user_id,
            holderPublicB64: h.public_key,
          })
        );
      }
      const res = await apiFetch<SupportThreadDetail>(`/v1/support/threads/${active.id}/escalate`, {
        method: "POST",
        token,
        body: { wrapped_keys: wrapped },
      });
      setActive(res.thread);
      setBanner({
        type: "ok",
        text: "This chat is now end-to-end encrypted. Meterly cannot read new messages.",
      });
      await loadMessages(res.thread);
      await boot();
    } catch (e) {
      setBanner({ type: "err", text: e instanceof Error ? e.message : "Could not reach a person" });
    } finally {
      setBusy(false);
    }
  };

  const display = (m: SupportMessage) => {
    if (m.e2e) return plain[m.id] || "Encrypted message";
    return m.body || "";
  };

  return (
    <div className="grid gap-4 lg:grid-cols-[260px_1fr]">
      <aside className="ml-card p-4">
        <div className="mb-3 flex items-center justify-between">
          <h1 className="text-sm font-semibold text-stone-800">Support</h1>
          <button
            type="button"
            onClick={startThread}
            disabled={busy}
            className="rounded-full bg-ink px-2.5 py-1 text-xs font-semibold text-volt disabled:opacity-50"
          >
            New chat
          </button>
        </div>
        <ul className="space-y-1">
          {threads.map((th) => (
            <li key={th.id}>
              <button
                type="button"
                onClick={() => setActive(th)}
                className={`w-full rounded-xl px-3 py-2 text-left text-xs ${
                  active?.id === th.id ? "bg-volt/20 text-ink" : "hover:bg-stone-50"
                }`}
              >
                <div className="font-medium">
                  {th.mode === "HUMAN" ? "Person · encrypted" : "Meterly Agent"}
                </div>
                <div className="text-[11px] text-stone-500">{th.status.replaceAll("_", " ")}</div>
              </button>
            </li>
          ))}
          {threads.length === 0 && (
            <p className="text-xs text-stone-500">No chats yet. Start one to ask the Meterly Agent or a person.</p>
          )}
        </ul>
      </aside>

      <section className="flex min-h-[520px] flex-col ml-card shadow-xs">
        <header className="flex flex-wrap items-center justify-between gap-2 border-b border-stone-100 px-5 py-3">
          <div>
            <div className="flex items-center gap-2 text-sm font-semibold">
              <MessageCircle className="h-4 w-4 text-ink" />
              {active?.mode === "HUMAN" ? "Chat with a person" : "Chat with Meterly Agent"}
            </div>
            <p className="mt-0.5 text-[11px] text-stone-500">
              {active?.mode === "HUMAN"
                ? "End-to-end encrypted. Keys stay on this device. Meterly cannot read these messages."
                : "The agent processes these messages to answer you. Not end-to-end encrypted."}
            </p>
          </div>
          {active && active.mode === "AGENT" && (
            <button
              type="button"
              onClick={escalate}
              disabled={busy}
              className="inline-flex items-center gap-1.5 rounded-lg border border-stone-200 bg-volt/20 px-3 py-1.5 text-xs font-semibold text-ink hover:bg-volt/20"
            >
              <Lock className="h-3.5 w-3.5" />
              Talk to a person
            </button>
          )}
        </header>

        {banner && (
          <div
            className={`mx-5 mt-3 rounded-lg px-3 py-2 text-xs ${
              banner.type === "ok" ? "bg-volt/20 text-ink" : "bg-rose-50 text-rose-800"
            }`}
          >
            {banner.text}
          </div>
        )}

        <div className="flex-1 space-y-3 overflow-y-auto px-5 py-4">
          {!active && (
            <div className="flex h-full flex-col items-center justify-center gap-2 text-center text-sm text-stone-500">
              <Sparkles className="h-6 w-6 text-volt" />
              Ask the agent about tokens, remote credit, Pay-for-Me or your purchases — or talk to a person.
            </div>
          )}
          {messages.map((m) => (
            <div
              key={m.id}
              className={`max-w-[85%] rounded-2xl px-3.5 py-2 text-sm leading-relaxed ${
                m.sender_kind === "CUSTOMER"
                  ? "ml-auto bg-ink text-volt"
                  : m.sender_kind === "SYSTEM"
                  ? "mx-auto bg-stone-100 text-stone-600 text-xs"
                  : "bg-stone-100 text-stone-800"
              }`}
            >
              <div className="mb-1 flex items-center gap-1 text-[10px] uppercase tracking-wide opacity-70">
                {m.sender_kind === "AGENT" && <Sparkles className="h-3 w-3" />}
                {m.sender_kind === "OPERATOR" && <UserRound className="h-3 w-3" />}
                {m.e2e && <Shield className="h-3 w-3" />}
                {m.sender_kind.toLowerCase()}
                {m.e2e ? " · e2e" : ""}
              </div>
              <div className="whitespace-pre-wrap">{display(m)}</div>
            </div>
          ))}
          <div ref={bottom} />
        </div>

        <form
          className="flex gap-2 border-t border-stone-100 p-3"
          onSubmit={(e) => {
            e.preventDefault();
            send();
          }}
        >
          <input
            value={draft}
            onChange={(e) => setDraft(e.target.value)}
            placeholder={
              active?.mode === "HUMAN"
                ? "Encrypted message to a person…"
                : "Ask the Meterly Agent…"
            }
            disabled={!active || busy}
            className="flex-1 rounded-xl border border-stone-200 px-3 py-2 text-sm outline-none focus:border-ink"
          />
          <button
            type="submit"
            disabled={!active || busy || !draft.trim()}
            className="rounded-full bg-ink px-4 py-2 text-sm font-semibold text-volt hover:bg-volt disabled:opacity-40"
          >
            Send
          </button>
        </form>
      </section>
    </div>
  );
}
