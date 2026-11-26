"use client";

import React from "react";
import { Capabilities } from "@/lib/types";

function toneClass(status: string): string {
  const s = (status || "").toUpperCase();
  if (
    s === "DELIVERED" ||
    s === "CONFIRMED" ||
    s === "SUCCEEDED" ||
    s === "PAID" ||
    s === "SETTLED" ||
    s === "ACTIVE" ||
    s === "VERIFIED"
  ) {
    return "bg-emerald-50 text-emerald-800 ring-1 ring-emerald-600/25";
  }
  if (s === "ACCEPTED") {
    // Non-negotiable product truth: ACCEPTED != DELIVERED
    return "bg-sky-50 text-sky-900 ring-1 ring-sky-600/30";
  }
  if (
    s === "UNKNOWN" ||
    s === "MANUAL_REVIEW" ||
    s === "PENDING_APPROVAL" ||
    s === "REVERIFY_REQUIRED"
  ) {
    return "bg-amber-50 text-amber-900 ring-1 ring-amber-600/35";
  }
  if (
    s === "FAILED" ||
    s === "FAILED_REFUNDED" ||
    s === "CANCELLED" ||
    s === "EXPIRED" ||
    s === "PAUSED" ||
    s === "STOPPED"
  ) {
    return "bg-rose-50 text-rose-800 ring-1 ring-rose-600/25";
  }
  if (s === "REFUNDED" || s === "REFUND_PENDING" || s === "REFUNDING") {
    return "bg-purple-50 text-purple-800 ring-1 ring-purple-600/25";
  }
  return "bg-stone-100 text-stone-700 ring-1 ring-stone-400/30";
}

export function StatusPill({
  status,
  prefix,
}: {
  status: string;
  prefix?: string;
}) {
  return (
    <span
      className={`inline-flex items-center gap-1.5 rounded-full px-2.5 py-0.5 text-xs font-medium tracking-tight ${toneClass(
        status
      )}`}
    >
      {prefix ? <span className="opacity-70">{prefix}:</span> : null}
      <span className="font-mono font-semibold">{status || "—"}</span>
    </span>
  );
}

export function ThreeDimensionStatus({
  payment,
  vending,
  delivery,
  method,
}: {
  payment: string;
  vending: string;
  delivery: string;
  method?: string;
}) {
  return (
    <div className="space-y-1.5">
      <div className="flex flex-wrap items-center gap-1.5">
        <StatusPill prefix="Pay" status={payment} />
        <StatusPill prefix="Vend" status={vending} />
        <StatusPill
          prefix={method === "REMOTE" ? "Remote" : "Deliver"}
          status={delivery}
        />
      </div>
      {delivery === "ACCEPTED" && (
        <p className="text-xs font-medium text-sky-800 bg-sky-50/80 border border-sky-200 rounded-md px-2.5 py-1">
          Accepted by meter network — awaiting physical meter confirmation. Not yet delivered.
        </p>
      )}
      {(vending === "UNKNOWN" || delivery === "UNKNOWN") && (
        <p className="text-xs font-medium text-amber-900 bg-amber-50/90 border border-amber-200 rounded-md px-2.5 py-1">
          Ambiguous provider response (UNKNOWN). Meterly is reconciling status before any retry and will never auto-refund while electricity may have vended.
        </p>
      )}
    </div>
  );
}

export function CapabilityBadges({ caps }: { caps?: Capabilities }) {
  if (!caps) return null;
  const remoteUsable =
    caps.remote_credit.supported &&
    caps.remote_credit.authorised &&
    caps.remote_credit.reachable !== false;
  const readingConfirmed =
    caps.remote_reading.supported &&
    caps.remote_reading.authorised &&
    caps.remote_reading.confirmed &&
    caps.remote_reading.reachable !== false;

  return (
    <div className="flex flex-wrap gap-1.5 text-xs">
      <span className="rounded-md bg-stone-100 px-2 py-0.5 font-medium text-stone-700">
        STS Token Ready
      </span>
      {remoteUsable ? (
        <span className="rounded-md bg-emerald-50 px-2 py-0.5 font-medium text-emerald-800 ring-1 ring-emerald-600/20">
          Smart Remote Credit {caps.remote_credit.confirmed ? "(Confirmed)" : "(Eligible)"}
        </span>
      ) : (
        <span className="rounded-md bg-stone-100 px-2 py-0.5 text-stone-500">
          Keypad Token Only
        </span>
      )}
      {readingConfirmed && (
        <span className="rounded-md bg-teal-50 px-2 py-0.5 font-medium text-teal-800 ring-1 ring-teal-600/20">
          Live kWh Reading
        </span>
      )}
    </div>
  );
}
