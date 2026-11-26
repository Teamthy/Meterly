import type { Metadata } from "next";
import "./globals.css";
import { AuthProvider } from "@/components/AuthContext";
import { AppShell } from "@/components/AppShell";

export const metadata: Metadata = {
  title: "Meterly — Nigerian Prepaid Electricity & Smart Remote Delivery",
  description:
    "Buy prepaid electricity from anywhere: smart remote meter delivery, verified STS token fallback, Pay-for-Me links, Split the Meter pots, AutoPower, and Landlord Power Desk.",
};

export default function RootLayout({
  children,
}: {
  children: React.ReactNode;
}) {
  return (
    <html lang="en">
      <body>
        <AuthProvider>
          <AppShell>{children}</AppShell>
        </AuthProvider>
      </body>
    </html>
  );
}
