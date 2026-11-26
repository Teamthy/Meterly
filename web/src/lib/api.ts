export class ApiError extends Error {
  code: string;
  status: number;
  requestId?: string;

  constructor(status: number, code: string, message: string, requestId?: string) {
    super(message);
    this.name = "ApiError";
    this.status = status;
    this.code = code;
    this.requestId = requestId;
  }
}

export function newIdempotencyKey(prefix = "web"): string {
  if (typeof crypto !== "undefined" && typeof crypto.randomUUID === "function") {
    return `${prefix}-${crypto.randomUUID()}`;
  }
  return `${prefix}-${Date.now()}-${Math.random().toString(36).slice(2, 12)}`;
}

let defaultIdentityHeaders: Record<string, string> = {};

/** Dev OIDC-stand-in: verified email/phone/name captured on every authed call. */
export function setDefaultIdentityHeaders(headers: Record<string, string>) {
  defaultIdentityHeaders = { ...headers };
}

export async function apiFetch<T>(
  path: string,
  options: {
    method?: string;
    token?: string;
    body?: unknown;
    idempotencyKey?: string;
    headers?: Record<string, string>;
  } = {}
): Promise<T> {
  const headers: Record<string, string> = {
    ...defaultIdentityHeaders,
    ...(options.headers || {}),
  };
  if (options.token) {
    headers["Authorization"] = `Bearer ${options.token}`;
  }
  if (options.idempotencyKey) {
    headers["Idempotency-Key"] = options.idempotencyKey;
  }
  let bodyPayload: BodyInit | undefined;
  if (options.body !== undefined) {
    headers["Content-Type"] = "application/json";
    bodyPayload = JSON.stringify(options.body);
  }

  const res = await fetch(path, {
    method: options.method || "GET",
    headers,
    body: bodyPayload,
    cache: "no-store",
  });

  if (res.status === 204) {
    return undefined as T;
  }

  const contentType = res.headers.get("content-type") || "";
  if (!contentType.includes("application/json")) {
    const text = await res.text();
    if (!res.ok) {
      throw new ApiError(res.status, "HTTP_ERROR", text || res.statusText);
    }
    return text as unknown as T;
  }

  const json = await res.json();
  if (!res.ok) {
    const errObj = json?.error || {};
    throw new ApiError(
      res.status,
      errObj.code || "API_ERROR",
      errObj.message || res.statusText,
      errObj.request_id
    );
  }
  return json as T;
}

export function formatNaira(minor: number | undefined | null): string {
  const safe = typeof minor === "number" && !Number.isNaN(minor) ? minor : 0;
  const naira = safe / 100;
  return new Intl.NumberFormat("en-NG", {
    style: "currency",
    currency: "NGN",
    minimumFractionDigits: safe % 100 === 0 ? 0 : 2,
    maximumFractionDigits: 2,
  }).format(naira);
}

export function formatSTSToken(raw: string): string {
  const digits = raw.replace(/\D/g, "");
  if (digits.length === 20) {
    return digits.match(/.{1,4}/g)!.join("-");
  }
  return raw;
}

export function extractQuoteId(q: Record<string, unknown>): string {
  return String(q.quote_id || q.ID || q.id || "");
}

export function extractQuoteTotalMinor(q: Record<string, unknown>): number {
  if (typeof q.total_minor === "number") return q.total_minor;
  const tot = q.Total as { Minor?: number } | undefined;
  return tot?.Minor ?? 0;
}

export function extractQuoteFeeMinor(q: Record<string, unknown>): number {
  if (typeof q.fee_minor === "number") return q.fee_minor;
  const fee = q.Fee as { Minor?: number } | undefined;
  return fee?.Minor ?? 10000;
}

export function extractQuoteUnits(q: Record<string, unknown>): number {
  if (typeof q.estimated_units_kwh === "number") return q.estimated_units_kwh;
  if (typeof q.EstimatedUnitsKWh === "number") return q.EstimatedUnitsKWh;
  return 0;
}

export function extractMockPayRef(checkoutUrl?: string): string | null {
  if (!checkoutUrl) return null;
  if (checkoutUrl.startsWith("https://checkout.mock/")) {
    return checkoutUrl.slice("https://checkout.mock/".length).trim();
  }
  return null;
}

export function toRelativeSharePath(shareUrl?: string, fallbackPrefix?: string, code?: string): string {
  if (code && fallbackPrefix) {
    return `${fallbackPrefix}/${encodeURIComponent(code)}`;
  }
  if (!shareUrl) return "/";
  try {
    const u = new URL(shareUrl);
    return u.pathname;
  } catch {
    return shareUrl.startsWith("/") ? shareUrl : `/${shareUrl}`;
  }
}

export const DISCOS = [
  { code: "IKEDC", name: "Ikeja Electric (IKEDC)" },
  { code: "EKEDC", name: "Eko Electricity (EKEDC)" },
  { code: "AEDC", name: "Abuja Electricity (AEDC)" },
  { code: "IBEDC", name: "Ibadan Electricity (IBEDC)" },
  { code: "EEDC", name: "Enugu Electricity (EEDC)" },
  { code: "PHED", name: "Port Harcourt Electric (PHED)" },
  { code: "KEDCO", name: "Kano Electricity (KEDCO)" },
  { code: "KAEDCO", name: "Kaduna Electric (KAEDCO)" },
  { code: "JED", name: "Jos Electricity (JED)" },
  { code: "YEDC", name: "Yola Electricity (YEDC)" },
] as const;
