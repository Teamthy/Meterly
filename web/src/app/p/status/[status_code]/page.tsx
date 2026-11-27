"use client";

import React, { use, useCallback, useEffect, useState } from "react";
import { apiFetch, formatNaira } from "@/lib/api";
import { PublicPayStatusView } from "@/lib/types";
import { StatusPill, ThreeDimensionStatus } from "@/components/StatusBadges";
import { CheckCircle2, RefreshCw } from "lucide-react";

export default function GuestPayStatusPage({
  params,
}: {
  params: Promise<{ status_code: string }>;
}) {
  const { status_code } = use(params);
  const [data, setData] = useState<PublicPayStatusView | null>(null);
  const [err, setErr] = useState<string | null>(null);

  const loadStatus = useCallback(async () => {
    try {
      const res = await apiFetch<PublicPayStatusView>(
        `/v1/public/pay/status/${encodeURIComponent(status_code)}`
      );
      setData(res);
    } catch (e) {
      setErr(e instanceof Error ? e.message : "Could not load status");
    }
  }, [status_code]);

  useEffect(() => {
    loadStatus();
  }, [loadStatus]);

  if (err && !data) {
    return (
      <div className="mx-auto max-w-lg rounded-2xl border border-rose-200 bg-white p-8 text-center text-sm text-rose-700">
        {err}
      </div>
    );
  }

  if (!data) {
    return (
      <div className="mx-auto max-w-lg ml-card p-8 text-center text-sm text-stone-500">
        Loading payment status…
      </div>
    );
  }

  const receipt = data.receipt || {};

  return (
    <div className="mx-auto max-w-lg ml-card p-6 shadow-xs space-y-4">
      <div className="flex items-center justify-between">
        <div className="flex items-center gap-2">
          <CheckCircle2 className="h-5 w-5 text-ink" />
          <h1 className="text-lg font-bold text-stone-900">
            Guest Payment Receipt
          </h1>
        </div>
        <StatusPill status={data.status} />
      </div>

      <ThreeDimensionStatus
        payment={data.payment_status}
        vending={data.vending_status}
        delivery={data.delivery_status}
        method={data.delivery_method}
      />

      <div className="rounded-xl bg-stone-50 border border-stone-200 p-4 space-y-2 text-xs">
        <div className="flex justify-between">
          <span className="text-stone-500">Meter</span>
          <span className="font-mono font-semibold">
            {String(receipt.masked_meter_number || "—")} (
            {String(receipt.disco || "—")})
          </span>
        </div>
        <div className="flex justify-between">
          <span className="text-stone-500">Electricity Amount</span>
          <span className="font-mono font-semibold">
            {formatNaira(Number(receipt.amount_minor || 0))}
          </span>
        </div>
        <div className="flex justify-between">
          <span className="text-stone-500">Total Paid</span>
          <span className="font-mono font-bold text-stone-900">
            {formatNaira(Number(receipt.total_minor || 0))}
          </span>
        </div>
      </div>

      <p className="text-xs text-stone-500 leading-relaxed">
        Token-free payer receipt: The electricity credit (or STS keypad token) is delivered directly to the requester&apos;s Meterly account.
      </p>

      <button
        type="button"
        onClick={loadStatus}
        className="inline-flex items-center gap-1.5 rounded-xl border border-stone-300 px-3.5 py-2 text-xs font-semibold text-stone-700 hover:bg-stone-50"
      >
        <RefreshCw className="h-3.5 w-3.5" /> Refresh Status
      </button>
    </div>
  );
}
