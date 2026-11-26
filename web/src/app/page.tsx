"use client";

import React, { useCallback, useEffect, useState } from "react";
import { useAuth } from "@/components/AuthContext";
import {
  apiFetch,
  DISCOS,
  extractQuoteFeeMinor,
  extractQuoteId,
  extractQuoteTotalMinor,
  extractQuoteUnits,
  formatNaira,
  formatSTSToken,
  newIdempotencyKey,
} from "@/lib/api";
import {
  Meter,
  MeterValidationResult,
  NotificationPreferences,
  TokenRevealResponse,
  TokenShare,
  TransactionView,
} from "@/lib/types";
import {
  CapabilityBadges,
  StatusPill,
  ThreeDimensionStatus,
} from "@/components/StatusBadges";
import {
  Zap,
  Plus,
  Copy,
  Check,
  Gift,
  FileText,
  CreditCard,
  Trash2,
  RefreshCw,
  Bell,
  Phone,
  Radio,
} from "lucide-react";

const MOCK_PRESETS = [
  {
    label: "Smart meter (credit on meter)",
    meter: "70011112222",
    disco: "IKEDC",
    hint: "Remote push → ACCEPTED → CONFIRMED when the meter acknowledges. No keypad.",
  },
  {
    label: "Keypad STS (token fallback)",
    meter: "45011112222",
    disco: "EKEDC",
    hint: "No AMI path — 20-digit STS token you type on the meter",
  },
  {
    label: "Vend fails → refund",
    meter: "45011110000",
    disco: "AEDC",
    hint: "Definitive vend failure triggers automatic PSP refund",
  },
  {
    label: "Timeout → UNKNOWN",
    meter: "45011119999",
    disco: "IBEDC",
    hint: "Lost vend response → UNKNOWN → reconcile, never blind retry",
  },
];

export default function ConsumerHubPage() {
  const { token, email, openCheckout } = useAuth();

  const [meters, setMeters] = useState<Meter[]>([]);
  const [txns, setTxns] = useState<TransactionView[]>([]);
  const [prefs, setPrefs] = useState<NotificationPreferences | null>(null);
  const [loading, setLoading] = useState(true);
  const [banner, setBanner] = useState<{ type: "ok" | "err"; text: string } | null>(
    null
  );

  // Meter registration state
  const [meterNum, setMeterNum] = useState("70011112222");
  const [disco, setDisco] = useState("IKEDC");
  const [meterLabel, setMeterLabel] = useState("Home smart meter");
  const [isBeneficiary, setIsBeneficiary] = useState(false);
  const [validation, setValidation] = useState<MeterValidationResult | null>(null);
  const [validating, setValidating] = useState(false);

  // Purchase flow state
  const [selectedMeterId, setSelectedMeterId] = useState<string>("");
  const [amountNaira, setAmountNaira] = useState<string>("5000");
  const [deliveryPref, setDeliveryPref] = useState<"AUTO" | "TOKEN" | "REMOTE">("AUTO");
  const [buyerEmail, setBuyerEmail] = useState<string>(email);
  const [isGift, setIsGift] = useState(false);
  const [recipientName, setRecipientName] = useState("");
  const [recipientPhone, setRecipientPhone] = useState("");
  const [notifyRecipient, setNotifyRecipient] = useState(true);
  const [purchasing, setPurchasing] = useState(false);

  // Token reveal & save card state
  const [revealedTokens, setRevealedTokens] = useState<
    Record<string, TokenRevealResponse>
  >({});
  const [copiedId, setCopiedId] = useState<string | null>(null);
  const [tokenShares, setTokenShares] = useState<Record<string, TokenShare[]>>({});
  const [sendDest, setSendDest] = useState<Record<string, string>>({});
  const [sendBusy, setSendBusy] = useState<string | null>(null);
  const [saveCardTxnId, setSaveCardTxnId] = useState<string | null>(null);
  const [cardConsent, setCardConsent] = useState(false);

  // Phone link OTP state (M11)
  const [linkPhone, setLinkPhone] = useState("+2348031234567");
  const [otpCode, setOtpCode] = useState("");
  const [otpSentMasked, setOtpSentMasked] = useState<string | null>(null);

  useEffect(() => {
    setBuyerEmail(email);
  }, [email]);

  const loadAll = useCallback(async () => {
    setLoading(true);
    try {
      const [mRes, tRes, pRes] = await Promise.all([
        apiFetch<{ data: Meter[] }>("/v1/meters", { token }),
        apiFetch<{ data: TransactionView[] }>("/v1/electricity/transactions", {
          token,
        }),
        apiFetch<NotificationPreferences>("/v1/me/notification-preferences", {
          token,
        }).catch(() => null),
      ]);
      const meterList = mRes.data || [];
      setMeters(meterList);
      setTxns(tRes.data || []);
      if (pRes) setPrefs(pRes);
      if (meterList.length > 0) {
        setSelectedMeterId((prev) =>
          meterList.some((m) => m.meter_id === prev) ? prev : meterList[0].meter_id
        );
      } else {
        setSelectedMeterId("");
      }
    } catch (e) {
      setBanner({
        type: "err",
        text: e instanceof Error ? e.message : "Failed to load account data",
      });
    } finally {
      setLoading(false);
    }
  }, [token]);

  useEffect(() => {
    setRevealedTokens({});
    loadAll();
  }, [loadAll]);

  const refreshTxns = useCallback(async () => {
    try {
      const tRes = await apiFetch<{ data: TransactionView[] }>(
        "/v1/electricity/transactions",
        { token }
      );
      setTxns(tRes.data || []);
    } catch {
      // poll: keep the last good list
    }
  }, [token]);

  useEffect(() => {
    const waiting = txns.some(
      (t) =>
        t.delivery_method === "REMOTE" && t.delivery_status === "ACCEPTED"
    );
    if (!waiting) return;
    const id = window.setInterval(() => {
      void refreshTxns();
    }, 4000);
    return () => window.clearInterval(id);
  }, [txns, refreshTxns]);

  const selectedMeter = meters.find((m) => m.meter_id === selectedMeterId);
  const selectedSupportsRemote =
    selectedMeter?.capabilities?.remote_credit?.supported &&
    selectedMeter?.capabilities?.remote_credit?.authorised &&
    selectedMeter?.capabilities?.remote_credit?.reachable !== false;

  useEffect(() => {
    // Selling point: prefer credit-on-meter. AUTO picks REMOTE when the meter
    // can take it, otherwise STS token. Never force remote on a keypad meter.
    setDeliveryPref(selectedSupportsRemote ? "AUTO" : "TOKEN");
  }, [selectedSupportsRemote, selectedMeterId]);

  const handleValidateMeter = async () => {
    setValidating(true);
    setBanner(null);
    try {
      const v = await apiFetch<MeterValidationResult>("/v1/meters/validate", {
        method: "POST",
        token,
        body: { meter_number: meterNum.trim(), disco_code: disco },
      });
      setValidation(v);
    } catch (e) {
      setValidation(null);
      setBanner({
        type: "err",
        text: e instanceof Error ? e.message : "Meter validation failed",
      });
    } finally {
      setValidating(false);
    }
  };

  const handleSaveMeter = async (e: React.FormEvent) => {
    e.preventDefault();
    setBanner(null);
    try {
      if (isBeneficiary) {
        const saved = await apiFetch<{ meter_id: string }>("/v1/beneficiaries", {
          method: "POST",
          token,
          body: {
            meter_number: meterNum.trim(),
            disco_code: disco,
            display_name: meterLabel.trim() || "Family Meter",
          },
        });
        await loadAll();
        setSelectedMeterId(saved.meter_id);
      } else {
        const saved = await apiFetch<Meter>("/v1/meters", {
          method: "POST",
          token,
          body: {
            meter_number: meterNum.trim(),
            disco_code: disco,
            label: meterLabel.trim() || "My Meter",
          },
        });
        await loadAll();
        setSelectedMeterId(saved.meter_id);
      }
      setValidation(null);
      setBanner({
        type: "ok",
        text: "Meter verified and saved with PURCHASE access.",
      });
    } catch (err) {
      setBanner({
        type: "err",
        text: err instanceof Error ? err.message : "Could not save meter",
      });
    }
  };

  const handleRemoveMeter = async (id: string) => {
    try {
      await apiFetch(`/v1/meters/${id}`, { method: "DELETE", token });
      setBanner({
        type: "ok",
        text: "Removed saved meter access (active AutoPower rules for this meter are automatically paused).",
      });
      await loadAll();
    } catch (err) {
      setBanner({
        type: "err",
        text: err instanceof Error ? err.message : "Could not remove meter",
      });
    }
  };

  const handleBuyElectricity = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!selectedMeterId) {
      setBanner({ type: "err", text: "Save and select a meter first." });
      return;
    }
    const naira = Number(amountNaira);
    if (!Number.isFinite(naira) || naira <= 0) {
      setBanner({ type: "err", text: "Enter a valid Naira amount." });
      return;
    }
    setPurchasing(true);
    setBanner(null);
    try {
      const amountMinor = Math.round(naira * 100);
      const quote = await apiFetch<Record<string, unknown>>(
        "/v1/electricity/quotes",
        {
          method: "POST",
          token,
          body: {
            meter_id: selectedMeterId,
            amount_minor: amountMinor,
            currency: "NGN",
            delivery_preference: deliveryPref,
          },
        }
      );
      const quoteId = extractQuoteId(quote);
      const totalMinor = extractQuoteTotalMinor(quote);
      const feeMinor = extractQuoteFeeMinor(quote);
      const estUnits = extractQuoteUnits(quote);

      const txn = await apiFetch<TransactionView>(
        "/v1/electricity/transactions",
        {
          method: "POST",
          token,
          idempotencyKey: newIdempotencyKey("buy"),
          body: {
            quote_id: quoteId,
            email: buyerEmail.trim(),
            is_gift: isGift,
            recipient_name: isGift ? recipientName.trim() : "",
            recipient_phone: isGift ? recipientPhone.trim() : "",
            notify_recipient: isGift ? notifyRecipient : false,
          },
        }
      );
      await loadAll();
      if (txn.checkout_url) {
        openCheckout({
          checkoutUrl: txn.checkout_url,
          totalMinor: txn.total_minor || totalMinor,
          title: `Buy Electricity • ${selectedMeter?.masked_meter_number || ""}`,
          subtitle: `Includes ${formatNaira(feeMinor)} platform fee • Est. ${estUnits.toFixed(
            1
          )} kWh (${txn.delivery_method})`,
          onComplete: async () => {
            await loadAll();
            setBanner({
              type: "ok",
              text:
                txn.delivery_method === "REMOTE"
                  ? "Payment verified. Credit is being pushed to the meter — ACCEPTED is not delivered until the meter confirms."
                  : "Payment verified server-side. Reveal the STS token when vending is CONFIRMED.",
            });
          },
        });
      }
    } catch (err) {
      setBanner({
        type: "err",
        text: err instanceof Error ? err.message : "Purchase creation failed",
      });
    } finally {
      setPurchasing(false);
    }
  };

  const loadTokenShares = async (txnId: string) => {
    try {
      const res = await apiFetch<{ data: TokenShare[] }>(
        `/v1/electricity/transactions/${txnId}/token/shares`,
        { token }
      );
      setTokenShares((prev) => ({ ...prev, [txnId]: res.data || [] }));
    } catch {
      // listing is optional
    }
  };

  const handleRevealToken = async (txnId: string) => {
    try {
      const res = await apiFetch<TokenRevealResponse>(
        `/v1/electricity/transactions/${txnId}/token`,
        { token }
      );
      setRevealedTokens((prev) => ({ ...prev, [txnId]: res }));
      await loadTokenShares(txnId);
    } catch (err) {
      setBanner({
        type: "err",
        text: err instanceof Error ? err.message : "Token not available",
      });
    }
  };

  const handleSendToken = async (
    txnId: string,
    channel: "SMS" | "EMAIL" | "WHATSAPP"
  ) => {
    const dest = (sendDest[txnId] || "").trim();
    setSendBusy(`${txnId}:${channel}`);
    try {
      await apiFetch(`/v1/electricity/transactions/${txnId}/token/shares`, {
        method: "POST",
        token,
        idempotencyKey: newIdempotencyKey("send"),
        body: dest ? { channel, destination: dest } : { channel },
      });
      await loadTokenShares(txnId);
      setBanner({
        type: "ok",
        text: dest
          ? `Third-party ${channel} send accepted (requires FEATURE_GIFT_SMS in prod).`
          : `Token queued to your verified ${channel}. Delivery is tracked separately.`,
      });
    } catch (err) {
      setBanner({
        type: "err",
        text: err instanceof Error ? err.message : "Send failed",
      });
    } finally {
      setSendBusy(null);
    }
  };

  const handleCopyToken = async (txnId: string, rawToken: string) => {
    const formatted = formatSTSToken(rawToken);
    try {
      await navigator.clipboard.writeText(formatted);
    } catch {
      // ignore clipboard restrictions
    }
    setCopiedId(txnId);
    setTimeout(() => setCopiedId(null), 2000);
    // Record audited COPY share event (FR-007)
    await apiFetch(`/v1/electricity/transactions/${txnId}/token/shares`, {
      method: "POST",
      token,
      idempotencyKey: newIdempotencyKey("copy"),
      body: { channel: "COPY" },
    }).catch(() => null);
  };

  const handleSettleRemoteDev = async (txnId: string) => {
    try {
      await apiFetch("/v1/dev/remote/settle", { method: "POST" });
      await apiFetch(`/v1/dev/transactions/${txnId}/resume`, {
        method: "POST",
      });
      await loadAll();
      setBanner({
        type: "ok",
        text: "Smart meter acknowledged credit — remote delivery is now CONFIRMED.",
      });
    } catch (err) {
      setBanner({
        type: "err",
        text: err instanceof Error ? err.message : "Remote settle failed",
      });
    }
  };

  const handleResumeDev = async (txnId: string) => {
    try {
      await apiFetch(`/v1/dev/transactions/${txnId}/resume`, {
        method: "POST",
      });
      await loadAll();
      setBanner({
        type: "ok",
        text: "Worker recovery sweep executed (queried provider status before any retry).",
      });
    } catch (err) {
      setBanner({
        type: "err",
        text: err instanceof Error ? err.message : "Resume failed",
      });
    }
  };

  const handleOpenHTMLReceipt = async (txnId: string) => {
    try {
      const html = await apiFetch<string>(
        `/v1/electricity/transactions/${txnId}/receipt?format=html`,
        { token }
      );
      const blob = new Blob([html], { type: "text/html" });
      const url = URL.createObjectURL(blob);
      window.open(url, "_blank", "noopener,noreferrer");
    } catch (err) {
      setBanner({
        type: "err",
        text: err instanceof Error ? err.message : "Could not load receipt",
      });
    }
  };

  const handleSaveCard = async (txnId: string) => {
    if (!cardConsent) {
      setBanner({
        type: "err",
        text: "Explicit consent is required to save a card for AutoPower.",
      });
      return;
    }
    try {
      await apiFetch(`/v1/me/payment-methods/${txnId}/save`, {
        method: "POST",
        token,
        body: {
          consent: true,
          consent_text:
            "I authorise Meterly to charge this saved card for scheduled or low-balance AutoPower rules within my configured spend caps.",
        },
      });
      setSaveCardTxnId(null);
      setCardConsent(false);
      setBanner({
        type: "ok",
        text: "Card mandate encrypted & saved! Configure schedules on the AutoPower tab.",
      });
    } catch (err) {
      setBanner({
        type: "err",
        text:
          err instanceof Error
            ? err.message
            : "Could not save card (ensure FEATURE_AUTOPOWER=true)",
      });
    }
  };

  const handleTogglePref = async (key: keyof NotificationPreferences) => {
    if (!prefs) return;
    const next = {
      sms: prefs.sms,
      email: prefs.email,
      whatsapp: prefs.whatsapp,
      [key]: !prefs[key],
    };
    try {
      const updated = await apiFetch<NotificationPreferences>(
        "/v1/me/notification-preferences",
        {
          method: "PUT",
          token,
          body: next,
        }
      );
      setPrefs(updated);
    } catch (err) {
      setBanner({
        type: "err",
        text: err instanceof Error ? err.message : "Failed to update preferences",
      });
    }
  };

  const handleStartPhoneLink = async () => {
    try {
      const res = await apiFetch<{ status: string; phone_masked: string }>(
        "/v1/channels/phone/link/start",
        {
          method: "POST",
          token,
          body: { phone: linkPhone.trim() },
        }
      );
      setOtpSentMasked(res.phone_masked);
      setBanner({
        type: "ok",
        text: `6-digit SMS verification code sent to ${res.phone_masked}.`,
      });
    } catch (err) {
      setBanner({
        type: "err",
        text: err instanceof Error ? err.message : "Phone link start failed",
      });
    }
  };

  const handleVerifyPhoneLink = async () => {
    try {
      const res = await apiFetch<{ status: string; phone_masked: string }>(
        "/v1/channels/phone/link/verify",
        {
          method: "POST",
          token,
          body: { phone: linkPhone.trim(), code: otpCode.trim() },
        }
      );
      setOtpSentMasked(null);
      setOtpCode("");
      setBanner({
        type: "ok",
        text: `Phone ${res.phone_masked} linked for USSD (*347*88#) & WhatsApp purchases!`,
      });
    } catch (err) {
      setBanner({
        type: "err",
        text: err instanceof Error ? err.message : "OTP verification failed",
      });
    }
  };

  return (
    <div className="space-y-6">
      {/* Hero Banner */}
      <div className="rounded-2xl bg-gradient-to-r from-[#0B1F1A] via-[#113128] to-[#163D32] p-6 text-white shadow-sm">
        <div className="flex flex-wrap items-center justify-between gap-4">
          <div className="max-w-2xl space-y-1.5">
            <span className="inline-flex items-center gap-1.5 rounded-full bg-emerald-400/15 px-3 py-0.5 text-xs font-medium text-emerald-300 border border-emerald-400/25">
              <Radio className="h-3 w-3" /> Credit on the meter — not a code to type
            </span>
            <h1 className="text-2xl font-bold tracking-tight sm:text-3xl">
              Units land on the meter.
            </h1>
            <p className="text-sm text-stone-300 leading-relaxed">
              On a compatible smart meter we push credit remotely. You do not climb a ladder or punch 20 digits.
              We only say it is delivered when the meter confirms — network &quot;accepted&quot; is not enough.
              Keypad meters still get an STS token as fallback.
            </p>
          </div>
          <button
            type="button"
            onClick={loadAll}
            className="inline-flex items-center gap-2 rounded-xl bg-white/10 px-3.5 py-2 text-xs font-semibold text-white border border-white/15 hover:bg-white/15"
          >
            <RefreshCw className={`h-3.5 w-3.5 ${loading ? "animate-spin" : ""}`} />
            Refresh State
          </button>
        </div>
      </div>

      {banner && (
        <div
          className={`flex items-center justify-between rounded-xl border px-4 py-3 text-sm ${
            banner.type === "ok"
              ? "border-emerald-200 bg-emerald-50 text-emerald-900"
              : "border-rose-200 bg-rose-50 text-rose-900"
          }`}
        >
          <span>{banner.text}</span>
          <button
            type="button"
            onClick={() => setBanner(null)}
            className="text-xs font-semibold underline opacity-75 hover:opacity-100"
          >
            Dismiss
          </button>
        </div>
      )}

      <div className="grid grid-cols-1 gap-6 lg:grid-cols-12">
        {/* Left Column: Add Meter + Buy Electricity */}
        <div className="space-y-6 lg:col-span-5">
          {/* Register Meter Card */}
          <section className="rounded-2xl border border-stone-200 bg-white p-5 shadow-xs">
            <div className="flex items-center justify-between mb-3">
              <h2 className="text-base font-bold text-stone-900 flex items-center gap-2">
                <Plus className="h-4 w-4 text-emerald-600" />
                Add a meter
              </h2>
              <span className="text-[11px] font-medium text-stone-500">
                Grants PURCHASE only
              </span>
            </div>

            {/* Sandbox presets */}
            <div className="mb-4 space-y-1.5">
              <p className="text-[11px] font-semibold uppercase tracking-wider text-stone-500">
                Try it: smart meter first (sandbox)
              </p>
              <div className="grid grid-cols-2 gap-1.5">
                {MOCK_PRESETS.map((p) => (
                  <button
                    key={p.meter}
                    type="button"
                    title={p.hint}
                    onClick={() => {
                      setMeterNum(p.meter);
                      setDisco(p.disco);
                      setMeterLabel(p.label);
                      setValidation(null);
                    }}
                    className={`rounded-lg border px-2.5 py-1.5 text-left text-xs transition ${
                      meterNum === p.meter
                        ? "border-emerald-500 bg-emerald-50/70 text-emerald-950 font-semibold"
                        : "border-stone-200 bg-stone-50 text-stone-700 hover:bg-stone-100"
                    }`}
                  >
                    <div className="truncate">{p.label}</div>
                    <div className="font-mono text-[10px] text-stone-500">
                      {p.disco} • {p.meter}
                    </div>
                  </button>
                ))}
              </div>
            </div>

            <form onSubmit={handleSaveMeter} className="space-y-3">
              <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
                <div>
                  <label className="block text-xs font-medium text-stone-700 mb-1">
                    DisCo
                  </label>
                  <select
                    value={disco}
                    onChange={(e) => setDisco(e.target.value)}
                    className="w-full rounded-xl border border-stone-300 bg-white px-3 py-2 text-sm focus:border-emerald-600 focus:outline-none"
                  >
                    {DISCOS.map((d) => (
                      <option key={d.code} value={d.code}>
                        {d.name}
                      </option>
                    ))}
                  </select>
                </div>
                <div>
                  <label className="block text-xs font-medium text-stone-700 mb-1">
                    Meter Number (11–13 digits)
                  </label>
                  <input
                    type="text"
                    value={meterNum}
                    onChange={(e) => setMeterNum(e.target.value)}
                    required
                    className="w-full rounded-xl border border-stone-300 px-3 py-2 font-mono text-sm focus:border-emerald-600 focus:outline-none"
                  />
                </div>
              </div>

              <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
                <div>
                  <label className="block text-xs font-medium text-stone-700 mb-1">
                    Nickname / Beneficiary Label
                  </label>
                  <input
                    type="text"
                    value={meterLabel}
                    onChange={(e) => setMeterLabel(e.target.value)}
                    placeholder="e.g. Home, Mum Ibadan"
                    className="w-full rounded-xl border border-stone-300 px-3 py-2 text-sm focus:border-emerald-600 focus:outline-none"
                  />
                </div>
                <div className="flex items-end pb-2">
                  <label className="flex items-center gap-2 text-xs text-stone-700 cursor-pointer">
                    <input
                      type="checkbox"
                      checked={isBeneficiary}
                      onChange={(e) => setIsBeneficiary(e.target.checked)}
                      className="rounded border-stone-300 text-emerald-600 focus:ring-emerald-500"
                    />
                    Save as someone else&apos;s meter (Beneficiary)
                  </label>
                </div>
              </div>

              {validation && (
                <div className="rounded-xl border border-emerald-200 bg-emerald-50/70 p-3 text-xs space-y-1.5">
                  <div className="flex items-center justify-between font-semibold text-emerald-950">
                    <span>
                      {validation.customer_name} ({validation.masked_meter_number})
                    </span>
                    <span className="font-mono">{validation.meter_type}</span>
                  </div>
                  <CapabilityBadges caps={validation.capabilities} />
                </div>
              )}

              <div className="flex gap-2 pt-1">
                <button
                  type="button"
                  disabled={validating}
                  onClick={handleValidateMeter}
                  className="rounded-xl border border-stone-300 bg-white px-3.5 py-2 text-xs font-semibold text-stone-800 hover:bg-stone-50"
                >
                  {validating ? "Checking DisCo..." : "Verify Identity"}
                </button>
                <button
                  type="submit"
                  className="flex-1 rounded-xl bg-[#0B1F1A] px-4 py-2 text-xs font-semibold text-white hover:bg-emerald-950"
                >
                  Save Meter
                </button>
              </div>
            </form>
          </section>

          {/* Buy Electricity Card */}
          <section className="rounded-2xl border border-stone-200 bg-white p-5 shadow-xs">
            <h2 className="text-base font-bold text-stone-900 flex items-center gap-2 mb-3">
              <Zap className="h-4 w-4 text-emerald-600" />
              Put credit on the meter
            </h2>

            <form onSubmit={handleBuyElectricity} className="space-y-3.5">
              <div>
                <label className="block text-xs font-medium text-stone-700 mb-1">
                  Select Saved Meter
                </label>
                {meters.length === 0 ? (
                  <p className="rounded-xl border border-dashed border-stone-300 p-3 text-xs text-stone-500">
                    No saved meters yet — save one above to start buying.
                  </p>
                ) : (
                  <select
                    value={selectedMeterId}
                    onChange={(e) => setSelectedMeterId(e.target.value)}
                    className="w-full rounded-xl border border-stone-300 bg-white px-3 py-2 text-sm focus:border-emerald-600 focus:outline-none"
                  >
                    {meters.map((m) => (
                      <option key={m.meter_id} value={m.meter_id}>
                        {m.label ? `${m.label} • ` : ""}
                        {m.masked_meter_number} ({m.disco})
                      </option>
                    ))}
                  </select>
                )}
              </div>

              {selectedMeter && (
                <div className="flex items-center justify-between rounded-xl bg-stone-50 px-3 py-2 border border-stone-200/80">
                  <CapabilityBadges caps={selectedMeter.capabilities} />
                  <button
                    type="button"
                    onClick={() => handleRemoveMeter(selectedMeter.meter_id)}
                    title="Remove saved meter access"
                    className="text-stone-400 hover:text-rose-600 p-1"
                  >
                    <Trash2 className="h-3.5 w-3.5" />
                  </button>
                </div>
              )}

              <div className="grid grid-cols-2 gap-3">
                <div>
                  <label className="block text-xs font-medium text-stone-700 mb-1">
                    Amount (₦ Naira)
                  </label>
                  <input
                    type="number"
                    min={100}
                    max={500000}
                    step={100}
                    value={amountNaira}
                    onChange={(e) => setAmountNaira(e.target.value)}
                    required
                    className="w-full rounded-xl border border-stone-300 px-3 py-2 font-mono text-sm tabular-nums focus:border-emerald-600 focus:outline-none"
                  />
                </div>
                <div>
                  <label className="block text-xs font-medium text-stone-700 mb-1">
                    How credit arrives
                  </label>
                  <select
                    value={deliveryPref}
                    onChange={(e) =>
                      setDeliveryPref(e.target.value as "AUTO" | "TOKEN" | "REMOTE")
                    }
                    className="w-full rounded-xl border border-stone-300 bg-white px-3 py-2 text-sm focus:border-emerald-600 focus:outline-none"
                  >
                    {selectedSupportsRemote ? (
                      <>
                        <option value="AUTO">On the meter (recommended)</option>
                        <option value="REMOTE">Force remote push</option>
                        <option value="TOKEN">STS token fallback</option>
                      </>
                    ) : (
                      <option value="TOKEN">STS token (this meter has no remote path)</option>
                    )}
                  </select>
                </div>
              </div>

              <div className="flex gap-1.5">
                {[2000, 5000, 10000, 25000].map((preset) => (
                  <button
                    key={preset}
                    type="button"
                    onClick={() => setAmountNaira(String(preset))}
                    className="flex-1 rounded-lg border border-stone-200 bg-stone-50 py-1 font-mono text-xs font-medium text-stone-700 hover:bg-stone-100"
                  >
                    ₦{preset.toLocaleString()}
                  </button>
                ))}
              </div>

              <div>
                <label className="block text-xs font-medium text-stone-700 mb-1">
                  Receipt Email (Required by Paystack)
                </label>
                <input
                  type="email"
                  value={buyerEmail}
                  onChange={(e) => setBuyerEmail(e.target.value)}
                  required
                  className="w-full rounded-xl border border-stone-300 px-3 py-2 text-sm focus:border-emerald-600 focus:outline-none"
                />
              </div>

              <div className="rounded-xl border border-stone-200 bg-stone-50/70 p-3 space-y-2.5">
                <label className="flex items-center gap-2 text-xs font-semibold text-stone-800 cursor-pointer">
                  <input
                    type="checkbox"
                    checked={isGift}
                    onChange={(e) => setIsGift(e.target.checked)}
                    className="rounded border-stone-300 text-emerald-600"
                  />
                  <Gift className="h-3.5 w-3.5 text-emerald-600" />
                  Buying as a Gift for someone else
                </label>

                {isGift && (
                  <div className="grid grid-cols-1 gap-2.5 sm:grid-cols-2 pt-1">
                    <input
                      type="text"
                      value={recipientName}
                      onChange={(e) => setRecipientName(e.target.value)}
                      placeholder="Recipient name (e.g. Mum)"
                      className="rounded-lg border border-stone-300 bg-white px-2.5 py-1.5 text-xs"
                    />
                    <input
                      type="tel"
                      value={recipientPhone}
                      onChange={(e) => setRecipientPhone(e.target.value)}
                      placeholder="+2348020001111 (encrypted)"
                      className="rounded-lg border border-stone-300 bg-white px-2.5 py-1.5 font-mono text-xs"
                    />
                    <label className="col-span-full flex items-center gap-2 text-[11px] text-stone-600">
                      <input
                        type="checkbox"
                        checked={notifyRecipient}
                        onChange={(e) => setNotifyRecipient(e.target.checked)}
                      />
                      Request gift SMS token delivery (sent only if FEATURE_GIFT_SMS is enabled & recipient has not opted out)
                    </label>
                  </div>
                )}
              </div>

              {selectedSupportsRemote && deliveryPref !== "TOKEN" && (
                <p className="text-[11px] text-emerald-800 bg-emerald-50 border border-emerald-200 rounded-lg px-3 py-2">
                  This meter can take remote credit. After payment we push units to it.
                  You will not get a token unless the push fails and an operator falls back.
                </p>
              )}
              {!selectedSupportsRemote && selectedMeter && (
                <p className="text-[11px] text-stone-600 bg-stone-50 border border-stone-200 rounded-lg px-3 py-2">
                  This meter is keypad-only. You will receive a 20-digit STS token to type in.
                  Smart meters (sandbox numbers starting with 7) skip the keypad.
                </p>
              )}

              <button
                type="submit"
                disabled={purchasing || !selectedMeterId}
                className="w-full rounded-xl bg-emerald-600 px-4 py-3 text-sm font-semibold text-white shadow-xs transition hover:bg-emerald-700 disabled:opacity-50"
              >
                {purchasing
                  ? "Pricing…"
                  : selectedSupportsRemote && deliveryPref !== "TOKEN"
                    ? `Push ₦${Number(amountNaira).toLocaleString()} to the meter`
                    : `Pay for STS token (${formatNaira(
                        Math.round((Number(amountNaira) || 0) * 100) + 10000
                      )} incl. fee)`}
              </button>
            </form>
          </section>

          {/* Preferences & Offline Phone Linking */}
          <section className="rounded-2xl border border-stone-200 bg-white p-5 shadow-xs space-y-4">
            <div className="flex items-center justify-between">
              <h3 className="text-sm font-bold text-stone-900 flex items-center gap-2">
                <Bell className="h-4 w-4 text-emerald-600" />
                Notification Channels & Offline Phone Link
              </h3>
            </div>

            {prefs && (
              <div className="flex flex-wrap gap-2">
                {(["sms", "email", "whatsapp"] as const).map((ch) => (
                  <button
                    key={ch}
                    type="button"
                    onClick={() => handleTogglePref(ch)}
                    className={`rounded-xl border px-3 py-1.5 text-xs font-medium transition ${
                      prefs[ch]
                        ? "border-emerald-500 bg-emerald-50 text-emerald-900"
                        : "border-stone-200 bg-stone-50 text-stone-500"
                    }`}
                  >
                    {ch.toUpperCase()}: {prefs[ch] ? "Enabled" : "Off"}
                  </button>
                ))}
              </div>
            )}

            <div className="border-t border-stone-100 pt-3 space-y-2">
              <p className="text-xs font-medium text-stone-700 flex items-center gap-1.5">
                <Phone className="h-3.5 w-3.5 text-stone-500" />
                Link Phone via SMS OTP (for *347*88# USSD & WhatsApp)
              </p>
              <div className="flex gap-2">
                <input
                  type="tel"
                  value={linkPhone}
                  onChange={(e) => setLinkPhone(e.target.value)}
                  className="flex-1 rounded-xl border border-stone-300 px-3 py-1.5 font-mono text-xs"
                  placeholder="+2348031234567"
                />
                <button
                  type="button"
                  onClick={handleStartPhoneLink}
                  className="rounded-xl border border-stone-300 px-3 py-1.5 text-xs font-semibold hover:bg-stone-50"
                >
                  Send OTP
                </button>
              </div>
              {otpSentMasked && (
                <div className="flex gap-2 pt-1">
                  <input
                    type="text"
                    maxLength={6}
                    value={otpCode}
                    onChange={(e) => setOtpCode(e.target.value)}
                    placeholder={`6-digit code sent to ${otpSentMasked}`}
                    className="flex-1 rounded-xl border border-emerald-400 px-3 py-1.5 font-mono text-xs"
                  />
                  <button
                    type="button"
                    onClick={handleVerifyPhoneLink}
                    className="rounded-xl bg-emerald-600 px-3 py-1.5 text-xs font-semibold text-white"
                  >
                    Verify
                  </button>
                </div>
              )}
            </div>
          </section>
        </div>

        {/* Right Column: Honest 3-Dimension Transaction Ledger */}
        <div className="lg:col-span-7 space-y-4">
          <div className="flex items-center justify-between">
            <h2 className="text-lg font-bold text-stone-900">
              Is it on the meter yet?
            </h2>
            <span className="font-mono text-xs text-stone-500">
              {txns.length} record{txns.length === 1 ? "" : "s"}
            </span>
          </div>

          {txns.length === 0 ? (
            <div className="rounded-2xl border border-dashed border-stone-300 bg-white p-10 text-center space-y-2">
              <Zap className="mx-auto h-8 w-8 text-stone-300" />
              <p className="text-sm font-medium text-stone-700">
                No purchases yet.
              </p>
              <p className="text-xs text-stone-500">
                Save the smart-meter preset (700…) and pay — credit is pushed to the meter, not typed.
              </p>
            </div>
          ) : (
            <div className="space-y-3.5">
              {txns.map((t) => {
                const meterObj = meters.find((m) => m.meter_id === t.meter_id);
                const revealed = revealedTokens[t.transaction_id];
                const canRevealToken =
                  t.delivery_method === "TOKEN" &&
                  (t.token_available === true ||
                    t.vending_status === "CONFIRMED");

                return (
                  <div
                    key={t.transaction_id}
                    className="rounded-2xl border border-stone-200 bg-white p-5 shadow-xs space-y-3.5"
                  >
                    <div className="flex flex-wrap items-start justify-between gap-2">
                      <div>
                        <div className="flex items-center gap-2">
                          <span className="font-mono text-base font-bold text-stone-900 tabular-nums">
                            {formatNaira(t.amount_minor)}
                          </span>
                          <span className="text-xs text-stone-500">
                            (+{formatNaira(t.fee_minor)} fee)
                          </span>
                          <StatusPill status={t.status} />
                          {t.is_gift && (
                            <span className="rounded-full bg-amber-50 px-2 py-0.5 text-[11px] font-medium text-amber-800 ring-1 ring-amber-500/20">
                              Gift{t.recipient_name ? `: ${t.recipient_name}` : ""}
                            </span>
                          )}
                          {t.credit_on_meter && (
                            <span className="rounded-full bg-emerald-50 px-2 py-0.5 text-[11px] font-semibold text-emerald-900 ring-1 ring-emerald-600/25">
                              On the meter
                            </span>
                          )}
                          {t.delivery_method === "REMOTE" &&
                            !t.credit_on_meter &&
                            t.delivery_status === "ACCEPTED" && (
                              <span className="rounded-full bg-sky-50 px-2 py-0.5 text-[11px] font-semibold text-sky-900 ring-1 ring-sky-600/25">
                                Pushing — not confirmed
                              </span>
                            )}
                        </div>
                        <p className="mt-0.5 text-xs text-stone-500 font-mono">
                          {meterObj
                            ? `${meterObj.masked_meter_number} (${meterObj.disco})`
                            : `Meter ${t.meter_id.slice(0, 8)}…`}{" "}
                          • {t.delivery_method}
                          {t.units_kwh > 0 ? ` • ${t.units_kwh.toFixed(1)} kWh` : ""}
                        </p>
                      </div>
                      <span className="font-mono text-[11px] text-stone-400">
                        {new Date(t.created_at).toLocaleString()}
                      </span>
                    </div>

                    <ThreeDimensionStatus
                      payment={t.payment_status}
                      vending={t.vending_status}
                      delivery={t.delivery_status}
                      method={t.delivery_method}
                    />

                    {t.review_reason && (
                      <p className="rounded-lg bg-amber-50 border border-amber-200 px-3 py-1.5 text-xs text-amber-900">
                        Review note: {t.review_reason}
                      </p>
                    )}

                    {t.delivery_method === "REMOTE" && (
                      <div
                        className={`rounded-xl border p-3.5 text-xs space-y-1 ${
                          t.credit_on_meter
                            ? "border-emerald-300 bg-emerald-50/70 text-emerald-950"
                            : t.delivery_status === "FAILED"
                              ? "border-rose-200 bg-rose-50 text-rose-950"
                              : "border-sky-200 bg-sky-50/80 text-sky-950"
                        }`}
                      >
                        <p className="font-semibold uppercase tracking-wider text-[11px]">
                          Credit on the meter
                        </p>
                        {t.credit_on_meter || t.delivery_status === "CONFIRMED" ? (
                          <p>
                            The meter confirmed the units. There is no keypad token
                            for this purchase — copy/send stay blocked.
                          </p>
                        ) : t.delivery_status === "ACCEPTED" ? (
                          <p>
                            Pushed to the meter network. ACCEPTED ≠ delivered.
                            Waiting for the meter to confirm (sandbox: use the
                            button below).
                          </p>
                        ) : t.delivery_status === "FAILED" ? (
                          <p>
                            Remote push failed. An operator can release the STS
                            token only after confirming the HES did not credit the
                            meter.
                          </p>
                        ) : (
                          <p>Remote delivery status: {t.delivery_status}.</p>
                        )}
                      </div>
                    )}

                    {/* Revealed STS Token Box */}
                    {revealed && t.delivery_method !== "REMOTE" && (
                      <div className="rounded-xl border border-emerald-300 bg-emerald-50/70 p-3.5 space-y-2">
                        <div className="flex items-center justify-between">
                          <span className="text-[11px] font-semibold uppercase tracking-wider text-emerald-900">
                            20-Digit STS Prepaid Token ({revealed.masked_meter_number})
                          </span>
                          <button
                            type="button"
                            onClick={() =>
                              handleCopyToken(t.transaction_id, revealed.token)
                            }
                            className="inline-flex items-center gap-1 rounded-lg bg-emerald-700 px-2.5 py-1 text-xs font-semibold text-white hover:bg-emerald-800"
                          >
                            {copiedId === t.transaction_id ? (
                              <>
                                <Check className="h-3.5 w-3.5" /> Copied
                              </>
                            ) : (
                              <>
                                <Copy className="h-3.5 w-3.5" /> Copy Token
                              </>
                            )}
                          </button>
                        </div>
                        <p className="font-mono text-xl font-bold tracking-wider text-stone-950 tabular-nums">
                          {formatSTSToken(revealed.token)}
                        </p>
                        <p className="text-[11px] text-emerald-900/80">
                          {revealed.note}
                        </p>
                        <div className="border-t border-emerald-200/80 pt-2 space-y-2">
                          <p className="text-[11px] font-semibold text-emerald-950">
                            Send this token (self = your verified contact; other number needs gift SMS legal flag)
                          </p>
                          <input
                            value={sendDest[t.transaction_id] || ""}
                            onChange={(e) =>
                              setSendDest((prev) => ({
                                ...prev,
                                [t.transaction_id]: e.target.value,
                              }))
                            }
                            placeholder="Leave empty for yourself, or +234… / email"
                            className="w-full rounded-lg border border-emerald-200 bg-white px-2.5 py-1.5 text-xs font-mono"
                          />
                          <div className="flex flex-wrap gap-1.5">
                            {(["SMS", "EMAIL", "WHATSAPP"] as const).map((ch) => (
                              <button
                                key={ch}
                                type="button"
                                disabled={sendBusy === `${t.transaction_id}:${ch}`}
                                onClick={() => handleSendToken(t.transaction_id, ch)}
                                className="rounded-lg bg-white px-2.5 py-1 text-[11px] font-semibold text-emerald-900 ring-1 ring-emerald-600/25 hover:bg-emerald-100 disabled:opacity-50"
                              >
                                {ch}
                              </button>
                            ))}
                          </div>
                          {(tokenShares[t.transaction_id] || []).length > 0 && (
                            <ul className="text-[11px] text-emerald-950/80 space-y-0.5">
                              {(tokenShares[t.transaction_id] || []).map((s) => (
                                <li key={s.share_id} className="font-mono">
                                  {s.channel}
                                  {s.destination_masked ? ` → ${s.destination_masked}` : ""}{" "}
                                  · {new Date(s.recorded_at).toLocaleString()}
                                </li>
                              ))}
                            </ul>
                          )}
                        </div>
                      </div>
                    )}

                    {/* Save Card Consent Drawer */}
                    {saveCardTxnId === t.transaction_id && (
                      <div className="rounded-xl border border-stone-200 bg-stone-50 p-3 space-y-2">
                        <label className="flex items-start gap-2 text-xs text-stone-700 cursor-pointer">
                          <input
                            type="checkbox"
                            checked={cardConsent}
                            onChange={(e) => setCardConsent(e.target.checked)}
                            className="mt-0.5 rounded border-stone-300 text-emerald-600"
                          />
                          <span>
                            I explicitly consent to saving the reusable card authorization from this payment for scheduled or low-balance AutoPower recharges within my spend caps.
                          </span>
                        </label>
                        <div className="flex gap-2">
                          <button
                            type="button"
                            onClick={() => handleSaveCard(t.transaction_id)}
                            className="rounded-lg bg-emerald-600 px-3 py-1.5 text-xs font-semibold text-white"
                          >
                            Confirm & Encrypt Card Mandate
                          </button>
                          <button
                            type="button"
                            onClick={() => setSaveCardTxnId(null)}
                            className="rounded-lg border border-stone-300 bg-white px-3 py-1.5 text-xs text-stone-600"
                          >
                            Cancel
                          </button>
                        </div>
                      </div>
                    )}

                    {/* Action Bar */}
                    <div className="flex flex-wrap items-center gap-2 pt-1 border-t border-stone-100">
                      {t.payment_status === "PENDING" && t.checkout_url && (
                        <button
                          type="button"
                          onClick={() =>
                            openCheckout({
                              checkoutUrl: t.checkout_url!,
                              totalMinor: t.total_minor,
                              title: "Complete Pending Checkout",
                              onComplete: loadAll,
                            })
                          }
                          className="rounded-lg bg-emerald-600 px-3 py-1.5 text-xs font-semibold text-white hover:bg-emerald-700"
                        >
                          Complete Payment
                        </button>
                      )}

                      {canRevealToken && !revealed && (
                        <button
                          type="button"
                          onClick={() => handleRevealToken(t.transaction_id)}
                          className="rounded-lg bg-[#0B1F1A] px-3 py-1.5 text-xs font-semibold text-white hover:bg-emerald-950"
                        >
                          Reveal STS Token
                        </button>
                      )}

                      {t.delivery_method === "REMOTE" &&
                        t.delivery_status === "ACCEPTED" && (
                          <button
                            type="button"
                            onClick={() => handleSettleRemoteDev(t.transaction_id)}
                            className="rounded-lg bg-sky-600 px-3 py-1.5 text-xs font-semibold text-white hover:bg-sky-700"
                          >
                            Meter confirmed (sandbox)
                          </button>
                        )}

                      {(t.vending_status === "UNKNOWN" ||
                        t.delivery_status === "UNKNOWN") && (
                        <button
                          type="button"
                          onClick={() => handleResumeDev(t.transaction_id)}
                          className="rounded-lg bg-amber-600 px-3 py-1.5 text-xs font-semibold text-white hover:bg-amber-700"
                        >
                          Reconcile UNKNOWN Status
                        </button>
                      )}

                      <button
                        type="button"
                        onClick={() => handleOpenHTMLReceipt(t.transaction_id)}
                        className="inline-flex items-center gap-1 rounded-lg border border-stone-200 px-2.5 py-1.5 text-xs font-medium text-stone-700 hover:bg-stone-50"
                      >
                        <FileText className="h-3.5 w-3.5" /> Printable Receipt
                      </button>

                      {t.payment_status === "CONFIRMED" && (
                        <button
                          type="button"
                          onClick={() => {
                            setSaveCardTxnId(t.transaction_id);
                            setCardConsent(false);
                          }}
                          className="inline-flex items-center gap-1 rounded-lg border border-stone-200 px-2.5 py-1.5 text-xs font-medium text-stone-700 hover:bg-stone-50"
                        >
                          <CreditCard className="h-3.5 w-3.5" /> Save Card for AutoPower
                        </button>
                      )}
                    </div>
                  </div>
                );
              })}
            </div>
          )}
        </div>
      </div>
    </div>
  );
}
