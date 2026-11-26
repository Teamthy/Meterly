import { NextRequest, NextResponse } from "next/server";
import crypto from "crypto";

const apiOrigin = (
  process.env.METERLY_API_ORIGIN || "http://127.0.0.1:8080"
).replace(/\/+$/, "");

export async function POST(req: NextRequest) {
  if (
    process.env.NODE_ENV === "production" &&
    process.env.ENABLE_OFFLINE_SIMULATOR !== "true"
  ) {
    return NextResponse.json(
      { error: { code: "FEATURE_DISABLED", message: "Simulator is dev-only" } },
      { status: 404 }
    );
  }

  const body = await req.json();
  const channel = String(body.channel || "").toUpperCase();

  if (channel === "USSD") {
    const secret = process.env.USSD_SHARED_SECRET || "dev-ussd-secret";
    const params = new URLSearchParams({
      sessionId: String(body.sessionId || "sim-1"),
      serviceCode: String(body.serviceCode || "*347*88#"),
      phoneNumber: String(body.phoneNumber || "+2348031234567"),
      text: String(body.text || ""),
    });
    const raw = params.toString();
    const sig =
      "sha256=" +
      crypto.createHmac("sha256", secret).update(raw, "utf8").digest("hex");

    const upstream = await fetch(`${apiOrigin}/v1/channels/ussd/inbound`, {
      method: "POST",
      headers: {
        "Content-Type": "application/x-www-form-urlencoded",
        "X-USSD-Signature": sig,
      },
      body: raw,
      cache: "no-store",
    });
    const reply = await upstream.text();
    return NextResponse.json(
      {
        status: upstream.status,
        reply,
        charCount: reply.length,
      },
      { status: upstream.ok ? 200 : upstream.status }
    );
  }

  if (channel === "WHATSAPP") {
    const appSecret =
      process.env.WHATSAPP_APP_SECRET || "dev-whatsapp-app-secret";
    const fromDigits = String(body.phoneNumber || "+2348031234567").replace(
      /^\+/,
      ""
    );
    const payloadObj = {
      object: "whatsapp_business_account",
      entry: [
        {
          id: "waba-dev",
          changes: [
            {
              field: "messages",
              value: {
                messages: [
                  {
                    from: fromDigits,
                    id: `wamid.sim.${Date.now()}.${Math.random()
                      .toString(36)
                      .slice(2, 8)}`,
                    timestamp: String(Math.floor(Date.now() / 1000)),
                    type: "text",
                    text: { body: String(body.text || "") },
                  },
                ],
              },
            },
          ],
        },
      ],
    };
    const raw = JSON.stringify(payloadObj);
    const sig =
      "sha256=" +
      crypto.createHmac("sha256", appSecret).update(raw, "utf8").digest("hex");

    const upstream = await fetch(`${apiOrigin}/v1/channels/whatsapp/inbound`, {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        "X-Hub-Signature-256": sig,
      },
      body: raw,
      cache: "no-store",
    });
    const data = await upstream.json().catch(() => ({}));
    return NextResponse.json(data, { status: upstream.status });
  }

  return NextResponse.json(
    { error: { code: "INVALID_REQUEST", message: "Unknown channel" } },
    { status: 400 }
  );
}
