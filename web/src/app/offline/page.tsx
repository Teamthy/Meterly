"use client";

import React, { useState } from "react";
import { Smartphone, MessageSquare, RotateCcw, Send } from "lucide-react";

export default function OfflineChannelsSimulatorPage() {
  const [phone, setPhone] = useState("+2348031234567");

  // USSD Simulator state
  const [sessionId, setSessionId] = useState(() => `ussd-sim-${Date.now()}`);
  const [hops, setHops] = useState<string[]>([]);
  const [screen, setScreen] = useState<string>(
    "Press Dial *347*88# to launch a live Africa's Talking USSD session."
  );
  const [charCount, setCharCount] = useState<number>(0);
  const [ussdInput, setUssdInput] = useState("");
  const [ended, setEnded] = useState(true);
  const [busy, setBusy] = useState(false);

  // WhatsApp webhook state
  const [waInput, setWaInput] = useState("buy");
  const [waLog, setWaLog] = useState<Array<{ dir: "in" | "ack"; text: string }>>(
    []
  );

  const sendUssd = async (nextHops: string[], sid: string) => {
    setBusy(true);
    try {
      const res = await fetch("/api/dev/offline", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          channel: "USSD",
          sessionId: sid,
          serviceCode: "*347*88#",
          phoneNumber: phone.trim(),
          text: nextHops.join("*"),
        }),
      });
      const json = await res.json();
      const reply = String(json.reply || JSON.stringify(json));
      setScreen(reply);
      setCharCount(reply.length);
      setEnded(!reply.startsWith("CON "));
    } catch (e) {
      setScreen(e instanceof Error ? e.message : "USSD request failed");
      setEnded(true);
    } finally {
      setBusy(false);
    }
  };

  const handleDial = () => {
    const sid = `ussd-sim-${Date.now()}`;
    setSessionId(sid);
    setHops([]);
    setUssdInput("");
    sendUssd([], sid);
  };

  const handleReplyUssd = (e: React.FormEvent) => {
    e.preventDefault();
    if (!ussdInput.trim() || ended) return;
    const next = [...hops, ussdInput.trim()];
    setHops(next);
    setUssdInput("");
    sendUssd(next, sessionId);
  };

  const handleSendWhatsApp = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!waInput.trim()) return;
    const msg = waInput.trim();
    setWaInput("");
    setWaLog((prev) => [...prev, { dir: "in", text: msg }]);
    try {
      const res = await fetch("/api/dev/offline", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          channel: "WHATSAPP",
          phoneNumber: phone.trim(),
          text: msg,
        }),
      });
      const json = await res.json();
      setWaLog((prev) => [
        ...prev,
        {
          dir: "ack",
          text: `Webhook ${res.status}: ${JSON.stringify(
            json
          )} — conversational reply dispatched to WhatsApp sender`,
        },
      ]);
    } catch (err) {
      setWaLog((prev) => [
        ...prev,
        {
          dir: "ack",
          text: err instanceof Error ? err.message : "Webhook failed",
        },
      ]);
    }
  };

  return (
    <div className="space-y-6">
      <div className="rounded-2xl bg-[#0B1F1A] p-6 text-white">
        <span className="text-xs font-semibold uppercase tracking-wider text-emerald-300">
          F5 • Meterly Offline (USSD & WhatsApp Channels)
        </span>
        <h1 className="mt-1 text-2xl font-bold">
          Buy power on any feature phone (*347*88#) or WhatsApp chat.
        </h1>
        <p className="mt-1 max-w-2xl text-sm text-stone-300">
          Drives the exact same orchestrator state machine: CIDR + HMAC webhook auth, strict <code className="font-mono">&lt;= 182</code> character USSD frames, ~180s session TTL creating zero orphan transactions, SIM-swap OTP defense, and audited <code className="font-mono">&lt;= 3/day</code> Last Token resends.
        </p>
      </div>

      <div className="rounded-2xl border border-stone-200 bg-white p-4 flex flex-wrap items-center gap-3">
        <label className="text-xs font-semibold text-stone-700">
          Simulated Caller E.164 Phone:
        </label>
        <input
          type="tel"
          value={phone}
          onChange={(e) => setPhone(e.target.value)}
          className="rounded-xl border border-stone-300 px-3 py-1.5 font-mono text-xs"
        />
        <span className="text-xs text-stone-500">
          Tip: Link this phone via SMS OTP on the <strong>Buy Power</strong> tab to unlock verified daily caps & saved meters!
        </span>
      </div>

      <div className="grid grid-cols-1 gap-6 lg:grid-cols-2">
        {/* USSD Handset Simulator */}
        <section className="rounded-2xl border border-stone-200 bg-white p-5 shadow-xs space-y-4">
          <div className="flex items-center justify-between">
            <h2 className="text-base font-bold text-stone-900 flex items-center gap-2">
              <Smartphone className="h-4 w-4 text-emerald-600" />
              Africa&apos;s Talking USSD Simulator (*347*88#)
            </h2>
            <button
              type="button"
              onClick={handleDial}
              disabled={busy}
              className="inline-flex items-center gap-1.5 rounded-xl bg-emerald-600 px-3.5 py-1.5 text-xs font-semibold text-white hover:bg-emerald-700"
            >
              <RotateCcw className="h-3.5 w-3.5" /> Dial *347*88#
            </button>
          </div>

          <div className="mx-auto max-w-sm rounded-3xl border-4 border-stone-800 bg-stone-950 p-4 text-emerald-300 shadow-lg space-y-3">
            <div className="flex items-center justify-between text-[10px] font-mono text-stone-400 border-b border-stone-800 pb-2">
              <span>NG-TELCO GSM • {sessionId.slice(-8)}</span>
              <span
                className={
                  charCount > 182 ? "text-rose-400 font-bold" : "text-emerald-400"
                }
              >
                {charCount} / 182 chars
              </span>
            </div>

            <pre className="min-h-[150px] whitespace-pre-wrap font-mono text-xs leading-relaxed text-emerald-200">
              {screen}
            </pre>

            {!ended && (
              <form onSubmit={handleReplyUssd} className="flex gap-2 pt-2">
                <input
                  type="text"
                  value={ussdInput}
                  onChange={(e) => setUssdInput(e.target.value)}
                  placeholder="Enter option (e.g. 1)"
                  className="flex-1 rounded-lg bg-stone-900 border border-stone-700 px-2.5 py-1.5 font-mono text-xs text-white focus:border-emerald-400 focus:outline-none"
                />
                <button
                  type="submit"
                  disabled={busy}
                  className="rounded-lg bg-emerald-500 px-3 py-1.5 text-xs font-bold text-stone-950"
                >
                  Send
                </button>
              </form>
            )}
          </div>
        </section>

        {/* WhatsApp Cloud API Webhook Simulator */}
        <section className="rounded-2xl border border-stone-200 bg-white p-5 shadow-xs space-y-4">
          <h2 className="text-base font-bold text-stone-900 flex items-center gap-2">
            <MessageSquare className="h-4 w-4 text-emerald-600" />
            WhatsApp Cloud API Inbound Webhook (X-Hub-Signature-256)
          </h2>

          <div className="flex flex-wrap gap-1.5">
            {["hi", "buy", "1", "5000", "last token", "help", "STOP"].map(
              (kw) => (
                <button
                  key={kw}
                  type="button"
                  onClick={() => setWaInput(kw)}
                  className="rounded-lg border border-stone-200 bg-stone-50 px-2.5 py-1 font-mono text-xs text-stone-700 hover:bg-stone-100"
                >
                  {kw}
                </button>
              )
            )}
          </div>

          <div className="h-52 overflow-y-auto rounded-xl border border-stone-200 bg-stone-50 p-3 space-y-2 text-xs">
            {waLog.length === 0 ? (
              <p className="text-stone-400">
                Send an HMAC-signed WhatsApp message below (e.g. &ldquo;buy&rdquo; or &ldquo;last token&rdquo;) to exercise the conversational engine.
              </p>
            ) : (
              waLog.map((m, i) => (
                <div
                  key={i}
                  className={`rounded-lg px-3 py-2 ${
                    m.dir === "in"
                      ? "ml-8 bg-emerald-600 text-white"
                      : "mr-8 bg-white border border-stone-200 text-stone-700 font-mono text-[11px]"
                  }`}
                >
                  {m.text}
                </div>
              ))
            )}
          </div>

          <form onSubmit={handleSendWhatsApp} className="flex gap-2">
            <input
              type="text"
              value={waInput}
              onChange={(e) => setWaInput(e.target.value)}
              placeholder="Message ('buy', '1', '5000', 'last token', 'STOP')"
              className="flex-1 rounded-xl border border-stone-300 px-3 py-2 text-xs"
            />
            <button
              type="submit"
              className="inline-flex items-center gap-1.5 rounded-xl bg-[#0B1F1A] px-4 py-2 text-xs font-semibold text-white"
            >
              <Send className="h-3.5 w-3.5" /> Dispatch Signed Webhook
            </button>
          </form>
        </section>
      </div>
    </div>
  );
}
